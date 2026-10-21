package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

func actions(ds []event.Decision) []event.Action {
	var out []event.Action
	for _, d := range ds {
		out = append(out, d.Action)
	}
	return out
}

// escalate drives one rf loop to Denying: a nudge, then three more
// occurrences at deny severity.
func (f *fixture) escalate(t *testing.T) {
	t.Helper()
	if ds := f.post(0, rf(0.6)); len(ds) != 1 || ds[0].WouldHave != event.ActionNudge {
		t.Fatalf("no nudge: %+v", ds)
	}
	for i := 0; i < 3; i++ {
		f.post(0, rf(0.8))
	}
	if l := f.s.Loops["rf:npm test:1"]; l.Stage != session.StageDenying {
		t.Fatalf("stage %s, want denying", l.Stage)
	}
}

func TestEnforceMode(t *testing.T) {
	f := newFixture(config.Default())
	f.s.Allow = event.ActionHalt
	if ds := f.post(0, rf(0.6)); len(ds) != 1 || ds[0].Action != event.ActionNudge || !strings.HasPrefix(ds[0].Message, "LoopGuard observation:") {
		t.Fatalf("nudge: %+v", ds)
	}
	for i := 0; i < 3; i++ {
		f.post(0, rf(0.8))
	}
	if ds := f.pre("cat"); len(ds) != 0 {
		t.Errorf("an unrelated action was denied: %+v", ds)
	}
	for i, want := range []event.Action{event.ActionDeny, event.ActionDeny, event.ActionHalt, event.ActionHalt} {
		ds := f.pre("npm test")
		if len(ds) != 1 || ds[0].Action != want || ds[0].WouldHave != want || ds[0].Message == "" {
			t.Fatalf("attempt %d: %+v, want %s", i+1, ds, want)
		}
		if want == event.ActionDeny && !strings.HasPrefix(ds[0].Message, "LoopGuard blocked this command") {
			t.Errorf("deny message %q", ds[0].Message)
		}
		if want == event.ActionHalt && (!strings.HasPrefix(ds[0].Message, HaltPrefix) || !strings.HasSuffix(ds[0].Message, "loopguard pause --session t)")) {
			t.Errorf("halt message %q", ds[0].Message)
		}
	}
	// A human prompt resets the loops: the action runs again.
	f.s.HumanPrompt(&event.Event{})
	if ds := f.pre("npm test"); len(ds) != 0 {
		t.Errorf("after a human prompt: %+v", ds)
	}
}

func TestObserveModeRecordsHaltOnce(t *testing.T) {
	f := newFixture(config.Default())
	f.escalate(t)
	var got []event.Action
	for i := 0; i < 5; i++ {
		ds := f.pre("npm test")
		got = append(got, would(ds)...)
		for _, d := range ds {
			if d.Action != event.ActionNone {
				t.Fatalf("observe mode returned %s", d.Action)
			}
		}
	}
	want := []event.Action{event.ActionDeny, event.ActionDeny, event.ActionHalt}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("would have %v, want %v", got, want)
	}
}

func TestSubagentNeverHalts(t *testing.T) {
	th := config.Default()
	th.Policy.HaltAfterMinutes = 1
	f := newFixture(th)
	f.s.Allow, f.s.Subagent = event.ActionHalt, true
	f.escalate(t)
	for i := 0; i < 10; i++ {
		f.clock = f.clock.Add(time.Minute) // past halt_after_minutes too
		ds := f.pre("npm test")
		if len(ds) != 1 || ds[0].Action != event.ActionDeny {
			t.Fatalf("subagent attempt %d: %v, want deny", i+1, actions(ds))
		}
	}
}

func TestManualModeRaisesThresholds(t *testing.T) {
	f := newFixture(config.Default())
	f.s.PermMode = "default"
	f.post(0, rf(0.6))
	for i := 0; i < 3; i++ {
		f.post(0, rf(0.8))
	}
	if f.s.Loops["rf:npm test:1"].Stage != session.StageNudged {
		t.Fatal("denied after 3 occurrences in default permission mode")
	}
	f.post(0, rf(0.8))
	var got []event.Action
	for i := 0; i < 4; i++ {
		got = append(got, would(f.pre("npm test"))...)
	}
	if len(got) != 4 || got[2] != event.ActionDeny || got[3] != event.ActionHalt {
		t.Errorf("default mode: %v, want deny×3 then halt", got)
	}
}

// loopState is a repeated-failure loop with realistic evidence.
func loopState(lines, files []string) *session.LoopState {
	return &session.LoopState{
		Key: "rf:npm test:1", Detector: event.DetectorRepeatedFailure, Family: "npm test", Count: 5, Denies: 2,
		StartedAt: t0, Evidence: event.Evidence{Family: "npm test", ErrorLines: lines, Files: files},
	}
}

