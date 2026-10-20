package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
)

// TestInitDoctorStatusUninstall runs the M2 user flow as subprocesses:
// init (twice), doctor (which starts the daemon and sends an event
// through a real loopguard-hook), status, capture on/off, and uninstall.
func TestInitDoctorStatusUninstall(t *testing.T) {
	if testing.Short() {
		t.Skip("builds loopguard-hook and starts a daemon")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "lg home")
	killDaemonAtCleanup(t, dir)
	hook := filepath.Join(root, "bin dir", "loopguard-hook")
	if runtime.GOOS == "windows" {
		hook += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", hook, "../loopguard-hook").CombinedOutput(); err != nil {
		t.Fatalf("building loopguard-hook: %v\n%s", err, out)
	}
	settings := filepath.Join(root, "claude dir", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o700)
	original := `{"model": "opus", "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "other"}]}]}}`
	os.WriteFile(settings, []byte(original), 0o600)

	out, _, _ := lg(t, dir, "", "init", "--settings", settings, "--hook", hook, "--dry-run")
	if !strings.Contains(out, "+") || !strings.Contains(out, "Dry run: nothing written.") {
		t.Errorf("dry run:\n%s", out)
	}
	if b, _ := os.ReadFile(settings); string(b) != original {
		t.Fatal("dry run wrote the settings")
	}
	out, _, _ = lg(t, dir, "", "init", "--settings", settings, "--hook", hook)
	if !strings.Contains(out, "Installed LoopGuard hooks") || !strings.Contains(out, "backup:") || !strings.Contains(out, "loopguard uninstall") {
		t.Errorf("init:\n%s", out)
	}
	out, _, _ = lg(t, dir, "", "init", "--settings", settings, "--hook", hook)
	if !strings.Contains(out, "already installed") {
		t.Errorf("second init:\n%s", out)
	}

	out, _, _ = lg(t, dir, "", "doctor", "--settings", settings, "--json")
	var doc struct {
		Checks []adapter.Check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	byName := map[string]adapter.Check{}
	for _, c := range doc.Checks {
		byName[c.Name] = c
	}
	for _, name := range []string{"binaries", "hooks installed", "hook binary", "daemon", "token", "round trip", "database", "capture"} {
		if c, ok := byName[name]; !ok || c.Status != adapter.StatusOK {
			t.Errorf("doctor check %q: %+v", name, c)
		}
	}

	lg(t, dir, "", "capture", "on")
	out, _, _ = lg(t, dir, "", "status", "--json")
	var st statusReport
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	if !st.Daemon.Up || !st.Daemon.Capture || len(st.Sessions) != 0 {
		t.Errorf("status: %+v (the doctor's session must not be listed)", st)
	}
	if out, _, _ = lg(t, dir, "", "status"); !strings.Contains(out, "Daemon: up") || !strings.Contains(out, "Active sessions (0)") {
		t.Errorf("status:\n%s", out)
	}
	if out, _, _ = lg(t, dir, "", "capture", "status"); !strings.Contains(out, "Capture: on") {
		t.Errorf("capture status:\n%s", out)
	}
	lg(t, dir, "", "capture", "off")
	if _, err := os.Stat(filepath.Join(dir, "capture.on")); err == nil {
		t.Error("capture off left the flag file")
	}

	out, _, _ = lg(t, dir, "", "uninstall", "--settings", settings)
	if !strings.Contains(out, "Removed LoopGuard hooks") {
		t.Errorf("uninstall:\n%s", out)
	}
	var got, want any
	b, _ := os.ReadFile(settings)
	json.Unmarshal(b, &got)
	json.Unmarshal([]byte(original), &want)
	if !jsonEqual(got, want) {
		t.Errorf("after uninstall:\n%s\nwant:\n%s", b, original)
	}
	lg(t, dir, "", "daemon", "stop")
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
