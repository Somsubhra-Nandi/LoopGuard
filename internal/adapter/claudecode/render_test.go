package claudecode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

func TestRender(t *testing.T) {
	ev := func(p event.Phase) *event.Event { return &event.Event{Phase: p} }
	msg := "npm test has failed 4 times with the same error: <Expected 110> & more."
	cases := []struct {
		name  string
		ev    *event.Event
		d     event.Decision
		want  string
		empty bool
	}{
		{name: "none", ev: ev(event.PhasePost), d: event.Decision{Action: event.ActionNone, WouldHave: event.ActionHalt, Message: msg}, empty: true},
		{name: "zero decision", ev: ev(event.PhasePre), d: event.Decision{}, empty: true},
		{
			name: "nudge on post", ev: ev(event.PhasePost), d: event.Decision{Action: event.ActionNudge, Message: msg},
			want: `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"` + msg + `"}}`,
		},
		{
			name: "nudge on failure", ev: ev(event.PhasePostFailure), d: event.Decision{Action: event.ActionNudge, Message: msg},
			want: `{"hookSpecificOutput":{"hookEventName":"PostToolUseFailure","additionalContext":"` + msg + `"}}`,
		},
		{
			name: "deny on pre", ev: ev(event.PhasePre), d: event.Decision{Action: event.ActionDeny, Message: msg},
			want: `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"` + msg + `"}}`,
		},
		{
			name: "halt on pre", ev: ev(event.PhasePre), d: event.Decision{Action: event.ActionHalt, Message: "Stuck.\nDetails", Reason: "stuck on a failing test"},
			want: `{"continue":false,"stopReason":"Stuck.\nDetails","systemMessage":"LoopGuard halted this session: stuck on a failing test"}`,
		},
		{
			name: "halt on batch end without reason", ev: ev(event.PhaseBatchEnd), d: event.Decision{Action: event.ActionHalt, Message: "Stuck on npm test.\nMore"},
			want: `{"continue":false,"stopReason":"Stuck on npm test.\nMore","systemMessage":"LoopGuard halted this session: Stuck on npm test."}`,
		},
		{
			name: "halt with notify", ev: ev(event.PhasePost), d: event.Decision{Action: event.ActionHalt, Message: "halted\x07now", Reason: "r", Notify: true},
			want: `{"continue":false,"stopReason":"halted\u0007now","systemMessage":"LoopGuard halted this session: r","terminalSequence":"\u001b]777;notify;LoopGuard;halted now\u0007"}`,
		},
		// Actions that don't apply to the phase render as none.
		{name: "deny on post", ev: ev(event.PhasePost), d: event.Decision{Action: event.ActionDeny, Message: msg}, empty: true},
		{name: "nudge on pre", ev: ev(event.PhasePre), d: event.Decision{Action: event.ActionNudge, Message: msg}, empty: true},
		{name: "halt on stop", ev: ev(event.PhaseStop), d: event.Decision{Action: event.ActionHalt, Message: msg}, empty: true},
		{name: "halt on prompt", ev: ev(event.PhasePrompt), d: event.Decision{Action: event.ActionHalt, Message: msg}, empty: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := Render(tc.ev, tc.d)
			if code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
			if tc.empty {
				if out != nil {
					t.Errorf("stdout = %s, want none", out)
				}
				return
			}
			if !json.Valid(out) {
				t.Fatalf("stdout is not JSON: %s", out)
			}
			if string(out) != tc.want {
				t.Errorf("stdout =\n %s\nwant\n %s", out, tc.want)
			}
		})
	}
}

func TestRenderNeverAllowsAndClips(t *testing.T) {
	huge := strings.Repeat("x", 20000)
	for _, a := range []event.Action{event.ActionNudge, event.ActionDeny, event.ActionHalt} {
		for _, p := range []event.Phase{event.PhasePre, event.PhasePost, event.PhasePostFailure, event.PhaseBatchEnd} {
			out, _ := Render(&event.Event{Phase: p}, event.Decision{Action: a, Message: huge})
			if strings.Contains(string(out), `"allow"`) {
				t.Errorf("%s on %s emitted allow", a, p)
			}
			if len(out) > 10000 {
				t.Errorf("%s on %s: %d bytes of output, want under Claude Code's 10,000-character cap", a, p, len(out))
			}
		}
	}
}