func TestTemplates(t *testing.T) {
	s := session.New("claude-code:1a2b3c4d-5e6f/agent-1", event.AgentClaudeCode, config.Default())
	secret := "sk-ant-api03-" + strings.Repeat("A", 40)
	long := strings.Repeat("x", 5000)
	var files []string
	for i := 0; i < 40; i++ {
		files = append(files, "src/"+strings.Repeat("d", 30)+"/file.ts")
	}
	loops := []*session.LoopState{
		loopState([]string{"Error: token " + secret + " rejected", "Expected: 110", "Received: 100"}, []string{"src/cart.ts"}),
		loopState([]string{long, long, long}, files),
		loopState(nil, nil),
		{Key: "er:1", Detector: event.DetectorExactRepeat, ActionFP: 1, Count: 3, StartedAt: t0,
			Evidence: event.Evidence{Family: "curl", Commands: []string{"curl -H 'Authorization: Bearer " + secret + "' x"}}},
		{Key: "os:a.go", Detector: event.DetectorOscillation, Path: "a.go", Count: 2, StartedAt: t0},
		{Key: "st:1", Detector: event.DetectorStall, Count: 25, StartedAt: t0},
	}
	banned := []string{"SYSTEM", "System:", "you must", "You must", "MUST", "IMPORTANT", "ignore previous", "<system"}
	for i, l := range loops {
		m := newMsg(s, l, t0.Add(4*time.Minute), "")
		halt := haltMessage(m.haltReason(false, 0, 0), m.session)
		for kind, text := range map[string]string{"nudge": m.nudge(), "deny": m.deny(nil), "halt": halt,
			"reason": m.haltReason(false, 0, 0), "time halt": haltMessage(m.haltReason(true, 34, 30), m.session)} {
			if n := len([]rune(text)); n > MaxMessage || n == 0 {
				t.Errorf("loop %d %s: %d characters", i, kind, n)
			}
			for _, b := range banned {
				if strings.Contains(text, b) {
					t.Errorf("loop %d %s contains %q: %s", i, kind, b, text)
				}
			}
			if strings.Contains(text, secret) {
				t.Errorf("loop %d %s leaks a secret: %s", i, kind, text)
			}
		}
		if !strings.HasSuffix(halt, "(To turn LoopGuard off for this session: loopguard pause --session 1a2b3c4d)") {
			t.Errorf("loop %d halt doesn't end with the pause command: %s", i, halt)
		}
		// A halt ends a turn, not the session: the only "session" is in the
		// optional pause command.
		scope := strings.TrimSuffix(halt, "(To turn LoopGuard off for this session: loopguard pause --session 1a2b3c4d)")
		if strings.Contains(strings.ToLower(scope), "session") || !strings.HasPrefix(halt, "LoopGuard stopped this turn: ") ||
			!strings.Contains(halt, "Your next message continues normally.") {
			t.Errorf("loop %d halt describes its scope wrongly: %s", i, halt)
		}
	}

	m := newMsg(s, loops[0], t0.Add(4*time.Minute), "")
	want := "LoopGuard observation: `npm test` has failed 5 times in a row with the same error (first failure 4 min ago).\n" +
		"Output excerpt:\n" +
		"    Error: token <REDACTED:anthropic_key> rejected\n" +
		"    Expected: 110\n" +
		"    Received: 100\n" +
		"Between attempts, these files were edited: src/cart.ts. The error did not change after any of these edits.\n" +
		"Repeating similar edits is unlikely to change the result. Options that often help: re-reading the full error and the code it points to, checking assumptions (versions, config, env), trying a different approach, or asking the user."
	if got := m.nudge(); got != want {
		t.Errorf("nudge:\n%s\nwant:\n%s", got, want)
	}
	if got := haltMessage(m.haltReason(false, 0, 0), m.session); got != "LoopGuard stopped this turn: `npm test` failed 5 times with the same error "+
		"and the loop continued after 2 blocks. Your next message continues normally. "+
		"(To turn LoopGuard off for this session: loopguard pause --session 1a2b3c4d)" {
		t.Errorf("halt: %s", got)
	}
	cmd := &event.ToolCall{Command: "go test ./pkg/a"}
	if got := m.deny(cmd); !strings.HasPrefix(got, "LoopGuard blocked this command: it is the same `go test ./pkg/a` run") {
		t.Errorf("deny names %q, want the command", got)
	}
	if got := newMsg(s, loops[2], t0.Add(20*time.Second), "").nudge(); !strings.Contains(got, "in a row with the same error (first failure less than a minute ago).\nNo files were edited") {
		t.Errorf("nudge without evidence: %s", got)
	}
	if got := newMsg(s, loops[1], t0, "").nudge(); !strings.Contains(got, "and 34 more") {
		t.Errorf("long file list not cut: %s", got)
	}
}

