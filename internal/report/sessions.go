package report

import (
	"context"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// SessionRow is one agent session (its main stream and subagents) in the
// dashboard's session list.
type SessionRow struct {
	SessionID      string    `json:"session_id"`
	Project        string    `json:"project"` // the repo root, else the working directory
	Cwd            string    `json:"cwd,omitempty"`
	Repo           string    `json:"repo,omitempty"`
	Keys           []string  `json:"keys"` // the streams' session keys, main first
	StartedAt      time.Time `json:"started_at"`
	LastSeenAt     time.Time `json:"last_seen_at"`
	Ended          bool      `json:"ended"`
	ToolCalls      int64     `json:"tool_calls"`
	Failures       int64     `json:"failures"`
	Loops          int       `json:"loops"` // as the summary counts them (LoopSpans)
	Tokens         int64     `json:"tokens"`
	CostUSD        float64   `json:"cost_usd"`
	UnpricedTokens int64     `json:"unpriced_tokens,omitempty"`
	// ActiveSeconds is the time the session was active (ActiveDuration).
	ActiveSeconds float64 `json:"active_seconds"`
	// Transcript is the main stream's transcript, as the hooks gave it.
	Transcript string `json:"transcript,omitempty"`
	// UsageIncomplete: the transcript is gone and LoopGuard never read it,
	// so the usage (and cost) shown lacks what it held.
	UsageIncomplete bool `json:"usage_incomplete,omitempty"`
}

// Duration is the wall-clock time from the session's first event to its
// latest.
func (r SessionRow) Duration() time.Duration { return r.LastSeenAt.Sub(r.StartedAt) }

// Active is the time the session was active (see ActiveDuration).
func (r SessionRow) Active() time.Duration {
	return time.Duration(r.ActiveSeconds * float64(time.Second))
}

// SessionFilter selects sessions for ListSessions.
type SessionFilter struct {
	Since time.Time // last seen at or after
	From  time.Time // started at or after (zero: any)
	Until time.Time // started before (zero: any)
	Limit int       // 0: 1000
}

// ListSessions lists agent sessions, most recently seen first, with their
// usage priced and their loops counted the same way as the summary's
// periods.
func ListSessions(ctx context.Context, st *store.Store, pricing *cost.Pricing, f SessionFilter, nudgeAt float64) ([]SessionRow, error) {
	if f.Limit <= 0 {
		f.Limit = 1000
	}
	streams, err := st.ListSessions(ctx, store.ListOptions{Since: f.Since, Limit: 100000})
	if err != nil {
		return nil, err
	}
	return sessionRows(ctx, st, pricing, streams, f, nudgeAt)
}

// SessionByID is one agent session's row (ok false: no such session).
func SessionByID(ctx context.Context, st *store.Store, pricing *cost.Pricing, id string, nudgeAt float64) (row SessionRow, ok bool, err error) {
	streams, err := st.SessionStreams(ctx, id)
	if err != nil || len(streams) == 0 {
		return row, false, err
	}
	f := SessionFilter{Since: streams[0].LastSeenAt, Limit: 1}
	for _, s := range streams {
		if s.LastSeenAt.Before(f.Since) {
			f.Since = s.LastSeenAt
		}
	}
	rows, err := sessionRows(ctx, st, pricing, streams, f, nudgeAt)
	if err != nil || len(rows) == 0 {
		return row, false, err
	}
	return rows[0], true, nil
}

func sessionRows(ctx context.Context, st *store.Store, pricing *cost.Pricing, streams []store.Session, f SessionFilter, nudgeAt float64) ([]SessionRow, error) {
	var out []SessionRow
	byID := map[string]int{}
	for _, s := range streams { // most recently seen first
		if s.SessionID == DoctorSession {
			continue
		}
		i, ok := byID[s.SessionID]
		if !ok {
			i = len(out)
			byID[s.SessionID] = i
			out = append(out, SessionRow{SessionID: s.SessionID, StartedAt: s.StartedAt, LastSeenAt: s.LastSeenAt, Ended: true})
		}
		r := &out[i]
		main := !strings.Contains(s.Key, "/")
		if main {
			r.Keys = append([]string{s.Key}, r.Keys...)
			r.Cwd, r.Repo = s.Cwd, s.Repo
			r.Ended = !s.EndedAt.IsZero()
			r.Transcript = s.Transcript
		} else {
			r.Keys = append(r.Keys, s.Key)
		}
		if r.Cwd == "" && r.Repo == "" {
			r.Cwd, r.Repo = s.Cwd, s.Repo
		}
		if s.StartedAt.Before(r.StartedAt) {
			r.StartedAt = s.StartedAt
		}
		if s.LastSeenAt.After(r.LastSeenAt) {
			r.LastSeenAt = s.LastSeenAt
		}
		r.ToolCalls += s.ToolCalls
		r.Failures += s.Failures
	}
	kept := out[:0]
	for _, r := range out {
		r.Project = r.Repo
		if r.Project == "" {
			r.Project = r.Cwd
		}
		if (!f.From.IsZero() && r.StartedAt.Before(f.From)) || (!f.Until.IsZero() && !r.StartedAt.Before(f.Until)) {
			continue
		}
		kept = append(kept, r)
		if len(kept) == f.Limit {
			break
		}
	}
	out = kept
	if len(out) == 0 {
		return []SessionRow{}, nil
	}

	ids := make([]string, len(out))
	idx := make(map[string]int, len(out))
	for i, r := range out {
		ids[i], idx[r.SessionID] = r.SessionID, i
	}
	rows, err := st.UsageRows(ctx, store.UsageFilter{SessionIDs: ids})
	if err != nil {
		return nil, err
	}
	// Events, for the active time: the first and last hook events, the
	// tool calls, and the model calls.
	times := make([][]time.Time, len(out))
	var keys []string
	keyRow := map[string]int{}
	for i, r := range out {
		times[i] = []time.Time{r.StartedAt, r.LastSeenAt}
		for _, k := range r.Keys {
			keys = append(keys, k)
			keyRow[k] = i
		}
	}
	for i, p := range Price(rows, pricing) {
		ri := idx[rows[i].SessionID]
		r := &out[ri]
		r.Tokens += p.Usage.Total()
		if p.Priced {
			r.CostUSD += p.USD
		} else {
			r.UnpricedTokens += p.Usage.Total()
		}
		times[ri] = append(times[ri], rows[i].TS)
	}
	calls, err := st.CallTimes(ctx, keys)
	if err != nil {
		return nil, err
	}
	for k, ts := range calls {
		times[keyRow[k]] = append(times[keyRow[k]], ts...)
	}
	for i := range out {
		r := &out[i]
		r.ActiveSeconds = ActiveDuration(times[i]).Seconds()
		if r.Transcript != "" && !fileExists(r.Transcript) {
			read, err := st.HasTranscriptOffset(ctx, r.Transcript)
			if err != nil {
				return nil, err
			}
			r.UsageIncomplete = !read
		}
	}
	spans, err := st.LoopSpans(ctx, f.Since, nudgeAt)
	if err != nil {
		return nil, err
	}
	for _, sp := range spans {
		_, id, _ := strings.Cut(sp.SessionKey, ":") // agent:session[/subagent]
		id, _, _ = strings.Cut(id, "/")
		if i, ok := idx[id]; ok {
			out[i].Loops++
		}
	}
	return out, nil
}
