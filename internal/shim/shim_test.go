package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
)

const samplePayload = `{"session_id":"s1","hook_event_name":"PreToolUse","cwd":"/repo","tool_name":"Bash","tool_input":{"command":"npm test"},"tool_use_id":"t1"}`

// fakeHome is a LoopGuard home with a token, and daemon.json pointing at
// port (if non-zero).
func fakeHome(t *testing.T, port int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home with spaces")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := home.EnsureToken(dir); err != nil {
		t.Fatal(err)
	}
	if port != 0 {
		if err := home.WriteDaemonInfo(dir, home.DaemonInfo{PID: os.Getpid(), Port: port}); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func serverPort(t *testing.T, s *httptest.Server) int {
	return s.Listener.Addr().(*net.TCPAddr).Port
}

type result struct {
	code           int
	stdout, stderr string
	spawns         int
	took           time.Duration
}

func run(t *testing.T, dir, stdin string, env map[string]string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	spawns := 0
	start := time.Now()
	code := Run(Options{
		Agent: "claude-code", Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut, Home: dir,
		Getenv: func(k string) string { return env[k] },
		Spawn:  func(string) error { spawns++; return nil },
	})
	return result{code, out.String(), errOut.String(), spawns, time.Since(start)}
}

func TestRoundTripPassesReplyThrough(t *testing.T) {
	// The handler hands what it saw over a channel: the race detector
	// doesn't see the TCP round trip as synchronization.
	seen := make(chan [4]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- [4]string{r.Header.Get("Authorization"), r.URL.Query().Get("agent"), r.Header.Get(timeoutHeader), string(b)}
		w.Write([]byte(`{"stdout":"{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\"}}","exit":0}`))
	}))
	defer srv.Close()
	dir := fakeHome(t, serverPort(t, srv))
	tok, _ := home.ReadToken(dir)

	r := run(t, dir, samplePayload, nil)
	if r.code != 0 || r.stdout != `{"hookSpecificOutput":{"hookEventName":"PreToolUse"}}` || r.stderr != "" {
		t.Errorf("result = %+v", r)
	}
	if got := <-seen; got != [4]string{"Bearer " + tok, "claude-code", "800", samplePayload} {
		t.Errorf("request (auth, agent, timeout, body) = %q", got)
	}
	if r.spawns != 0 {
		t.Error("spawned a daemon although one is running")
	}
}

func TestForwardsHowClaudeCodeRuns(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.RawQuery
		w.Write([]byte(`{"stdout":"","exit":0}`))
	}))
	defer srv.Close()
	dir := fakeHome(t, serverPort(t, srv))
	run(t, dir, samplePayload, map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli", "CLAUDE_CODE_SESSION_ATTENDED": "1"})
	if got := <-seen; got != "agent=claude-code&entry=cli&attended=1" {
		t.Errorf("query %q", got)
	}
	run(t, dir, samplePayload, nil)
	if got := <-seen; got != "agent=claude-code" {
		t.Errorf("query without the environment %q", got)
	}
}

func TestExitCodeFromReply(t *testing.T) {
	for reply, want := range map[string]int{
		`{"stdout":"","exit":0}`:    0,
		`{"stdout":"x","exit":3}`:   3,
		`{"stdout":"x","exit":-1}`:  0,
		`{"stdout":"x","exit":999}`: 0,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(reply)) }))
		r := run(t, fakeHome(t, serverPort(t, srv)), samplePayload, nil)
		srv.Close()
		if r.code != want {
			t.Errorf("reply %s: exit %d, want %d", reply, r.code, want)
		}
		if want == 0 && reply != `{"stdout":"","exit":0}` && r.stdout != "" {
			t.Errorf("reply %s: invalid exit code but stdout %q printed", reply, r.stdout)
		}
	}
}

func TestDaemonSlowFailsOpen(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Write([]byte(`{"stdout":"late","exit":0}`))
	}))
	defer srv.Close()
	defer close(release)
	dir := fakeHome(t, serverPort(t, srv))

	r := run(t, dir, samplePayload, nil)
	if r.code != 0 || r.stdout != "" || r.stderr != "" || r.spawns != 0 {
		t.Errorf("slow daemon: %+v", r)
	}
	if r.took < 750*time.Millisecond || r.took > 2*time.Second {
		t.Errorf("gave up after %v, want about 800 ms", r.took)
	}
	// LOOPGUARD_HOOK_TIMEOUT_MS lowers the timeout, never raises it.
	r = run(t, dir, samplePayload, map[string]string{EnvTimeoutMS: "100"})
	if r.code != 0 || r.stdout != "" || r.took > 600*time.Millisecond {
		t.Errorf("with a 100 ms timeout: %+v", r)
	}
}

func TestBadRepliesFailOpen(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"garbage": func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("\x00\xffnot json")) },
		"401":     func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "unauthorized", http.StatusUnauthorized) },
		"500": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"stdout":"x","exit":0}`, http.StatusInternalServerError)
		},
		"empty":        func(http.ResponseWriter, *http.Request) {},
		"hang up":      func(w http.ResponseWriter, _ *http.Request) { panic(http.ErrAbortHandler) },
		"wrong shape":  func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"stdout":7}`)) },
		"huge garbage": func(w http.ResponseWriter, _ *http.Request) { w.Write(bytes.Repeat([]byte("a"), 2<<20)) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			r := run(t, fakeHome(t, serverPort(t, srv)), samplePayload, nil)
			if r.code != 0 || r.stdout != "" || r.stderr != "" {
				t.Errorf("%+v", r)
			}
		})
	}
}