func TestOutcomes(t *testing.T) {
	sig := func(key string) event.Signal { return event.Signal{LoopKey: key} }
	nudge := event.Decision{LoopKey: "rf:npm test:1", Detector: event.DetectorRepeatedFailure, WouldHave: event.ActionNudge, Seq: 1}
	anchor := &event.ErrorSignature{Exact: 7, Loc: 1, Sim: 0}
	other := &event.ErrorSignature{Exact: 8, Loc: 2, Sim: ^uint64(0)}

	setup := func() (*session.SessionState, *Outcomes) {
		s := session.New("cc:o", event.AgentClaudeCode, config.Default())
		s.Loops[nudge.LoopKey] = &session.LoopState{Key: nudge.LoopKey, Detector: nudge.Detector, Family: "npm test", Anchor: anchor,
			Identities: map[string]bool{"npm test": true}}
		o := NewOutcomes(4)
		if r := o.Add(s, []event.Decision{nudge}); len(r) != 0 || o.Pending() != 1 {
			t.Fatalf("Add: %v", r)
		}
		return s, o
	}
	// call's family is also its identity, unless given as "family|identity".
	call := func(s *session.SessionState, family string, sig *event.ErrorSignature) *event.ToolCall {
		identity := family
		if f, id, ok := strings.Cut(family, "|"); ok {
			family, identity = f, id
		}
		c := &event.ToolCall{Event: &event.Event{ToolName: event.ToolBash}, Family: family, Identity: identity, ErrorSig: sig, TodosDone: -1, FailingTests: -1}
		s.Assign(c.Event)
		s.Record(c)
		return c
	}
	run := func(s *session.SessionState, o *Outcomes, n int, family string, es *event.ErrorSignature, signal bool) []Resolved {
		var out []Resolved
		for i := 0; i < n; i++ {
			var sigs []event.Signal
			if signal {
				sigs = []event.Signal{sig(nudge.LoopKey)}
			}
			out = append(out, o.Observe(s, call(s, family, es), sigs, nil)...)
		}
		return out
	}
	one := func(name string, r []Resolved, want string) {
		t.Helper()
		if len(r) != 1 || r[0].Outcome != want || r[0].Decision.Seq != nudge.Seq {
			t.Errorf("%s: %+v, want %s", name, r, want)
		}
	}

	s, o := setup()
	one("keeps failing the same way", run(s, o, 10, "npm test", anchor, true), OutcomeContinued)

	s, o = setup()
	r := run(s, o, 3, "npm test", anchor, true)
	r = append(r, run(s, o, 1, "npm test", nil, false)...) // the family passed
	r = append(r, run(s, o, 6, "npm test", anchor, true)...)
	one("family succeeded", r, OutcomeRecovered)

	s, o = setup()
	r = run(s, o, 2, "npm test", other, false) // a different error
	r = append(r, run(s, o, 8, "npm test", anchor, true)...)
	one("error changed", r, OutcomeRecovered)

	s, o = setup()
	r = run(s, o, 4, "npm test", anchor, true)
	r = append(r, run(s, o, 6, "cat", nil, false)...) // quiet for the last 6 calls
	one("signal stopped", r, OutcomeMovedOn)

	s, o = setup()
	r = run(s, o, 3, "npm test", anchor, true)
	r = append(r, run(s, o, 1, "npm test|npm test -- cart", nil, false)...) // another command of the family passed
	r = append(r, run(s, o, 6, "cat", nil, false)...)
	one("another command of the family passed", r, OutcomeMovedOn)

	s, o = setup()
	r = run(s, o, 3, "npm test", anchor, true)
	r = append(r, o.Observe(s, nil, nil, []event.Decision{{LoopKey: nudge.LoopKey, WouldHave: event.ActionHalt}})...)
	one("halted", r, OutcomeHalted)

	s, o = setup()
	run(s, o, 3, "npm test", anchor, true)
	one("ended first", o.End(), OutcomeUnknown)

	_, o = setup()
	if r := o.Add(s, []event.Decision{{LoopKey: "x", WouldHave: event.ActionHalt}}); len(r) != 1 || r[0].Outcome != OutcomeHalted {
		t.Errorf("a halt resolves at once: %+v", r)
	}
}
