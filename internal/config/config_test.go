package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) File {
	t.Helper()
	f, warnings, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	if len(warnings) > 0 {
		t.Fatalf("Parse(%q): warnings %v", s, warnings)
	}
	return f
}

func TestParse(t *testing.T) {
	f := mustParse(t, `
mode: enforce
observe_until: 2026-10-23T09:00:00Z
detectors:
  repeated_failure: { enabled: true, min_count: 5 }
policy:
  halt_after_minutes: 30
budgets:
  session: { soft_usd: 5 }
`)
	c := Defaults().Apply(f, GlobalFile)
	if c.Mode != ModeGuard || c.ModeWritten != ModeEnforce || c.ModeSource != GlobalFile || !c.ObserveUntil.Equal(time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("mode %q from %q until %v", c.Mode, c.ModeSource, c.ObserveUntil)
	}
	if c.Thresholds.Detectors.RepeatedFailure.MinCount != 5 || c.Thresholds.Policy.HaltAfterMinutes != 30 {
		t.Errorf("thresholds %+v", c.Thresholds)
	}
	if c.Thresholds.Detectors.ExactRepeat.MinCount != 3 {
		t.Error("an absent key lost its default")
	}

	if _, _, err := Parse(nil); err != nil {
		t.Errorf("empty file: %v", err)
	}
	if _, w, err := Parse([]byte("mode: observe\nfuture_key: 1\n")); err != nil || len(w) != 1 || !strings.Contains(w[0], "future_key") {
		t.Errorf("unknown key: warnings %v, err %v", w, err)
	}
	for _, bad := range []string{
		"mode: on",
		"mode: off",
		"notify: { on: [loops] }",
		"notify: { webhook: { format: teams } }",
		"notify: { webhook: { url: 'ftp://x' } }",
		"notify: { ntfy: { topic: 'a b' } }",
		"notify: { desktop: maybe }",
		"mode: [enforce]",
		"detectors: { repeated_failure: { min_count: 1 } }",
		"policy: { nudge_threshold: 1.5 }",
		"polling_patterns: ['(']",
		"observe_until: soon",
		"mode: enforce\n  bad indent: x",
	} {
		if _, _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%q) accepted an invalid file", bad)
		}
	}
}

func TestRepoFileOnlyRaises(t *testing.T) {
	global := Defaults().Apply(mustParse(t, `
mode: observe
detectors: { oscillation: { enabled: false } }
`), GlobalFile)
	c, notes := global.Tighten(mustParse(t, `
mode: enforce
observe_until: 2030-01-01T00:00:00Z
detectors:
  window: 500
  repeated_failure: { min_count: 10 }
  exact_repeat: { min_count: 2, enabled: true }
  oscillation: { enabled: true }
policy:
  halt_after_minutes: 5
  deny_min_severity: 0.9
polling_families: ["npm test"]
`), "/r/"+RepoFile)

	if c.Mode != ModeGuard || c.ModeSource != "/r/"+RepoFile {
		t.Errorf("a repo file didn't raise the level: mode %q from %q", c.Mode, c.ModeSource)
	}
	if !c.ObserveUntil.IsZero() || c.Thresholds.Detectors.Window != 50 {
		t.Error("a repo file set a global-only key")
	}
	if c.Thresholds.Detectors.RepeatedFailure.MinCount != 4 {
		t.Errorf("min_count raised to %d", c.Thresholds.Detectors.RepeatedFailure.MinCount)
	}
	if c.Thresholds.Detectors.ExactRepeat.MinCount != 2 || c.Thresholds.Policy.HaltAfterMinutes != 5 {
		t.Error("a lower threshold was not applied")
	}
	if c.Thresholds.Policy.DenyMinSeverity != 0.7 {
		t.Errorf("deny_min_severity raised to %g", c.Thresholds.Policy.DenyMinSeverity)
	}
	if c.Thresholds.Detectors.Oscillation.Enabled {
		t.Error("a repo file turned on a detector the global config turns off")
	}
	if c.Thresholds.PollingFamilies[0] != "sleep" || len(c.Thresholds.PollingFamilies) != len(Default().PollingFamilies) {
		t.Error("a repo file changed the polling list")
	}
	want := []string{"observe_until", "detectors.window", "min_count: 10", "oscillation.enabled: true",
		"deny_min_severity: 0.9", "polling_families"}
	all := strings.Join(notes, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("no note about %q in:\n%s", w, all)
		}
	}

	// A repo file can't lower the level, nor turn LoopGuard off.
	guard := Defaults().Apply(mustParse(t, "mode: guard"), GlobalFile)
	for _, lower := range []string{"mode: observe", "mode: notify", "mode: assist", "enabled: false"} {
		c, notes = guard.Tighten(mustParse(t, lower), RepoFile)
		if m := c.EffectiveMode(time.Now()); m.Mode != ModeGuard || len(notes) != 1 || !strings.Contains(notes[0], "lower") {
			t.Errorf("%q against guard: %+v, notes %v", lower, m, notes)
		}
	}
	// Allowed: raising, the same level (no note), and turning a detector off.
	notify := Defaults().Apply(mustParse(t, "mode: notify"), GlobalFile)
	c, notes = notify.Tighten(mustParse(t, "mode: assist\ndetectors: { repeated_failure: { enabled: false } }"), RepoFile)
	if c.Mode != ModeAssist || c.ModeSource != RepoFile || c.Thresholds.Detectors.RepeatedFailure.Enabled || len(notes) != 0 {
		t.Errorf("mode %q from %q, rf enabled %v, notes %v", c.Mode, c.ModeSource, c.Thresholds.Detectors.RepeatedFailure.Enabled, notes)
	}
	if c, notes := guard.Tighten(mustParse(t, "mode: enforce"), RepoFile); len(notes) != 0 || c.ModeSource != GlobalFile {
		t.Errorf("the same level: notes %v, source %q", notes, c.ModeSource)
	}
	// The repo's level also applies after the global grace period only.
	grace := Defaults().Apply(mustParse(t, "mode: observe\nobserve_until: 2030-01-01T00:00:00Z"), GlobalFile)
	if c, _ := grace.Tighten(mustParse(t, "mode: guard"), RepoFile); c.EffectiveMode(time.Now()).Mode != ModeObserve {
		t.Errorf("a repo file skipped the grace period: %+v", c.EffectiveMode(time.Now()))
	}
}

