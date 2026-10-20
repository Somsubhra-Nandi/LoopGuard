package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
	"github.com/Somsubhra-Nandi/loopguard/internal/capture"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// Request limits.
const (
	MaxPayload = 4 << 20 // hook payloads (same cap as the shim)
	// TimeoutHeader carries the shim's own timeout in milliseconds. The
	// daemon replies within it, minus replyMargin for the trip back.
	TimeoutHeader  = "X-LoopGuard-Timeout-Ms"
	defaultTimeout = 800 * time.Millisecond
	replyMargin    = 50 * time.Millisecond
	minBudget      = 10 * time.Millisecond
)

// EventReply is the /v1/events response: the hook output, already
// rendered by the agent's adapter, for the shim to print as is.
type EventReply struct {
	Stdout string `json:"stdout"`
	Exit   int    `json:"exit"`
}

type server struct {
	token     string
	homeID    string
	version   string
	adapters  adapter.Registry
	mgr       *Manager
	st        *store.Store
	w         *store.Writer
	met       *metrics
	log       *slog.Logger
	capture   *capture.Capturer
	captureOn *capture.Switch
	started   time.Time
	shutdown  func()
}

func (d *server) storeMode() string {
	if d.st == nil {
		return "memory-only"
	}
	return "sqlite"
}

func (d *server) close() {
	d.w.Close()
	if d.st != nil {
		d.st.Close()
	}
}

// janitor evicts idle actors.
func (d *server) janitor(ctx context.Context) {
	every := d.mgr.idle / 4
	if every > time.Minute {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.mgr.evictIdle()
		}
	}
}

func (d *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", d.healthz)
	mux.Handle("POST /v1/events", d.auth(d.events))
	mux.Handle("GET /v1/sessions", d.auth(d.sessions))
	mux.Handle("GET /v1/sessions/{id}", d.auth(d.sessionDetail))
	mux.Handle("GET /v1/decisions", d.auth(d.decisions))
	mux.Handle("GET /metrics", d.auth(d.metrics))
	mux.Handle("POST /v1/shutdown", d.auth(d.shutdownHandler))
	return d.hostCheck(mux)
}

// hostCheck rejects requests whose Host isn't 127.0.0.1 or localhost, so a
// web page can't reach the daemon through DNS rebinding.
func (d *server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && !strings.EqualFold(host, "localhost") {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auth requires "Authorization: Bearer <token>".
func (d *server) auth(h http.HandlerFunc) http.Handler {
	want := []byte("Bearer " + d.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func (d *server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Health{OK: true, Service: "loopguard", Version: d.version, PID: os.Getpid(),
		HomeID: d.homeID, Store: d.storeMode(), Capture: d.captureOn.Enabled(), Started: d.started})
}

// events ingests one hook payload and replies with the rendered decision.
// Every failure answers "none" (empty stdout, exit 0) with status 200: the
// shim fails open on errors anyway, and this way it never has to tell them
// apart.
func (d *server) events(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	deadline := start.Add(budget(r.Header.Get(TimeoutHeader)))
	agent := r.URL.Query().Get("agent")
	none := func(kind string) {
		if kind != "" {
			d.met.error(kind)
		}
		writeJSON(w, http.StatusOK, EventReply{})
		d.met.latency(time.Since(start))
	}

	ad, ok := d.adapters.Get(agent)
	if !ok {
		d.log.Debug("event for unknown agent", "agent", agent)
		none("unknown_agent")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxPayload+1))
	if err != nil || len(body) > MaxPayload {
		none("read")
		return
	}
	if d.captureOn.Enabled() {
		go func() {
			if err := d.capture.Save(agent, body); err != nil {
				d.log.Warn("capture failed", "err", err)
			}
		}()
	}
	ev, err := ad.Parse(body)
	if err != nil {
		d.log.Debug("unparsable payload", "agent", agent, "err", err)
		d.met.event(agent, "invalid")
		none("parse")
		return
	}
	if ev.Phase == "" {
		d.met.event(agent, "unknown")
		none("")
		return
	}
	d.met.event(agent, string(ev.Phase))

	ch := d.mgr.Submit(ev, ad)
	if ch == nil {
		none("") // queue full or shutting down; counted by the manager
		return
	}
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case rep := <-ch:
		writeJSON(w, http.StatusOK, EventReply{Stdout: string(rep.stdout), Exit: rep.exit})
		d.met.latency(time.Since(start))
	case <-t.C:
		// The actor still processes the event, in order; only the
		// decision is late.
		none("timeout")
	case <-r.Context().Done():
		d.met.error("client_gone")
	}
}

// budget is how long the daemon may take to answer: the shim's timeout
// minus the margin for the reply's trip back.
func budget(header string) time.Duration {
	t := defaultTimeout
	if ms, err := strconv.Atoi(header); err == nil && ms > 0 {
		t = time.Duration(ms) * time.Millisecond
	}
	b := t - replyMargin
	if b < minBudget {
		b = minBudget
	}
	return b
}

// sessionView is a session in API responses.
type sessionView struct {
	store.Session
	Active bool `json:"active"`
}

func (d *server) sessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since time.Time
	if s := q.Get("since"); s != "" {
		dur, err := parseSince(s)
		if err != nil {
			http.Error(w, "bad since: "+err.Error(), http.StatusBadRequest)
			return
		}
		since = time.Now().Add(-dur)
	}
	repo := q.Get("repo")
	live := d.mgr.Live()

	var out []sessionView
	if d.st != nil {
		d.w.Flush() // show what was just processed
		rows, err := d.st.ListSessions(r.Context(), store.ListOptions{Since: since, Repo: repo, Limit: 200})
		if err != nil {
			d.log.Warn("listing sessions", "err", err)
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		for _, s := range rows {
			_, active := live[s.Key]
			out = append(out, sessionView{Session: s, Active: active})
		}
	} else {
		for _, s := range live {
			if s.LastSeenAt.Before(since) || (repo != "" && s.Repo != repo) {
				continue
			}
			out = append(out, sessionView{Session: s.Session, Active: true})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].LastSeenAt.After(out[j].LastSeenAt) })
	}
	if out == nil {
		out = []sessionView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out, "store": d.storeMode()})
}

