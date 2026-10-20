package policy

import (
	"fmt"
	"sort"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
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
	th      config.PolicyThresholds
	slow    *normalize.FamilyMatcher // never halt on time
	maxDist int
}

// New returns a policy engine using th.Policy, th.SlowFamilies, and the
// simhash distance (for "the same error").
func New(th config.Thresholds) *Engine {
	return &Engine{th: th.Policy, slow: normalize.NewFamilyMatcher(th.SlowFamilies), maxDist: th.Detectors.SimHashMaxDistance}
}

// HaltOverNote is the context given with the first human prompt after a
// halt, so the agent doesn't take the earlier stop for a standing order.
const HaltOverNote = "LoopGuard's earlier stop applied only to the previous turn; commands are not blocked now."

// Prompt is called after a human prompt has reset the loops. After a halt
// that reached the agent, it returns the note that the stop is over (an
// informational nudge, recorded nowhere: WouldHave is none).
func (e *Engine) Prompt(s *session.SessionState) []event.Decision {
	if !s.HaltNote {
		return nil
	}
	s.HaltNote = false
	if !s.Enforce {
		return nil
	}
	return []event.Decision{{Action: event.ActionNudge, WouldHave: event.ActionNone, Message: HaltOverNote,
		Reason: "first prompt after a halt", Seq: s.Seq()}}
}

// Post advances the state machine after a completed call c and the
// (deduplicated, severity-sorted) signals it produced. It returns a
// decision for each loop that would have been nudged.
func (e *Engine) Post(s *session.SessionState, c *event.ToolCall, sigs []event.Signal) []event.Decision {
	signaled := make(map[string]bool, len(sigs))
	for _, sig := range sigs {
		signaled[sig.LoopKey] = true
	}
	e.endRetries(s, c)
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
			// A repeated-failure loop only resets on progress of its family,
			// or, when it has command identities, of those commands
			// (identityProgress): another command of the family passing
			// (`npm test -- other.test.ts`) must not unblock the looping one.
			progress = s.Progress.FamilySince(l.Family, l.StageSeq)
			if len(l.Identities) > 0 {
				progress = e.identityProgress(s, l)
			}
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

// endRetries settles the one more try a denied loop allowed (Pre): if the
// run succeeded or failed with another error, the loop is over and resets;
// if it failed the same way, the loop goes back to denying, and the try
// counts toward the halt like a denied attempt.
func (e *Engine) endRetries(s *session.SessionState, c *event.ToolCall) {
	for _, l := range s.Loops {
		if !l.RetryPending || !matches(l, c) {
			continue
		}
		l.RetryPending = false
		if c.Failed() && (l.Anchor == nil || normalize.SameError(l.Anchor, c.ErrorSig, e.maxDist)) {
			l.Denies++
			continue
		}
		l.Stage, l.StageSeq, l.Occurrences, l.Denies = session.StageObserving, c.Seq(), 0, 0
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
	if c.Identity != "" {
		l.Identities[c.Identity] = true
	}
	for _, f := range sig.Evidence.Files {
		l.Files[f] = true
	}
	if c.Failed() {
		l.LastFailSeq = c.Seq()
	}
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
				out = append(out, e.halt(s, l, sig, c, m.haltReason(false, 0, 0)))
			}
			continue
		}
		if l.Stage != session.StageDenying {
			continue
		}
		// One more try after a real change since the loop's last failure.
		if !l.RetryPending && l.Denies > 0 && l.RetrySeq <= l.LastFailSeq && e.realChange(s, l) {
			l.RetrySeq, l.RetryPending = c.Seq(), true
			continue
		}
		// A subagent's loop is denied but never halted: a halt would end the
		// parent's turn too.
		canHalt := capabilities[l.Detector].Halt && !s.Subagent
		minutes, limit := e.loopMinutes(s, l, c, now)
		switch {
		// The first deny is Denies == 1; each matching attempt after it is
		// one more occurrence.
		case canHalt && l.Denies >= e.th.HaltAfterDenyOccurrences+e.extra(s):
			l.Stage = session.StageHalted
			out = append(out, e.halt(s, l, sig, c, m.haltReason(false, 0, 0)))
		// On time only once a deny was delivered (so a slow loop is always
		// blocked before it is stopped), and never for slow families.
		case canHalt && l.Denies > 0 && limit > 0 && minutes >= limit:
			l.Stage = session.StageHalted
			out = append(out, e.halt(s, l, sig, c, m.haltReason(true, minutes, limit)))
		default:
			l.Denies++
			out = append(out, e.decision(s, l, sig, c.Seq(), event.ActionDeny, m.deny(c), fmt.Sprintf(
				"action repeats a loop that kept going after a nudge (deny #%d)", l.Denies)))
		}
	}
	return out
}

