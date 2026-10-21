package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// driver runs real-shaped events through the whole pipeline for one
// session and collects the would-have actions.
type driver struct {
	t     *testing.T
	p     *Pipeline
	s     *session.SessionState
	clock time.Time
	n     int
	perm  string
	got   []string // "nudge@post", "deny@pre", "halt@pre"
}

func newDriver(t *testing.T, enforce bool) *driver {
	p := New(config.Default())
	d := &driver{t: t, p: p, s: p.NewSession("claude-code:drv", event.AgentClaudeCode),
		clock: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), perm: "auto"}
	if enforce {
		d.s.Allow = event.ActionHalt
	}
	d.handle(&event.Event{Phase: event.PhasePrompt, Prompt: "fix it"})
	return d
}

func (d *driver) handle(ev *event.Event) []event.Decision {
	ev.Agent, ev.SessionID, ev.Cwd, ev.PermMode = event.AgentClaudeCode, "drv", `C:\src\shop`, d.perm
	if ev.ReceivedAt.IsZero() {
		d.clock = d.clock.Add(time.Second)
		ev.ReceivedAt = d.clock
	}
	res := d.p.Handle(d.s, ev, 0)
	for _, dec := range res.Decisions {
		if dec.WouldHave != "" && dec.WouldHave != event.ActionNone {
			d.got = append(d.got, string(dec.WouldHave)+"@"+string(ev.Phase))
		}
	}
	return res.Decisions
}

