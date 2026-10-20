package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
)

// A settings file with other settings and another tool's hooks.
const existingSettings = `{
  "model": "opus",
  "permissions": {
    "allow": ["Bash(npm test:*)"]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [{ "type": "command", "command": "other-tool check 2>&1" }]
      }
    ],
    "Notification": [
      { "hooks": [{ "type": "command", "command": "notify-send hi" }] }
    ]
  },
  "theme": "dark"
}
`

func hookPath(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Program Files", "Loop Guard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "loopguard-hook.exe")
	if err := os.WriteFile(p, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeSettings(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "my home", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, b)
	}
	return m
}

func install(t *testing.T, settings, hook string) adapter.InstallReport {
	t.Helper()
	r, err := New().Install(adapter.InstallOpts{Settings: settings, Hook: hook})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func events() []string {
	var out []string
	for _, h := range Hooks {
		out = append(out, h.Event)
	}
	return out
}

func TestInstallMergesAndKeepsEverything(t *testing.T) {
	settings := writeSettings(t, existingSettings)
	hook := hookPath(t)
	r := install(t, settings, hook)
	if !r.Changed || !reflect.DeepEqual(r.Added, events()) || len(r.Updated)+len(r.Unchanged)+len(r.Removed) != 0 {
		t.Errorf("report: %+v", r)
	}

	// Backup: the original bytes.
	if !r.BackupCreated || r.BackupPath != settings+BackupSuffix {
		t.Errorf("backup: %q created=%v", r.BackupPath, r.BackupCreated)
	}
	if b, _ := os.ReadFile(settings + BackupSuffix); string(b) != existingSettings {
		t.Errorf("backup content:\n%s", b)
	}

	after, _ := os.ReadFile(settings)
	m := decode(t, after)
	if m["model"] != "opus" || m["theme"] != "dark" || m["permissions"] == nil {
		t.Errorf("other settings lost: %v", m)
	}
	// Key order is kept: model, permissions, hooks, theme.
	s := string(after)
	if !(strings.Index(s, `"model"`) < strings.Index(s, `"permissions"`) &&
		strings.Index(s, `"permissions"`) < strings.Index(s, `"hooks"`) &&
		strings.Index(s, `"hooks"`) < strings.Index(s, `"theme"`)) {
		t.Errorf("key order changed:\n%s", s)
	}
	if !strings.Contains(s, `"other-tool check 2>&1"`) {
		t.Errorf("other tool's command re-escaped or lost:\n%s", s)
	}

	hooks := m["hooks"].(map[string]any)
	if n := hooks["Notification"].([]any); len(n) != 1 {
		t.Errorf("Notification hooks: %v", n)
	}
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 || pre[0].(map[string]any)["matcher"] != "Bash" {
		t.Fatalf("PreToolUse groups: %v", pre)
	}
	ours := pre[1].(map[string]any)
	want := map[string]any{"matcher": "*", "hooks": []any{map[string]any{
		"type": "command", "command": hook, "args": []any{"--agent", "claude-code"}, "timeout": 2.0}}}
	if !reflect.DeepEqual(ours, want) {
		t.Errorf("PreToolUse hook = %v, want %v", ours, want)
	}
	for _, h := range Hooks {
		groups := hooks[h.Event].([]any)
		g := groups[len(groups)-1].(map[string]any)
		hk := g["hooks"].([]any)[0].(map[string]any)
		if hk["command"] != hook || hk["timeout"] != float64(h.Timeout) {
			t.Errorf("%s: %v", h.Event, g)
		}
		if _, has := g["matcher"]; has != (h.Matcher != "") {
			t.Errorf("%s: matcher %v", h.Event, g["matcher"])
		}
	}

	// Idempotent: a second run changes nothing, not even bytes.
	r2 := install(t, settings, hook)
	again, _ := os.ReadFile(settings)
	if r2.Changed || string(again) != string(after) || !reflect.DeepEqual(r2.Unchanged, events()) || r2.BackupCreated {
		t.Errorf("second install changed something: %+v", r2)
	}

	// Uninstall removes only ours, and restores the original content.
	u, err := New().Uninstall(adapter.InstallOpts{Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	if !u.Changed || len(u.Removed) != len(Hooks) {
		t.Errorf("uninstall report: %+v", u)
	}
	restored, _ := os.ReadFile(settings)
	if !reflect.DeepEqual(decode(t, restored), decode(t, []byte(existingSettings))) {
		t.Errorf("after uninstall:\n%s\nwant the original:\n%s", restored, existingSettings)
	}
	if _, err := os.Stat(settings + BackupSuffix); err != nil {
		t.Error("uninstall removed the backup")
	}
	// Uninstalling again: nothing to do.
	if u2, _ := New().Uninstall(adapter.InstallOpts{Settings: settings}); u2.Changed {
		t.Errorf("second uninstall changed something: %+v", u2)
	}
}

func TestInstallNewFile(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "new dir", "settings.json")
	r := install(t, settings, hookPath(t))
	if !r.Changed || r.BackupPath != "" || r.Before != nil {
		t.Errorf("report: %+v", r)
	}
	b, _ := os.ReadFile(settings)
	if len(decode(t, b)["hooks"].(map[string]any)) != len(Hooks) || !strings.HasPrefix(string(b), "{\n  \"hooks\"") {
		t.Errorf("new file:\n%s", b)
	}
	// Uninstall leaves an empty object, not an empty "hooks".
	if _, err := New().Uninstall(adapter.InstallOpts{Settings: settings}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(settings); strings.TrimSpace(string(b)) != "{}" {
		t.Errorf("after uninstall: %s", b)
	}
}

func TestInstallUpdatesOldHooksInPlace(t *testing.T) {
	old := `{"hooks":{
  "PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"C:\\old place\\loopguard-hook.exe","args":["--agent","claude-code"],"timeout":9}]},
                {"matcher":"Bash","hooks":[{"type":"command","command":"theirs"}]}],
  "Stop":[{"hooks":[{"type":"command","command":"theirs-stop"},{"type":"command","command":"/usr/local/bin/loopguard-hook"}]}],
  "Notification":[{"hooks":[{"type":"command","command":"/opt/loopguard-hook","args":[]}]}]
}}`
	settings := writeSettings(t, old)
	hook := hookPath(t)
	r := install(t, settings, hook)
	if !reflect.DeepEqual(r.Updated, []string{"PreToolUse", "Stop"}) || !reflect.DeepEqual(r.Removed, []string{"Notification"}) {
		t.Errorf("report: updated %v, removed %v", r.Updated, r.Removed)
	}
	b, _ := os.ReadFile(settings)
	hooks := decode(t, b)["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	// Replaced in place: still first, before the other tool's group.
	if len(pre) != 2 || pre[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != hook ||
		pre[1].(map[string]any)["matcher"] != "Bash" {
		t.Errorf("PreToolUse: %v", pre)
	}
	// A mixed group keeps the other tool's hook; ours moves to its own group.
	stop := hooks["Stop"].([]any)
	var cmds []any
	for _, g := range stop {
		hs := g.(map[string]any)["hooks"].([]any)
		if len(hs) != 1 {
			t.Errorf("Stop group with %d hooks: %v", len(hs), g)
		}
		cmds = append(cmds, hs[0].(map[string]any)["command"])
	}
	if !reflect.DeepEqual(cmds, []any{hook, "theirs-stop"}) {
		t.Errorf("Stop: %v", stop)
	}
	if _, ok := hooks["Notification"]; ok {
		t.Errorf("Notification: our stale hook not removed: %v", hooks["Notification"])
	}
	found, _, err := FindHooks(b)
	if err != nil || len(found) != len(Hooks) {
		t.Errorf("FindHooks: %d hooks, %v", len(found), err)
	}
}

func TestInstallRefusesInvalidJSON(t *testing.T) {
	for _, bad := range []string{`{"model": "opus",}`, `[1,2]`, `{"hooks": []}`, `{"hooks": {"Stop": {}}}`, `{} {}`} {
		settings := writeSettings(t, bad)
		_, err := New().Install(adapter.InstallOpts{Settings: settings, Hook: hookPath(t)})
		if err == nil {
			t.Errorf("%s: no error", bad)
			continue
		}
		if b, _ := os.ReadFile(settings); string(b) != bad {
			t.Errorf("%s: file changed to %s", bad, b)
		}
		if _, err := os.Stat(settings + BackupSuffix); err == nil {
			t.Errorf("%s: backup written for a refused change", bad)
		}
	}
}

func TestInstallKeepsExistingBackup(t *testing.T) {
	settings := writeSettings(t, `{"a":1}`)
	if err := os.WriteFile(settings+BackupSuffix, []byte("first backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := install(t, settings, hookPath(t))
	if r.BackupCreated || r.BackupPath != settings+BackupSuffix {
		t.Errorf("report: %+v", r)
	}
	if b, _ := os.ReadFile(settings + BackupSuffix); string(b) != "first backup" {
		t.Errorf("backup overwritten: %s", b)
	}
}

func TestInstallKeepsFormatting(t *testing.T) {
	crlf := "{\r\n    \"model\": \"opus\"\r\n}\r\n"
	settings := writeSettings(t, crlf)
	install(t, settings, hookPath(t))
	b, _ := os.ReadFile(settings)
	s := string(b)
	if strings.Count(s, "\n") != strings.Count(s, "\r\n") || !strings.Contains(s, "\r\n    \"model\": \"opus\"") {
		t.Errorf("formatting not kept:\n%q", s)
	}
}

func TestInstallDryRunAndWarnings(t *testing.T) {
	content := `{"disableAllHooks": true}`
	settings := writeSettings(t, content)
	r, err := New().Install(adapter.InstallOpts{Settings: settings, Hook: hookPath(t), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || len(r.After) == 0 || string(r.Before) != content || len(r.Warnings) != 1 {
		t.Errorf("report: %+v", r)
	}
	if b, _ := os.ReadFile(settings); string(b) != content {
		t.Errorf("dry run wrote the file: %s", b)
	}
	if _, err := os.Stat(settings + BackupSuffix); err == nil {
		t.Error("dry run wrote a backup")
	}
	if _, err := New().Install(adapter.InstallOpts{Settings: settings, Hook: "relative/loopguard-hook"}); err == nil {
		t.Error("relative hook path accepted")
	}
}

func TestIsLoopGuardHook(t *testing.T) {
	for cmd, want := range map[string]bool{
		`C:\Users\A B\.loopguard\bin\loopguard-hook.exe`: true,
		`"C:\Program Files\loopguard-hook.exe"`:          true,
		"/home/a/.loopguard/bin/loopguard-hook":          true,
		"LoopGuard-Hook.EXE":                             true,
		"loopguard":                                      false,
		"loopguard-hook-wrapper":                         false,
		"/bin/my-loopguard-hook":                         false,
	} {
		if got := IsLoopGuardHook(cmd); got != want {
			t.Errorf("IsLoopGuardHook(%q) = %v", cmd, got)
		}
	}
}

func TestDoctor(t *testing.T) {
	status := func(cs []adapter.Check) map[string]adapter.Status {
		m := map[string]adapter.Status{}
		for _, c := range cs {
			m[c.Name] = c.Status
		}
		return m
	}
	missing := filepath.Join(t.TempDir(), "settings.json")
	if s := status(New().Doctor(missing)); s["hooks installed"] != adapter.StatusFail {
		t.Errorf("missing file: %v", s)
	}

	settings := writeSettings(t, `{"disableAllHooks": true}`)
	hook := hookPath(t)
	install(t, settings, hook)
	s := status(New().Doctor(settings))
	if s["hooks installed"] != adapter.StatusOK || s["hook binary"] != adapter.StatusOK || s["hooks enabled"] != adapter.StatusWarn {
		t.Errorf("installed: %v", s)
	}
	os.Remove(hook)
	if s := status(New().Doctor(settings)); s["hook binary"] != adapter.StatusFail {
		t.Errorf("binary gone: %v", s)
	}
	os.WriteFile(settings, []byte("{"), 0o600)
	if s := status(New().Doctor(settings)); s["hooks installed"] != adapter.StatusFail {
		t.Errorf("invalid JSON: %v", s)
	}
}
