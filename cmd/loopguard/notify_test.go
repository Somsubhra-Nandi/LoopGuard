package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/notify"
)

func TestNotifyCommands(t *testing.T) {
	dir := isolate(t)
	out := cli(t, false, "notify")
	for _, want := range []string{"Level: observe", "Nothing is sent at this level", "desktop   on   loop_detected, halt",
		"terminal  off", "ntfy      off", "webhook   off", "5 per hour"} {
		if !strings.Contains(out, want) {
			t.Errorf("notify lacks %q:\n%s", want, out)
		}
	}

	out = cli(t, false, "notify", "setup", "ntfy")
	f, _ := readConfig(t, dir)
	if f.Notify == nil || f.Notify.Ntfy == nil || f.Notify.Ntfy.Topic == nil || *f.Notify.Ntfy.Enabled != true {
		t.Fatalf("config after setup: %+v", f.Notify)
	}
	topic := *f.Notify.Ntfy.Topic
	for _, want := range []string{"Your topic: " + topic, "Keep the topic private", "https://ntfy.sh/" + topic, "loopguard notify test",
		"the level is observe, which sends nothing"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup ntfy lacks %q:\n%s", want, out)
		}
	}
	cli(t, false, "notify", "setup", "ntfy")
	if f, _ := readConfig(t, dir); *f.Notify.Ntfy.Topic != topic {
		t.Error("a second setup replaced the topic")
	}
	cli(t, false, "notify", "setup", "ntfy", "--new-topic", "--server", "https://ntfy.example.com/")
	if f, _ := readConfig(t, dir); *f.Notify.Ntfy.Topic == topic || *f.Notify.Ntfy.Server != "https://ntfy.example.com" {
		t.Errorf("--new-topic --server: %+v", f.Notify.Ntfy)
	}

	out = cli(t, false, "notify", "setup", "webhook", "https://hooks.example.com/services/T0/B0/s3cr3t", "--format", "discord")
	if strings.Contains(out, "s3cr3t") || !strings.Contains(out, "hooks.example.com") {
		t.Errorf("setup webhook output: %s", out)
	}
	cli(t, true, "notify", "setup", "webhook", "file:///etc/passwd")
	if out := cli(t, false, "notify"); strings.Contains(out, "s3cr3t") || !strings.Contains(out, "discord webhook at hooks.example.com") {
		t.Errorf("notify shows the webhook URL: %s", out)
	}
	// The file is still a valid config, with comments and other keys kept.
	if _, _, _, err := config.LoadFile(filepath.Join(dir, config.GlobalFile)); err != nil {
		t.Fatal(err)
	}

	// test: every enabled channel, each result reported.
	var got []string
	notifySenders = func() notify.Senders {
		s := notify.Senders{}
		for _, ch := range []string{config.ChannelDesktop, config.ChannelNtfy, config.ChannelWebhook} {
			s[ch] = notify.SenderFunc(func(_ context.Context, _ config.Channel, n notify.Notification) error {
				got = append(got, ch+":"+n.Event)
				if ch == config.ChannelWebhook {
					return errors.New("server answered 404 Not Found")
				}
				return nil
			})
		}
		return s
	}
	defer func() { notifySenders = notify.DefaultSenders }()
	out = cli(t, true, "notify", "test")
	if strings.Join(got, ",") != "desktop:test,ntfy:test,webhook:test" || !strings.Contains(out, "ntfy      sent") ||
		!strings.Contains(out, "webhook   failed: server answered 404") {
		t.Errorf("notify test: %v\n%s", got, out)
	}
}

// startDaemonIn runs a daemon in-process for the home dir.
func startDaemonIn(t *testing.T, dir string) home.DaemonInfo {
	t.Helper()
	ready := make(chan home.DaemonInfo, 1)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- daemon.Run(ctx, daemon.Options{Home: dir, Version: "test", Port: -1, Ready: func(i home.DaemonInfo) { ready <- i }})
	}()
	t.Cleanup(func() { cancel(); <-errc })
	select {
	case info := <-ready:
		return info
	case err := <-errc:
		t.Fatalf("daemon: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not start")
	}
	return home.DaemonInfo{}
}

// hookEvent sends one hook payload and returns the rendered reply.
func hookEvent(t *testing.T, dir string, port int, sid, event, extra string) string {
	t.Helper()
	payload := map[string]any{"session_id": sid, "hook_event_name": event, "cwd": filepath.Join(dir, "shop"), "permission_mode": "auto"}
	if extra != "" {
		if err := json.Unmarshal([]byte("{"+extra+"}"), &payload); err != nil {
			t.Fatal(err)
		}
	}
	var r daemon.EventReply
	if code, err := apiRequest(dir, port, http.MethodPost, "/v1/events?agent=claude-code", json.RawMessage(mustMarshal(payload)), &r); err != nil || code != 200 {
		t.Fatalf("event: %d %v", code, err)
	}
	return r.Stdout
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestStopCommand(t *testing.T) {
	dir := isolate(t)
	if out := cli(t, true, "stop", "abc"); !strings.Contains(out, "daemon isn't running") {
		t.Errorf("stop without a daemon: %s", out)
	}
	cli(t, true, "stop")
	cli(t, true, "stop", "abc", "--all")

	if err := os.WriteFile(filepath.Join(dir, config.GlobalFile), []byte("mode: assist\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info := startDaemonIn(t, dir)
	const sid = "5e5510aa-1111-2222-3333-444455556666"
	bash := func(i int) string {
		return fmt.Sprintf(`"tool_name":"Bash","tool_use_id":"t%d","tool_input":{"command":"npm run build"}`, i)
	}
	hookEvent(t, dir, info.Port, sid, "UserPromptSubmit", `"prompt":"build it"`)
	hookEvent(t, dir, info.Port, sid, "PreToolUse", bash(1))
	hookEvent(t, dir, info.Port, sid, "PostToolUse", bash(1)+`,"tool_response":{"stdout":"ok"}`)

	cli(t, true, "stop", "ffff")
	out := cli(t, false, "stop", "5e55")
	if !strings.Contains(out, "Stopping shop [5e5510aa]: its turn ends at its next tool call.") {
		t.Errorf("stop output: %s", out)
	}
	reply := hookEvent(t, dir, info.Port, sid, "PreToolUse", bash(2))
	if !strings.Contains(reply, `"continue":false`) || !strings.Contains(reply, `"stopReason":"Stopped by the user via loopguard stop."`) {
		t.Errorf("reply after stop: %s", reply)
	}

	// observe: refused, exit 1.
	if err := os.WriteFile(filepath.Join(dir, config.GlobalFile), []byte("mode: observe\n# changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(config.DefaultReloadEvery + 100*time.Millisecond)
	hookEvent(t, dir, info.Port, sid, "UserPromptSubmit", `"prompt":"again"`)
	if out := cli(t, true, "stop", "--all"); !strings.Contains(out, "Not stopping shop [5e5510aa]: it observes only") {
		t.Errorf("stop in observe: %s", out)
	}
}