// sessionDetail returns one session: by key ("claude-code:<id>") or by the
// agent's session id.
func (d *server) sessionDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	live := d.mgr.Live()
	ls, isLive := live[id]
	if !isLive {
		for _, s := range live {
			if s.SessionID == id && (!isLive || s.LastSeenAt.After(ls.LastSeenAt)) {
				ls, isLive = s, true
			}
		}
	}

	if d.st == nil {
		if !isLive {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session": sessionView{Session: ls.Session, Active: true}, "signals": nonNil(ls.Signals),
			"interventions": nonNil(ls.WouldHave), "store": d.storeMode(),
		})
		return
	}

	d.w.Flush()
	ctx := r.Context()
	s, err := d.st.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	calls, err1 := d.st.Calls(ctx, s.Key, 200)
	sigs, err2 := d.st.Signals(ctx, s.Key, 100)
	ivs, err3 := d.st.Interventions(ctx, s.Key, 100)
	if err := errors.Join(err1, err2, err3); err != nil {
		d.log.Warn("session detail", "session", s.Key, "err", err)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	_, active := live[s.Key]
	writeJSON(w, http.StatusOK, map[string]any{
		"session": sessionView{Session: s, Active: active}, "calls": calls, "signals": sigs,
		"interventions": ivs, "store": d.storeMode(),
	})
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

// parseSince parses "30m", "12h", "7d", "2w".
func parseSince(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		v, err := strconv.Atoi(n)
		return time.Duration(v) * 24 * time.Hour, err
	}
	if n, ok := strings.CutSuffix(s, "w"); ok {
		v, err := strconv.Atoi(n)
		return time.Duration(v) * 7 * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
}

func (d *server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	written, dropped, failed := d.w.Stats()
	d.met.write(w, d.mgr.Active(), storeStats{written: written, dropped: dropped, failed: failed, memoryOnly: d.st == nil})
}

func (d *server) shutdownHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	d.log.Info("shutdown requested")
	go d.shutdown()
}

// decisions returns the newest would-have decisions of all sessions,
// newest first (?limit=, default 10, at most 200).
func (d *server) decisions(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			http.Error(w, "bad limit", http.StatusBadRequest)
			return
		}
		limit = min(n, 200)
	}
	var out []store.Intervention
	if d.st != nil {
		d.w.Flush()
		rows, err := d.st.RecentInterventions(r.Context(), limit)
		if err != nil {
			d.log.Warn("listing decisions", "err", err)
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		out = rows
	} else {
		for _, s := range d.mgr.Live() {
			out = append(out, s.WouldHave...)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
		if len(out) > limit {
			out = out[:limit]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": nonNil(out), "store": d.storeMode()})
}
