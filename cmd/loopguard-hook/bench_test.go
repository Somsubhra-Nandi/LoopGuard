package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
)

// TestHookLatency measures the end-to-end hook overhead: the time from
// starting the hook process to its exit, against a real daemon, for
// loopguard-hook, `loopguard hook`, and an empty Go program (the
// process-start floor). It runs only with LOOPGUARD_BENCH_CALLS set:
//
//	$env:LOOPGUARD_BENCH_CALLS=500; go test ./cmd/loopguard-hook -run HookLatency -v
func TestHookLatency(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("LOOPGUARD_BENCH_CALLS"))
	if n <= 0 {
		t.Skip("set LOOPGUARD_BENCH_CALLS to run")
	}
	bin := t.TempDir()
	exe := func(name string) string {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		return filepath.Join(bin, name)
	}
	empty := filepath.Join(t.TempDir(), "empty.go")
	if err := os.WriteFile(empty, []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, b := range [][2]string{{"loopguard-hook", "."}, {"loopguard", "../loopguard"}, {"empty", empty}} {
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", exe(b[0]), b[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", b[0], err, out)
		}
	}

	dir := filepath.Join(t.TempDir(), "bench home")
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan home.DaemonInfo, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- daemon.Run(ctx, daemon.Options{Home: dir, Version: "bench", Port: -1, Ready: func(i home.DaemonInfo) { ready <- i }})
	}()
	select {
	case <-ready:
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not start")
	}
	defer func() { cancel(); <-errc }()

	variants := []struct {
		name string
		args []string
	}{
		{"empty program (floor)", []string{exe("empty")}},
		{"loopguard-hook", []string{exe("loopguard-hook"), "--agent", "claude-code"}},
		{"loopguard hook", []string{exe("loopguard"), "hook", "--agent", "claude-code"}},
	}
	env := append(os.Environ(), home.EnvHome+"="+dir, "LOOPGUARD_NO_AUTOSTART=1")
	const warmup = 20
	took := make([][]time.Duration, len(variants))
	for i := 0; i < warmup+n; i++ {
		payload := benchPayload(i)
		for v, variant := range variants {
			cmd := exec.Command(variant.args[0], variant.args[1:]...)
			cmd.Env = env
			cmd.Stdin = strings.NewReader(payload)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			start := time.Now()
			err := cmd.Run()
			d := time.Since(start)
			if err != nil || out.Len() != 0 {
				t.Fatalf("%s: err %v, output %q", variant.name, err, out.String())
			}
			if i >= warmup {
				took[v] = append(took[v], d)
			}
		}
	}
	for v, variant := range variants {
		d := took[v]
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		q := func(p float64) string { return d[int(p*float64(len(d)-1))].Round(100 * time.Microsecond).String() }
		t.Logf("%-22s n=%d  p50 %-7s p95 %-7s p99 %s", variant.name, len(d), q(0.50), q(0.95), q(0.99))
	}
}

// benchPayload alternates PreToolUse and PostToolUse events of one session.
func benchPayload(i int) string {
	ev := "PreToolUse"
	if i%2 == 1 {
		ev = "PostToolUse"
	}
	return `{"session_id":"bench","hook_event_name":"` + ev + `","cwd":"/repo","tool_name":"Bash",` +
		`"tool_input":{"command":"npm test"},"tool_use_id":"t` + strconv.Itoa(i/2) + `",` +
		`"tool_response":{"stdout":"Tests: 1 failed, 3 passed","stderr":"","interrupted":false}}`
}
