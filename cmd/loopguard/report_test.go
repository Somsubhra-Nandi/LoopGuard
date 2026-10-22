package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay/replaytest"
	"github.com/Somsubhra-Nandi/loopguard/internal/report"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

var updateReport = flag.Bool("update-report", false, "rewrite testdata/report-*.golden")

const reportSession = "5e551011-0000-4000-8000-000000000001"

// reportTranscript writes a synthetic session priced as Claude Opus 5.5
// (every call reads a 20,000-token cached prefix):
//
//   - turn 1: `npx jest` fails 5 times with the same error, with edits in
//     between (a repeated-failure loop), then one Read whose 200,000-character
//     result every later call carries;
//   - turn 2: two small commands.
//
// The large output clearly dominates the estimates.
func reportTranscript(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	b := replaytest.NewBuilder(reportSession, "")
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
	b.Bash("git status", "clean", false)
	dir := filepath.Join(root, replaytest.Project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, reportSession+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateReport {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./cmd/loopguard -run Report -update-report)", err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Errorf("%s differs; if intended, rerun with -update-report and review the diff.\ngot:\n%s", name, got)
	}
}

func TestReportSession(t *testing.T) {
	root := reportTranscript(t)
	isolate(t)
	out, errOut, code := runCLI(t, "", "report", "5e5510", "--path", root)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	checkGolden(t, "report-session.golden", out)

	js, _, code := runCLI(t, "", "report", "5e5510", "--path", root, "--json")
	if code != 0 {
		t.Fatal("json report failed")
	}
	checkGolden(t, "report-session.json.golden", js)
	var r report.Session
	if err := json.Unmarshal([]byte(js), &r); err != nil {
		t.Fatal(err)
	}
	if r.Basis != replay.CostBasis || r.Totals.Calls != 12 || r.Totals.Loops != 1 || r.Totals.Turns != 2 {
		t.Errorf("totals %+v", r.Totals)
	}
	// The large output dominates both lists, and the estimated shares add
	// up to no more than the real total.
	if g := r.Estimates.Top; !strings.HasPrefix(g[0].Group, "read ") || g[0].EstUSD < 3*g[1].EstUSD {
		t.Errorf("top groups %+v", g)
	}
	if o := r.Estimates.Largest[0]; o.ResultTokens != 50000 || o.Call != 10 {
		t.Errorf("largest output %+v", o)
	}
	var est, share float64
	for _, g := range r.Estimates.Top {
		est += g.EstUSD
		share += g.EstShare
	}
	if est > r.Totals.CostUSD+1e-9 || share > 1+1e-9 {
		t.Errorf("estimates %v (share %v) exceed the session's cost %v", est, share, r.Totals.CostUSD)
	}
	l := r.Streams[0].Turns[0].Loops
	if len(l) != 1 || l[0].Detector != "repeated_failure" || l[0].CostUSD == nil || *l[0].CostUSD <= 0 || l[0].Actions[0].Kind != "replay" {
		t.Errorf("loop %+v", l)
	}
	for _, s := range []string{out, js} {
		if strings.Contains(s, "0123456789") || strings.Contains(s, "make the cart test pass") {
			t.Error("raw output or prompt text in the report")
		}
	}

	if _, errOut, code := runCLI(t, "", "report", "nope", "--path", root); code == 0 || !strings.Contains(errOut, "no transcript") {
		t.Errorf("unknown session: %d %s", code, errOut)
	}
}

