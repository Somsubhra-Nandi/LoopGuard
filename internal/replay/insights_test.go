package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay/replaytest"
)

// runOne replays one transcript built in a test.
func runOne(t *testing.T, content string) *Result {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	streams, err := Discover(filepath.Dir(dir), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return Run(streams, Options{Thresholds: config.Default()})
}

func nearByDetector(res *Result, det string) []NearMiss {
	var out []NearMiss
	for _, n := range res.NearMisses {
		if n.Detector == det {
			out = append(out, n)
		}
	}
	return out
}

func TestNearMissRepeatedFailureBelowThreshold(t *testing.T) {
	b := replaytest.NewBuilder("s1", "")
	b.Prompt("go")
	for i := 0; i < 3; i++ {
		b.Bash("npx jest", replaytest.JestFail(i), true)
		b.Edit(replaytest.Cwd+`\src\cart.js`, fmt.Sprint("a", i), fmt.Sprint("a", i+1))
	}
	b.Bash("npx jest", "Tests: 4 passed, 4 total", false)
	res := runOne(t, b.String())
	if len(res.Loops) != 0 {
		t.Fatalf("loops = %d, want none", len(res.Loops))
	}
	ns := nearByDetector(res, event.DetectorRepeatedFailure)
	if len(ns) != 1 {
		t.Fatalf("near misses = %+v", res.NearMisses)
	}
	n := ns[0]
	if n.Count != 3 || n.Threshold != 4 || n.FromCall != 1 || n.ToCall != 5 || n.Family != "jest" ||
		!strings.Contains(n.Reason, "3 same-error failures of `jest`, threshold is 4") || !strings.Contains(n.Reason, "success") {
		t.Errorf("near miss = %+v", n)
	}
	if n.Closeness != 0.75 {
		t.Errorf("closeness = %v", n.Closeness)
	}
	for _, e := range n.Evidence {
		if strings.Contains(e, replaytest.Secret) {
			t.Error("secret in near-miss evidence")
		}
	}
}

func TestNearMissRepeatedFailureSuppressedByProgress(t *testing.T) {
	b := replaytest.NewBuilder("s1", "")
	b.Prompt("go")
	// Same error A four times, with two never-seen errors in between: the
	// family made 1.2 progress, so D2 stays quiet.
	for _, out := range []string{"A", "A", "B", "A", "C", "A"} {
		b.Bash("go test ./...", "--- FAIL: TestX (0.00s)\n    x_test.go:9: got "+out+", want Z\nFAIL", true)
		b.Edit(replaytest.Cwd+`\x.go`, "p"+out, "q"+out)
	}
	res := runOne(t, b.String())
	ns := nearByDetector(res, event.DetectorRepeatedFailure)
	if len(res.Loops) != 0 || len(ns) != 1 || ns[0].Count != 4 || ns[0].Closeness != 0.95 ||
		!strings.Contains(ns[0].Reason, "made 1.2 progress") {
		t.Fatalf("loops %d, near misses %+v", len(res.Loops), ns)
	}
}

func TestNearMissExcludesDetectedLoops(t *testing.T) {
	streams, _ := Discover(writeSynthetic(t), Filter{})
	res := Run(streams, Options{Thresholds: config.Default()})
	for _, n := range res.NearMisses {
		for _, l := range res.Loops {
			if l.Detector == n.Detector && l.AgentID == n.AgentID && overlaps(n.FromCall, n.ToCall, l.FirstCall, l.LastCall) {
				t.Errorf("near miss %+v overlaps detected loop %s calls %d-%d", n, l.LoopKey, l.FirstCall, l.LastCall)
			}
		}
	}
}

func TestNearMissExactRepeatFileAndStall(t *testing.T) {
	b := replaytest.NewBuilder("s1", "")
	b.Prompt("go")
	b.Bash("make lint", "ok", false)
	b.Bash("make lint", "ok", false) // 2 identical, threshold 3
	for i := 0; i < 5; i++ {
		b.Write(replaytest.Cwd+`\notes.md`, fmt.Sprint("draft ", i)) // 5 writes, no revisit
	}
	for i := 0; i < 12; i++ {
		b.Tool("Read", map[string]any{"file_path": fmt.Sprintf(`%s\f%d.go`, replaytest.Cwd, i)}, "package x", false)
	}
	res := runOne(t, b.String())
	er := nearByDetector(res, event.DetectorExactRepeat)
	if len(er) != 1 || er[0].Count != 2 || er[0].Threshold != 3 || !strings.Contains(er[0].Reason, "same call 2×") {
		t.Errorf("exact-repeat near misses = %+v", er)
	}
	os := nearByDetector(res, event.DetectorOscillation)
	if len(os) != 1 || os[0].File != "notes.md" || !strings.Contains(os[0].Reason, "written 5 times, no revisit") {
		t.Errorf("oscillation near misses = %+v", os)
	}
	// No progress from call 4: rewriting a file already touched this turn
	// scores 0, as do the reads.
	st := nearByDetector(res, event.DetectorStall)
	if len(st) != 1 || st[0].Count != 16 || st[0].FromCall != 4 || st[0].ToCall != 19 || st[0].Threshold != 25 {
		t.Errorf("stall near misses = %+v", st)
	}
	// Sorted closest first.
	for i := 1; i < len(res.NearMisses); i++ {
		if res.NearMisses[i].Closeness > res.NearMisses[i-1].Closeness {
			t.Fatalf("near misses not sorted: %+v", res.NearMisses)
		}
	}
}

func TestSoftFailures(t *testing.T) {
	b := replaytest.NewBuilder("s1", "")
	b.Prompt("go")
	b.Bash("python app.py", "starting\nTraceback (most recent call last):\nKeyError: 'x' token="+replaytest.Secret, false)
	b.Bash(`grep -rn "Error:" src`, "src/a.ts:3: throw new Error: bad", false) // read-only: not a failure
	b.Bash("pytest", "FAILED tests/test_a.py::test_x\nError: build broke", false)
	b.Bash("npm run build", "Exit code 2\nError: real failure", true)          // a hard failure, not soft
	b.Bash("node script.js", "Error: printed, not failed\nsee: FAILED", false) // no marker applies
	res := runOne(t, b.String())
	if len(res.SoftFailures) != 2 {
		t.Fatalf("soft failures = %+v", res.SoftFailures)
	}
	a, c := res.SoftFailures[0], res.SoftFailures[1]
	// "KeyError: 'x'" doesn't start its line with "Error:", and python is
	// not a test runner: only Traceback counts.
	if a.Call != 1 || a.Family != "python" || strings.Join(a.Markers, ",") != "Traceback" || !strings.HasPrefix(a.Line, "Traceback") {
		t.Errorf("first soft failure = %+v", a)
	}
	if c.Call != 3 || strings.Join(c.Markers, ",") != "FAILED,Error:" || c.Line != "FAILED tests/test_a.py::test_x" {
		t.Errorf("second soft failure = %+v", c)
	}
	counts := MarkerCounts(res.SoftFailures)
	if counts["FAILED"] != 1 || counts["Error:"] != 1 || counts["Traceback"] != 1 {
		t.Errorf("marker counts = %v", counts)
	}
	for _, s := range res.SoftFailures {
		if strings.Contains(s.Line+s.Command, replaytest.Secret) {
			t.Error("secret in soft-failure output")
		}
	}
}

func TestFailureShapeAndReportSections(t *testing.T) {
	res := runSynthetic(t)
	if len(res.Families) == 0 || res.Families[0].Family != "jest" || res.Families[0].Failures != 5 || res.Families[0].Runs != 5 {
		t.Errorf("families = %+v", res.Families)
	}
	if len(res.Streaks) != 2 || res.Streaks[0].Length != 5 || res.Streaks[0].Family != "jest" ||
		res.Streaks[0].FromCall != 1 || res.Streaks[0].ToCall != 9 || res.Streaks[0].Errors != 1 {
		t.Errorf("streaks = %+v", res.Streaks)
	}

	plain := NewReport(res, "/x", time.Time{}, ReportOptions{Top: 1}, time.Unix(0, 0), 0)
	var text bytes.Buffer
	if err := plain.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Failures by command family", "jest", "Longest same-family failure streak per session", "5× `jest`, calls 1-9, 1 distinct errors"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("report missing %q:\n%s", want, text.String())
		}
	}
	if strings.Contains(text.String(), "Near misses") || strings.Contains(text.String(), "Soft failures") {
		t.Error("near misses or soft failures shown without being asked for")
	}
	var js bytes.Buffer
	_ = plain.WriteJSON(&js)
	if strings.Contains(js.String(), "near_misses") || !strings.Contains(js.String(), "failure_streaks") {
		t.Errorf("JSON sections wrong:\n%s", js.String())
	}

	full := NewReport(res, "/x", time.Time{}, ReportOptions{Top: 0, NearMisses: 3, SoftFailures: true}, time.Unix(0, 0), 0)
	text.Reset()
	_ = full.WriteText(&text)
	for _, want := range []string{"Near misses", "Soft failures (0)", "loopguard label missed"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("full report missing %q:\n%s", want, text.String())
		}
	}
	js.Reset()
	_ = full.WriteJSON(&js)
	var back map[string]json.RawMessage
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if string(back["soft_failures"]) != "[]" || back["near_misses"] == nil || back["soft_failure_markers"] == nil {
		t.Errorf("requested sections missing or wrong: soft=%s near=%v", back["soft_failures"], back["near_misses"] != nil)
	}
}
