// Package pipeline wires the detection core together: normalize → session
// → detect → policy. Replay and the daemon both drive sessions through it,
// so offline and live detection behave the same.
package pipeline

import (
	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/detect"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
	"github.com/Somsubhra-Nandi/loopguard/internal/policy"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// Pipeline processes the events of any number of sessions. It holds no
// per-session state and is safe for concurrent use across sessions; a
// single session must be driven from one goroutine.
type Pipeline struct {
	th      config.Thresholds
	builder *normalize.Builder
	detect  *detect.Engine
	policy  *policy.Engine
}

// New returns a pipeline configured by th.
func New(th config.Thresholds) *Pipeline {
	return &Pipeline{
		th:      th,
		builder: normalize.BuilderFor(th),
		detect:  detect.New(th),
		policy:  policy.New(th),
	}
}

// NewSession returns an empty session.
func (p *Pipeline) NewSession(key string, agent event.AgentKind) *session.SessionState {
	return session.New(key, agent, p.th)
}

// permModeManual is the permission mode in which a human approves calls.
const permModeManual = "default"

// Result is the outcome of one event.
type Result struct {
	Call      *event.ToolCall // completed call (post events only)
	Signals   []event.Signal  // deduplicated, most severe first
	Decisions []event.Decision
}

// Handle processes one event. For post events, fileHashAfter is the file's
// content hash after an edit or write (0 if unknown); the caller reads the
// disk, this package never does.
func (p *Pipeline) Handle(s *session.SessionState, ev *event.Event, fileHashAfter uint64) Result {
	if ev.AgentID != "" {
		s.Subagent = true
	}
	switch ev.Phase {
	case event.PhasePrompt:
		s.HumanPrompt(ev)
		return Result{Decisions: p.policy.Prompt(s)}
	case event.PhasePre:
		return p.pre(s, ev)
	case event.PhasePost, event.PhasePostFailure:
		return p.post(s, ev, fileHashAfter)
	default:
		s.Assign(ev)
	}
	return Result{}
}

func (p *Pipeline) pre(s *session.SessionState, ev *event.Event) Result {
	s.Assign(ev)
	s.AddPending(ev)
	c := p.builder.Build(ev)
	return Result{Decisions: p.policy.Pre(s, c)}
}

func (p *Pipeline) post(s *session.SessionState, ev *event.Event, fileHashAfter uint64) Result {
	s.Assign(ev)
	c := p.builder.Build(ev)
	c.FileHashAfter = fileHashAfter
	// Duration is the tool's own run time when the agent reports it, else
	// the time since the pre event. In permission mode "default" the gap
	// between the two is mostly the human deciding on the permission
	// prompt, which time-based halts must not count.
	c.Duration = ev.RunTime
	if pre := s.TakePending(ev.ToolUseID); pre != nil {
		gap := ev.ReceivedAt.Sub(pre.ReceivedAt)
		if c.Duration == 0 {
			c.Duration = gap
		} else if ev.PermMode == permModeManual && gap > c.Duration {
			s.AddPermissionWait(gap-c.Duration, ev.ReceivedAt)
		}
	}
	s.Record(c)
	sigs := p.detect.Observe(s, c)
	return Result{Call: c, Signals: sigs, Decisions: p.policy.Post(s, c, sigs)}
}

// Stop returns the halt `loopguard stop` asked for, on the session's
// latest event (see policy.Engine.Stop).
func (p *Pipeline) Stop(s *session.SessionState, first bool) event.Decision {
	return p.policy.Stop(s, s.Seq(), first)
}

// BudgetNudge and BudgetHalt return the decisions for a budget signal
// (D6) on the session's latest event (see policy.Engine.BudgetNudge and
// BudgetHalt).
func (p *Pipeline) BudgetNudge(s *session.SessionState, sig event.Signal, seq uint64) event.Decision {
	return p.policy.BudgetNudge(s, sig, seq)
}

// BudgetHalt returns the halt for a hard budget signal; record is false
// for a halt already recorded in this turn.
func (p *Pipeline) BudgetHalt(s *session.SessionState, sig event.Signal, seq uint64, record bool) event.Decision {
	return p.policy.BudgetHalt(s, sig, seq, record)
}
