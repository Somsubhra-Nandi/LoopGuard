package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/capture"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

type testDaemon struct {
	t     *testing.T
	home  string
	info  home.DaemonInfo
	token string
	stop  func() error
	// query is added to /v1/events requests (e.g. "&entry=cli").
	query string
}

func startDaemon(t *testing.T, dir string, mod func(*Options)) *testDaemon {
	t.Helper()
	o := Options{Home: dir, Version: "test", Port: -1}
	if mod != nil {
		mod(&o)
	}
	ready := make(chan home.DaemonInfo, 1)
	o.Ready = func(i home.DaemonInfo) { ready <- i }
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Run(ctx, o) }()
	var info home.DaemonInfo
	select {
	case info = <-ready:
	case err := <-errc:
		cancel()
		t.Fatalf("Run: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("daemon did not start")
	}
	tok, err := home.ReadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var runErr error
	stop := func() error {
		once.Do(func() { cancel(); runErr = <-errc })
		return runErr
	}
	t.Cleanup(func() { _ = stop() })
	return &testDaemon{t: t, home: dir, info: info, token: tok, stop: stop}
}

func tempHome(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "Loop Guard home")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (d *testDaemon) url(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", d.info.Port, path)
}

func (d *testDaemon) do(method, path string, body []byte, hdr map[string]string) (int, []byte) {
	d.t.Helper()
	req, err := http.NewRequest(method, d.url(path), bytes.NewReader(body))
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (d *testDaemon) event(payload string) EventReply {
	d.t.Helper()
	code, b := d.do("POST", "/v1/events?agent=claude-code"+d.query, []byte(payload), nil)
	if code != http.StatusOK {
		d.t.Fatalf("POST /v1/events: %d %s", code, b)
	}
	var r EventReply
	if err := json.Unmarshal(b, &r); err != nil {
		d.t.Fatalf("reply %s: %v", b, err)
	}
	return r
}

func payload(event, sid string, extra string) string {
	s := fmt.Sprintf(`{"session_id":%q,"hook_event_name":%q,"cwd":"/repo","permission_mode":"bypassPermissions"`, sid, event)
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

// failingRun sends the pre and post_failure of one failing `npm test`.
func failingRun(d *testDaemon, sid string, i int) []EventReply {
	id := fmt.Sprintf(`"tool_name":"Bash","tool_use_id":"toolu_%d","tool_input":{"command":"npm test"}`, i)
	return []EventReply{
		d.event(payload("PreToolUse", sid, id)),
		d.event(payload("PostToolUseFailure", sid, id+`,"error":"FAIL src/cart.test.ts\n  ● total › adds tax\n    Expected: 110\n    Received: 100"`)),
	}
}

func TestDaemonObserveModeEndToEnd(t *testing.T) {
	d := startDaemon(t, tempHome(t), nil)

	if r := d.event(payload("SessionStart", "s1", `"source":"startup","model":"claude-opus-5-5"`)); r.Stdout != "" || r.Exit != 0 {
		t.Errorf("SessionStart reply = %+v", r)
	}
	d.event(payload("UserPromptSubmit", "s1", `"prompt":"fix the test"`))
	// Six identical failures: D2 fires from the 4th, and enforce mode would
	// have nudged. Observe mode must still answer "none" every time.
	for i := 1; i <= 6; i++ {
		for _, r := range failingRun(d, "s1", i) {
			if r.Stdout != "" || r.Exit != 0 {
				t.Fatalf("run %d: reply %+v, want none (observe mode)", i, r)
			}
		}
	}
	d.event(payload("Stop", "s1", ""))

	code, b := d.do("GET", "/v1/sessions", nil, nil)
	if code != 200 {
		t.Fatalf("GET /v1/sessions: %d %s", code, b)
	}
	var list struct {
		Sessions []sessionView `json:"sessions"`
		Store    string        `json:"store"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	if list.Store != "sqlite" || len(list.Sessions) != 1 {
		t.Fatalf("sessions = %s", b)
	}
	s := list.Sessions[0]
	if s.Key != "claude-code:s1" || s.ToolCalls != 6 || s.LoopsDetected != 1 || s.Model != "claude-opus-5-5" || !s.Active || s.Cwd != "/repo" {
		t.Errorf("session = %+v", s)
	}

	code, b = d.do("GET", "/v1/sessions/s1", nil, nil)
	if code != 200 {
		t.Fatalf("GET /v1/sessions/s1: %d %s", code, b)
	}
	var detail struct {
		Calls []struct {
			Seq         uint64   `json:"seq"`
			IsError     bool     `json:"is_error"`
			Family      string   `json:"family"`
			InputDigest string   `json:"input_digest"`
			ErrLines    []string `json:"err_lines"`
		} `json:"calls"`
		Signals       []struct{ Detector string }     `json:"signals"`
		Interventions []struct{ Action, Mode string } `json:"interventions"`
	}
	if err := json.Unmarshal(b, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Calls) != 6 || !detail.Calls[0].IsError || detail.Calls[0].Family != "npm test" || detail.Calls[0].InputDigest != "npm test" || len(detail.Calls[0].ErrLines) == 0 {
		t.Errorf("calls = %+v", detail.Calls)
	}
	for i := 1; i < len(detail.Calls); i++ {
		if detail.Calls[i].Seq <= detail.Calls[i-1].Seq {
			t.Errorf("calls out of order: %+v", detail.Calls)
		}
	}
	if n := len(detail.Signals); n == 0 || detail.Signals[n-1].Detector != "repeated_failure" {
		t.Errorf("signals = %+v", detail.Signals)
	}
	if len(detail.Interventions) == 0 || detail.Interventions[0].Action != "nudge" || detail.Interventions[0].Mode != "observe" {
		t.Errorf("interventions = %+v (want an observe-mode would-have nudge)", detail.Interventions)
	}
	if s.Failures != 6 {
		t.Errorf("session failures = %d, want 6", s.Failures)
	}
	code, b = d.do("GET", "/v1/decisions?limit=3", nil, nil)
	var decs struct {
		Decisions []store.Intervention `json:"decisions"`
	}
	if err := json.Unmarshal(b, &decs); code != 200 || err != nil || len(decs.Decisions) == 0 {
		t.Fatalf("GET /v1/decisions: %d %s", code, b)
	}
	if dd := decs.Decisions[0]; dd.Action != "nudge" || dd.Detector != "repeated_failure" || dd.Reason == "" ||
		dd.TS.IsZero() || dd.SessionKey != "claude-code:s1" {
		t.Errorf("decision = %+v", dd)
	}
	if code, _ := d.do("GET", "/v1/decisions?limit=x", nil, nil); code != 400 {
		t.Errorf("bad limit: %d, want 400", code)
	}
	if code, _ := d.do("GET", "/v1/sessions/nope", nil, nil); code != 404 {
		t.Errorf("unknown session: %d, want 404", code)
	}

	code, b = d.do("GET", "/metrics", nil, nil)
	m := string(b)
	for _, want := range []string{
		`loopguard_events_total{agent="claude-code",event="post_failure"} 6`,
		`loopguard_decisions_total{would_have="nudge"}`,
		`loopguard_event_latency_seconds_count 15`,
		`loopguard_active_sessions 1`,
		`loopguard_store_memory_only 0`,
	} {
		if code != 200 || !strings.Contains(m, want) {
			t.Errorf("metrics missing %q:\n%s", want, m)
		}
	}

	if err := d.stop(); err != nil {
		t.Errorf("Run returned %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.home, home.DaemonFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("daemon.json left behind after shutdown: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(d.home, home.LogDir, "daemon.log")); err != nil || fi.Size() == 0 {
		t.Errorf("daemon.log: %v", err)
	}
}

func TestDaemonAuthAndHost(t *testing.T) {
	d := startDaemon(t, tempHome(t), nil)

	resp, err := http.Get(d.url("/healthz"))
	if err != nil {
		t.Fatal(err)
	}
	var h Health
	json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if resp.StatusCode != 200 || !h.OK || h.Service != "loopguard" || h.HomeID != HomeID(d.home) || h.Store != "sqlite" {
		t.Errorf("healthz = %d %+v", resp.StatusCode, h)
	}

	for _, tc := range []struct {
		method, path, auth string
	}{
		{"POST", "/v1/events?agent=claude-code", ""},
		{"POST", "/v1/events?agent=claude-code", "Bearer wrong"},
		{"GET", "/v1/sessions", ""},
		{"GET", "/v1/sessions/x", "Basic " + d.token},
		{"GET", "/metrics", ""},
		{"POST", "/v1/shutdown", ""},
	} {
		req, _ := http.NewRequest(tc.method, d.url(tc.path), strings.NewReader("{}"))
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with auth %q: %d, want 401", tc.method, tc.path, tc.auth, resp.StatusCode)
		}
	}

	// DNS rebinding: a foreign Host is rejected even with the token.
	for host, want := range map[string]int{
		"evil.example.com":                       403,
		"127.0.0.1.evil.example.com":             403,
		fmt.Sprintf("localhost:%d", d.info.Port): 200,
		"LOCALHOST":                              200,
	} {
		req, _ := http.NewRequest("GET", d.url("/v1/sessions"), nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+d.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Host %q: %d, want %d", host, resp.StatusCode, want)
		}
	}
}

func TestDaemonFailsSoftOnBadInput(t *testing.T) {
	d := startDaemon(t, tempHome(t), nil)
	for name, tc := range map[string]struct{ path, body string }{
		"garbage":       {"/v1/events?agent=claude-code", "not json at all"},
		"no session":    {"/v1/events?agent=claude-code", `{"hook_event_name":"PreToolUse"}`},
		"unknown event": {"/v1/events?agent=claude-code", payload("Notification", "s", `"message":"hi"`)},
		"unknown agent": {"/v1/events?agent=other", payload("PreToolUse", "s", "")},
		"no agent":      {"/v1/events", payload("PreToolUse", "s", "")},
		"too large":     {"/v1/events?agent=claude-code", `{"x":"` + strings.Repeat("a", MaxPayload) + `"}`},
	} {
		code, b := d.do("POST", tc.path, []byte(tc.body), nil)
		if code != 200 || strings.TrimSpace(string(b)) != `{"stdout":"","exit":0}` {
			t.Errorf("%s: %d %s, want 200 with an empty decision", name, code, b)
		}
	}
	if _, b := d.do("GET", "/v1/sessions", nil, nil); !strings.Contains(string(b), `"sessions":[]`) {
		t.Errorf("bad input created sessions: %s", b)
	}
}

func TestDaemonSingleInstance(t *testing.T) {
	dir := tempHome(t)
	d := startDaemon(t, dir, nil)

	err := Run(context.Background(), Options{Home: dir, Port: d.info.Port})
	var already *AlreadyRunningError
	if !errors.As(err, &already) || already.Info.Port != d.info.Port {
		t.Fatalf("second Run = %v, want AlreadyRunningError for port %d", err, d.info.Port)
	}
	// Found by the port probe too, without daemon.json.
	os.Remove(filepath.Join(dir, home.DaemonFile))
	if err := Run(context.Background(), Options{Home: dir, Port: d.info.Port}); !errors.As(err, &already) {
		t.Fatalf("Run without daemon.json = %v, want AlreadyRunningError", err)
	}

	// A daemon for another home on the port doesn't count: the next port
	// is used.
	other := startDaemon(t, tempHome(t), func(o *Options) { o.Port = d.info.Port })
	if other.info.Port == d.info.Port {
		t.Error("second home got the same port")
	}
}

func TestDaemonStaleDaemonJSON(t *testing.T) {
	dir := tempHome(t)
	// A dead pid on a closed port: a crashed daemon.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := home.WriteDaemonInfo(dir, home.DaemonInfo{PID: 0x7ffffffd, Port: port}); err != nil {
		t.Fatal(err)
	}
	d := startDaemon(t, dir, nil)
	got, err := home.ReadDaemonInfo(dir)
	if err != nil || got.PID != os.Getpid() || got.Port != d.info.Port {
		t.Errorf("daemon.json = %+v, %v; want this daemon", got, err)
	}
}

func TestDaemonMemoryOnly(t *testing.T) {
	dir := tempHome(t)
	// A directory where the database file should be: it can't open.
	if err := os.MkdirAll(filepath.Join(dir, home.DBFile), 0o700); err != nil {
		t.Fatal(err)
	}
	d := startDaemon(t, dir, nil)
	for i := 1; i <= 5; i++ {
		failingRun(d, "mem", i)
	}
	_, b := d.do("GET", "/v1/sessions", nil, nil)
	if !strings.Contains(string(b), `"store":"memory-only"`) || !strings.Contains(string(b), `"tool_calls":5`) {
		t.Errorf("memory-only sessions = %s", b)
	}
	code, b := d.do("GET", "/v1/sessions/mem", nil, nil)
	if code != 200 || !strings.Contains(string(b), `"action":"nudge"`) {
		t.Errorf("memory-only detail = %d %s", code, b)
	}
	logb, _ := os.ReadFile(filepath.Join(dir, home.LogDir, "daemon.log"))
	if !strings.Contains(string(logb), "memory-only") {
		t.Errorf("no memory-only warning in the log:\n%s", logb)
	}
}

func TestDaemonCapture(t *testing.T) {
	dir := tempHome(t)
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home")
	}
	d := startDaemon(t, dir, func(o *Options) { o.Capture = true })
	cwd := filepath.Join(userHome, "src", "proj")
	cwdJSON, _ := json.Marshal(cwd)
	secret := "sk-ant-api03-" + strings.Repeat("A", 40)
	for i := 0; i < capture.MaxPerEvent+5; i++ {
		d.event(fmt.Sprintf(`{"session_id":"c","hook_event_name":"PreToolUse","cwd":%s,"tool_name":"Bash","tool_input":{"command":"curl -H 'x-api-key: %s' x"}}`, cwdJSON, secret))
	}
	d.event(`not json`)
	capDir := filepath.Join(dir, home.CaptureDir, "claude-code")
	var files []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		files, _ = filepath.Glob(filepath.Join(capDir, "PreToolUse-*.json"))
		if len(files) == capture.MaxPerEvent {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // any extra (wrong) writes land now
	files, _ = filepath.Glob(filepath.Join(capDir, "*"))
	if len(files) != capture.MaxPerEvent {
		t.Fatalf("captured %d files, want %d: %v", len(files), capture.MaxPerEvent, files)
	}
	b, err := os.ReadFile(filepath.Join(capDir, "PreToolUse-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, secret) || !strings.Contains(s, "<REDACTED:") {
		t.Errorf("secret not redacted:\n%s", s)
	}
	if strings.Contains(strings.ToLower(s), strings.ToLower(filepath.Base(userHome))) || !strings.Contains(s, capture.HomePlaceholder) {
		t.Errorf("home directory not replaced:\n%s", s)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil || v["hook_event_name"] != "PreToolUse" {
		t.Errorf("captured file is not the payload: %v", err)
	}
}

func TestBudget(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":     750 * time.Millisecond,
		"x":    750 * time.Millisecond,
		"-5":   750 * time.Millisecond,
		"300":  250 * time.Millisecond,
		"20":   minBudget,
		"5000": 4950 * time.Millisecond,
	} {
		if got := budget(in); got != want {
			t.Errorf("budget(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLogRotation(t *testing.T) {
	old := maxLogSize
	maxLogSize = 100
	t.Cleanup(func() { maxLogSize = old })
	path := filepath.Join(t.TempDir(), "logs", "daemon.log")
	l, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 59) + "\n")
	for i := 0; i < 10; i++ {
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	for _, p := range []string{path, path + ".1", path + ".2"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Size() > 100 {
			t.Errorf("%s: %v (size %d)", filepath.Base(p), err, fi.Size())
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("more backups than logBackups")
	}
}

// TestCaptureFlagNeedsNoRestart turns capture on and off with the flag
// file while the daemon runs.
func TestCaptureFlagNeedsNoRestart(t *testing.T) {
	dir := tempHome(t)
	d := startDaemon(t, dir, nil)
	capDir := filepath.Join(dir, home.CaptureDir, "claude-code")
	count := func() int {
		files, _ := filepath.Glob(filepath.Join(capDir, "*.json"))
		return len(files)
	}
	healthCapture := func() bool {
		h, ok := Healthy(d.info.Port, dir)
		if !ok {
			t.Fatal("daemon not healthy")
		}
		return h.Capture
	}
	stop := `{"session_id":"f","hook_event_name":"Stop"}`
	d.event(stop)
	time.Sleep(100 * time.Millisecond)
	if count() != 0 || healthCapture() {
		t.Fatal("captured with capture off")
	}

	if err := capture.SetFlag(dir, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(capture.SwitchTTL + 3*time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		d.event(stop)
		time.Sleep(50 * time.Millisecond)
	}
	if count() == 0 || !healthCapture() {
		t.Fatal("flag file did not turn capture on")
	}

	if err := capture.SetFlag(dir, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(capture.SwitchTTL + 100*time.Millisecond)
	before := count()
	d.event(stop)
	time.Sleep(100 * time.Millisecond)
	if count() != before || healthCapture() {
		t.Error("still capturing after the flag file was removed")
	}
}