func TestNotifyConfig(t *testing.T) {
	d := Defaults().Notify
	if !d.Wants(d.Desktop, EventLoopDetected) || !d.Wants(d.Desktop, EventHalt) || d.Wants(d.Desktop, EventNudge) ||
		d.Wants(d.Terminal, EventHalt) || d.Wants(d.Ntfy, EventHalt) || d.Wants(d.Webhook, EventHalt) {
		t.Errorf("defaults: %+v", d)
	}
	c := Defaults().Apply(mustParse(t, `
notify:
  on: [halt, stall]
  desktop: false
  terminal: true
  ntfy: { topic: loopguard-abc, on: [deny] }
  webhook: { url: "https://hooks.example.com/x", format: discord }
`), GlobalFile).Notify
	if c.Wants(c.Desktop, EventHalt) || !c.Wants(c.Terminal, EventStall) || c.Wants(c.Terminal, EventLoopDetected) {
		t.Errorf("channel switches or default events: %+v", c)
	}
	if !c.Wants(c.Ntfy, EventDeny) || c.Wants(c.Ntfy, EventHalt) || c.Ntfy.Server != DefaultNtfyServer {
		t.Errorf("ntfy: a topic turns it on, its own on: list wins: %+v", c.Ntfy)
	}
	if !c.Wants(c.Webhook, EventHalt) || c.Webhook.Format != FormatDiscord {
		t.Errorf("webhook: %+v", c.Webhook)
	}
	// The M3-A sample's events stay valid.
	mustParse(t, "notify: { on: [halt, budget_soft, budget_hard, stall], ntfy: { topic: '' } }")
	// A repo file can't change notifications.
	if _, notes := Defaults().Tighten(mustParse(t, "notify: { desktop: false }"), RepoFile); len(notes) != 1 {
		t.Errorf("notes %v", notes)
	}
}

func TestEffectiveMode(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		file, mode, why string
	}{
		{"", ModeObserve, "the default"},
		{"mode: observe", ModeObserve, "mode: observe in config.yaml"},
		{"mode: enforce", ModeGuard, "mode: enforce (= guard) in config.yaml"},
		{"mode: guard", ModeGuard, "mode: guard in config.yaml"},
		{"mode: notify", ModeNotify, "mode: notify in config.yaml"},
		{"mode: assist", ModeAssist, "mode: assist in config.yaml"},
		{"mode: enforce\nobserve_until: 2026-10-23T12:00:00Z", ModeObserve, "observe until 2026-10-23"},
		{"mode: assist\nobserve_until: 2026-10-23T12:00:00Z", ModeObserve, "then assist"},
		{"mode: enforce\nobserve_until: 2026-10-19T12:00:00Z", ModeGuard, "mode: enforce"},
		{"mode: observe\nobserve_until: 2026-10-19T12:00:00Z", ModeObserve, "mode: observe"},
		{"enabled: false\nmode: enforce", ModeOff, "enabled: false"},
	} {
		m := Defaults().Apply(mustParse(t, tt.file), GlobalFile).EffectiveMode(now)
		if m.Mode != tt.mode || !strings.Contains(m.Why, tt.why) {
			t.Errorf("%q: %+v, want %s (%s)", tt.file, m, tt.mode, tt.why)
		}
	}
}

