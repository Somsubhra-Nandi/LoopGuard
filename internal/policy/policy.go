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
// Once: at most one nudge per loop (D5's loop is the turn).
type Capabilities struct {
	Nudge, Deny, Halt, Once bool
}

// capabilities per detector. D4 nudges only, D5 nudges (and notifies), D6
// notifies and halts.
var capabilities = map[string]Capabilities{
	event.DetectorExactRepeat:     {Nudge: true, Deny: true, Halt: true},
	event.DetectorRepeatedFailure: {Nudge: true, Deny: true, Halt: true},
	event.DetectorOscillation:     {Nudge: true, Deny: true, Halt: true},
	event.DetectorReadThrash:      {Nudge: true},
	event.DetectorStall:           {Nudge: true, Once: true},
	event.DetectorBudget:          {Halt: true},
}

// permModeManual is the permission mode in which a human approves tool
// calls; deny and halt need more evidence there.
const permModeManual = "default"

// Engine is the escalation state machine (LLD §7.1).
//
// Every Decision records in WouldHave what the guard level does, with a
// message from the §7.3 templates. Action is that same action when the
// session's level allows it (SessionState.Allow: guard everything, assist
// nudges only), otherwise ActionNone (observe and notify levels, replay).
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
// halt, so the agent doesn't take the earlier stop for a standing order;
// BlocksOverNote is the same after a turn that had denies but no halt.
const (
	HaltOverNote   = "LoopGuard's earlier stop applied only to the previous turn; commands are not blocked now."
	BlocksOverNote = "LoopGuard's earlier blocks applied only to the previous turn; commands are not blocked now."
)

// Prompt is called after a human prompt has reset the loops. After a turn
// whose halt or denies reached the agent, it returns the note that they
// are over (an informational nudge, recorded nowhere: WouldHave is none).
// The notes are only ever set when the decision reached the agent.
func (e *Engine) Prompt(s *session.SessionState) []event.Decision {
	note, reason := HaltOverNote, "first prompt after a halt"
	switch {
	case s.HaltNote:
	case s.DenyNote:
		note, reason = BlocksOverNote, "first prompt after a turn with blocks"
	default:
		return nil
	}
	s.HaltNote, s.DenyNote = false, false
	return []event.Decision{{Action: event.ActionNudge, WouldHave: event.ActionNone, LoopKey: NoteKey, Message: note, Reason: reason, Seq: s.Seq()}}
}

// Loop keys of the decisions that aren't about a loop: the notes after a
// halt or blocks, and the user's stop.
const (
	NoteKey = "note"
	StopKey = "stop"
)

// Exempt reports whether a decision reaches the agent whatever the
// session's level: the user's own stop, and the note that an earlier stop
// or block is over (only ever given after one reached the agent).
func Exempt(d event.Decision) bool {
	return d.LoopKey == StopKey || d.LoopKey == NoteKey
}

// StopMessage is the stopReason of a halt the user asked for.
const StopMessage = "Stopped by the user via loopguard stop."

// Stop returns the halt that `loopguard stop` asked for, for the event
// with seq. The caller checks that the session's level isn't observe:
// a stop is the user's own request, so it applies at the notify and
// assist levels too. first is false for the same stop's later halts
// (the agent went on, e.g. after a late reply): those are not recorded
// again (WouldHave none).
func (e *Engine) Stop(s *session.SessionState, seq uint64, first bool) event.Decision {
	s.HaltNote = true
	would := event.ActionHalt
	if !first {
		would = event.ActionNone
	}
	return event.Decision{Action: event.ActionHalt, WouldHave: would, LoopKey: StopKey, Detector: event.DetectorUserStop,
		Message: StopMessage, Reason: "stopped by the user (loopguard stop)", Severity: 1, Seq: seq}
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
		if l.Nudges > 0 && (caps.Once || s.CallCount-l.LastNudgeCall < uint64(e.th.NudgeCooldownCalls)) {
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
// At the guard level a halted loop keeps halting matching actions until
// the next human prompt resets the loops, in case the agent goes on
// anyway (a halt reply that arrived too late is dropped). Other levels
// record the halt once.
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
			if allows(s, event.ActionHalt) {
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
			if allows(s, event.ActionDeny) {
				s.DenyNote = true
			}
			out = append(out, e.decision(s, l, sig, c.Seq(), event.ActionDeny, m.deny(c), fmt.Sprintf(
				"action repeats a loop that kept going after a nudge (deny #%d)", l.Denies)))
		}
	}
	return out
}

func (e *Engine) halt(s *session.SessionState, l *session.LoopState, sig event.Signal, c *event.ToolCall, reason string) event.Decision {
	if allows(s, event.ActionHalt) {
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

// allows reports whether the session's level lets action a reach the
// agent.
func allows(s *session.SessionState, a event.Action) bool {
	return a.Rank() > 0 && a.Rank() <= s.Allow.Rank()
}

// extra raises occurrence thresholds when a human approves each action.
func (e *Engine) extra(s *session.SessionState) int {
	if s.PermMode == permModeManual {
		return e.th.ManualModeExtraOccurrences
	}
	return 0
}

func (e *Engine) decision(s *session.SessionState, l *session.LoopState, sig event.Signal, seq uint64, would event.Action, message, reason string) event.Decision {
	action := event.ActionNone // a would-have only
	if allows(s, would) {
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
	case event.DetectorReadThrash:
		if sig.Evidence.Path != "" {
			return fmt.Sprintf("%s: %s was read %d times with no change to it", sig.Detector, sig.Evidence.Path, sig.Count)
		}
		return fmt.Sprintf("%s: the same `%s` search ran %d times with the same result", sig.Detector, sig.Evidence.Family, sig.Count)
	case event.DetectorStall:
		return fmt.Sprintf("%s: %d calls with no visible progress (%d failed, %d repeated reads)", sig.Detector, sig.Count, sig.Evidence.Failures, sig.Evidence.Repeats)
	}
	return fmt.Sprintf("%s: count %d", sig.Detector, sig.Count)
}
