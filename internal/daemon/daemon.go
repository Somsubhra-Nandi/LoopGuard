package daemon

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
	"github.com/Somsubhra-Nandi/loopguard/internal/adapter/claudecode"
	"github.com/Somsubhra-Nandi/loopguard/internal/capture"
	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/dashboard"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/notify"
	"github.com/Somsubhra-Nandi/loopguard/internal/proc"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// Ports (LLD §9.1): the default, or the first free one above it.
const (
	DefaultPort = 47821
	portRange   = 100
)

// Options configures a daemon. Zero values take the defaults.
type Options struct {
	Home    string // data directory (required)
	Version string
	Port    int // first port to try (0: DefaultPort; -1: any free port, for tests)
	// Capture forces payload capture on for the whole run (LOOPGUARD_CAPTURE=1);
	// otherwise it follows the flag file (`loopguard capture on`).
	Capture bool
	// Log receives the daemon's structured logs; nil logs to
	// <home>/logs/daemon.log only.
	Log *slog.Logger
	// Thresholds, if set, replaces the thresholds from config.yaml (tests);
	// the mode still comes from the config files.
	Thresholds  *config.Thresholds
	IdleTimeout time.Duration // actor eviction (0: DefaultIdle)
	// TailEvery is how often active sessions' transcripts are read for usage
	// (0: DefaultTailEvery; negative: never, for tests that poll by hand).
	TailEvery time.Duration
	// Senders replaces the notification senders (tests). nil: the real
	// ones, except in a test binary, where nothing is sent.
	Senders notify.Senders
	// Transcripts is where the dashboard reads transcripts (session
	// timelines, top costs); "": ~/.claude/projects, or nothing in a test
	// binary (tests never read the real home).
	Transcripts string
	// Ready, if set, is called once the daemon is listening and daemon.json
	// is written.
	Ready func(home.DaemonInfo)
}

// AlreadyRunningError is returned by Run when another daemon serves the
// same home directory.
type AlreadyRunningError struct{ Info home.DaemonInfo }

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("a LoopGuard daemon is already running (pid %d, port %d)", e.Info.PID, e.Info.Port)
}

