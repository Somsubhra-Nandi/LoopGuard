package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// pauses is the daemon's copy of the pauses table (`loopguard pause`).
// The CLI writes the table and then asks the daemon to reload; the janitor
// also reloads it now and then, and expiry is checked on every lookup.
type pauses struct {
	mu   sync.RWMutex
	list []store.Pause
}

func (p *pauses) set(list []store.Pause) {
	p.mu.Lock()
	p.list = list
	p.mu.Unlock()
}

func (p *pauses) all() []store.Pause {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]store.Pause(nil), p.list...)
}

// load replaces the pauses with the table's.
func (p *pauses) load(ctx context.Context, st *store.Store, now time.Time) error {
	if st == nil {
		return nil
	}
	list, err := st.Pauses(ctx, now)
	if err != nil {
		return err
	}
	p.set(list)
	return nil
}

// match returns the pause in effect for a session at now, if any.
func (p *pauses) match(sessionID, repo, cwd string, now time.Time) (store.Pause, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, ps := range p.list {
		if !ps.Active(now) {
			continue
		}
		switch ps.Scope {
		case store.PauseAll:
			return ps, true
		case store.PauseSession:
			if ps.Target == sessionID {
				return ps, true
			}
		case store.PauseRepo:
			if (repo != "" && samePath(repo, ps.Target)) || (cwd != "" && within(cleanPath(cwd), cleanPath(ps.Target))) {
				return ps, true
			}
		}
	}
	return store.Pause{}, false
}

func cleanPath(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func samePath(a, b string) bool { return cleanPath(a) == cleanPath(b) }

// describePause says what a pause covers, for "why" texts.
func describePause(p store.Pause) string {
	var s string
	switch p.Scope {
	case store.PauseAll:
		s = "everything is paused"
	case store.PauseSession:
		s = "this session is paused"
	default:
		s = "this repo is paused"
	}
	if p.Until.IsZero() {
		return s + " until resumed"
	}
	return s + " until " + p.Until.Local().Format("2006-01-02 15:04")
}

// sessionMode resolves the level for one event: the configuration's
// (config.yaml, observe_until, the repo's .loopguard.yaml), then pauses,
// which turn any level above observe into observe.
func (m *Manager) sessionMode(eff config.Effective, ev *event.Event, repo string, now time.Time) config.ModeInfo {
	mi := eff.ModeAt(now)
	if config.LevelRank(mi.Mode) <= config.LevelRank(config.ModeObserve) {
		return mi
	}
	if p, ok := m.pauses.match(ev.SessionID, repo, ev.Cwd, now); ok {
		return config.ModeInfo{Mode: config.ModeObserve, Why: describePause(p)}
	}
	return mi
}

// allowFor is the strongest action a level lets reach the agent.
func allowFor(level string) event.Action {
	switch level {
	case config.ModeGuard:
		return event.ActionHalt
	case config.ModeAssist:
		return event.ActionNudge
	}
	return event.ActionNone
}

// stopTTL is how long a stop request waits for its session's next event.
const stopTTL = time.Hour

// stops holds the `loopguard stop` requests, by the agent's session id
// (so a session's subagent streams are stopped with it). A request stays
// until the main stream's turn ends (Stop, a human prompt, SessionEnd) or
// it expires: every halt-capable event of the session gets the halt until
// then, in case the agent goes on anyway (a late reply is dropped).
//
// The requests are also kept in the store (stop_requests), so a daemon
// restart doesn't lose one: load reads them back at start.
type stops struct {
	mu   sync.Mutex
	reqs map[string]*stopReq

	st  *store.Store // nil: memory only
	log *slog.Logger
}

// persist runs a store write for the requests; a failure is logged (the
// request still works until a restart).
func (s *stops) persist(what string, f func(ctx context.Context, st *store.Store) error) {
	if s.st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f(ctx, s.st); err != nil && s.log != nil {
		s.log.Warn("stop request: "+what, "err", err)
	}
}

// load reads the stored requests not yet expired at now.
func (s *stops) load(ctx context.Context, now time.Time) error {
	if s.st == nil {
		return nil
	}
	rs, err := s.st.StopRequests(ctx, now.Add(-stopTTL))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reqs == nil {
		s.reqs = map[string]*stopReq{}
	}
	for _, r := range rs {
		s.reqs[r.SessionID] = &stopReq{at: r.RequestedAt, recorded: r.Recorded}
	}
	return nil
}

type stopReq struct {
	at       time.Time
	recorded bool // the halt was recorded as an intervention
}

func (s *stops) request(sessionID string, now time.Time) {
	s.mu.Lock()
	if s.reqs == nil {
		s.reqs = map[string]*stopReq{}
	}
	s.reqs[sessionID] = &stopReq{at: now}
	s.mu.Unlock()
	s.persist("saving", func(ctx context.Context, st *store.Store) error {
		return st.SetStopRequest(ctx, store.StopRequest{SessionID: sessionID, RequestedAt: now})
	})
}

// take reports whether a stop is pending for the session, and whether
// this is the first halt for it (to be recorded).
func (s *stops) take(sessionID string, now time.Time) (pending, first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.reqs[sessionID]
	if r == nil {
		return false, false
	}
	if now.Sub(r.at) > stopTTL {
		delete(s.reqs, sessionID)
		return false, false
	}
	first = !r.recorded
	r.recorded = true
	if first {
		at := r.at
		// Off the hook path, like clear.
		go s.persist("marking recorded", func(ctx context.Context, st *store.Store) error {
			return st.SetStopRequest(ctx, store.StopRequest{SessionID: sessionID, RequestedAt: at, Recorded: true})
		})
	}
	return true, first
}

func (s *stops) pending(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[sessionID] != nil
}

func (s *stops) clear(sessionID string) {
	s.mu.Lock()
	_, had := s.reqs[sessionID]
	delete(s.reqs, sessionID)
	s.mu.Unlock()
	if had {
		// Off the hook path: a store write can wait on a lock.
		go s.persist("deleting", func(ctx context.Context, st *store.Store) error { return st.DeleteStopRequest(ctx, sessionID) })
	}
}

func (s *stops) expire(now time.Time) {
	s.mu.Lock()
	var gone []string
	for id, r := range s.reqs {
		if now.Sub(r.at) > stopTTL {
			delete(s.reqs, id)
			gone = append(gone, id)
		}
	}
	s.mu.Unlock()
	for _, id := range gone {
		s.persist("deleting", func(ctx context.Context, st *store.Store) error { return st.DeleteStopRequest(ctx, id) })
	}
}
