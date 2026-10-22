package dashboard

import (
	"context"
	"crypto/rand"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/report"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// Prefix is where the dashboard is served on the daemon's port.
const Prefix = "/ui/"

//go:embed assets
var assets embed.FS

// Backend is what the dashboard needs from the running daemon: live state
// the database doesn't hold, and the daemon's own actions.
type Backend interface {
	// Live returns the live sessions by session key.
	Live() map[string]Live
	// Level is the level a session's next event gets (config files, then
	// pauses), as the daemon resolves it.
	Level(sessionID, repo, cwd string) config.ModeInfo
	// Config is the configuration in effect for a repo ("": outside any).
	Config(repo string) config.Effective
	// Pricing is the price table in use and its status.
	Pricing() (*cost.Pricing, cost.PricingStatus)
	// Usage is today's usage and each live session's, as the daemon counts
	// it for budgets (`loopguard status`).
	Usage() Usage
	// Stop asks a live session to stop its turn (`loopguard stop`).
	Stop(sessionID string) StopResult
	// ReloadPauses makes the daemon re-read the pauses table.
	ReloadPauses(ctx context.Context) error
	// Flush writes the rows the daemon has queued, so pages show them.
	Flush()
}

// Live is a live session's state.
type Live struct {
	SessionID string
	Mode      string
	ModeWhy   string
	Ended     bool
}

// UsageView is one scope's usage.
type UsageView struct {
	Tokens         int64
	CostUSD        float64
	UnpricedTokens int64
	ToolCalls      int64
	WallMinutes    float64
}

// Value returns the usage of a budget metric.
func (u UsageView) Value(metric string) float64 {
	switch metric {
	case config.MetricCost:
		return u.CostUSD
	case config.MetricTokens:
		return float64(u.Tokens)
	case config.MetricWall:
		return u.WallMinutes
	case config.MetricToolCalls:
		return float64(u.ToolCalls)
	}
	return 0
}

// Usage is today's usage and each live session's (by session id).
type Usage struct {
	Today    UsageView
	Sessions map[string]UsageView
}

// StopResult is the outcome of a stop request.
type StopResult struct {
	Stopped bool
	Why     string // why not: no such live session, or it observes only
}

// Options configures the dashboard.
type Options struct {
	Store       *store.Store // nil: the daemon runs memory-only
	Backend     Backend
	Transcripts string // the agent transcripts (~/.claude/projects)
	Port        int    // the daemon's port, for the Host check and links
	Version     string
	Home        string
	Log         *slog.Logger
	// Now, Location, and Random make tests deterministic (defaults:
	// time.Now, time.Local, crypto/rand).
	Now      func() time.Time
	Location *time.Location
	Random   io.Reader
}

// Dashboard serves the local web dashboard (LLD §9.3).
type Dashboard struct {
	o     Options
	auth  *authState
	pages *pages
	mux   *http.ServeMux

	topMu   sync.Mutex
	topAt   time.Time
	topCost *report.EstimateReport
	topErr  error

	snapMu     sync.Mutex
	snap       *snapshot
	refreshing chan struct{}
}

// New returns a dashboard.
func New(o Options) *Dashboard {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Random == nil {
		o.Random = rand.Reader
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	d := &Dashboard{o: o, auth: newAuth(o.Random, o.Now)}
	d.pages = mustParsePages()
	d.mux = d.routes()
	return d
}

func (d *Dashboard) routes() *http.ServeMux {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "assets/static")
	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /ui/login", d.login)
	page := func(h func(w http.ResponseWriter, r *http.Request, s *uiSession)) http.Handler {
		return d.authed(h)
	}
	mux.Handle("GET /ui/{$}", page(d.overview))
	mux.Handle("GET /ui/sessions", page(d.sessionList))
	mux.Handle("GET /ui/sessions/{id}", page(d.sessionDetail))
	mux.Handle("GET /ui/costs", page(d.topCosts))
	mux.Handle("GET /ui/settings", page(d.settings))
	mux.Handle("GET /ui/api/overview", page(d.overviewJSON))
	mux.Handle("GET /ui/api/sessions", page(d.sessionListJSON))
	mux.Handle("GET /ui/api/sessions/{id}", page(d.sessionDetailJSON))
	mux.Handle("POST /ui/confirm", d.authed(d.csrf(d.confirm)))
	mux.Handle("POST /ui/action", d.authed(d.csrf(d.action)))
	mux.Handle("POST /ui/logout", d.authed(d.csrf(d.logout)))
	mux.Handle("/ui/", d.authed(func(w http.ResponseWriter, r *http.Request, _ *uiSession) {
		d.errorPage(w, r, http.StatusNotFound, "Not found", "There is no such page.")
	}))
	return mux
}

// Handler serves everything under Prefix. Every response gets the
// security headers; a Host other than 127.0.0.1 or localhost with the
// daemon's port is refused (DNS rebinding).
func (d *Dashboard) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; "+
			"frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		if !d.goodHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		d.mux.ServeHTTP(w, r)
	})
}

// goodHost accepts exactly 127.0.0.1:<port> and localhost:<port>.
func (d *Dashboard) goodHost(host string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil || p != strconv.Itoa(d.o.Port) {
		return false
	}
	return h == "127.0.0.1" || strings.EqualFold(h, "localhost")
}

func (d *Dashboard) origin() string { return fmt.Sprintf("http://127.0.0.1:%d", d.o.Port) }

// NewLoginLink returns a one-time login link (LoginTTL, single use), and
// starts computing the totals the first page shows.
func (d *Dashboard) NewLoginLink() (string, time.Time, error) {
	code, exp, err := d.auth.newCode()
	if err != nil {
		return "", time.Time{}, err
	}
	d.prewarm()
	return d.origin() + "/ui/login?code=" + code, exp, nil
}

// now is the current time in the dashboard's location.
func (d *Dashboard) now() time.Time { return d.o.Now().In(d.o.Location) }
