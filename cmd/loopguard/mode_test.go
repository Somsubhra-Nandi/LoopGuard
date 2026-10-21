package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// cli runs the CLI in-process and returns stdout; a non-zero exit fails
// the test unless wantErr.
func cli(t *testing.T, wantErr bool, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(""), &stdout, &stderr)
	if (code != 0) != wantErr {
		t.Fatalf("loopguard %s: exit %d, stdout %q, stderr %q", strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	return stdout.String() + stderr.String()
}

func readConfig(t *testing.T, dir string) (config.File, bool) {
	t.Helper()
	f, exists, _, err := config.LoadFile(filepath.Join(dir, config.GlobalFile))
	if err != nil {
		t.Fatalf("config.yaml: %v", err)
	}
	return f, exists
}

// fakeInstall gives init a settings file and a hook binary to point at.
func fakeInstall(t *testing.T) (settings, hook string) {
	t.Helper()
	root := t.TempDir()
	hook = filepath.Join(root, exeName("loopguard-hook"))
	if err := os.WriteFile(hook, []byte("not run"), 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "claude", "settings.json"), hook
}

func TestInitSetsTheMode(t *testing.T) {
	t.Run("new install observes first", func(t *testing.T) {
		dir := isolate(t)
		settings, hook := fakeInstall(t)
		cli(t, false, "init", "--settings", settings, "--hook", hook, "--dry-run")
		if _, exists := readConfig(t, dir); exists {
			t.Fatal("init --dry-run wrote config.yaml")
		}
		out := cli(t, false, "init", "--settings", settings, "--hook", hook)
		f, _ := readConfig(t, dir)
		if f.Mode == nil || *f.Mode != config.ModeGuard || f.ObserveUntil == nil {
			t.Fatalf("config after a new install: %+v", f)
		}
		if d := time.Until(*f.ObserveUntil); d < 71*time.Hour || d > 73*time.Hour {
			t.Errorf("observe_until is %v away, want 3 days", d)
		}
		if !strings.Contains(out, "Level: observe (observe until ") || !strings.Contains(out, "then guard") {
			t.Errorf("init output:\n%s", out)
		}
		// Running it again changes nothing.
		before, _ := os.ReadFile(filepath.Join(dir, config.GlobalFile))
		cli(t, false, "init", "--settings", settings, "--hook", hook)
		if after, _ := os.ReadFile(filepath.Join(dir, config.GlobalFile)); string(after) != string(before) {
			t.Errorf("second init changed config.yaml:\n%s", after)
		}
	})
	t.Run("existing install keeps observing", func(t *testing.T) {
		dir := isolate(t)
		st, err := store.Open(filepath.Join(dir, home.DBFile)) // an M2 install: a database, no config.yaml
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		settings, hook := fakeInstall(t)
		out := cli(t, false, "init", "--settings", settings, "--hook", hook)
		if _, exists := readConfig(t, dir); exists {
			t.Error("init wrote config.yaml for an existing install")
		}
		if !strings.Contains(out, "Level: observe (the default") {
			t.Errorf("init output:\n%s", out)
		}
	})
	t.Run("--enforce", func(t *testing.T) {
		dir := isolate(t)
		settings, hook := fakeInstall(t)
		out := cli(t, false, "init", "--settings", settings, "--hook", hook, "--enforce")
		f, _ := readConfig(t, dir)
		if f.Mode == nil || *f.Mode != config.ModeGuard || f.ObserveUntil != nil || !strings.Contains(out, "Level: guard") {
			t.Errorf("init --enforce: %+v\n%s", f, out)
		}
	})
}

func TestModeCommand(t *testing.T) {
	dir := isolate(t)
	if out := cli(t, false, "mode"); !strings.Contains(out, "Level: observe (the default: no mode set in config.yaml)") {
		t.Errorf("mode with no config:\n%s", out)
	}
	os.WriteFile(filepath.Join(dir, config.GlobalFile), []byte("# mine\nmode: enforce\nobserve_until: 2999-01-01T00:00:00Z\n"), 0o600)
	if out := cli(t, false, "mode", "status"); !strings.Contains(out, "Level: observe (observe until 2999-01-01") {
		t.Errorf("mode during observe_until:\n%s", out)
	}
	out := cli(t, false, "mode", "enforce")
	f, _ := readConfig(t, dir)
	if f.ObserveUntil != nil || *f.Mode != config.ModeGuard || !strings.Contains(out, "Level: guard (mode: guard in config.yaml)") ||
		!strings.Contains(out, "old name of guard") {
		t.Errorf("mode enforce: %+v\n%s", f, out)
	}
	for _, level := range []string{"notify", "assist", "guard"} {
		if out := cli(t, false, "mode", level); !strings.Contains(out, "Level: "+level+" (mode: "+level+" in config.yaml)") ||
			!strings.Contains(out, levelHelp[level]) {
			t.Errorf("mode %s:\n%s", level, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, config.GlobalFile)); !strings.Contains(string(b), "# mine") {
		t.Errorf("comment lost:\n%s", b)
	}
	if out := cli(t, false, "mode", "observe"); !strings.Contains(out, "Level: observe (mode: observe in config.yaml)") {
		t.Errorf("mode observe:\n%s", out)
	}
	cli(t, true, "mode", "sometimes")

	os.WriteFile(filepath.Join(dir, config.GlobalFile), []byte("mode: [\n"), 0o600)
	if out := cli(t, false, "mode"); !strings.Contains(out, "is invalid, so the defaults are used") {
		t.Errorf("invalid config:\n%s", out)
	}
	cli(t, true, "mode", "enforce") // refuses to edit an invalid file
}

func TestPauseResumeCommands(t *testing.T) {
	dir := isolate(t)
	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := st.NewWriter(store.WriterOptions{})
	now := time.Now()
	for _, sid := range []string{"1a2b3c4d-1111", "1a2bffff-2222"} {
		w.Enqueue(store.Session{Key: "claude-code:" + sid, Agent: "claude-code", SessionID: sid, StartedAt: now, LastSeenAt: now})
	}
	w.Close()
	ctx := context.Background()

	if out := cli(t, false, "pause"); !strings.Contains(out, "Nothing is paused") {
		t.Errorf("pause with nothing paused:\n%s", out)
	}
	out := cli(t, false, "pause", "--session", "1a2b3c4d")
	if !strings.Contains(out, "Paused session 1a2b3c4d-1111 (until resumed)") || !strings.Contains(out, "loopguard resume --session 1a2b3c4d-1111") {
		t.Errorf("pause --session:\n%s", out)
	}
	cli(t, true, "pause", "--session", "1a2b")    // ambiguous
	cli(t, true, "pause", "--session", "zzzz")    // unknown
	cli(t, true, "pause", "--all", "--repo", ".") // two scopes
	cli(t, true, "pause", "--all", "--for", "soon")
	cli(t, false, "pause", "--all", "--for", "30m")
	cli(t, false, "pause", "--repo")

	ps, _ := st.Pauses(ctx, now)
	if len(ps) != 3 {
		t.Fatalf("pauses = %+v", ps)
	}
	for _, p := range ps {
		switch p.Scope {
		case store.PauseAll:
			if d := p.Until.Sub(now); d < 29*time.Minute || d > 31*time.Minute {
				t.Errorf("--for 30m: until %v", p.Until)
			}
		case store.PauseRepo:
			wd, _ := os.Getwd()
			if p.Target != gitRoot(wd) {
				t.Errorf("--repo alone: target %q, want %q", p.Target, gitRoot(wd))
			}
		}
	}
	if out := cli(t, false, "status"); !strings.Contains(out, "paused: session 1a2b3c4d-1111") {
		t.Errorf("status doesn't show the pause:\n%s", out)
	}
	out = cli(t, false, "resume", "--session", "claude-code:1a2b3c4d-1111/agent-3")
	if !strings.Contains(out, "Resumed (1 pause removed)") || !strings.Contains(out, "Still paused: everything") {
		t.Errorf("resume --session:\n%s", out)
	}
	if out := cli(t, false, "resume", "--all"); !strings.Contains(out, "Resumed (2 pauses removed)") {
		t.Errorf("resume --all:\n%s", out)
	}
	if ps, _ := st.Pauses(ctx, now); len(ps) != 0 {
		t.Errorf("after resume --all: %+v", ps)
	}
}

func TestStatsCommand(t *testing.T) {
	dir := isolate(t)
	if out := cli(t, false, "stats"); !strings.Contains(out, "Interventions (all time): 0") {
		t.Errorf("stats with no database:\n%s", out)
	}
	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	w := st.NewWriter(store.WriterOptions{})
	now := time.Now()
	add := func(seq uint64, action, mode, detector, outcome string) {
		w.Enqueue(store.Intervention{SessionKey: "claude-code:s", Seq: seq, TS: now, LoopKey: "k", Detector: detector,
			Action: action, Mode: mode, Outcome: outcome})
	}
	add(1, "nudge", store.ModeEnforce, "repeated_failure", "recovered")
	add(2, "nudge", store.ModeEnforce, "repeated_failure", "recovered")
	add(3, "nudge", store.ModeEnforce, "repeated_failure", "continued_loop")
	add(8, "nudge", store.ModeEnforce, "repeated_failure", "moved_on")
	add(4, "deny", store.ModeEnforce, "repeated_failure", "halted")
	add(5, "halt", store.ModeEnforce, "repeated_failure", "halted")
	add(6, "nudge", store.ModeObserve, "exact_repeat", "unknown")
	add(7, "nudge", store.ModeObserve, "exact_repeat", "")
	w.Close()
	st.Close()

	out := cli(t, false, "stats")
	for _, want := range []string{
		"Interventions (all time): 8",
		"nudge       4           2",
		"rec/moved/cont/halt/unk",
		"repeated_failure      4    1    1   2/1/1/2/0 (33%)",
		"exact_repeat          2    0    0   0/0/0/0/0",
		"0/0/0/0/1 (-) +1 pending",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stats lacks %q:\n%s", want, out)
		}
	}
	var r statsReport
	if err := json.Unmarshal([]byte(cli(t, false, "stats", "--json", "--since", "7d")), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Detectors) != 2 || r.Detectors[1].RecoveryReal == nil || *r.Detectors[1].RecoveryReal != 2.0/6 || r.Detectors[1].Real.MovedOn != 1 || r.Detectors[0].RecoveryWH != nil {
		t.Errorf("stats --json: %+v", r.Detectors)
	}
}
