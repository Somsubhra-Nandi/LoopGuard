package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
	"github.com/Somsubhra-Nandi/loopguard/internal/adapter/claudecode"
	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/pipeline"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// testAdapter wraps the Claude Code adapter with a Render hook.
type testAdapter struct {
	claudecode.Adapter
	render func(ev *event.Event, d event.Decision) ([]byte, int)
}

func (a testAdapter) Render(ev *event.Event, d event.Decision) ([]byte, int) {
	if a.render != nil {
		return a.render(ev, d)
	}
	return a.Adapter.Render(ev, d)
}

var _ adapter.Adapter = testAdapter{}

func newTestManager(t *testing.T, withStore bool) (*Manager, *store.Store, *store.Writer) {
	t.Helper()
	th := config.Default()
	var st *store.Store
	var w *store.Writer
	if withStore {
		var err error
		st, err = store.Open(filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		w = st.NewWriter(store.WriterOptions{})
	}
	m := newManager(pipeline.New(th), th.Policy.NudgeThreshold, st, w, slog.New(slog.NewTextHandler(io.Discard, nil)), newMetrics(), 0)
	t.Cleanup(func() {
		m.Close(context.Background())
		w.Close()
		if st != nil {
			st.Close()
		}
	})
	return m, st, w
}

func ev(sid string, phase event.Phase, toolUseID string) *event.Event {
	e := &event.Event{Agent: event.AgentClaudeCode, SessionID: sid, Phase: phase, Cwd: "/repo", ReceivedAt: time.Now()}
	if phase == event.PhasePre || phase == event.PhasePost || phase == event.PhasePostFailure {
		e.ToolName, e.ToolUseID = event.ToolBash, toolUseID
		e.Input = []byte(fmt.Sprintf(`{"command":"echo %s"}`, toolUseID))
		if phase != event.PhasePre {
			e.Output = []byte(`"ok"`)
		}
	}
	return e
}

func wait(t *testing.T, ch <-chan reply) reply {
	t.Helper()
	if ch == nil {
		t.Fatal("Submit returned no reply channel")
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
		return reply{}
	}
}

func TestActorRecoversFromPanic(t *testing.T) {
	m, _, _ := newTestManager(t, false)
	boom := testAdapter{render: func(*event.Event, event.Decision) ([]byte, int) { panic("render bug") }}
	if r := wait(t, m.Submit(ev("p", event.PhasePre, "t1"), boom)); r.stdout != nil || r.exit != 0 {
		t.Errorf("reply after panic = %+v, want none", r)
	}
	// The actor survives, and numbering continues.
	ok := testAdapter{}
	if r := wait(t, m.Submit(ev("p", event.PhasePost, "t1"), ok)); r.stdout != nil || r.exit != 0 {
		t.Errorf("reply after recovery = %+v", r)
	}
	m.met.mu.Lock()
	panics := m.met.errors["panic"]
	m.met.mu.Unlock()
	if panics != 1 {
		t.Errorf("panic metric = %d, want 1", panics)
	}
	if s := m.Live()["claude-code:p"]; s.ToolCalls != 1 {
		t.Errorf("summary after recovery = %+v", s.Session)
	}
}

func TestActorKeepsOrder(t *testing.T) {
	m, st, w := newTestManager(t, true)
	ad := testAdapter{}
	// Submissions from one goroutine are processed in submission order,
	// even while other sessions are busy.
	var wg sync.WaitGroup
	for _, sid := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var chans []<-chan reply
			for i := 0; i < 60; i++ {
				id := fmt.Sprintf("t%03d", i)
				chans = append(chans, m.Submit(ev(sid, event.PhasePre, id), ad), m.Submit(ev(sid, event.PhasePost, id), ad))
			}
			for _, ch := range chans {
				wait(t, ch)
			}
		}()
	}
	wg.Wait()
	w.Flush()
	for _, sid := range []string{"a", "b", "c"} {
		calls, err := st.Calls(context.Background(), "claude-code:"+sid, 1000)
		if err != nil || len(calls) != 60 {
			t.Fatalf("session %s: %d calls, %v", sid, len(calls), err)
		}
		for i, c := range calls {
			// Each pre/post pair takes two seqs; the post is the even one.
			if c.Seq != uint64(2*(i+1)) || c.InputDigest != fmt.Sprintf("echo t%03d", i) {
				t.Fatalf("session %s call %d = seq %d %q: out of order", sid, i, c.Seq, c.InputDigest)
			}
		}
	}
}

func TestQueueFullAnswersNone(t *testing.T) {
	m, _, _ := newTestManager(t, false)
	release := make(chan struct{})
	slow := testAdapter{render: func(*event.Event, event.Decision) ([]byte, int) { <-release; return nil, 0 }}
	first := m.Submit(ev("q", event.PhasePre, "t0"), slow)
	time.Sleep(20 * time.Millisecond) // the actor is now blocked in Render
	var dropped int
	for i := 0; i < actorQueue+10; i++ {
		if m.Submit(ev("q", event.PhaseStop, ""), slow) == nil {
			dropped++
		}
	}
	close(release)
	wait(t, first)
	if dropped != 10 {
		t.Errorf("dropped %d submissions, want 10 (queue of %d)", dropped, actorQueue)
	}
}

