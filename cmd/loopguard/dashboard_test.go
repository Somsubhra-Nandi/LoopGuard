package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/dashboard"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
	"github.com/Somsubhra-Nandi/loopguard/internal/report"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// TestDashboardCommand: `loopguard dashboard` gets a one-time link from
// the running daemon, prints it, and opens it.
func TestDashboardCommand(t *testing.T) {
	dir := isolate(t)
	ready := make(chan home.DaemonInfo, 1)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- daemon.Run(ctx, daemon.Options{Home: dir, Version: version, Port: -1, Ready: func(i home.DaemonInfo) { ready <- i }})
	}()
	t.Cleanup(func() { cancel(); <-errc })
	var info home.DaemonInfo
	select {
	case info = <-ready:
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not start")
	}
	var opened []string
	old := openBrowser
	openBrowser = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openBrowser = old })

	out, errOut, code := runCLI(t, "", "dashboard")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	link := strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "Dashboard: ")
	if !strings.HasPrefix(link, "http://127.0.0.1:") || !strings.Contains(link, "/ui/login?code=") || len(opened) != 1 || opened[0] != link ||
		!strings.Contains(out, "works once, for 2 minutes") {
		t.Fatalf("output %q, opened %v (daemon port %d)", out, opened, info.Port)
	}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := browser.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the printed link: %d", resp.StatusCode)
	}

	if out, _, code := runCLI(t, "", "dashboard", "--no-open"); code != 0 || len(opened) != 1 || !strings.Contains(out, "Dashboard: ") {
		t.Errorf("--no-open: %d %q %v", code, out, opened)
	}
}

// TestDashboardMatchesReport: on the same database, the dashboard's
// numbers are `loopguard report --json`'s.
func TestDashboardMatchesReport(t *testing.T) {
	dir := isolate(t)
	root := reportTranscript(t)
	at := time.Date(2026, 10, 21, 15, 0, 0, 0, time.Local)
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })

	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	streams, _ := replay.Discover(root, replay.Filter{Session: reportSession})
	loops := replay.Run(streams, replay.Options{Thresholds: config.Default()}).Loops
	if len(loops) != 1 {
		t.Fatalf("fixture transcript: %d loops", len(loops))
	}
	w := st.NewWriter(store.WriterOptions{})
	add := func(sid string, start time.Time, calls int, outTokens int64, model string) {
		key := "claude-code:" + sid
		w.Enqueue(store.Session{Key: key, Agent: "claude-code", SessionID: sid, Repo: "/work/" + sid[:4], Cwd: "/work/" + sid[:4], StartedAt: start,
			LastSeenAt: start.Add(time.Duration(calls) * time.Minute), ToolCalls: int64(calls)})
		for i := 1; i <= calls; i++ {
			w.Enqueue(store.Call{SessionKey: key, Seq: uint64(i), TS: start.Add(time.Duration(i) * time.Minute), Tool: "bash"})
			w.Enqueue(store.Usage{SessionKey: key, SessionID: sid, MessageID: key + "-" + string(rune('a'+i)), TS: start.Add(time.Duration(i) * time.Minute),
				Model: model, Input: 7, Output: outTokens, CacheRead: 30000, CacheWrite: 900, CacheWrite1h: 600})
		}
	}
	add(reportSession, at.Add(-3*time.Hour), 8, 1200, "claude-opus-5-5")
	add("week-0001", at.Add(-4*24*time.Hour), 5, 800, "claude-sonnet-5-5")
	add("month-001", at.Add(-25*24*time.Hour), 3, 300, "mystery-model")
	key := "claude-code:" + reportSession
	w.Enqueue(store.Signal{SessionKey: key, Seq: 5, Detector: "repeated_failure", LoopKey: loops[0].LoopKey, Severity: 0.7, Evidence: []byte(`{"seqs":[2,3,4,5]}`)})
	w.Enqueue(store.Intervention{SessionKey: key, Seq: 5, TS: at.Add(-3*time.Hour + 5*time.Minute), LoopKey: loops[0].LoopKey, Detector: "repeated_failure",
		Action: "nudge", Mode: store.ModeEnforce, Outcome: "recovered"})
	w.Close()

	js, errOut, code := runCLI(t, "", "report", "--json", "--path", root)
	if code != 0 {
		t.Fatalf("report: %s", errOut)
	}
	var want report.Summary
	if err := json.Unmarshal([]byte(js), &want); err != nil {
		t.Fatal(err)
	}
	sjs, errOut, code := runCLI(t, "", "report", reportSession, "--json", "--path", root)
	if code != 0 {
		t.Fatalf("report session: %s", errOut)
	}
	var wantSession report.Session
	if err := json.Unmarshal([]byte(sjs), &wantSession); err != nil {
		t.Fatal(err)
	}

	d := dashboard.New(dashboard.Options{Store: st, Transcripts: root, Port: 47821, Version: version, Home: dir,
		Now: func() time.Time { return at }, Location: time.Local})
	get := sign(t, d)
	var got struct {
		Summary report.Summary `json:"summary"`
	}
	if err := json.Unmarshal(get("/ui/api/overview"), &got); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got.Summary.Periods)
	wb, _ := json.Marshal(want.Periods)
	if string(gb) != string(wb) {
		t.Errorf("overview periods differ from report --json:\n got %s\nwant %s", gb, wb)
	}
	if p := want.Periods; p[0].Sessions != 1 || p[1].Sessions != 2 || p[2].Sessions != 3 || p[0].Loops != 1 || p[2].UnpricedTokens == 0 {
		t.Errorf("fixture periods %+v", p)
	}
	html := string(get("/ui/"))
	for _, p := range want.Periods {
		for _, s := range []string{cost.FormatUSD(p.CostUSD), cost.FormatUSD(p.LoopCostUSD), report.CompactCount(p.Tokens)} {
			if !strings.Contains(html, s) {
				t.Errorf("the overview page lacks %s (%s)", s, p.Name)
			}
		}
	}

	var detail struct {
		Report report.Session `json:"report"`
	}
	if err := json.Unmarshal(get("/ui/api/sessions/"+reportSession), &detail); err != nil {
		t.Fatal(err)
	}
	gs, _ := json.Marshal(detail.Report)
	ws, _ := json.Marshal(wantSession)
	if string(gs) != string(ws) {
		t.Errorf("session detail differs from report <id> --json:\n got %s\nwant %s", gs, ws)
	}
	page := string(get("/ui/sessions/" + reportSession))
	for _, s := range []string{cost.FormatUSD(wantSession.Totals.CostUSD), cost.FormatUSD(wantSession.Totals.LoopCostUSD), "recovered"} {
		if !strings.Contains(page, s) {
			t.Errorf("the session page lacks %s", s)
		}
	}
}

// sign signs in to d through a one-time link and returns a GET helper.
func sign(t *testing.T, d *dashboard.Dashboard) func(path string) []byte {
	t.Helper()
	link, _, err := d.NewLoginLink()
	if err != nil {
		t.Fatal(err)
	}
	do := func(path string, ck *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:47821"
		if ck != nil {
			req.AddCookie(ck)
		}
		rec := httptest.NewRecorder()
		d.Handler().ServeHTTP(rec, req)
		return rec
	}
	r := do(strings.TrimPrefix(link, "http://127.0.0.1:47821"), nil)
	cks := r.Result().Cookies()
	if r.Code != http.StatusSeeOther || len(cks) != 1 {
		t.Fatalf("login: %d", r.Code)
	}
	return func(path string) []byte {
		t.Helper()
		r := do(path, cks[0])
		if r.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, r.Code, r.Body)
		}
		return r.Body.Bytes()
	}
}