func (e *Engine) halt(s *session.SessionState, l *session.LoopState, sig event.Signal, c *event.ToolCall, reason string) event.Decision {
	if s.Enforce {
		s.HaltNote = true
	}
	return e.decision(s, l, sig, c.Seq(), event.ActionHalt, haltMessage(reason, ShortSessionID(s.Key)), reason)
}

// loopMinutes returns how long loop l has lasted, without the time spent
// waiting for permission decisions (SessionState.PermissionWait), and the
// time-based halt limit for the action c: max(halt_after_minutes, 3× the
// median run time of c's command), or 0 (no time-based halt) for slow
// families.
func (e *Engine) loopMinutes(s *session.SessionState, l *session.LoopState, c *event.ToolCall, now time.Time) (minutes, limit float64) {
	d := now.Sub(l.StartedAt) - (s.PermissionWait - l.WaitAtStart)
	minutes = d.Minutes()
	fam := c.Family
	if fam == "" {
		fam = l.Family
	}
	if e.slow.Matches(fam) || e.slow.Matches(l.Family) {
		return minutes, 0
	}
	limit = e.th.HaltAfterMinutes
	if m := 3 * s.MedianRunTime(c.Identity).Minutes(); m > limit {
		limit = m
	}
	return minutes, limit
}

// identityProgress is the progress that resets repeated-failure loop l:
// one of its commands succeeded since it entered its stage (1.0), or
// failed with an error other than the loop's (0.6, as a new error scores
// in §5.1). Other commands of the family don't count.
func (e *Engine) identityProgress(s *session.SessionState, l *session.LoopState) float64 {
	best := 0.0
	calls := s.Calls.Tail(session.CallHistory)
	for i := len(calls) - 1; i >= 0 && calls[i].Seq() > l.StageSeq; i-- {
		c := calls[i]
		if c.Identity == "" || !l.Identities[c.Identity] || c.Polling {
			continue
		}
		switch {
		case !c.Failed():
			return 1.0
		case l.Anchor != nil && !normalize.SameError(l.Anchor, c.ErrorSig, e.maxDist):
			best = max(best, c.FamilyProgress)
		}
	}
	return best
}

// realChange reports whether something changed since the loop's last
// failing run that could change its outcome: a successful edit or write to
// a file not edited during the loop. (A different command identity is not
// blocked in the first place.)
func (e *Engine) realChange(s *session.SessionState, l *session.LoopState) bool {
	if l.Detector != event.DetectorRepeatedFailure {
		return false
	}
	calls := s.Calls.Tail(session.CallHistory)
	for i := len(calls) - 1; i >= 0 && calls[i].Seq() > l.LastFailSeq; i-- {
		c := calls[i]
		switch c.Event.ToolName {
		case event.ToolEdit, event.ToolWrite, event.ToolPatch:
			if !c.Failed() && c.FilePath != "" && !l.Files[c.FilePath] {
				return true
			}
		}
	}
	return false
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
		Key:        sig.LoopKey,
		Detector:   sig.Detector,
		Stage:      session.StageObserving,
		StartedAt:  c.Event.ReceivedAt.Add(-sig.Evidence.Elapsed),
		FirstSeq:   sig.Since,
		StageSeq:   c.Seq(),
		Identities: map[string]bool{},
		Files:      map[string]bool{},
	}
	l.WaitAtStart = s.PermissionWaitAt(l.StartedAt)
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
// (D1), the same command identity as one of its failing runs (D2; the
// same family for tools without identities), or a write to the same file
// (D3).
func matches(l *session.LoopState, c *event.ToolCall) bool {
	switch {
	case l.ActionFP != 0:
		return c.ActionFP == l.ActionFP
	case l.Family != "":
		if c.Identity != "" && len(l.Identities) > 0 {
			return l.Identities[c.Identity]
		}
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
