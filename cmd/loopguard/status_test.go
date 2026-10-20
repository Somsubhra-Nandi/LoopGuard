package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

func TestPrintStatus(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	r := statusReport{
		Daemon: daemonStatus{Up: true, PID: 42, Port: 47821, Version: "v1", Uptime: 7380, Mode: "observe"},
		Sessions: []store.Session{{Key: "claude-code:a", SessionID: "a", Cwd: `C:\src\shop`, LastSeenAt: now.Add(-12 * time.Second),
			ToolCalls: 140, Failures: 9, LoopsDetected: 1}},
		Decisions: []decisionView{{Intervention: store.Intervention{SessionKey: "claude-code:a", TS: now.Add(-3 * time.Minute),
			Detector: "repeated_failure", Action: "nudge", Reason: "repeated_failure: `npm test` failed 4 times\nmore"}, Project: `C:\src\shop`}},
	}
	var b bytes.Buffer
	printStatus(&b, r, now)
	out := b.String()
	for _, want := range []string{
		"Daemon: up 2h03m (pid 42, port 47821, version v1), observe mode, capture off",
		"Active sessions (1)", `C:\src\shop`, "last activity 12s ago, 140 tool calls, 9 failures, 1 loop",
		"3m ago   nudge  repeated_failure  shop: `npm test` failed 4 times",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "more") {
		t.Errorf("reason not cut at its first line:\n%s", out)
	}

	b.Reset()
	printStatus(&b, statusReport{Daemon: daemonStatus{Mode: "observe"}}, now)
	if out := b.String(); !strings.Contains(out, "Daemon: down") || strings.Contains(out, "Active sessions") || !strings.Contains(out, "nothing yet") {
		t.Errorf("daemon down:\n%s", out)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 45 * time.Second: "45s", 12 * time.Minute: "12m", 185 * time.Minute: "3h05m", 52 * time.Hour: "2d04h",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
