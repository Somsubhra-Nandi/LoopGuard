package session

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
)

func TestRing(t *testing.T) {
	r := NewRing[int](3)
	if _, ok := r.Last(); ok || r.Len() != 0 {
		t.Fatal("new ring not empty")
	}
	for i := 1; i <= 5; i++ {
		r.Push(i)
	}
	if r.Len() != 3 || r.Cap() != 3 {
		t.Fatalf("Len=%d Cap=%d", r.Len(), r.Cap())
	}
	if got := r.Tail(10); !reflect.DeepEqual(got, []int{3, 4, 5}) {
		t.Errorf("Tail(10) = %v", got)
	}
	if got := r.Tail(2); !reflect.DeepEqual(got, []int{4, 5}) {
		t.Errorf("Tail(2) = %v", got)
	}
	if r.At(0) != 3 {
		t.Errorf("At(0) = %d", r.At(0))
	}
	if v, _ := r.Last(); v != 5 {
		t.Errorf("Last = %d", v)
	}
	r.Reset()
	if r.Len() != 0 || len(r.Tail(3)) != 0 {
		t.Error("Reset did not empty the ring")
	}
	defer func() {
		if recover() == nil {
			t.Error("At out of range did not panic")
		}
	}()
	r.At(0)
}

func TestProgressTracker(t *testing.T) {
	p := NewProgressTracker(4)
	for i, s := range []float64{0.4, 0, 1, 0.3, 0.6} {
		p.Add(uint64(i+1), s)
	}
	// Capacity 4: the first score (seq 1) is gone.
	if got := p.Recent(2); !approx(got, 0.9) {
		t.Errorf("Recent(2) = %v", got)
	}
	if got := p.Recent(100); !approx(got, 1.9) {
		t.Errorf("Recent(100) = %v", got)
	}
	if got := p.Since(3); !approx(got, 0.9) {
		t.Errorf("Since(3) = %v (exclusive of seq 3)", got)
	}
	if got := p.Since(5); got != 0 {
		t.Errorf("Since(last) = %v", got)
	}
}

// harness drives a session with hand-made calls.
type harness struct {
	t *testing.T
	s *SessionState
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, s: New("cc:test", event.AgentClaudeCode, config.Default())}
}

func (h *harness) record(c *event.ToolCall) float64 {
	h.s.Assign(c.Event)
	h.s.Record(c)
	return c.Progress
}

func bash(family, errText string, failing int) *event.ToolCall {
	c := &event.ToolCall{
		Event:        &event.Event{ToolName: event.ToolBash, ReceivedAt: time.Unix(0, 0)},
		Family:       family,
		FailingTests: failing,
		TodosDone:    -1,
	}
	if errText != "" {
		c.ErrorSig = normalize.Signature(errText)
	}
	return c
}

func edit(path string) *event.ToolCall {
	return &event.ToolCall{
		Event:     &event.Event{ToolName: event.ToolEdit},
		Family:    event.ToolEdit,
		FilePath:  path,
		Edits:     []event.EditPair{{Old: 1, New: 2}},
		TodosDone: -1,
	}
}

func TestProgressScores(t *testing.T) {
	h := newHarness(t)
	steps := []struct {
		name string
		c    *event.ToolCall
		want float64
	}{
		{"first failure of a family", bash("npm test", "Error: A failed", 3), 0},
		{"same error again", bash("npm test", "Error: A failed", 3), 0},
		{"error changed to a new signature", bash("npm test", "Error: B failed", 3), 0.6},
		{"back to a known signature", bash("npm test", "Error: A failed", 3), 0},
		{"fewer failing tests", bash("npm test", "Error: A failed", 2), 0.6},
		{"edit to a file new this turn", edit("/r/a.ts"), 0.4},
		{"edit to the same file again", edit("/r/a.ts"), 0},
		{"failing family succeeds", bash("npm test", "", -1), 1.0},
		{"family succeeds again", bash("npm test", "", -1), 0},
		{"successful new family", bash("go vet", "", -1), 0.3},
		{"failing new family", bash("go build", "Error: x", -1), 0},
	}
	for _, st := range steps {
		if got := h.record(st.c); got != st.want {
			t.Errorf("%s: score = %v, want %v", st.name, got, st.want)
		}
	}

	// A human prompt starts a new turn: the same file counts as new again.
	h.s.HumanPrompt(&event.Event{})
	if got := h.record(edit("/r/a.ts")); got != 0.4 {
		t.Errorf("edit after prompt: score = %v, want 0.4", got)
	}

	todo := func(done int) *event.ToolCall {
		return &event.ToolCall{Event: &event.Event{ToolName: "other:TodoWrite"}, Family: "other:TodoWrite", TodosDone: done}
	}
	if got := h.record(todo(0)); got != 0 {
		t.Errorf("first todo list: %v", got)
	}
	if got := h.record(todo(1)); got != 0.8 {
		t.Errorf("todo completed: %v, want 0.8", got)
	}
	if got := h.record(todo(1)); got != 0 {
		t.Errorf("unchanged todo list: %v", got)
	}
}

