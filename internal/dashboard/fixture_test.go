package dashboard

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay/replaytest"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

const testPort = 47821

// at is "now" in every test: the fixture's sessions are before it.
var at = time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)

// Fixture sessions.
const (
	sidShop  = "aaaa1111-0000-4000-8000-000000000001" // live, guard, a loop, a transcript
	sidAPI   = "bbbb2222-0000-4000-8000-000000000002" // 3 days ago, a subagent, its repo paused
	sidOld   = "cccc3333-0000-4000-8000-000000000003" // 20 days ago, an unpriced model
	repoShop = "/work/shop"
	repoAPI  = "/work/api"
	topic    = "secret-topic-123"
)

// fakeBackend is the daemon for tests: real config files and pauses, fixed
// live state and usage.
type fakeBackend struct {
	home    string
	st      *store.Store
	cfg     *config.Loader
	live    map[string]Live
	usage   Usage
	stopped []string
	reloads int
	pauses  []store.Pause // as the daemon keeps them: in memory, reloaded on request
}

func (b *fakeBackend) Live() map[string]Live { return b.live }

func (b *fakeBackend) Level(sessionID, repo, cwd string) config.ModeInfo {
	m := b.cfg.For(repo).ModeAt(at)
	if config.LevelRank(m.Mode) > config.LevelRank(config.ModeObserve) {
		for _, p := range b.pauses {
			if (p.Scope == store.PauseRepo && p.Target == repo) || (p.Scope == store.PauseSession && p.Target == sessionID) {
				return config.ModeInfo{Mode: config.ModeObserve, Why: "paused"}
			}
		}
	}
	return m
}

func (b *fakeBackend) Config(repo string) config.Effective { return b.cfg.For(repo) }

func (b *fakeBackend) Pricing() (*cost.Pricing, cost.PricingStatus) { return cost.Status(b.home) }

func (b *fakeBackend) Usage() Usage { return b.usage }

func (b *fakeBackend) Stop(id string) StopResult {
	if l, ok := b.live["claude-code:"+id]; !ok || l.Mode == config.ModeObserve {
		return StopResult{Why: "it observes only"}
	}
	b.stopped = append(b.stopped, id)
	return StopResult{Stopped: true}
}

func (b *fakeBackend) ReloadPauses(ctx context.Context) error {
	b.reloads++
	var err error
	b.pauses, err = b.st.Pauses(ctx, at)
	return err
}

func (b *fakeBackend) Flush() {}

// zeros is a deterministic "random" source, so tokens are stable in the
// golden files.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

type fixture struct {
	t           *testing.T
	home        string
	transcripts string
	st          *store.Store
	be          *fakeBackend
	d           *Dashboard
	clockMu     sync.Mutex // the dashboard reads the clock on its own goroutines too
	clock       time.Time
}

func (f *fixture) now() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.clock
}

func (f *fixture) setClock(t time.Time) {
	f.clockMu.Lock()
	f.clock = t
	f.clockMu.Unlock()
}

