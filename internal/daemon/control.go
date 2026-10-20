package daemon

import (
	"context"
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

// sessionMode resolves the mode for one event: the configuration's
// effective mode (config.yaml, observe_until, the repo's .loopguard.yaml),
// then pauses, which turn enforce into observe.
func (m *Manager) sessionMode(eff config.Effective, ev *event.Event, repo string, now time.Time) config.ModeInfo {
	mi := eff.ModeAt(now)
	if mi.Mode != config.ModeEnforce {
		return mi
	}
	if p, ok := m.pauses.match(ev.SessionID, repo, ev.Cwd, now); ok {
		return config.ModeInfo{Mode: config.ModeObserve, Why: describePause(p)}
	}
	return mi
}
