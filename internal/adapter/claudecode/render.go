package claudecode

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// Output limits. Claude Code caps additionalContext at 10,000 characters;
// templates aim for far less (LLD §3.2), this is only a backstop.
const (
	maxContext     = 9000
	maxShort       = 160 // systemMessage summary of a halt
	maxTerminalMsg = 200
)

type hookSpecific struct {
	HookEventName            string `json:"hookEventName"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

type output struct {
	Continue           *bool         `json:"continue,omitempty"`
	StopReason         string        `json:"stopReason,omitempty"`
	SystemMessage      string        `json:"systemMessage,omitempty"`
	HookSpecificOutput *hookSpecific `json:"hookSpecificOutput,omitempty"`
	TerminalSequence   string        `json:"terminalSequence,omitempty"`
}

// Render turns a decision into Claude Code hook output (LLD §3.2):
//
//   - none: no output.
//   - nudge (post / post_failure): additionalContext next to the tool result.
//   - deny (pre): permissionDecision "deny" with the message as the reason.
//   - halt (pre / post / post_failure / batch_end): continue:false with a
//     stopReason, and a systemMessage for the user; on a pre, the action
//     is also denied.
//   - Notify adds a terminal notification sequence.
//
// An action that doesn't apply to the event's phase (a deny on a post, a
// nudge on a pre) renders as none, so a policy bug can't produce output
// Claude Code would misread. The exit code is always 0: exit codes have
// per-event side effects, JSON is explicit. LoopGuard never emits "allow".
func Render(ev *event.Event, d event.Decision) (stdout []byte, exitCode int) {
	var o output
	switch {
	case d.Action == event.ActionNudge && (ev.Phase == event.PhasePost || ev.Phase == event.PhasePostFailure):
		name := "PostToolUse"
		if ev.Phase == event.PhasePostFailure {
			name = "PostToolUseFailure"
		}
		o.HookSpecificOutput = &hookSpecific{HookEventName: name, AdditionalContext: clip(d.Message, maxContext)}
	case d.Action == event.ActionDeny && ev.Phase == event.PhasePre:
		o.HookSpecificOutput = &hookSpecific{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: clip(d.Message, maxContext),
		}
	case d.Action == event.ActionHalt && haltPhase(ev.Phase):
		f := false
		o.Continue = &f
		o.StopReason = clip(d.Message, maxContext)
		o.SystemMessage = "LoopGuard halted this session: " + short(d)
		if ev.Phase == event.PhasePre {
			// A halt is decided on the looping action itself: deny it too, so
			// it doesn't run while the turn stops. The full text is already in
			// stopReason; the reason only needs the summary.
			o.HookSpecificOutput = &hookSpecific{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "deny",
				PermissionDecisionReason: "LoopGuard halted this session: " + short(d),
			}
		}
	}
	if d.Notify && d.Message != "" {
		o.TerminalSequence = "\x1b]777;notify;LoopGuard;" + terminalSafe(d.Message) + "\a"
	}
	if o == (output{}) {
		return nil, 0
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(o); err != nil {
		return nil, 0
	}
	return bytes.TrimRight(b.Bytes(), "\n"), 0
}

func haltPhase(p event.Phase) bool {
	switch p {
	case event.PhasePre, event.PhasePost, event.PhasePostFailure, event.PhaseBatchEnd:
		return true
	}
	return false
}

// short summarizes a halt in one line: the reason, or else the message's
// first line.
func short(d event.Decision) string {
	s := d.Reason
	if s == "" {
		s, _, _ = strings.Cut(d.Message, "\n")
	}
	return clip(strings.TrimSpace(s), maxShort)
}

// terminalSafe strips control characters, which would end the escape
// sequence early, and clips the message.
func terminalSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
	return clip(strings.TrimSpace(s), maxTerminalMsg)
}