func js(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// run runs a bash command: its pre, and (unless enforce mode blocked it)
// its post. It reports the pre's decision ("" for none). runFor is the
// tool's own run time; wait is extra time before the post (a permission
// prompt).
func (d *driver) run(cmd, failure string, runFor, wait time.Duration) string {
	d.n++
	id := fmt.Sprintf("t%d", d.n)
	in := js(map[string]string{"command": cmd})
	pre := d.handle(&event.Event{Phase: event.PhasePre, ToolName: event.ToolBash, ToolUseID: id, Input: in})
	kind := ""
	for _, dec := range pre {
		kind = string(dec.WouldHave)
	}
	if d.s.Allow == event.ActionHalt && (kind == "deny" || kind == "halt") {
		return kind
	}
	d.clock = d.clock.Add(runFor + wait)
	post := &event.Event{Phase: event.PhasePost, ToolName: event.ToolBash, ToolUseID: id, Input: in, RunTime: runFor, ReceivedAt: d.clock}
	if failure != "" {
		post.Phase, post.IsError, post.Output = event.PhasePostFailure, true, js(failure)
	} else {
		post.Output = js(map[string]any{"stdout": "Tests: 8 passed, 8 total"})
	}
	d.handle(post)
	return kind
}

func (d *driver) edit(path string) {
	d.n++
	in := js(map[string]string{"file_path": path, "old_string": fmt.Sprint("a", d.n), "new_string": fmt.Sprint("b", d.n)})
	id := fmt.Sprintf("t%d", d.n)
	d.handle(&event.Event{Phase: event.PhasePre, ToolName: event.ToolEdit, ToolUseID: id, Input: in})
	d.handle(&event.Event{Phase: event.PhasePost, ToolName: event.ToolEdit, ToolUseID: id, Input: in, Output: js("ok")})
}

const (
	failTax   = "FAIL src/cart.test.ts\n  ● cart › total › adds tax\n    Expected: 110\n    Received: 100\n      at Object.<anonymous> (src/cart.test.ts:13:24)"
	failOther = "FAIL src/cart.test.ts\n  ● cart › total › rounds\n    Expected: 3.5\n    Received: 3\n      at Object.<anonymous> (src/cart.test.ts:20:11)"
)

// denying drives a session to the point where `npm test` is blocked: 7
// identical failures with edits to src/cart.ts between them, then a deny.
func denying(t *testing.T) *driver {
	d := newDriver(t, true)
	for i := 0; i < 7; i++ {
		d.edit("src/cart.ts")
		d.run("npm test", failTax, time.Second, 0)
	}
	if k := d.run("npm test", failTax, time.Second, 0); k != "deny" {
		t.Fatalf("8th attempt: %q, want deny (decisions %v)", k, d.got)
	}
	return d
}

func TestRetryAfterRealChange(t *testing.T) {
	t.Run("same error again goes back to denying", func(t *testing.T) {
		d := denying(t)
		d.edit("src/tax.ts") // a file not edited during the loop
		if k := d.run("npm test", failTax, time.Second, 0); k != "" {
			t.Fatalf("the try after a real change: %q, want it to run", k)
		}
		// It failed the same way: denied again, and that try counted toward
		// the halt (deny #1, the try, then the halt).
		if k := d.run("npm test", failTax, time.Second, 0); k != "halt" {
			t.Errorf("after the failed try: %q, want halt (%v)", k, d.got)
		}
	})
	t.Run("only one try per change", func(t *testing.T) {
		d := denying(t)
		d.edit("src/tax.ts")
		d.run("npm test", failTax, time.Second, 0)
		d.edit("src/tax.ts") // edited during the loop by now
		if k := d.run("npm test", failTax, time.Second, 0); k == "" {
			t.Error("a second try without a new change ran")
		}
	})
	t.Run("an edit of a loop file is not a real change", func(t *testing.T) {
		d := denying(t)
		d.edit("src/cart.ts")
		if k := d.run("npm test", failTax, time.Second, 0); k != "deny" {
			t.Errorf("after editing src/cart.ts again: %q, want deny", k)
		}
	})
	t.Run("a changed error resets the loop", func(t *testing.T) {
		d := denying(t)
		d.edit("src/tax.ts")
		d.run("npm test", failOther, time.Second, 0)
		if k := d.run("npm test", failOther, time.Second, 0); k != "" {
			t.Errorf("after the error changed: %q, want no block", k)
		}
	})
	t.Run("success resets the loop", func(t *testing.T) {
		d := denying(t)
		d.edit("src/tax.ts")
		d.run("npm test", "", time.Second, 0)
		if k := d.run("npm test", failTax, time.Second, 0); k != "" {
			t.Errorf("after the command passed: %q, want no block", k)
		}
	})
	t.Run("a different command identity is not blocked", func(t *testing.T) {
		d := denying(t)
		for _, cmd := range []string{"npm test -- src/tax.test.ts", "npx jest src/cart.test.ts", "cat README.md"} {
			if k := d.run(cmd, "", time.Second, 0); k != "" {
				t.Errorf("%s: %q, want no block", cmd, k)
			}
		}
		for _, cmd := range []string{"npm run test", "npm  test --silent"} {
			if k := d.run(cmd, failTax, time.Second, 0); k == "" {
				t.Errorf("%s (the same command reworded) ran", cmd)
			}
		}
	})
}

// TestSlowRunsNudgeDenyHaltInOrder: runs of 10 min each. The loop is past
// halt_after_minutes long before it is denied, but the halt still comes
// only after a deny, with the limit raised to 3× the median run time.
func TestSlowRunsNudgeDenyHaltInOrder(t *testing.T) {
	d := newDriver(t, false) // observe: every attempt runs, so all 8 fail
	for i := 0; i < 9; i++ {
		d.edit("src/cart.ts")
		d.run("npm test", failTax, 10*time.Minute, 0)
	}
	got := strings.Join(d.got, " ")
	if got != "nudge@post_failure deny@pre halt@pre" {
		t.Errorf("decisions = %s, want nudge, deny, halt in that order", got)
	}
	if d.s.MedianRunTime("npm test") != 10*time.Minute {
		t.Errorf("median run time %v", d.s.MedianRunTime("npm test"))
	}
}

// TestPermissionWaitDoesNotCount: in permission mode "default", hours
// spent on permission prompts don't make a fast loop halt on time; only
// the counts (raised by one in this mode) do.
func TestPermissionWaitDoesNotCount(t *testing.T) {
	d := newDriver(t, true)
	d.perm = "default"
	for i := 0; i < 8; i++ {
		d.edit("src/cart.ts")
		d.run("npm test", failTax, 2*time.Second, 30*time.Minute)
	}
	if d.s.PermissionWait < 3*time.Hour {
		t.Fatalf("permission wait %v", d.s.PermissionWait)
	}
	var kinds []string
	for i := 0; i < 4; i++ {
		kinds = append(kinds, d.run("npm test", failTax, 2*time.Second, 30*time.Minute))
	}
	if strings.Join(kinds, " ") != "deny deny deny halt" {
		t.Errorf("attempts after arming: %v, want 3 denies then the count halt", kinds)
	}
}

func TestHaltNoteOnNextPrompt(t *testing.T) {
	d := denying(t)
	d.run("npm test", failTax, time.Second, 0)
	if k := d.run("npm test", failTax, time.Second, 0); k != "halt" {
		t.Fatalf("want a halt, got %q", k)
	}
	ds := d.handle(&event.Event{Phase: event.PhasePrompt, Prompt: "try another way"})
	if len(ds) != 1 || ds[0].Action != event.ActionNudge || ds[0].WouldHave != event.ActionNone || !strings.Contains(ds[0].Message, "previous turn") {
		t.Fatalf("first prompt after the halt: %+v", ds)
	}
	if ds := d.handle(&event.Event{Phase: event.PhasePrompt, Prompt: "again"}); len(ds) != 0 {
		t.Errorf("the note came twice: %+v", ds)
	}
}
