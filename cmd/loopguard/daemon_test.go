package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/proc"
)

// lg runs this test binary as the loopguard binary with LOOPGUARD_HOME=dir.
func lg(t *testing.T, dir, stdin string, args ...string) (stdout, stderr string, took time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), subprocessTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "LOOPGUARD_HOME="+dir)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	start := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("loopguard %s: %v (stderr: %s)", strings.Join(args, " "), err, errOut.String())
	}
	return out.String(), errOut.String(), time.Since(start)
}

// killDaemonAtCleanup makes sure no test daemon outlives the test (and
// keeps files in its temp home open, which Windows can't delete).
func killDaemonAtCleanup(t *testing.T, dir string) {
	t.Cleanup(func() {
		info, err := home.ReadDaemonInfo(dir)
		if err != nil || !proc.Alive(info.PID) {
			return
		}
		if _, ok := daemon.Healthy(info.Port, dir); ok {
			_ = proc.Kill(info.PID)
			for i := 0; i < 100 && proc.Alive(info.PID); i++ {
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
}

func waitForDaemon(t *testing.T, dir string, within time.Duration) home.DaemonInfo {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if info, err := home.ReadDaemonInfo(dir); err == nil {
			if _, ok := daemon.Healthy(info.Port, dir); ok {
				return info
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no daemon came up within %v", within)
	return home.DaemonInfo{}
}

const hookPayload = `{"session_id":"cli-1","hook_event_name":"PreToolUse","cwd":"/repo","tool_name":"Bash","tool_input":{"command":"go test ./..."},"tool_use_id":"t1"}`

func TestHookStartsDaemonThenRoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	dir := filepath.Join(t.TempDir(), "home with spaces")
	killDaemonAtCleanup(t, dir)

	// Daemon down: the hook exits 0 at once, prints nothing, and starts it.
	out, errOut, took := lg(t, dir, hookPayload, "hook", "--agent", "claude-code")
	if out != "" || errOut != "" {
		t.Fatalf("hook with no daemon printed stdout %q stderr %q", out, errOut)
	}
	t.Logf("hook with no daemon: %v", took)
	info := waitForDaemon(t, dir, 15*time.Second)
	if info.Version == "" || info.StartedAt.IsZero() {
		t.Errorf("daemon.json = %+v", info)
	}

	// Daemon up: the event round-trips (observe mode: no output).
	out, errOut, _ = lg(t, dir, strings.Replace(hookPayload, "PreToolUse", "PostToolUse", 1), "hook")
	if out != "" || errOut != "" {
		t.Errorf("hook with daemon printed stdout %q stderr %q", out, errOut)
	}
	out, _, _ = lg(t, dir, "", "daemon", "status")
	if !strings.Contains(out, "running") || !strings.Contains(out, "sessions: 1 active") {
		t.Errorf("status:\n%s", out)
	}

	out, _, _ = lg(t, dir, "", "daemon", "stop")
	if !strings.Contains(out, "stopped") {
		t.Errorf("stop: %s", out)
	}
	if proc.Alive(info.PID) {
		t.Error("daemon still alive after stop")
	}
	if _, err := os.Stat(filepath.Join(dir, home.DaemonFile)); err == nil {
		t.Error("daemon.json left after stop")
	}
	out, _, _ = lg(t, dir, "", "daemon", "status")
	if !strings.Contains(out, "not running") {
		t.Errorf("status after stop: %s", out)
	}
	out, _, _ = lg(t, dir, "", "daemon", "stop")
	if !strings.Contains(out, "not running") {
		t.Errorf("second stop: %s", out)
	}
}

func TestDaemonDetach(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real processes")
	}
	dir := t.TempDir()
	killDaemonAtCleanup(t, dir)
	out, _, _ := lg(t, dir, "", "daemon", "--detach")
	if !strings.Contains(out, "started") {
		t.Fatalf("--detach: %s", out)
	}
	info := waitForDaemon(t, dir, 5*time.Second)
	out, _, _ = lg(t, dir, "", "daemon", "--detach")
	if !strings.Contains(out, "already running") {
		t.Errorf("second --detach: %s", out)
	}
	// A foreground start next to a running daemon says so and exits 0.
	out, _, _ = lg(t, dir, "", "daemon")
	if !strings.Contains(out, "already running") {
		t.Errorf("foreground start next to a running daemon: %s", out)
	}
	lg(t, dir, "", "daemon", "stop")
	if proc.Alive(info.PID) {
		t.Error("daemon still alive after stop")
	}
}
