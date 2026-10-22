package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// Actions (POST with the CSRF token, each confirmed on a page of its own
// first).
const (
	actPauseSession  = "pause-session"
	actResumeSession = "resume-session"
	actPauseRepo     = "pause-repo"
	actResumeRepo    = "resume-repo"
	actStop          = "stop"
)

// pauseFor are the pause lengths offered ("" until resumed).
var pauseFor = map[string]time.Duration{"": 0, "30m": 30 * time.Minute, "2h": 2 * time.Hour, "1d": 24 * time.Hour}

// button is one action form on a page; Pause ones offer a length.
type button struct {
	Action, Target, Label, Back string
	Pause, Danger               bool
}

// buttons are a page's action forms (html/template has no dict, so the
// token travels with them).
type buttons struct {
	CSRF string
	List []button
}

func sessionButtons(csrf, back string, dd detailData) buttons {
	b := buttons{CSRF: csrf}
	if !dd.Found {
		return b
	}
	id, repo := dd.Row.SessionID, dd.Row.Repo
	if dd.Paused != "" {
		b.List = append(b.List, button{Action: actResumeSession, Target: id, Label: "Resume session", Back: back})
	} else {
		b.List = append(b.List, button{Action: actPauseSession, Target: id, Label: "Pause session", Back: back, Pause: true})
	}
	if repo != "" {
		if dd.RepoPaused != "" {
			b.List = append(b.List, button{Action: actResumeRepo, Target: repo, Label: "Resume repo", Back: back})
		} else {
			b.List = append(b.List, button{Action: actPauseRepo, Target: repo, Label: "Pause repo", Back: back, Pause: true})
		}
	}
	if dd.CanStop {
		b.List = append(b.List, button{Action: actStop, Target: id, Label: "Stop its turn", Back: back, Danger: true})
	}
	return b
}

// act is a validated action request.
type act struct {
	Kind   string
	Target string
	For    string
	Back   string
	CSRF   string
	// For the confirm page.
	Title, Text, Button string
}

func (d *Dashboard) parseAct(ctx context.Context, f url.Values) (act, error) {
	a := act{Kind: f.Get("action"), Target: f.Get("target"), For: f.Get("for"), Back: safeBack(f.Get("back"))}
	if _, ok := pauseFor[a.For]; !ok {
		return a, errors.New("unknown pause length")
	}
	session := slices.Contains([]string{actPauseSession, actResumeSession, actStop}, a.Kind)
	repo := slices.Contains([]string{actPauseRepo, actResumeRepo}, a.Kind)
	switch {
	case !session && !repo:
		return a, errors.New("unknown action")
	case a.Target == "":
		return a, errors.New("no target")
	case d.o.Store == nil && a.Kind != actStop:
		return a, errors.New("the database is unavailable (the daemon runs memory-only), so nothing can be paused")
	}
	length := "until you resume it"
	if a.For != "" {
		length = "for " + map[string]string{"30m": "30 minutes", "2h": "2 hours", "1d": "1 day"}[a.For]
	}
	if repo {
		ok, err := d.knownRepo(ctx, a.Target)
		if err != nil {
			return a, err
		}
		if !ok {
			return a, errors.New("LoopGuard has no session in that repo")
		}
	}
	switch a.Kind {
	case actPauseSession, actResumeSession:
		ids, err := d.o.Store.SessionIDs(ctx, a.Target, 2)
		if err != nil {
			return a, err
		}
		if !slices.Contains(ids, a.Target) {
			return a, errors.New("no such session")
		}
	case actStop:
		l, ok := d.live()["claude-code:"+a.Target]
		if !ok || l.Ended {
			return a, errors.New("that session is not active")
		}
	}
	switch a.Kind {
	case actPauseSession:
		a.Title, a.Button = "Pause session "+shortID(a.Target)+"?", "Pause it"
		a.Text = "LoopGuard keeps watching session " + a.Target + " but only records what it would do " + length +
			": no notifications, nudges, blocks, or stops."
	case actResumeSession:
		a.Title, a.Button = "Resume session "+shortID(a.Target)+"?", "Resume it"
		a.Text = "Session " + a.Target + " gets its configured level again (a pause of its repo, or of everything, still applies)."
	case actPauseRepo:
		a.Title, a.Button = "Pause repo "+baseName(a.Target)+"?", "Pause it"
		a.Text = "Every session in " + a.Target + " is only recorded " + length + ": no notifications, nudges, blocks, or stops."
	case actResumeRepo:
		a.Title, a.Button = "Resume repo "+baseName(a.Target)+"?", "Resume it"
		a.Text = "Sessions in " + a.Target + " get their configured level again."
	case actStop:
		a.Title, a.Button = "Stop session "+shortID(a.Target)+"'s turn?", "Stop the turn"
		a.Text = "The next tool call of session " + a.Target + " (or of its subagents) is blocked and its turn ends, as with " +
			"loopguard stop " + shortID(a.Target) + ". The session stays usable: your next message continues normally."
	}
	return a, nil
}