// Run runs the daemon until ctx is done or a shutdown request arrives.
func Run(ctx context.Context, o Options) error {
	if o.Home == "" {
		return errors.New("daemon: no home directory")
	}
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	if err := os.MkdirAll(o.Home, 0o700); err != nil {
		return err
	}
	logf, err := openLogFile(filepath.Join(o.Home, home.LogDir, "daemon.log"))
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	defer logf.Close()
	log := slog.New(teeHandler{slog.NewJSONHandler(logf, &slog.HandlerOptions{Level: logLevel()}), o.Log})

	token, err := home.EnsureToken(o.Home)
	if err != nil {
		return fmt.Errorf("API token: %w", err)
	}
	id := homeID(o.Home)

	// Single instance: a live daemon in daemon.json, or one answering on
	// a port we try, for this same home.
	if info, err := home.ReadDaemonInfo(o.Home); err == nil && proc.Alive(info.PID) && probe(info.Port, id) {
		return &AlreadyRunningError{Info: info}
	}
	ln, err := listen(o.Port, id)
	if err != nil {
		return err
	}

	st, err := store.Open(filepath.Join(o.Home, home.DBFile))
	var w *store.Writer
	if err != nil {
		log.Warn("database unavailable; running memory-only (nothing is persisted)", "err", err)
		st = nil
	} else {
		w = st.NewWriter(store.WriterOptions{OnError: func(err error, rows int) {
			log.Warn("store write failed", "rows", rows, "err", err)
		}})
	}

	// config.yaml and the repos' .loopguard.yaml, re-read when they change.
	// An invalid file is logged and skipped (defaults: observe mode).
	cfg := config.NewLoader(o.Home, config.LoaderOptions{
		Thresholds: o.Thresholds,
		OnIssue: func(path string, err error, warnings []string) {
			if err != nil {
				log.Warn("config file is invalid; ignoring it", "path", path, "err", err)
			}
			for _, w := range warnings {
				log.Warn("config file", "path", path, "warning", w)
			}
		},
	})
	eff := cfg.For("")
	met := newMetrics()
	d := &server{
		token:    token,
		homeID:   id,
		version:  o.Version,
		adapters: adapter.NewRegistry(claudecode.New()),
		mgr:      newManager(cfg, st, w, log, met, o.IdleTimeout),
		st:       st,
		w:        w,
		met:      met,
		log:      log,
		started:  time.Now().UTC(),
	}
	d.home = o.Home
	d.dash = dashboard.New(dashboard.Options{Store: st, Backend: dashBackend{d}, Transcripts: transcriptsDir(o.Transcripts),
		Port: ln.Addr().(*net.TCPAddr).Port, Version: o.Version, Home: o.Home, Log: log})
	d.mgr.notifier = notify.NewDispatcher(senders(o.Senders), log)
	// pricing.yaml: the shipped table, written once so the user can edit it.
	if _, err := cost.EnsurePricing(o.Home); err != nil {
		log.Warn("writing pricing.yaml", "err", err)
	}
	d.mgr.book.pricing.OnError = func(err error) { log.Warn("pricing.yaml is invalid; using the shipped prices", "err", err) }
	if st != nil {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		// Outcomes still open were being tracked by a daemon that is gone.
		if n, err := st.CloseOpenOutcomes(sctx); err != nil {
			log.Warn("closing open intervention outcomes", "err", err)
		} else if n > 0 {
			log.Info("interventions from an earlier run marked outcome unknown", "count", n)
		}
		if err := d.mgr.pauses.load(sctx, st, time.Now()); err != nil {
			log.Warn("loading pauses", "err", err)
		}
		if err := d.mgr.stops.load(sctx, time.Now()); err != nil {
			log.Warn("loading stop requests", "err", err)
		}
		d.mgr.book.loadAnnounced(sctx, time.Now())
		if names, err := st.RepoSpellings(sctx); err != nil {
			log.Warn("loading repo names", "err", err)
		} else {
			d.mgr.repos.load(names)
		}
		cancel()
	}
	uh, _ := os.UserHomeDir()
	d.capture = capture.New(capture.Dir(o.Home), uh)
	d.captureOn = capture.NewSwitch(o.Home, o.Capture)

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	d.shutdown = stop
	srv := &http.Server{
		Handler:           d.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	info := home.DaemonInfo{PID: os.Getpid(), Port: ln.Addr().(*net.TCPAddr).Port, Version: o.Version, StartedAt: time.Now().UTC()}
	if err := home.WriteDaemonInfo(o.Home, info); err != nil {
		stop()
		srv.Close()
		d.close()
		return fmt.Errorf("writing %s: %w", home.DaemonFile, err)
	}
	mode := eff.ModeAt(time.Now())
	log.Info("daemon started", "pid", info.PID, "port", info.Port, "version", o.Version,
		"store", d.storeMode(), "capture", d.captureOn.Enabled(), "mode", mode.Mode, "mode_why", mode.Why)
	if o.Ready != nil {
		o.Ready(info)
	}

	go d.janitor(ctx)
	if o.TailEvery >= 0 {
		go d.tailLoop(ctx, cmp.Or(o.TailEvery, DefaultTailEvery))
	}
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		log.Error("server stopped", "err", err)
	}

	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	d.mgr.Close(sctx)
	d.mgr.notifier.Close(sctx)
	d.close()
	// Remove daemon.json only if it is still ours.
	if cur, err := home.ReadDaemonInfo(o.Home); err == nil && cur.PID == info.PID && cur.Port == info.Port {
		os.Remove(filepath.Join(o.Home, home.DaemonFile))
	}
	log.Info("daemon stopped")
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// transcriptsDir is the dashboard's transcripts directory.
func transcriptsDir(dir string) string {
	switch {
	case dir != "":
		return dir
	case testing.Testing():
		return filepath.Join(os.TempDir(), "loopguard-test-no-transcripts")
	}
	dir, _ = replay.DefaultPath()
	return dir
}

// senders returns the notification senders: o's, else the real ones. A
// test binary never sends for real (no desktop pop-ups or network calls
// from go test), whatever its config says.
func senders(o notify.Senders) notify.Senders {
	switch {
	case o != nil:
		return o
	case testing.Testing():
		return notify.Senders{}
	}
	return notify.DefaultSenders()
}

func logLevel() slog.Level {
	if os.Getenv("LOOPGUARD_DEBUG") == "1" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// homeID identifies a home directory without revealing it, so /healthz
// can tell "a daemon for this home" from one for another.
func homeID(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	dir = filepath.Clean(dir)
	if runtime.GOOS == "windows" {
		dir = strings.ToLower(dir)
	}
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:8])
}