func TestDaemonDownStartsItRateLimited(t *testing.T) {
	dir := fakeHome(t, 0) // no daemon.json
	r := run(t, dir, samplePayload, nil)
	if r.code != 0 || r.stdout != "" || r.stderr != "" || r.spawns != 1 {
		t.Fatalf("first event with no daemon: %+v (want one spawn)", r)
	}
	if r.took > 500*time.Millisecond {
		t.Errorf("took %v: must not wait for the daemon", r.took)
	}
	if r = run(t, dir, samplePayload, nil); r.spawns != 0 {
		t.Error("spawned again within 10 s")
	}
	// After 10 s, it tries again.
	old := time.Now().Add(-AutostartEvery - time.Second)
	if err := os.Chtimes(filepath.Join(dir, autostartLock), old, old); err != nil {
		t.Fatal(err)
	}
	if r = run(t, dir, samplePayload, nil); r.spawns != 1 {
		t.Errorf("did not retry after 10 s: %+v", r)
	}
	// LOOPGUARD_NO_AUTOSTART=1 never starts it.
	os.Chtimes(filepath.Join(dir, autostartLock), old, old)
	if r = run(t, dir, samplePayload, map[string]string{EnvNoAutostart: "1"}); r.spawns != 0 || r.code != 0 {
		t.Errorf("autostart disabled: %+v", r)
	}
	// The home directory is created if missing.
	missing := filepath.Join(t.TempDir(), "new home")
	if r = run(t, missing, samplePayload, nil); r.spawns != 1 {
		t.Errorf("missing home: %+v", r)
	}
}

func TestStaleDaemonJSONStartsDaemon(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens there now
	r := run(t, fakeHome(t, port), samplePayload, nil)
	if r.code != 0 || r.stdout != "" || r.spawns != 1 {
		t.Errorf("stale daemon.json: %+v", r)
	}
}

func TestDisableAndOversize(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"stdout":"x","exit":0}`))
	}))
	defer srv.Close()
	dir := fakeHome(t, serverPort(t, srv))

	in := &countingReader{r: strings.NewReader(samplePayload)}
	var out bytes.Buffer
	code := Run(Options{Stdin: in, Stdout: &out, Home: dir, Getenv: func(k string) string {
		return map[string]string{EnvDisable: "1"}[k]
	}})
	if code != 0 || out.Len() != 0 || hits.Load() != 0 || in.n != len(samplePayload) {
		t.Errorf("disabled: code %d, stdout %q, %d requests, read %d bytes", code, out.String(), hits.Load(), in.n)
	}

	big := `{"x":"` + strings.Repeat("a", MaxPayload) + `"}`
	in = &countingReader{r: strings.NewReader(big)}
	code = Run(Options{Stdin: in, Stdout: &out, Home: dir, Getenv: func(string) string { return "" }})
	if code != 0 || out.Len() != 0 || hits.Load() != 0 || in.n != len(big) {
		t.Errorf("oversize: code %d, %d requests, read %d of %d bytes", code, hits.Load(), in.n, len(big))
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("boom") }

func TestDebugAndPanics(t *testing.T) {
	dir := fakeHome(t, 0)
	r := run(t, dir, samplePayload, map[string]string{EnvDebug: "1", EnvNoAutostart: "1"})
	if r.code != 0 || r.stdout != "" || !strings.Contains(r.stderr, "daemon not running") {
		t.Errorf("debug: %+v", r)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"stdout":"x","exit":0}`)) }))
	defer srv.Close()
	code := Run(Options{Stdin: strings.NewReader("{}"), Stdout: panicWriter{}, Home: fakeHome(t, serverPort(t, srv)),
		Getenv: func(string) string { return "" }})
	if code != 0 {
		t.Errorf("panic while writing stdout: exit %d", code)
	}
}

// TestRealDaemonRoundTrip sends events through the shim to a real daemon.
func TestRealDaemonRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "real home")
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan home.DaemonInfo, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- daemon.Run(ctx, daemon.Options{Home: dir, Version: "test", Port: -1, Ready: func(i home.DaemonInfo) { ready <- i }})
	}()
	var info home.DaemonInfo
	select {
	case info = <-ready:
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not start")
	}
	defer func() { cancel(); <-errc }()

	for _, p := range []string{
		`{"session_id":"rt","hook_event_name":"SessionStart","source":"startup"}`,
		strings.Replace(samplePayload, `"s1"`, `"rt"`, 1),
		strings.Replace(strings.Replace(samplePayload, `"s1"`, `"rt"`, 1), "PreToolUse", "PostToolUse", 1),
	} {
		r := run(t, dir, p, nil)
		if r.code != 0 || r.stdout != "" || r.stderr != "" || r.spawns != 0 {
			t.Errorf("event %s: %+v (observe mode: want no output)", p[:60], r)
		}
	}

	tok, _ := home.ReadToken(dir)
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(info.Port)+"/v1/sessions/rt", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var detail struct {
		Session struct {
			ToolCalls int `json:"tool_calls"`
		} `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil || resp.StatusCode != 200 || detail.Session.ToolCalls != 1 {
		t.Errorf("session after round trip: %d %+v %v", resp.StatusCode, detail, err)
	}
}
