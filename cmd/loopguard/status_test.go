package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

func TestPrintStatus(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	r := statusReport{
		Daemon: daemonStatus{Up: true, PID: 42, Port: 47821, Version: "v1", Uptime: 7380},
		Mode: modeReport{Mode: config.ModeInfo{Mode: "guard", Why: "mode: guard in config.yaml"},
			Config: fileReport{Path: `C:\h\config.yaml`, Exists: true},
			Pauses: []store.Pause{{Scope: store.PauseSession, Target: "1a2b3c4d-x"}}},
		Sessions: []sessionStatus{
			{Session: store.Session{Key: "claude-code:a", SessionID: "a", Cwd: `C:\src\shop`, LastSeenAt: now.Add(-12 * time.Second),
				ToolCalls: 140, Failures: 9, LoopsDetected: 1}, Mode: "guard", ModeWhy: "mode: guard in config.yaml"},
			{Session: store.Session{Key: "claude-code:1a2b3c4d-x", SessionID: "1a2b3c4d-x", Cwd: `C:\src\api`, LastSeenAt: now},
				Mode: "observe", ModeWhy: "this session is paused until resumed"},
		},
		Decisions: []decisionView{
			{Intervention: store.Intervention{SessionKey: "claude-code:a", TS: now.Add(-3 * time.Minute), Mode: store.ModeEnforce,
				Detector: "repeated_failure", Action: "nudge", Outcome: "recovered", Reason: "repeated_failure: `npm test` failed 4 times\nmore"}, Project: `C:\src\shop`},
			{Intervention: store.Intervention{SessionKey: "claude-code:a", TS: now.Add(-4 * time.Minute), Mode: store.ModeObserve,
				Detector: "exact_repeat", Action: "deny", Reason: "x"}, Project: `C:\src\shop`},
			{Intervention: store.Intervention{SessionKey: "claude-code:a", TS: now.Add(-5 * time.Minute), Mode: store.ModeEnforce,
				Detector: "repeated_failure", Action: "nudge", Outcome: "moved_on", Reason: "y"}, Project: `C:\src\shop`},
		},
	}
	var b bytes.Buffer
	printStatus(&b, r, now)
	out := b.String()
	for _, want := range []string{
		"Level: guard (mode: guard in config.yaml)",
		"paused: session 1a2b3c4d-x (until resumed)",
		"Daemon: up 2h03m (pid 42, port 47821, version v1), capture off",
		"Active sessions (2)", `C:\src\shop  [a]`, "last activity 12s ago, 140 tool calls, 9 failures, 1 loop",
		"[1a2b3c4d]", "level observe (this session is paused until resumed)\n",
		"level guard (mode: guard in config.yaml); stop its turn: loopguard stop a\n",
		"3m ago   real  nudge repeated_failure  recovered      shop: `npm test` failed 4 times",
		"4m ago   would deny  exact_repeat      pending        shop: x",
		"5m ago   real  nudge repeated_failure  moved_on       shop: y",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "more") {
		t.Errorf("reason not cut at its first line:\n%s", out)
	}

	b.Reset()
	printStatus(&b, statusReport{Mode: modeReport{Mode: config.ModeInfo{Mode: "observe", Why: "the default"}}}, now)
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