// listen binds 127.0.0.1 on the first free port from first on. A port
// that is taken by a daemon for the same home means one is running.
func listen(first int, id string) (net.Listener, error) {
	if first < 0 {
		return net.Listen("tcp", "127.0.0.1:0")
	}
	var lastErr error
	for p := first; p < first+portRange && p <= 65535; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return ln, nil
		}
		lastErr = err
		if probe(p, id) {
			info, _ := healthInfo(p)
			return nil, &AlreadyRunningError{Info: home.DaemonInfo{PID: info.PID, Port: p, Version: info.Version}}
		}
	}
	return nil, fmt.Errorf("no free port in %d-%d: %w", first, first+portRange-1, lastErr)
}

// Health is the /healthz response.
type Health struct {
	OK      bool      `json:"ok"`
	Service string    `json:"service"`
	Version string    `json:"version"`
	PID     int       `json:"pid"`
	HomeID  string    `json:"home_id"`
	Store   string    `json:"store"`   // "sqlite" | "memory-only"
	Capture bool      `json:"capture"` // payload capture is on
	Started time.Time `json:"started_at"`
}

// healthInfo fetches /healthz from a local port.
func healthInfo(port int) (Health, error) {
	var h Health
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("healthz: %s", resp.Status)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&h)
	return h, err
}

// probe reports whether a LoopGuard daemon for this home answers on port.
func probe(port int, id string) bool {
	h, err := healthInfo(port)
	return err == nil && h.Service == "loopguard" && h.HomeID == id
}

// HomeID is exported for the CLI (status/stop), which checks that the
// daemon in daemon.json serves this home.
func HomeID(dir string) string { return homeID(dir) }

// Healthy fetches /healthz from the daemon on port and checks that it
// serves this home.
func Healthy(port int, dir string) (Health, bool) {
	h, err := healthInfo(port)
	return h, err == nil && h.Service == "loopguard" && h.HomeID == homeID(dir)
}

// teeHandler sends records to the file handler and, if set, a second
// logger (the terminal in foreground mode).
type teeHandler struct {
	file  slog.Handler
	extra *slog.Logger
}

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.file.Enabled(ctx, l) || (t.extra != nil && t.extra.Handler().Enabled(ctx, l))
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if t.file.Enabled(ctx, r.Level) {
		err = t.file.Handle(ctx, r.Clone())
	}
	if t.extra != nil && t.extra.Handler().Enabled(ctx, r.Level) {
		err = errors.Join(err, t.extra.Handler().Handle(ctx, r))
	}
	return err
}

func (t teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	var extra *slog.Logger
	if t.extra != nil {
		extra = slog.New(t.extra.Handler().WithAttrs(as))
	}
	return teeHandler{t.file.WithAttrs(as), extra}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	var extra *slog.Logger
	if t.extra != nil {
		extra = slog.New(t.extra.Handler().WithGroup(name))
	}
	return teeHandler{t.file.WithGroup(name), extra}
}
