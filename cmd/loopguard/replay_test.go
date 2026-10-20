package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay/replaytest"
)

func synthetic(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := replaytest.WriteTree(root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPGUARD_HOME", t.TempDir())
	return root
}

func runCLI(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(stdin), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestReplayText(t *testing.T) {
	root := synthetic(t)
	out, errOut, code := runCLI(t, "", "replay", "--path", root, "--since", "all")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"Scanned 1 sessions (2 streams", "Loops found          3", "Top 3 loops", "`jest`", "src/flags.ts"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, replaytest.Secret) {
		t.Error("secret in the report")
	}
}

func TestReplayJSON(t *testing.T) {
	root := synthetic(t)
	out, errOut, code := runCLI(t, "", "replay", "--path", root, "--since", "all", "--json", "--top", "1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var r replay.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.SchemaVersion != replay.SchemaVersion || r.TotalLoops != 3 || len(r.TopLoops) != 1 || r.Since != nil {
		t.Errorf("report = %+v", r)
	}
}

func TestReplayNearMissesAndSoftFailures(t *testing.T) {
	root := synthetic(t)
	out, errOut, code := runCLI(t, "", "replay", "--path", root, "--since", "all", "--near-misses", "--soft-failures")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"Failures by command family", "Longest same-family failure streak", "Near misses (", "Soft failures ("} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	out, errOut, code = runCLI(t, "", "replay", "--path", root, "--since", "all", "--near-misses=1", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var r replay.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.NearMisses == nil || len(*r.NearMisses) > 1 || r.SoftFailures != nil || len(r.FailureStreaks) == 0 {
		t.Errorf("json: near=%v soft=%v streaks=%d", r.NearMisses, r.SoftFailures, len(r.FailureStreaks))
	}
}

func TestReplayFlags(t *testing.T) {
	root := synthetic(t)
	for _, args := range [][]string{
		{"replay", "--no-such-flag"},
		{"replay", "extra-arg"},
		{"replay", "--path", root, "--since", "yesterday"},
		{"replay", "--path", root, "--calls"},
		{"replay", "--path", root, "--export-anonymized", "5e55"},
		{"replay", "--path", filepath.Join(root, "missing")},
	} {
		if _, _, code := runCLI(t, "", args...); code == 0 {
			t.Errorf("%q: exit 0, want an error", args)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"30d": now.AddDate(0, 0, -30), "12h": now.Add(-12 * time.Hour), "2w": now.AddDate(0, 0, -14),
		"90m": now.Add(-90 * time.Minute), "all": {}, "0": {}, "1.5d": now.Add(-36 * time.Hour),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"d", "-3d", "soon", "3x"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) succeeded", bad)
		}
	}
}

func TestReplayLabel(t *testing.T) {
	root := synthetic(t)
	labels := filepath.Join(t.TempDir(), "labels.jsonl")
	// Three loops: yes, no, then quit before the third.
	out, errOut, code := runCLI(t, "y\nmaybe\nn\nq\n", "replay", "--path", root, "--since", "all", "--label", "--labels", labels)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "3 to label") || !strings.Contains(out, "Saved 2 labels") {
		t.Errorf("output:\n%s", out)
	}
	ls, err := replay.ReadLabels(labels)
	if err != nil || len(ls) != 2 || ls[0].Verdict != replay.VerdictReal || ls[1].Verdict != replay.VerdictFalsePositive {
		t.Fatalf("labels = %+v, %v", ls, err)
	}
	// Re-running only asks about the unlabeled loop; EOF ends it.
	out, _, _ = runCLI(t, "", "replay", "--path", root, "--since", "all", "--label", "--labels", labels)
	if !strings.Contains(out, "2 already labeled, 1 to label") {
		t.Errorf("rerun output:\n%s", out)
	}
}

func TestLabelMissedAndEval(t *testing.T) {
	root := synthetic(t)
	labels := filepath.Join(t.TempDir(), "labels.jsonl")
	out, errOut, code := runCLI(t, "", "eval", "--path", root, "--labels", labels)
	if code != 0 || !strings.Contains(out, "no labels") {
		t.Errorf("eval without labels: %d %s %s", code, out, errOut)
	}
	if _, errOut, code := runCLI(t, "", "label", "missed", replaytest.SessionID[:8], "100", "120", "flaky retries", "--labels", labels); code != 0 {
		t.Fatalf("label missed: %s", errOut)
	}
	if _, _, code := runCLI(t, "", "label", "missed", "x", "5", "2", "--labels", labels); code == 0 {
		t.Error("backwards range accepted")
	}
	runCLI(t, "y\ny\ny\n", "replay", "--path", root, "--since", "all", "--label", "--labels", labels)
	out, errOut, code = runCLI(t, "", "eval", "--path", root, "--labels", labels)
	if code != 0 {
		t.Fatalf("eval: %s", errOut)
	}
	for _, want := range []string{"repeated_failure", "oscillation", "100%", "real loops 4: caught 3, missed 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("eval output missing %q:\n%s", want, out)
		}
	}
}

func TestReplayCallsAndExport(t *testing.T) {
	root := synthetic(t)
	out, errOut, code := runCLI(t, "", "replay", "--path", root, "--since", "all", "--session", "5e55a0c1", "--calls")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "FAIL  bash   npx jest") || !strings.Contains(out, replaytest.SessionID+"/"+replaytest.AgentID) {
		t.Errorf("calls output:\n%s", out)
	}
	if strings.Contains(out, replaytest.Secret) {
		t.Error("secret in the call list")
	}

	dest := filepath.Join(t.TempDir(), "anon.jsonl")
	out, errOut, code = runCLI(t, "", "replay", "--path", root, "--export-anonymized", "5e55", "--out", dest)
	if code != 0 || !strings.Contains(out, "14 tool calls") {
		t.Fatalf("export: %d %s %s", code, out, errOut)
	}
	b, err := os.ReadFile(dest)
	if err != nil || strings.Contains(string(b), replaytest.PromptText) {
		t.Errorf("export: %v, prompt leaked=%v", err, strings.Contains(string(b), replaytest.PromptText))
	}
}
