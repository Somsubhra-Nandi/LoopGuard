package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseArgs(t *testing.T) {
	for _, tt := range []struct {
		args    []string
		agent   string
		version bool
	}{
		{nil, "", false},
		{[]string{"--agent", "codex"}, "codex", false},
		{[]string{"--agent=codex"}, "codex", false},
		{[]string{"--agent"}, "", false},
		{[]string{"--future-flag", "x", "--agent", "claude-code"}, "claude-code", false},
		{[]string{"--version"}, "", true},
	} {
		agent, version := parseArgs(tt.args)
		if agent != tt.agent || version != tt.version {
			t.Errorf("parseArgs(%q) = %q, %v; want %q, %v", tt.args, agent, version, tt.agent, tt.version)
		}
	}
}

// TestBinaryFailsOpen builds loopguard-hook and runs it with no daemon:
// it must exit 0 and print nothing.
func TestBinaryFailsOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	exe := filepath.Join(t.TempDir(), "loopguard-hook")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--agent", "claude-code")
	cmd.Env = append(os.Environ(), "LOOPGUARD_HOME="+filepath.Join(t.TempDir(), "empty home"), "LOOPGUARD_NO_AUTOSTART=1")
	cmd.Stdin = strings.NewReader(`{"session_id":"s","hook_event_name":"Stop"}`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("no daemon: err %v, stdout %q, stderr %q", err, stdout.String(), stderr.String())
	}

	out, err := exec.CommandContext(ctx, exe, "--version").Output()
	if err != nil || string(out) != "loopguard-hook dev\n" {
		t.Errorf("--version = %q, %v", out, err)
	}
}

// TestMinimalDependencies keeps the hook binary small: every package it
// links costs start-up time on every tool call.
func TestMinimalDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	exact := map[string]bool{"fmt": true, "reflect": true, "encoding/json": true, "log": true,
		"github.com/Somsubhra-Nandi/loopguard/internal/home": true}
	prefixes := []string{"net/http", "crypto/", "github.com/spf13/", "golang.org/x/", "modernc.org/",
		"github.com/Somsubhra-Nandi/loopguard/internal/daemon"}
	for _, dep := range strings.Fields(string(out)) {
		bad := exact[dep]
		for _, p := range prefixes {
			bad = bad || strings.HasPrefix(dep, p)
		}
		if bad {
			t.Errorf("loopguard-hook depends on %s", dep)
		}
	}
}