func TestEvictionAndResume(t *testing.T) {
	m, st, w := newTestManager(t, true)
	ad := testAdapter{}
	now := time.Now()
	m.clock = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("t%d", i)
		wait(t, m.Submit(ev("e", event.PhasePre, id), ad))
		wait(t, m.Submit(ev("e", event.PhasePost, id), ad))
	}
	m.evictIdle()
	if m.Active() != 1 {
		t.Fatal("evicted an active session")
	}
	now = now.Add(DefaultIdle + time.Minute)
	m.evictIdle()
	if m.Active() != 0 {
		t.Fatalf("idle session not evicted: %d active", m.Active())
	}
	// Let the evicted actor's last rows land, as hours of idleness would.
	time.Sleep(50 * time.Millisecond)
	w.Flush()

	wait(t, m.Submit(ev("e", event.PhasePre, "t9"), ad))
	wait(t, m.Submit(ev("e", event.PhasePost, "t9"), ad))
	w.Flush()
	calls, err := st.Calls(context.Background(), "claude-code:e", 100)
	if err != nil || len(calls) != 4 {
		t.Fatalf("calls after resume = %d, %v; want 4 (none lost to seq clashes)", len(calls), err)
	}
	if last := calls[3]; last.Seq != 8 || last.InputDigest != "echo t9" {
		t.Errorf("resumed call = seq %d %q, want seq 8", last.Seq, last.InputDigest)
	}
	sess, err := st.GetSession(context.Background(), "claude-code:e")
	if err != nil || sess.ToolCalls != 4 {
		t.Errorf("session after resume = %+v, %v; want 4 tool calls", sess, err)
	}
}

func TestSubagentsAreSeparateSessions(t *testing.T) {
	m, _, _ := newTestManager(t, false)
	ad := testAdapter{}
	main := ev("s", event.PhaseStop, "")
	sub := ev("s", event.PhaseStop, "")
	sub.AgentID = "agent-1"
	wait(t, m.Submit(main, ad))
	wait(t, m.Submit(sub, ad))
	live := m.Live()
	if _, ok := live["claude-code:s"]; !ok {
		t.Error("main session missing")
	}
	if s, ok := live["claude-code:s/agent-1"]; !ok || s.SessionID != "s" {
		t.Errorf("subagent session = %+v, %v", s, ok)
	}
}

func TestObserveOnly(t *testing.T) {
	for _, a := range []event.Action{event.ActionNudge, event.ActionDeny, event.ActionHalt} {
		d := observeOnly(pick([]event.Decision{{Action: event.ActionNone}, {Action: a, Message: "m", Notify: true}}))
		if d.Action != event.ActionNone || d.Notify {
			t.Errorf("observeOnly let %s through: %+v", a, d)
		}
		for _, p := range []event.Phase{event.PhasePre, event.PhasePost, event.PhasePostFailure, event.PhaseBatchEnd} {
			if out, code := claudecode.Render(&event.Event{Phase: p}, d); out != nil || code != 0 {
				t.Errorf("observe-mode %s on %s rendered %s", a, p, out)
			}
		}
	}
	if d := pick(nil); d.Action != event.ActionNone {
		t.Errorf("pick(nil) = %+v", d)
	}
	if d := pick([]event.Decision{{Action: event.ActionNudge, LoopKey: "a"}, {Action: event.ActionHalt, LoopKey: "b"}, {Action: event.ActionDeny}}); d.LoopKey != "b" {
		t.Errorf("pick chose %+v, want the halt", d)
	}
}

func TestTimeoutAnswersNone(t *testing.T) {
	m, _, _ := newTestManager(t, false)
	release := make(chan struct{})
	defer close(release)
	slow := testAdapter{render: func(*event.Event, event.Decision) ([]byte, int) {
		<-release
		return []byte(`{"late":true}`), 0
	}}
	srv := &server{token: "tok", adapters: adapter.Registry{event.AgentClaudeCode: slow}, mgr: m, met: m.met, log: m.log}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/events?agent=claude-code", strings.NewReader(payload("Stop", "slow", "")))
	req.Host = "127.0.0.1"
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set(TimeoutHeader, "150") // budget: 100 ms
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	took := time.Since(start)
	if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != `{"stdout":"","exit":0}` {
		t.Errorf("reply = %d %s, want none", resp.StatusCode, b)
	}
	if took < 90*time.Millisecond || took > time.Second {
		t.Errorf("replied after %v, want about the 100 ms budget", took)
	}
	m.met.mu.Lock()
	timeouts := m.met.errors["timeout"]
	m.met.mu.Unlock()
	if timeouts != 1 {
		t.Errorf("timeout metric = %d", timeouts)
	}
}
