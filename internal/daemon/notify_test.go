package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/notify"
	"github.com/Somsubhra-Nandi/loopguard/internal/policy"
)

// sent records notifications by channel.
type sent struct {
	mu  sync.Mutex
	got map[string][]notify.Notification
}

func (s *sent) senders() notify.Senders {
	out := notify.Senders{}
	for _, ch := range []string{config.ChannelDesktop, config.ChannelNtfy, config.ChannelWebhook} {
		out[ch] = notify.SenderFunc(func(_ context.Context, _ config.Channel, n notify.Notification) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.got[ch] = append(s.got[ch], n)
			return nil
		})
	}
	return out
}

// wait returns the events sent on a channel once there are n.
func (s *sent) wait(t *testing.T, ch string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		var evs []string
		for _, x := range s.got[ch] {
			evs = append(evs, x.Event)
		}
		s.mu.Unlock()
		if len(evs) >= n || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNotifyLevel(t *testing.T) {
	rec := &sent{got: map[string][]notify.Notification{}}
	d := startDaemon(t, homeWithConfig(t, `mode: notify
notify:
  terminal: true
  ntfy: { topic: loopguard-test, on: [loop_detected] }
`), func(o *Options) { o.Senders = rec.senders() })
	d.query = "&entry=cli&attended=1"
	s := newScenario(d, "nt1-abcdefgh")
	s.cwd = gitRepo(t, "")
	s.prompt("fix the cart")
	var terminal []string
	for i := 1; i <= 12; i++ {
		s.edit("src/cart.ts")
		pre, post := s.bash("npm test", npmFailure)
		for _, r := range []EventReply{pre, post} {
			if k := replyKind(t, r); k != "none" {
				t.Fatalf("attempt %d: the notify level touched the agent: %s", i, r.Stdout)
			}
			if r.Stdout != "" {
				terminal = append(terminal, r.Stdout)
			}
		}
	}
	// One loop: one terminal notification, at the 4th failure, and one on
	// each other channel, whatever happens after.
	if len(terminal) != 1 || !strings.Contains(terminal[0], `"terminalSequence":"\u001b]777;notify;LoopGuard;LoopGuard: repo: Loop detected: \"npm test\" failed 4 times in a row`) ||
		!strings.Contains(terminal[0], "To stop it: loopguard stop nt1-abcd") {
		t.Errorf("terminal notifications: %q", terminal)
	}
	if got := rec.wait(t, config.ChannelDesktop, 1); len(got) != 1 || got[0] != config.EventLoopDetected {
		t.Errorf("desktop got %v", got)
	}
	if got := rec.wait(t, config.ChannelNtfy, 1); len(got) != 1 {
		t.Errorf("ntfy got %v", got)
	}

	// Not interactive (no entrypoint, or claude -p): no terminal sequence.
	d.query = "&entry=sdk-cli"
	s2 := newScenario(d, "nt2")
	s2.prompt("go")
	for i := 1; i <= 5; i++ {
		s2.edit("src/cart.ts")
		if _, post := s2.bash("npm test", npmFailure); post.Stdout != "" {
			t.Fatalf("non-interactive reply %s", post.Stdout)
		}
	}
}

func TestGuardNotifiesHalts(t *testing.T) {
	rec := &sent{got: map[string][]notify.Notification{}}
	d := startDaemon(t, homeWithConfig(t, "mode: guard\nnotify: { ntfy: { topic: loopguard-t, on: [halt, deny] } }\n"),
		func(o *Options) { o.Senders = rec.senders() })
	s := newScenario(d, "gh1")
	s.prompt("go")
	at := firstOf(s, 12)
	if at["halt"] == 0 {
		t.Fatalf("no halt: %v", at)
	}
	got := rec.wait(t, config.ChannelNtfy, 2)
	if strings.Join(got, ",") != "deny,halt" {
		t.Errorf("ntfy got %v, want the first deny and the halt (the per-loop limit holds back later denies)", got)
	}
	if got := rec.wait(t, config.ChannelDesktop, 2); strings.Join(got, ",") != "loop_detected,halt" {
		t.Errorf("desktop got %v", got)
	}
}

func TestObserveSendsNothing(t *testing.T) {
	rec := &sent{got: map[string][]notify.Notification{}}
	d := startDaemon(t, homeWithConfig(t, "mode: observe\nnotify: { terminal: true }\n"), func(o *Options) { o.Senders = rec.senders() })
	d.query = "&entry=cli"
	s := newScenario(d, "ob1")
	s.prompt("go")
	firstOf(s, 12)
	time.Sleep(100 * time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 0 {
		t.Errorf("observe sent %v", rec.got)
	}
}

func postStop(t *testing.T, d *testDaemon, body string) (int, StopReply) {
	t.Helper()
	code, b := d.do("POST", "/v1/stop", []byte(body), nil)
	var r StopReply
	if code != 400 {
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("%d %s: %v", code, b, err)
		}
	}
	return code, r
}

func TestStopRequests(t *testing.T) {
	d := startDaemon(t, homeWithConfig(t, "mode: notify\n"), nil)
	s := newScenario(d, "aaaa1111-session")
	s.prompt("refactor the cart")
	s.bash("npm run build", "")
	other := newScenario(d, "aaaa2222-session")
	other.prompt("go")

	if code, r := postStop(t, d, `{"session":"aaaa"}`); code != 409 || len(r.Matches) != 2 {
		t.Errorf("ambiguous prefix: %d %+v", code, r)
	}
	if code, _ := postStop(t, d, `{"session":"zzzz"}`); code != 404 {
		t.Errorf("unknown prefix: %d", code)
	}
	if code, _ := postStop(t, d, `{}`); code != 400 {
		t.Errorf("empty request: %d", code)
	}
	code, r := postStop(t, d, `{"session":"aaaa1111"}`)
	if code != 200 || len(r.Stopped) != 1 || r.Stopped[0].SessionID != "aaaa1111-session" || r.Stopped[0].Mode != config.ModeNotify {
		t.Fatalf("stop: %d %+v", code, r)
	}

	// The next tool event halts, even with no loop and at the notify level,
	// and so does every one after it until the turn ends.
	for i := 0; i < 2; i++ {
		pre, _ := s.bash("npm run build", "")
		var o struct {
			Continue   *bool  `json:"continue"`
			StopReason string `json:"stopReason"`
		}
		json.Unmarshal([]byte(pre.Stdout), &o)
		if replyKind(t, pre) != "halt" || o.StopReason != policy.StopMessage {
			t.Fatalf("pre %d after stop: %s", i, pre.Stdout)
		}
	}
	// The other session goes on.
	if pre, _ := other.bash("ls", ""); pre.Stdout != "" {
		t.Errorf("another session was stopped: %s", pre.Stdout)
	}
	// Recorded once, as the user's stop.
	var stops int
	for _, iv := range d.interventions(t, "claude-code:aaaa1111-session") {
		if iv.Detector == event.DetectorUserStop && iv.Action == "halt" {
			stops++
		}
	}
	if stops != 1 {
		t.Errorf("%d user_stop interventions, want 1", stops)
	}
	// The turn ends; the next prompt hears that the stop is over, and
	// nothing is stopped any more.
	s.send("Stop", `"stop_hook_active":false`)
	r1 := s.prompt("carry on")
	if replyKind(t, r1) != "nudge" || !strings.Contains(r1.Stdout, policy.HaltOverNote) {
		t.Errorf("prompt after the stop: %s", r1.Stdout)
	}
	if pre, _ := s.bash("npm run build", ""); pre.Stdout != "" {
		t.Errorf("still stopped after the turn ended: %s", pre.Stdout)
	}

	// --all; a session that only observes is refused.
	if err := os.WriteFile(filepath.Join(d.home, config.GlobalFile), []byte("mode: observe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(config.DefaultReloadEvery + 100*time.Millisecond)
	other.bash("ls", "")
	s.bash("ls", "")
	code, r = postStop(t, d, `{"all":true}`)
	if code != 200 || len(r.Refused) != 2 || len(r.Stopped) != 0 {
		t.Errorf("--all in observe: %d %+v", code, r)
	}
}