// TestReportSessionLive: with LoopGuard's database, the loop shows what
// LoopGuard really did and the outcome.
func TestReportSessionLive(t *testing.T) {
	root := reportTranscript(t)
	dir := isolate(t)
	streams, _ := replay.Discover(root, replay.Filter{Session: reportSession})
	res := replay.Run(streams, replay.Options{Thresholds: config.Default()})
	if len(res.Loops) != 1 {
		t.Fatalf("loops %+v", res.Loops)
	}
	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	key := "claude-code:" + reportSession
	w := st.NewWriter(store.WriterOptions{})
	w.Enqueue(store.Session{Key: key, Agent: "claude-code", SessionID: reportSession, StartedAt: replaytest.T0, LastSeenAt: replaytest.T0.Add(time.Hour)})
	w.Enqueue(store.Intervention{SessionKey: key, Seq: 20, TS: replaytest.T0.Add(5 * time.Minute), LoopKey: res.Loops[0].LoopKey,
		Detector: "repeated_failure", Action: "nudge", Mode: store.ModeEnforce, Outcome: "recovered", Reason: "repeated_failure: `jest` failed 4 times"})
	w.Enqueue(store.Intervention{SessionKey: key, Seq: 30, TS: replaytest.T0.Add(6 * time.Minute), LoopKey: "bg:session:cost_usd",
		Detector: "budget", Action: "nudge", Mode: store.ModeEnforce, Outcome: "n/a", Reason: "this session's API-equivalent cost is $5.20"})
	w.Close()
	st.Close()

	out, errOut, code := runCLI(t, "", "report", reportSession, "--path", root)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"with what LoopGuard did live", "LoopGuard: real nudge at +", "outcome recovered",
		"Other: real nudge (budget)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// TestReportSummary seeds a database and checks the periods.
func TestReportSummary(t *testing.T) {
	dir := isolate(t)
	at := time.Date(2026, 10, 21, 15, 0, 0, 0, time.Local)
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })

	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	w := st.NewWriter(store.WriterOptions{})
	add := func(sid string, start time.Time, calls int, usd float64, model string) {
		key := "claude-code:" + sid
		w.Enqueue(store.Session{Key: key, Agent: "claude-code", SessionID: sid, StartedAt: start, LastSeenAt: start.Add(time.Duration(calls) * time.Minute)})
		for i := 1; i <= calls; i++ {
			w.Enqueue(store.Call{SessionKey: key, Seq: uint64(i), TS: start.Add(time.Duration(i) * time.Minute), Tool: "bash"})
		}
		w.Enqueue(store.Usage{SessionKey: key, SessionID: sid, MessageID: "m-" + sid, TS: start.Add(time.Minute), Model: model, Output: max(int64(usd*50_000), 100)})
	}
	add("today-1", at.Add(-2*time.Hour), 4, 2, "claude-opus-5-5")
	add("week-1", at.Add(-3*24*time.Hour), 2, 1, "claude-opus-5-5")
	add("month-1", at.Add(-20*24*time.Hour), 1, 0, "mystery-model")
	// A loop in today's session over calls 2-4, and its nudge.
	w.Enqueue(store.Signal{SessionKey: "claude-code:today-1", Seq: 4, Detector: "repeated_failure", LoopKey: "rf:x", Severity: 0.7,
		Evidence: []byte(`{"seqs":[2,3,4]}`)})
	w.Enqueue(store.Intervention{SessionKey: "claude-code:today-1", Seq: 4, TS: at.Add(-time.Hour), LoopKey: "rf:x", Detector: "repeated_failure",
		Action: "nudge", Mode: store.ModeObserve})
	w.Close()
	st.Close()

	empty := t.TempDir()
	js, errOut, code := runCLI(t, "", "report", "--json", "--path", empty)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var r report.Summary
	if err := json.Unmarshal([]byte(js), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Database || len(r.Periods) != 3 {
		t.Fatalf("%+v", r)
	}
	type row struct {
		sessions   int
		calls      int64
		usd        float64
		loops      int
		would      int64
		loopSec    float64
		unpriced   bool
		loopUSDPos bool
	}
	want := []row{{1, 4, 2, 1, 1, 120, false, false}, {2, 6, 3, 1, 1, 120, false, false}, {3, 7, 3, 1, 1, 120, true, false}}
	for i, p := range r.Periods {
		got := row{p.Sessions, p.ToolCalls, p.CostUSD, p.Loops, p.Interventions.Would, p.LoopSeconds, len(p.UnknownModels) > 0, p.LoopCostUSD > 0}
		if fmt.Sprintf("%.6f", got.usd) != fmt.Sprintf("%.6f", want[i].usd) {
			t.Errorf("%s cost %v, want %v", p.Name, got.usd, want[i].usd)
		}
		got.usd = want[i].usd
		if got != want[i] {
			t.Errorf("%s: %+v, want %+v", p.Name, got, want[i])
		}
	}
	text, _, _ := runCLI(t, "", "report", "--path", empty)
	for _, w := range []string{"API-equivalent", "cost inside loops (est.)", "$2.00", "Top 0 by estimated cost (est.)"} {
		if !strings.Contains(text, w) {
			t.Errorf("summary text lacks %q:\n%s", w, text)
		}
	}

	// No database yet: the transcripts' estimates still show.
	isolate(t)
	root := reportTranscript(t)
	text, _, code = runCLI(t, "", "report", "--path", root)
	if code != 0 || !strings.Contains(text, "recorded nothing yet") || !strings.Contains(text, "Largest outputs") {
		t.Errorf("no database:\n%s", text)
	}
}