func TestLoader(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	var issues []string
	l := NewLoader(home, LoaderOptions{ReloadEvery: -1, OnIssue: func(p string, err error, w []string) {
		issues = append(issues, filepath.Base(p))
	}})
	write := func(path, s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		// Make the change visible even where mtimes are coarse.
		future := time.Now().Add(time.Duration(len(s)) * time.Second)
		os.Chtimes(path, future, future)
	}
	now := time.Now()

	e := l.For(repo)
	if e.Global.Exists || e.Repo.Exists || e.ModeAt(now).Mode != ModeObserve {
		t.Fatalf("no files: %+v", e)
	}
	write(filepath.Join(home, GlobalFile), "mode: enforce\n")
	if e = l.For(repo); e.ModeAt(now).Mode != ModeGuard {
		t.Fatalf("after writing mode: enforce: %+v", e.ModeAt(now))
	}
	key := e.Key
	write(filepath.Join(repo, RepoFile), "detectors: { repeated_failure: { min_count: 2 } }\n")
	if e = l.For(repo); e.Thresholds.Detectors.RepeatedFailure.MinCount != 2 || e.Key == key {
		t.Errorf("repo file not applied: %+v", e.Thresholds.Detectors.RepeatedFailure)
	}
	if other := l.For(""); other.Thresholds.Detectors.RepeatedFailure.MinCount != 4 {
		t.Error("a repo file leaked into another repo")
	}

	// An invalid global file falls back to the defaults (observe).
	write(filepath.Join(home, GlobalFile), "mode: enforce\npolicy: [\n")
	e = l.For(repo)
	if e.Global.Err == nil || e.ModeAt(now).Mode != ModeObserve || !strings.Contains(e.ModeAt(now).Why, "invalid") {
		t.Errorf("invalid global: err %v, mode %+v", e.Global.Err, e.ModeAt(now))
	}
	if e.Thresholds.Detectors.RepeatedFailure.MinCount != 2 {
		t.Error("the valid repo file was dropped along with the invalid global one")
	}
	write(filepath.Join(repo, RepoFile), "mode: sometimes\n")
	if e = l.For(repo); e.Repo.Err == nil || e.Thresholds.Detectors.RepeatedFailure.MinCount != 4 {
		t.Errorf("invalid repo file: %+v", e.Repo)
	}
	if strings.Join(issues, ",") != "config.yaml,.loopguard.yaml" {
		t.Errorf("issues reported: %v", issues)
	}

	// Cached between stats.
	cached := NewLoader(home, LoaderOptions{ReloadEvery: time.Hour})
	cached.For("")
	write(filepath.Join(home, GlobalFile), "mode: enforce\n")
	if e := cached.For(""); e.Global.Err == nil {
		t.Error("re-read before ReloadEvery")
	}
}

func TestEditFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", GlobalFile)
	if err := EditFile(path, Edit{Key: "mode", Value: "observe"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "# LoopGuard configuration") || !strings.Contains(string(b), "mode: observe") {
		t.Errorf("new file:\n%s", b)
	}

	os.WriteFile(path, []byte("# mine\nmode: observe # keep this\npolicy:\n  halt_after_minutes: 30\n"), 0o600)
	until := "2026-10-23T09:00:00Z"
	if err := EditFile(path, Edit{Key: "mode", Value: "enforce"}, Edit{Key: "observe_until", Value: until}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	s := string(b)
	for _, want := range []string{"# mine", "mode: enforce # keep this", "halt_after_minutes: 30", "observe_until: " + until} {
		if !strings.Contains(s, want) {
			t.Errorf("edited file lacks %q:\n%s", want, s)
		}
	}
	f := mustParse(t, s)
	if f.ObserveUntil == nil || f.ObserveUntil.Format(time.RFC3339) != until {
		t.Errorf("observe_until reads back as %v", f.ObserveUntil)
	}
	if err := EditFile(path, Edit{Key: "observe_until", Remove: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "observe_until") {
		t.Errorf("not removed:\n%s", b)
	}

	os.WriteFile(path, []byte("mode: [\n"), 0o600)
	if err := EditFile(path, Edit{Key: "mode", Value: "enforce"}); err == nil {
		t.Error("edited an invalid file")
	}
	if b, _ := os.ReadFile(path); string(b) != "mode: [\n" {
		t.Error("an invalid file was changed")
	}
	if err := EditFile(filepath.Join(t.TempDir(), GlobalFile), Edit{Key: "mode", Value: "always"}); err == nil {
		t.Error("wrote an invalid value")
	}
}
