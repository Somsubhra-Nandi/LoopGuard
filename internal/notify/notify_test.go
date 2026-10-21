package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

func loop(ev string) Notification {
	return New(ev, "shop", "claude-code:1a2b3c4d-5e6f-7a8b/agent-x", "rf:npm test:9f", event.DetectorRepeatedFailure,
		"npm test", 6, 12*time.Minute, t0)
}

func TestContent(t *testing.T) {
	n := loop(config.EventLoopDetected)
	if n.Title() != "LoopGuard: shop" {
		t.Errorf("title %q", n.Title())
	}
	want := `Loop detected: "npm test" failed 6 times in a row with the same error, over 12 min. To stop it: loopguard stop 1a2b3c4d`
	if n.Body() != want {
		t.Errorf("body:\n%s\nwant:\n%s", n.Body(), want)
	}
	if b := loop(config.EventHalt).Body(); strings.Contains(b, "loopguard stop") || !strings.HasPrefix(b, "Stopped the turn: ") {
		t.Errorf("halt body %q", b)
	}
	// Only the allowed fields: a family that is a path keeps its base
	// name, secrets are redacted, and odd characters dropped.
	odd := New(config.EventLoopDetected, `C:\Users\jane\proj<x>`, "claude-code:s", "k", event.DetectorRepeatedFailure,
		"/home/jane/secret-project/run.sh --token ghp_abcdefghijklmnopqrstuvwxyz0123456789", 4, 0, t0)
	for _, part := range []string{odd.Title(), odd.Body()} {
		for _, bad := range []string{"/home", "jane/", `\`, "secret-project", "ghp_", "<"} {
			if strings.Contains(part, bad) {
				t.Errorf("%q leaks %q", part, bad)
			}
		}
	}
	for _, det := range []string{event.DetectorExactRepeat, event.DetectorOscillation, event.DetectorReadThrash, event.DetectorStall} {
		n := New(config.EventStall, "shop", "claude-code:s", "k", det, "read", 25, time.Minute, t0)
		if b := n.Body(); strings.Contains(b, "/") || !strings.Contains(b, "25") {
			t.Errorf("%s: %q", det, b)
		}
	}
}

func TestLimits(t *testing.T) {
	l := &limiter{last: map[string]time.Time{}}
	at := t0
	n := loop(config.EventLoopDetected)
	if !l.allow(n, at) {
		t.Fatal("first")
	}
	n.Event = config.EventNudge
	if l.allow(n, at.Add(9*time.Minute)) {
		t.Error("same loop within 10 min")
	}
	if !l.allow(n, at.Add(10*time.Minute)) {
		t.Error("same loop after 10 min")
	}
	n.Event = config.EventHalt
	if !l.allow(n, at.Add(11*time.Minute)) {
		t.Error("a halt is always sent once")
	}
	// At most 5 per hour, halts included.
	for i := 0; i < 2; i++ {
		other := loop(config.EventLoopDetected)
		other.LoopKey = "k" + string(rune('a'+i))
		if !l.allow(other, at.Add(12*time.Minute)) {
			t.Errorf("loop %d", i)
		}
	}
	other := loop(config.EventHalt)
	other.LoopKey = "late"
	if l.allow(other, at.Add(30*time.Minute)) {
		t.Error("6th in an hour")
	}
	if !l.allow(other, at.Add(61*time.Minute)) {
		t.Error("after the hour")
	}
	test := Notification{Event: EventTest}
	for i := 0; i < 10; i++ {
		if !l.allow(test, at) {
			t.Fatal("tests are never limited")
		}
	}
}

type recorder struct {
	mu   sync.Mutex
	got  map[string][]Notification
	fail int // fail this many sends first
	done chan struct{}
}

func (r *recorder) sender(name string) Sender {
	return SenderFunc(func(_ context.Context, _ config.Channel, n Notification) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.fail > 0 && name == "ntfy" {
			r.fail--
			return errors.New("down")
		}
		r.got[name] = append(r.got[name], n)
		r.done <- struct{}{}
		return nil
	})
}

func TestDispatcher(t *testing.T) {
	r := &recorder{got: map[string][]Notification{}, done: make(chan struct{}, 16), fail: 1}
	block := make(chan struct{})
	senders := Senders{
		config.ChannelDesktop: r.sender("desktop"),
		config.ChannelNtfy:    r.sender("ntfy"),
		// A webhook that hangs must not delay anything else.
		config.ChannelWebhook: SenderFunc(func(ctx context.Context, _ config.Channel, _ Notification) error {
			<-block
			return nil
		}),
	}
	d := NewDispatcher(senders, nil)
	d.backoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	d.now = func() time.Time { return t0 }
	cfg := config.DefaultNotify()
	cfg.Ntfy = config.Channel{Enabled: true, Topic: "t", On: []string{config.EventHalt}}
	cfg.Webhook = config.Channel{Enabled: true, URL: "https://x", Format: "slack"}

	start := time.Now()
	sent := d.Dispatch(cfg, loop(config.EventLoopDetected))
	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("Dispatch blocked for %v", time.Since(start))
	}
	if strings.Join(sent, ",") != "desktop,webhook" {
		t.Errorf("queued on %v, want desktop and webhook (ntfy is only for halts)", sent)
	}
	// ntfy's first attempt fails; it gets the halt on the first retry.
	if sent := d.Dispatch(cfg, loop(config.EventHalt)); strings.Join(sent, ",") != "desktop,ntfy,webhook" {
		t.Errorf("halt queued on %v", sent)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
	}
	r.mu.Lock()
	if len(r.got["desktop"]) != 2 || len(r.got["ntfy"]) != 1 || r.fail != 0 {
		t.Errorf("sent %v", r.got)
	}
	r.mu.Unlock()
	// Terminal: only interactive, only its events.
	cfg.Terminal = config.Channel{Enabled: true}
	if s := d.Terminal(cfg, loop(config.EventHalt), false); s != "" {
		t.Error("terminal notification for a non-interactive session")
	}
	if s := d.Terminal(cfg, loop(config.EventHalt), true); !strings.HasPrefix(s, "LoopGuard: shop: Stopped the turn") {
		t.Errorf("terminal %q", s)
	}
	close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Close(ctx)
	if sent := d.Dispatch(cfg, loop(config.EventHalt)); len(sent) != 0 {
		t.Errorf("queued after Close: %v", sent)
	}
}

func TestNtfyAndWebhook(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Errorf("%s: not JSON: %s", r.URL.Path, b)
		}
		mu.Lock()
		bodies[r.URL.Path] = m
		mu.Unlock()
		if r.URL.Path == "/fail/secret-token" {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	n := loop(config.EventHalt)

	if err := (Ntfy{}).Send(ctx, config.Channel{Server: srv.URL + "/", Topic: "loopguard-x"}, n); err != nil {
		t.Fatal(err)
	}
	if m := bodies["/"]; m["topic"] != "loopguard-x" || m["title"] != "LoopGuard: shop" || m["priority"] != 4.0 || !strings.HasPrefix(m["message"].(string), "Stopped the turn") {
		t.Errorf("ntfy body %v", m)
	}
	for format, key := range map[string]string{"slack": "text", "discord": "content", "generic": "stop_command"} {
		if err := (Webhook{}).Send(ctx, config.Channel{URL: srv.URL + "/" + format, Format: format}, loop(config.EventLoopDetected)); err != nil {
			t.Fatal(err)
		}
		if _, ok := bodies["/"+format][key]; !ok {
			t.Errorf("%s payload %v lacks %s", format, bodies["/"+format], key)
		}
	}
	if g := bodies["/generic"]; g["family"] != "npm test" || g["count"] != 6.0 || g["session"] != "1a2b3c4d" || g["duration_seconds"] != 720.0 {
		t.Errorf("generic payload %v", g)
	}
	// Errors never carry the URL: a webhook URL is a secret.
	err := (Webhook{}).Send(ctx, config.Channel{URL: srv.URL + "/fail/secret-token"}, n)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error %v", err)
	}
	err = (Webhook{}).Send(ctx, config.Channel{URL: "http://127.0.0.1:1/secret-token"}, n)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("connection error %v", err)
	}
}

func TestNewTopic(t *testing.T) {
	a, err := NewTopic()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewTopic()
	if a == b || !regexp.MustCompile(`^loopguard-[a-z2-7]{26}$`).MatchString(a) {
		t.Errorf("topics %q %q", a, b)
	}
	if _, _, err := config.Parse([]byte("notify: { ntfy: { topic: " + a + " } }")); err != nil {
		t.Errorf("config rejects the topic: %v", err)
	}
}

func TestDesktopCommandKeepsTextOutOfTheScript(t *testing.T) {
	defer func(f func(string) (string, error)) { lookPath = f }(lookPath)
	lookPath = func(name string) (string, error) { return "/bin/" + name, nil }
	title, body := `LoopGuard: a'b"c`, `$(rm -rf ~); "x" <toast>`
	c, err := desktopCommand(title, body)
	if err != nil {
		t.Fatal(err)
	}
	inArgs, inEnv := false, false
	for _, a := range c.args {
		if a == body {
			inArgs = true
		} else if strings.Contains(a, "rm -rf") {
			t.Errorf("body spliced into an argument: %q", a)
		}
	}
	for _, e := range c.env {
		if strings.HasSuffix(e, "="+body) {
			inEnv = true
		}
	}
	if !inArgs && !inEnv {
		t.Errorf("body passed neither as an argument nor in the environment: %+v", c)
	}
}