func (d *Dashboard) knownRepo(ctx context.Context, repo string) (bool, error) {
	rows, err := d.o.Store.ListSessions(ctx, store.ListOptions{Repo: repo, Limit: 1})
	return len(rows) > 0, err
}

// safeBack keeps a return link inside the dashboard.
func safeBack(s string) string {
	if !strings.HasPrefix(s, "/ui/") || strings.Contains(s, "//") || strings.ContainsAny(s, "\\\r\n") {
		return "/ui/"
	}
	return s
}

func (d *Dashboard) confirm(w http.ResponseWriter, r *http.Request, s *uiSession) {
	ctx, cancel := d.ctx(r)
	defer cancel()
	a, err := d.parseAct(ctx, r.PostForm)
	if err != nil {
		d.render(w, http.StatusBadRequest, "message", view{Title: "Can't do that", CSRF: s.CSRF,
			Data: message{Title: "Can't do that", Text: err.Error(), Back: a.Back}})
		return
	}
	a.CSRF = s.CSRF
	d.render(w, http.StatusOK, "confirm", view{Title: a.Title, CSRF: s.CSRF, Data: a})
}

func (d *Dashboard) action(w http.ResponseWriter, r *http.Request, s *uiSession) {
	ctx, cancel := d.ctx(r)
	defer cancel()
	a, err := d.parseAct(ctx, r.PostForm)
	if err == nil {
		var text string
		text, err = d.perform(ctx, a)
		if err == nil {
			d.o.Log.Info("dashboard action", "action", a.Kind, "target", a.Target, "for", a.For)
			d.render(w, http.StatusOK, "message", view{Title: "Done", CSRF: s.CSRF, Data: message{Title: "Done", Text: text, Back: a.Back, OK: true}})
			return
		}
	}
	d.render(w, http.StatusBadRequest, "message", view{Title: "Can't do that", CSRF: s.CSRF,
		Data: message{Title: "Can't do that", Text: err.Error(), Back: a.Back}})
}

func (d *Dashboard) perform(ctx context.Context, a act) (string, error) {
	now := d.o.Now()
	st := d.o.Store
	var until time.Time
	if dur := pauseFor[a.For]; dur > 0 {
		until = now.Add(dur)
	}
	reload := func() string {
		if err := d.o.Backend.ReloadPauses(ctx); err != nil {
			return " (The daemon will apply it within 30 seconds.)"
		}
		return ""
	}
	switch a.Kind {
	case actPauseSession, actPauseRepo:
		scope := store.PauseSession
		if a.Kind == actPauseRepo {
			scope = store.PauseRepo
		}
		if err := st.SetPause(ctx, store.Pause{Scope: scope, Target: a.Target, Until: until, CreatedAt: now}); err != nil {
			return "", err
		}
		return fmt.Sprintf("Paused %s %s: %s.%s", scope, a.Target, pauseText(store.Pause{Until: until}, d.o.Location), reload()), nil
	case actResumeSession, actResumeRepo:
		scope := store.PauseSession
		if a.Kind == actResumeRepo {
			scope = store.PauseRepo
		}
		n, err := st.DeletePauses(ctx, scope, a.Target)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "Nothing to resume: " + scope + " " + a.Target + " was not paused.", nil
		}
		return "Resumed " + scope + " " + a.Target + "." + reload(), nil
	case actStop:
		res := d.o.Backend.Stop(a.Target)
		if !res.Stopped {
			return "", errors.New("not stopped: " + res.Why)
		}
		return "Session " + a.Target + "'s turn ends at its next tool call. Your next message to it continues normally.", nil
	}
	return "", errors.New("unknown action")
}
