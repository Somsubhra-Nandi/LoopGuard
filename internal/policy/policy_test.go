package policy

import (
	"math/rand"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

var t0 = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

// fixture drives the policy directly, with hand-made calls and signals.
type fixture struct {
	e     *Engine
	s     *session.SessionState
	clock time.Time
}

func newFixture(th config.Thresholds) *fixture {
	return &fixture{e: New(th), s: session.New("cc:t", event.AgentClaudeCode, th), clock: t0}
}

// call records a completed call that makes exactly the given progress. The
// call has no family or file, so the session scores it 0 by itself.
func (f *fixture) call(progress float64) *event.ToolCall {
	f.clock = f.clock.Add(30 * time.Second)
	c := &event.ToolCall{Event: &event.Event{ToolName: "other:test", ReceivedAt: f.clock}, TodosDone: -1}
	f.s.Assign(c.Event)
	f.s.Record(c)
	f.s.Progress.Add(c.Seq(), progress)
	return c
}

func (f *fixture) post(progress float64, sigs ...event.Signal) []event.Decision {
	return f.e.Post(f.s, f.call(progress), sigs)
}

func (f *fixture) pre(family string) []event.Decision {
	f.clock = f.clock.Add(time.Second)
	c := &event.ToolCall{Event: &event.Event{Phase: event.PhasePre, ToolName: event.ToolBash, ReceivedAt: f.clock}, Family: family}
	f.s.Assign(c.Event)
	return f.e.Pre(f.s, c)
}

func rf(sev float64) event.Signal {
	return event.Signal{Detector: event.DetectorRepeatedFailure, LoopKey: "rf:npm test:1", Severity: sev, Count: 4, Evidence: event.Evidence{Family: "npm test"}}
}

func would(ds []event.Decision) []event.Action {
	var out []event.Action
	for _, d := range ds {
		out = append(out, d.WouldHave)
	}
	return out
}

func TestNudgeThreshold(t *testing.T) {
	f := newFixture(config.Default())
	if ds := f.post(0, rf(0.5)); len(ds) != 0 {
		t.Errorf("below threshold: %v", would(ds))
	}
	ds := f.post(0, rf(0.55))
	if len(ds) != 1 || ds[0].WouldHave != event.ActionNudge || ds[0].Action != event.ActionNone || ds[0].Reason == "" {
		t.Errorf("at threshold: %+v", ds)
	}
}

func TestResetOnProgress(t *testing.T) {
	f := newFixture(config.Default())
	f.post(0, rf(0.6))
	l := f.s.Loops["rf:npm test:1"]
	f.post(1.0) // the family recovered
	if l.Stage != session.StageObserving {
		t.Errorf("stage after progress = %s, want observing", l.Stage)
	}
}

func TestResetWhenQuiet(t *testing.T) {
	th := config.Default()
	f := newFixture(th)
	f.post(0, rf(0.6))
	l := f.s.Loops["rf:npm test:1"]
	for i := 0; i < th.Policy.ResetQuietCalls-1; i++ {
		f.post(0)
	}
	if l.Stage != session.StageNudged {
		t.Fatalf("reset too early: %s", l.Stage)
	}
	f.post(0)
	if l.Stage != session.StageObserving {
		t.Errorf("stage after %d quiet calls = %s", th.Policy.ResetQuietCalls, l.Stage)
	}
}

func TestDeniedLoopResetsOnProgress(t *testing.T) {
	f := newFixture(config.Default())
	f.post(0, rf(0.6))
	for i := 0; i < 3; i++ {
		f.post(0, rf(0.8))
	}
	l := f.s.Loops["rf:npm test:1"]
	if l.Stage != session.StageDenying {
		t.Fatalf("stage = %s, want denying", l.Stage)
	}
	f.post(0.5)
	if l.Stage != session.StageDenying {
		t.Fatal("reset with progress below the threshold")
	}
	f.post(0.5)
	if l.Stage != session.StageObserving {
		t.Errorf("stage = %s, want observing after 1.0 progress", l.Stage)
	}
	if ds := f.pre("npm test"); len(ds) != 0 {
		t.Errorf("reset loop still denies: %v", would(ds))
	}
}

func TestDenyNeedsSeverity(t *testing.T) {
	f := newFixture(config.Default())
	f.post(0, rf(0.6))
	for i := 0; i < 5; i++ {
		f.post(0, rf(0.65)) // persists, but below DenyMinSeverity
	}
	if st := f.s.Loops["rf:npm test:1"].Stage; st != session.StageNudged {
		t.Errorf("stage = %s, want nudged", st)
	}
	f.post(0, rf(0.7))
	if st := f.s.Loops["rf:npm test:1"].Stage; st != session.StageDenying {
		t.Errorf("stage = %s, want denying", st)
	}
}

func TestDenyOnlyMatchingAction(t *testing.T) {
	f := newFixture(config.Default())
	f.post(0, rf(0.6))
	for i := 0; i < 3; i++ {
		f.post(0, rf(0.8))
	}
	if ds := f.pre("go test"); len(ds) != 0 {
		t.Errorf("unrelated action denied: %v", would(ds))
	}
	if ds := f.pre("npm test"); len(ds) != 1 || ds[0].WouldHave != event.ActionDeny {
		t.Errorf("looping action: %v", would(ds))
	}
}

func TestHaltAfterMinutes(t *testing.T) {
	th := config.Default()
	f := newFixture(th)
	f.post(0, rf(0.6))
	for i := 0; i < 3; i++ {
		f.post(0, rf(0.8))
	}
	f.clock = f.clock.Add(time.Duration(th.Policy.HaltAfterMinutes) * time.Minute)
	ds := f.pre("npm test")
	if len(ds) != 1 || ds[0].WouldHave != event.ActionHalt {
		t.Errorf("after %v min: %v", th.Policy.HaltAfterMinutes, would(ds))
	}
	if ds := f.pre("npm test"); len(ds) != 0 {
		t.Errorf("halted twice: %v", would(ds))
	}
}

func TestNudgeOnlyDetectorNeverDenies(t *testing.T) {
	f := newFixture(config.Default())
	rt := event.Signal{Detector: event.DetectorReadThrash, LoopKey: "rt:/a", Severity: 0.6}
	if ds := f.post(0, rt); len(would(ds)) != 1 {
		t.Fatalf("read thrash not nudged: %v", would(ds))
	}
	rt.Severity = 1
	for i := 0; i < 10; i++ {
		f.post(0, rt)
	}
	if st := f.s.Loops["rt:/a"].Stage; st != session.StageNudged {
		t.Errorf("read thrash escalated to %s", st)
	}
}

func TestMaxNudgesPerTurn(t *testing.T) {
	th := config.Default()
	f := newFixture(th)
	nudges := 0
	for i := 0; i < th.Policy.MaxNudgesPerTurn+2; i++ {
		s := event.Signal{Detector: event.DetectorOscillation, LoopKey: "os:/f" + string(rune('a'+i)), Severity: 0.7}
		nudges += len(f.post(0, s))
	}
	if nudges != th.Policy.MaxNudgesPerTurn {
		t.Errorf("nudges = %d, want %d", nudges, th.Policy.MaxNudgesPerTurn)
	}
	f.s.HumanPrompt(&event.Event{})
	if ds := f.post(0, event.Signal{Detector: event.DetectorOscillation, LoopKey: "os:/z", Severity: 0.7}); len(ds) != 1 {
		t.Errorf("no nudge in the next turn: %v", would(ds))
	}
}

// TestObserveModeOnly checks the core invariant of this milestone: whatever
// the input, the policy never returns an action other than "none".
func TestObserveModeOnly(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	dets := []string{event.DetectorExactRepeat, event.DetectorRepeatedFailure, event.DetectorOscillation, event.DetectorReadThrash, event.DetectorStall, event.DetectorBudget}
	fams := []string{"npm test", "go test", "ls"}
	for run := 0; run < 50; run++ {
		f := newFixture(config.Default())
		for i := 0; i < 200; i++ {
			fam := fams[rng.Intn(len(fams))]
			var ds []event.Decision
			switch rng.Intn(4) {
			case 0:
				ds = f.pre(fam)
			case 1:
				f.s.HumanPrompt(&event.Event{})
			default:
				var sigs []event.Signal
				for n := rng.Intn(3); n > 0; n-- {
					sigs = append(sigs, event.Signal{
						Detector: dets[rng.Intn(len(dets))], LoopKey: "k" + string(rune('0'+rng.Intn(4))),
						Severity: rng.Float64(), Evidence: event.Evidence{Family: fam},
					})
				}
				ds = f.post(rng.Float64()*0.5, sigs...)
			}
			for _, d := range ds {
				if d.Action != event.ActionNone {
					t.Fatalf("run %d step %d: action %q", run, i, d.Action)
				}
				if d.WouldHave == event.ActionNone || d.WouldHave == "" {
					t.Fatalf("run %d step %d: decision without a would-have action", run, i)
				}
			}
		}
	}
}