const testConfig = `# test config
mode: guard
budgets:
  daily: { soft_usd: 20, hard_usd: 50 }
  session: { hard_tool_calls: 400 }
notify:
  ntfy: { topic: ` + topic + `, on: [halt] }
`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, home: t.TempDir(), transcripts: t.TempDir(), clock: at}
	if err := os.WriteFile(filepath.Join(f.home, config.GlobalFile), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(f.home, "loopguard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.st = st
	loopKey := f.writeTranscript()
	seedStore(t, st, loopKey)
	f.be = &fakeBackend{home: f.home, st: st, cfg: config.NewLoader(f.home, config.LoaderOptions{Now: func() time.Time { return at }}),
		live: map[string]Live{"claude-code:" + sidShop: {SessionID: sidShop, Mode: config.ModeGuard, ModeWhy: "mode: guard in config.yaml"}},
		usage: Usage{Today: UsageView{Tokens: 1_234_567, CostUSD: 23.5, ToolCalls: 12, WallMinutes: 120},
			Sessions: map[string]UsageView{sidShop: {Tokens: 1_000_000, CostUSD: 3.1, ToolCalls: 12}}}}
	if err := f.be.ReloadPauses(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.be.reloads = 0

	f.d = New(Options{Store: st, Backend: f.be, Transcripts: f.transcripts, Port: testPort, Version: "test", Home: f.home,
		Now: f.now, Location: time.UTC, Random: zeros{}})
	return f
}

// writeTranscript writes sidShop's transcript (a repeated-failure loop on
// `npx jest`, then a large read) and returns the loop's key.
func (f *fixture) writeTranscript() string {
	t := f.t
	b := replaytest.NewBuilder(sidShop, "")
	b.Advance(at.Add(-2 * time.Hour).Sub(replaytest.T0)) // the same time as its database rows
	b.Model, b.CacheRead = "claude-opus-5-5", 20000
	b.Prompt("make the cart test pass")
	for i := 0; i < 5; i++ {
		b.Bash("npx jest", replaytest.JestFail(i), true)
		if i < 4 {
			b.Edit(replaytest.Cwd+`\src\cart.js`, fmt.Sprintf("v%d", i), fmt.Sprintf("v%d", i+1))
		}
	}
	b.Tool("Read", map[string]any{"file_path": replaytest.Cwd + `\logs\big.log`}, strings.Repeat("0123456789", 20000), false)
	b.Prompt("now list the files")
	b.Bash("ls src", "cart.js\nflags.ts", false)
	dir := filepath.Join(f.transcripts, replaytest.Project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sidShop+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at.Add(-time.Hour), at.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	streams, _ := replay.Discover(f.transcripts, replay.Filter{Session: sidShop})
	res := replay.Run(streams, replay.Options{Thresholds: config.Default()})
	if len(res.Loops) != 1 {
		t.Fatalf("fixture transcript: %d loops", len(res.Loops))
	}
	return res.Loops[0].LoopKey
}

// seedStore writes the fixture sessions.
func seedStore(t *testing.T, st *store.Store, loopKey string) {
	t.Helper()
	w := st.NewWriter(store.WriterOptions{})
	session := func(key, sid, repo, cwd string, start time.Time, calls, failures int, ended bool) {
		s := store.Session{Key: key, Agent: "claude-code", SessionID: sid, Repo: repo, Cwd: cwd, StartedAt: start,
			LastSeenAt: start.Add(time.Duration(calls) * time.Minute), ToolCalls: int64(calls), Failures: int64(failures)}
		if ended {
			s.EndedAt = s.LastSeenAt
		}
		w.Enqueue(s)
		for i := 1; i <= calls; i++ {
			w.Enqueue(store.Call{SessionKey: key, Seq: uint64(i), TS: start.Add(time.Duration(i) * time.Minute), Tool: "bash", Family: "npm test",
				IsError: i <= failures})
		}
	}
	usage := func(key, sid, repo string, ts time.Time, n int, model string, output int64) {
		for i := 0; i < n; i++ {
			w.Enqueue(store.Usage{SessionKey: key, SessionID: sid, Repo: repo, MessageID: fmt.Sprintf("%s-m%d", key, i),
				TS: ts.Add(time.Duration(i) * time.Minute), Model: model, Input: 10, Output: output, CacheRead: 20000, CacheWrite: 2000, CacheWrite1h: 2000})
		}
	}
	shop := "claude-code:" + sidShop
	session(shop, sidShop, repoShop, repoShop, at.Add(-2*time.Hour), 12, 5, false)
	usage(shop, sidShop, repoShop, at.Add(-2*time.Hour), 12, "claude-opus-5-5", 3000)
	// A repeated-failure loop over calls 1-6, nudged then denied (real).
	w.Enqueue(store.Signal{SessionKey: shop, Seq: 6, Detector: "repeated_failure", LoopKey: loopKey, Severity: 0.75, Count: 5,
		Evidence: []byte(`{"seqs":[1,2,3,4,5,6]}`)})
	w.Enqueue(store.Intervention{SessionKey: shop, Seq: 4, TS: at.Add(-2*time.Hour + 4*time.Minute), LoopKey: loopKey, Detector: "repeated_failure",
		Action: "nudge", Mode: store.ModeEnforce, Outcome: "continued_loop", Reason: "repeated_failure: `jest` failed 4 times"})
	w.Enqueue(store.Intervention{SessionKey: shop, Seq: 6, TS: at.Add(-2*time.Hour + 6*time.Minute), LoopKey: loopKey, Detector: "repeated_failure",
		Action: "deny", Mode: store.ModeEnforce, Outcome: "recovered", Reason: "repeated_failure: `jest` failed 5 times"})

	api := "claude-code:" + sidAPI
	session(api, sidAPI, repoAPI, repoAPI+"/cmd", at.Add(-3*24*time.Hour), 30, 2, true)
	session(api+"/agent-x", sidAPI, repoAPI, repoAPI+"/cmd", at.Add(-3*24*time.Hour+10*time.Minute), 5, 0, true)
	usage(api, sidAPI, repoAPI, at.Add(-3*24*time.Hour), 20, "claude-sonnet-5-5", 8000)
	usage(api+"/agent-x", sidAPI, repoAPI, at.Add(-3*24*time.Hour+10*time.Minute), 4, "claude-haiku-5-5", 2000)
	w.Enqueue(store.Intervention{SessionKey: api, Seq: 9, TS: at.Add(-3*24*time.Hour + 9*time.Minute), LoopKey: "ex:abc", Detector: "exact_repeat",
		Action: "nudge", Mode: store.ModeObserve, Outcome: "moved_on", Reason: "exact_repeat: the same call 3 times"})

	old := "claude-code:" + sidOld
	session(old, sidOld, "", "/tmp/scratch", at.Add(-20*24*time.Hour), 3, 0, true)
	usage(old, sidOld, "", at.Add(-20*24*time.Hour), 2, "mystery-model", 500)

	// The doctor's synthetic session is never counted.
	session("claude-code:loopguard-doctor", "loopguard-doctor", "", "/home", at.Add(-time.Hour), 1, 0, true)
	w.Close()
	if err := st.SetPause(context.Background(), store.Pause{Scope: store.PauseRepo, Target: repoAPI, CreatedAt: at.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

// client signs in through a one-time link and keeps the cookie.
type client struct {
	t      *testing.T
	d      *Dashboard
	cookie *http.Cookie
}

func (f *fixture) login() *client {
	f.t.Helper()
	link, _, err := f.d.NewLoginLink()
	if err != nil {
		f.t.Fatal(err)
	}
	c := &client{t: f.t, d: f.d}
	resp := c.do(http.MethodGet, strings.TrimPrefix(link, f.d.origin()), nil, nil)
	if resp.Code != http.StatusSeeOther {
		f.t.Fatalf("login: %d %s", resp.Code, resp.Body)
	}
	for _, ck := range resp.Result().Cookies() {
		if ck.Name == cookieName {
			c.cookie = ck
		}
	}
	if c.cookie == nil {
		f.t.Fatal("login set no cookie")
	}
	return c
}

// do sends a request to the dashboard as a browser on 127.0.0.1 would.
func (c *client) do(method, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = bytes.NewBufferString(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	req.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c.d.Handler().ServeHTTP(rec, req)
	return rec
}

func (c *client) get(path string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.do(http.MethodGet, path, nil, nil)
}

// csrfToken is the session's form token (the zeros source makes it fixed).
func (c *client) csrfToken() string {
	c.t.Helper()
	s, ok := c.d.auth.lookup(c.cookie.Value)
	if !ok {
		c.t.Fatal("no session")
	}
	return s.CSRF
}
