package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
)

// doctorSessionID is the session `loopguard doctor` sends its synthetic
// event as; it is never stopped.
const doctorSessionID = "loopguard-doctor"

// StopRequest is the body of POST /v1/stop: a session id or its first
// characters, or all.
type StopRequest struct {
	Session string `json:"session,omitempty"`
	All     bool   `json:"all,omitempty"`
}

// StopTarget is a session a stop request named.
type StopTarget struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project,omitempty"` // the session's working directory
	Mode      string `json:"mode"`
	ModeWhy   string `json:"mode_why,omitempty"`
}

// StopReply is the answer to POST /v1/stop: the sessions whose turn will
// stop at their next tool call, and those that observe (where a stop
// doesn't apply).
type StopReply struct {
	Stopped []StopTarget `json:"stopped"`
	Refused []StopTarget `json:"refused"`
	Error   string       `json:"error,omitempty"`
	Matches []string     `json:"matches,omitempty"` // an ambiguous prefix's sessions
}

// stop handles `loopguard stop`: it marks the named live sessions so that
// their next tool event gets the halt reply (LLD §7.1). 404: no live
// session matches; 409: the prefix matches several.
func (d *server) stop(w http.ResponseWriter, r *http.Request) {
	var req StopRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || (req.Session == "") == !req.All {
		http.Error(w, "give a session or all", http.StatusBadRequest)
		return
	}
	byID := map[string]StopTarget{}
	for key, s := range d.mgr.Live() {
		if strings.Contains(key, "/") || s.SessionID == doctorSessionID || !s.EndedAt.IsZero() {
			continue // subagent streams stop with their session
		}
		if req.All || strings.HasPrefix(s.SessionID, req.Session) {
			byID[s.SessionID] = StopTarget{SessionID: s.SessionID, Project: s.Cwd, Mode: s.Mode, ModeWhy: s.ModeWhy}
		}
	}
	reply := StopReply{Stopped: []StopTarget{}, Refused: []StopTarget{}}
	if !req.All {
		if t, ok := byID[req.Session]; ok {
			byID = map[string]StopTarget{req.Session: t} // an exact id wins
		}
		switch len(byID) {
		case 0:
			reply.Error = "no active session id starts with " + req.Session
			writeJSON(w, http.StatusNotFound, reply)
			return
		case 1:
		default:
			reply.Error = req.Session + " matches several active sessions"
			for id := range byID {
				reply.Matches = append(reply.Matches, id)
			}
			sort.Strings(reply.Matches)
			writeJSON(w, http.StatusConflict, reply)
			return
		}
	}
	now := time.Now()
	for _, t := range byID {
		if config.LevelRank(t.Mode) <= config.LevelRank(config.ModeObserve) {
			reply.Refused = append(reply.Refused, t)
			continue
		}
		d.mgr.stops.request(t.SessionID, now)
		reply.Stopped = append(reply.Stopped, t)
		d.log.Info("stop requested", "session", t.SessionID)
	}
	for _, ts := range [][]StopTarget{reply.Stopped, reply.Refused} {
		sort.Slice(ts, func(i, j int) bool { return ts[i].SessionID < ts[j].SessionID })
	}
	writeJSON(w, http.StatusOK, reply)
}

// interactive reports whether a hook event comes from a terminal session
// a human attends, from what the shim forwards of Claude Code's
// environment: CLAUDE_CODE_ENTRYPOINT is "cli" for the terminal app (other
// values: SDK, IDE, desktop), and CLAUDE_CODE_SESSION_ATTENDED isn't "0".
// An older shim forwards nothing: not interactive.
func interactive(q url.Values) bool {
	return q.Get("entry") == "cli" && q.Get("attended") != "0"
}
