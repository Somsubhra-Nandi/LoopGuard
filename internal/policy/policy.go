package policy

import (
	"fmt"
	"sort"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// Capabilities says how far a detector's loops may escalate (LLD §7.1).
type Capabilities struct {
	Nudge, Deny, Halt bool
}

// capabilities per detector. D4 nudges only, D5 nudges (and notifies), D6
// notifies and halts.
var capabilities = map[string]Capabilities{
	event.DetectorExactRepeat:     {Nudge: true, Deny: true, Halt: true},
	event.DetectorRepeatedFailure: {Nudge: true, Deny: true, Halt: true},
	event.DetectorOscillation:     {Nudge: true, Deny: true, Halt: true},
	event.DetectorReadThrash:      {Nudge: true},
	event.DetectorStall:           {Nudge: true},
	event.DetectorBudget:          {Halt: true},
}

// permModeManual is the permission mode in which a human approves tool
// calls; deny and halt need more evidence there.
const permModeManual = "default"

// Engine is the escalation state machine (LLD §7.1).
//
// Every Decision records in WouldHave what enforce mode does, with a
// message from the §7.3 templates. Action is that same action only when
// the session's Enforce flag is set (its effective mode is enforce);
// otherwise it is ActionNone (observe mode, replay).
//
// A subagent's stream (SessionState.Subagent) is nudged and denied but
// never halted: a halt ends the whole turn, including the parent's.
type Engine struct {
	th config.PolicyThresholds
}

// New returns a policy engine using th.Policy.
func New(th config.Thresholds) *Engine { return &Engine{th: th.Policy} }

// Post advances the state machine after a completed call c and the
// (deduplicated, severity-sorted) signals it produced. It returns a
// decision for each loop that would have been nudged.
func (e *Engine) Post(s *session.SessionState, c *event.ToolCall, sigs []event.Signal) []event.Decision {
	signaled := make(map[string]bool, len(sigs))
	for _, sig := range sigs {
		signaled[sig.LoopKey] = true
	}
	e.resetLoops(s, signaled)

	var out []event.Decision
	for _, sig := range sigs {
		if d, ok := e.onSignal(s, c, sig); ok {
			out = append(out, d)
		}
	}
	return out
}

// resetLoops returns nudged and denied loops to Observing once progress has
// been made, and nudged loops once they have been quiet for long enough.
func (e *Engine) resetLoops(s *session.SessionState, signaled map[string]bool) {
	for key, l := range s.Loops {
		if signaled[key] {
			continue
		}
		progress := s.Progress.Since(l.StageSeq)
		if l.Detector == event.DetectorRepeatedFailure {
			// A repeated-failure loop only resets on progress of its family.
			progress = s.Progress.FamilySince(l.Family, l.StageSeq)
		}
		switch l.Stage {
		case session.StageNudged:
			if progress >= e.th.ResetProgress || s.CallCount-l.LastSignalCall >= uint64(e.th.ResetQuietCalls) {
				l.Stage, l.StageSeq, l.Occurrences = session.StageObserving, s.Seq(), 0
			}
		case session.StageDenying:
			if progress >= e.th.ResetProgress {
				l.Stage, l.StageSeq, l.Occurrences = session.StageObserving, s.Seq(), 0
			}
		}
	}
}

func (e *Engine) onSignal(s *session.SessionState, c *event.ToolCall, sig event.Signal) (event.Decision, bool) {
	l := s.Loops[sig.LoopKey]
	if l == nil {
		l = newLoop(s, c, sig)
		s.Loops[sig.LoopKey] = l
	}
	l.LastSignalSeq = c.Seq()
	l.LastSignalCall = s.CallCount
	l.LastSeverity = sig.Severity
	l.Count, l.Evidence = sig.Count, sig.Evidence
	caps := capabilities[sig.Detector]

	switch l.Stage {
	case session.StageObserving:
		if !caps.Nudge || sig.Severity < e.th.NudgeThreshold {
			return event.Decision{}, false
		}
		if l.Nudges > 0 && s.CallCount-l.LastNudgeCall < uint64(e.th.NudgeCooldownCalls) {
			return event.Decision{}, false // cooldown
		}
		if s.NudgesThisTurn >= e.th.MaxNudgesPerTurn {
			return event.Decision{}, false
		}
		l.Stage, l.StageSeq, l.Occurrences = session.StageNudged, c.Seq(), 0
		l.Nudges++
		l.LastNudgeCall = s.CallCount
		s.NudgesThisTurn++
		return e.decision(s, l, sig, c.Seq(), event.ActionNudge, newMsg(s, l, c.Event.ReceivedAt, c.Event.Cwd).nudge(), fmt.Sprintf(
			"%s; severity %.2f ≥ nudge threshold %.2f", describe(sig), sig.Severity, e.th.NudgeThreshold)), true

	case session.StageNudged:
		l.Occurrences++
		need := e.th.DenyAfterNudgeOccurrences + e.extra(s)
		if caps.Deny && l.Occurrences >= need && sig.Severity >= e.th.DenyMinSeverity {
			// Armed: the deny itself is decided on the next matching pre event.
			l.Stage, l.StageSeq, l.Occurrences = session.StageDenying, c.Seq(), 0
			l.Denies = 0
		}
	}
	return event.Decision{}, false
}

// Pre checks an action about to run (built from a pre event) against loops
// that are being denied. A matching action would have been denied, or, once
// the loop kept going after the first deny or has lasted too long, the
// session would have been halted.
//
// In enforce mode a halted loop keeps halting matching actions until the
// next human prompt resets the loops, in case the agent goes on anyway (a
// halt reply that arrived too late is dropped). Observe mode records the
// halt once.
func (e *Engine) Pre(s *session.SessionState, c *event.ToolCall) []event.Decision {
	var out []event.Decision
	now := c.Event.ReceivedAt
	for _, l := range sortedLoops(s) {
		if !matches(l, c) {
			continue
		}
		m := newMsg(s, l, now, c.Event.Cwd)
		sig := event.Signal{Detector: l.Detector, LoopKey: l.Key, Severity: l.LastSeverity}
		if l.Stage == session.StageHalted {
			if s.Enforce {
				out = append(out, e.decision(s, l, sig, c.Seq(), event.ActionHalt, m.halt(), "the action repeats a halted loop"))
			}
			continue
		}
		if l.Stage != session.StageDenying {
			continue
		}
		// A subagent's loop is denied but never halted: a halt would end the
		// parent's turn too.
		canHalt := capabilities[l.Detector].Halt && !s.Subagent
		minutes := now.Sub(l.StartedAt).Minutes()
		switch {
		// The first deny is Denies == 1; each matching attempt after it is
		// one more occurrence.
		case canHalt && l.Denies >= e.th.HaltAfterDenyOccurrences+e.extra(s):
			l.Stage = session.StageHalted
			out = append(out, e.decision(s, l, sig, c.Seq(), event.ActionHalt, m.halt(), m.haltReason(false, 0, 0)))
		case canHalt && minutes >= e.th.HaltAfterMinutes:
			l.Stage = session.StageHalted
			out = append(out, e.decision(s, l, sig, c.Seq(), event.ActionHalt, m.halt(), m.haltReason(true, minutes, e.th.HaltAfterMinutes)))
		default:
			l.Denies++
			out = append(out, e.decision(s, l, sig, c.Seq(), event.ActionDeny, m.deny(), fmt.Sprintf(
				"action repeats a loop that kept going after a nudge (deny #%d)", l.Denies)))
		}
	}
	return out
}

// extra raises occurrence thresholds when a human approves each action.
func (e *Engine) extra(s *session.SessionState) int {
	if s.PermMode == permModeManual {
		return e.th.ManualModeExtraOccurrences
	}
	return 0
}

func (e *Engine) decision(s *session.SessionState, l *session.LoopState, sig event.Signal, seq uint64, would event.Action, message, reason string) event.Decision {
	action := event.ActionNone // observe mode
	if s.Enforce {
		action = would
	}
	return event.Decision{
		Action:    action,
		LoopKey:   l.Key,
		Message:   message,
		WouldHave: would,
		Reason:    reason,
		Detector:  sig.Detector,
		Severity:  sig.Severity,
		Seq:       seq,
	}
}

func newLoop(s *session.SessionState, c *event.ToolCall, sig event.Signal) *session.LoopState {
	l := &session.LoopState{
		Key:       sig.LoopKey,
		Detector:  sig.Detector,
		Stage:     session.StageObserving,
		StartedAt: c.Event.ReceivedAt.Add(-sig.Evidence.Elapsed),
		FirstSeq:  sig.Since,
		StageSeq:  c.Seq(),
	}
	switch sig.Detector {
	case event.DetectorExactRepeat:
		l.ActionFP = sig.Evidence.ActionFP
	case event.DetectorRepeatedFailure:
		l.Family = sig.Evidence.Family
		l.Anchor = sig.Evidence.Anchor
	case event.DetectorOscillation:
		l.Path = sig.Evidence.Path
	}
	return l
}

// matches reports whether the action c belongs to loop l: the same action
// (D1), the same family (D2), or a write to the same file (D3).
func matches(l *session.LoopState, c *event.ToolCall) bool {
	switch {
	case l.ActionFP != 0:
		return c.ActionFP == l.ActionFP
	case l.Family != "":
		return c.Family == l.Family
	case l.Path != "":
		switch c.Event.ToolName {
		case event.ToolEdit, event.ToolWrite, event.ToolPatch:
			return c.FilePath == l.Path
		}
	}
	return false
}

func sortedLoops(s *session.SessionState) []*session.LoopState {
	out := make([]*session.LoopState, 0, len(s.Loops))
	for _, l := range s.Loops {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// describe summarizes a signal in one clause.
func describe(sig event.Signal) string {
	switch sig.Detector {
	case event.DetectorRepeatedFailure:
		return fmt.Sprintf("%s: `%s` failed %d times with the same error", sig.Detector, sig.Evidence.Family, sig.Count)
	case event.DetectorExactRepeat:
		return fmt.Sprintf("%s: the same action ran %d times with the same result", sig.Detector, sig.Count)
	case event.DetectorOscillation:
		return fmt.Sprintf("%s: %s went back to a previous version %d times", sig.Detector, sig.Evidence.Path, sig.Count)
	}
	return fmt.Sprintf("%s: count %d", sig.Detector, sig.Count)
}