func TestRecordStats(t *testing.T) {
	h := newHarness(t)
	h.record(bash("npm test", "Error: A", 1))
	h.record(bash("npm test", "Error: A", 1))
	fs := h.s.Families["npm test"]
	if fs.Runs != 2 || fs.Fails != 2 || fs.ConsecFails != 2 || len(fs.Sigs) != 1 {
		t.Errorf("family stats: %+v", fs)
	}
	h.record(bash("npm test", "", -1))
	if fs.ConsecFails != 0 || fs.LastSuccessSeq != 3 {
		t.Errorf("after success: %+v", fs)
	}
	if es := h.s.Errors[normalize.Signature("Error: A").Exact]; es == nil || es.Count != 2 || es.FirstSeq != 1 || es.LastSeq != 2 {
		t.Errorf("error stats: %+v", es)
	}

	w := &event.ToolCall{Event: &event.Event{ToolName: event.ToolWrite}, FilePath: "/r/b.ts", FileHashAfter: 42, TodosDone: -1}
	h.record(&event.ToolCall{Event: &event.Event{ToolName: event.ToolRead}, FilePath: "/r/b.ts", TodosDone: -1})
	if h.s.Files["/r/b.ts"].Reads != 1 {
		t.Error("read not counted")
	}
	h.record(w)
	fh := h.s.Files["/r/b.ts"]
	if fh.Reads != 0 || len(fh.Writes) != 1 || fh.Writes[0].Hash != 42 || fh.LastWrite != w.Seq() {
		t.Errorf("file history: %+v", fh)
	}
	for i := 0; i < FileHistoryN+5; i++ {
		h.record(&event.ToolCall{Event: &event.Event{ToolName: event.ToolWrite}, FilePath: "/r/b.ts", FileHashAfter: uint64(i + 100), TodosDone: -1})
	}
	if len(fh.Writes) != FileHistoryN {
		t.Errorf("history not capped: %d", len(fh.Writes))
	}
}

func TestWindowRespectsTurn(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		h.record(bash("ls", "", -1))
	}
	if n := len(h.s.Window(3)); n != 3 {
		t.Errorf("Window(3) = %d calls", n)
	}
	h.s.HumanPrompt(&event.Event{})
	if n := len(h.s.Window(50)); n != 0 {
		t.Errorf("window after prompt = %d calls, want 0", n)
	}
	if h.s.WindowStart(50) != h.s.Seq()+1 {
		t.Error("WindowStart of an empty window")
	}
	h.record(bash("ls", "", -1))
	if w := h.s.Window(50); len(w) != 1 || w[0].Seq() != h.s.Seq() {
		t.Errorf("window = %v", w)
	}
}

func TestPending(t *testing.T) {
	h := newHarness(t)
	ev := &event.Event{ToolUseID: "t1"}
	h.s.AddPending(ev)
	h.s.AddPending(&event.Event{}) // no id: ignored
	if got := h.s.TakePending("t1"); got != ev {
		t.Error("pending event not returned")
	}
	if h.s.TakePending("t1") != nil {
		t.Error("pending event returned twice")
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestResumeAt(t *testing.T) {
	s := New("k", event.AgentClaudeCode, config.Default())
	s.ResumeAt(41)
	ev := &event.Event{}
	s.Assign(ev)
	if ev.Seq != 42 {
		t.Errorf("seq after ResumeAt(41) = %d, want 42", ev.Seq)
	}
	s.ResumeAt(10) // never backwards
	s.Assign(ev)
	if ev.Seq != 43 {
		t.Errorf("seq after ResumeAt(10) = %d, want 43", ev.Seq)
	}
}
