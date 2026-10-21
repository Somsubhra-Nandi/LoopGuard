package policy

import (
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// Intervention outcomes (LLD §11 interventions.outcome).
const (
	OutcomeRecovered = "recovered"
	OutcomeMovedOn   = "moved_on"
	OutcomeContinued = "continued_loop"
	OutcomeHalted    = "halted"
	OutcomeUnknown   = "unknown"
	// OutcomeNA is for interventions that aren't about a loop (self-
	// protection, the user's stop): there is nothing to recover from.
	OutcomeNA = "n/a"
)

// OutcomeAfterCalls is how many completed calls after an intervention its
// outcome is decided.
const OutcomeAfterCalls = 10

// quietCalls: a loop that hasn't signaled in the last quietCalls of those
// calls has stopped.
const quietCalls = OutcomeAfterCalls / 2

// Resolved is an intervention whose outcome is now known.
type Resolved struct {
	Decision event.Decision
	Outcome  string
}

// Outcomes tracks what happened after each intervention (real or
// would-have) of one session, and resolves it OutcomeAfterCalls completed
// calls later:
//
//   - halted: the intervention was a halt, or the loop was halted since;
//   - recovered: the looping command itself (one of the loop's command
//     identities) later succeeded or failed with another error (D2), or
//     the repeated action gave a different result (D1);
//   - moved_on: none of that, but the loop stopped signaling (no signal
//     in the last quietCalls calls): the agent did something else, which
//     says nothing about whether the problem was solved;
//   - continued_loop: otherwise.
//
// End resolves the rest when the turn or the session ends first: as
// recovered if the loop already had, else unknown. Not safe for
// concurrent use; the session's actor owns it.
type Outcomes struct {
	maxDist int
	pending []*pendingOutcome
}

type pendingOutcome struct {
	d          event.Decision
	family     string
	identities map[string]bool
	anchor     *event.ErrorSignature
	actionFP   uint64
	resultFP   uint64
	startCall  uint64
	lastSignal uint64 // CallCount at the loop's latest signal
	recovered  bool
}

// NewOutcomes returns a tracker; maxDist is the simhash distance within
// which two errors are the same (detectors.simhash_max_distance).
func NewOutcomes(maxDist int) *Outcomes { return &Outcomes{maxDist: maxDist} }

// Pending returns the number of interventions not resolved yet.
func (o *Outcomes) Pending() int { return len(o.pending) }

// Add starts tracking the decisions made on the latest event. A halt is
// resolved at once.
func (o *Outcomes) Add(s *session.SessionState, ds []event.Decision) []Resolved {
	var out []Resolved
	for _, d := range ds {
		if d.WouldHave == "" || d.WouldHave == event.ActionNone {
			continue
		}
		if d.Detector == event.DetectorSelfProtection || d.Detector == event.DetectorUserStop || d.Detector == event.DetectorBudget {
			out = append(out, Resolved{d, OutcomeNA})
			continue
		}
		if d.WouldHave == event.ActionHalt {
			out = append(out, Resolved{d, OutcomeHalted})
			continue
		}
		p := &pendingOutcome{d: d, startCall: s.CallCount, lastSignal: s.CallCount, identities: map[string]bool{}}
		if l := s.Loops[d.LoopKey]; l != nil {
			p.family, p.anchor, p.actionFP = l.Family, l.Anchor, l.ActionFP
			if p.family == "" {
				p.family = l.Evidence.Family
			}
			for id := range l.Identities {
				p.identities[id] = true
			}
		}
		if p.actionFP != 0 {
			for _, c := range s.Calls.Tail(session.CallHistory) {
				if c.ActionFP == p.actionFP {
					p.resultFP = c.ResultFP
				}
			}
		}
		o.pending = append(o.pending, p)
	}
	return out
}

// Observe updates the pending interventions with the latest event's
// completed call (nil for other events), signals, and decisions, and
// returns those that are resolved now.
func (o *Outcomes) Observe(s *session.SessionState, c *event.ToolCall, sigs []event.Signal, ds []event.Decision) []Resolved {
	if len(o.pending) == 0 {
		return nil
	}
	halted := map[string]bool{}
	for _, d := range ds {
		if d.WouldHave == event.ActionHalt {
			halted[d.LoopKey] = true
		}
	}
	signaled := map[string]bool{}
	for _, sig := range sigs {
		signaled[sig.LoopKey] = true
	}
	var out []Resolved
	kept := o.pending[:0]
	for _, p := range o.pending {
		key := p.d.LoopKey
		if halted[key] {
			out = append(out, Resolved{p.d, OutcomeHalted})
			continue
		}
		if signaled[key] {
			p.lastSignal = s.CallCount
		}
		if c != nil && o.recovers(p, c) {
			p.recovered = true
		}
		if c == nil || s.CallCount < p.startCall+OutcomeAfterCalls {
			kept = append(kept, p)
			continue
		}
		outcome := OutcomeContinued
		switch {
		case p.recovered:
			outcome = OutcomeRecovered
		case p.lastSignal+quietCalls <= s.CallCount:
			outcome = OutcomeMovedOn
		}
		out = append(out, Resolved{p.d, outcome})
	}
	o.pending = kept
	return out
}

// recovers reports whether c shows that p's loop is over.
func (o *Outcomes) recovers(p *pendingOutcome, c *event.ToolCall) bool {
	switch p.d.Detector {
	case event.DetectorRepeatedFailure:
		// The same command, by identity (the family for tools without
		// identities): another command of the family passing is moving on.
		same := c.Family == p.family
		if c.Identity != "" && len(p.identities) > 0 {
			same = p.identities[c.Identity]
		}
		if !same || c.Polling {
			return false
		}
		return !c.Failed() || (p.anchor != nil && !normalize.SameError(p.anchor, c.ErrorSig, o.maxDist))
	case event.DetectorExactRepeat:
		return p.actionFP != 0 && c.ActionFP == p.actionFP && c.ResultFP != p.resultFP
	}
	return false
}

// End resolves every pending intervention: the turn or the session ended
// (or the session's state was lost) before its outcome was due.
func (o *Outcomes) End() []Resolved {
	out := make([]Resolved, 0, len(o.pending))
	for _, p := range o.pending {
		outcome := OutcomeUnknown
		if p.recovered {
			outcome = OutcomeRecovered
		}
		out = append(out, Resolved{p.d, outcome})
	}
	o.pending = nil
	return out
}
