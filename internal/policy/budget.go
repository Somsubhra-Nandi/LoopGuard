package policy

import (
	"fmt"
	"strconv"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// BudgetNudge returns the one nudge for a soft budget signal (D6): a
// factual note for the agent, delivered with a post at the assist and
// guard levels, recorded as a would-have below them. The caller makes
// sure it is given once per period.
func (e *Engine) BudgetNudge(s *session.SessionState, sig event.Signal, seq uint64) event.Decision {
	b := sig.Evidence.Budget
	msg := fmt.Sprintf("LoopGuard observation: %s is %s, past the soft budget of %s (%s). Nothing is blocked; this is a heads-up. "+
		"Options that often help: finishing the current step and checking with the user before starting more work.",
		budgetSubject(b), budgetValue(b.Metric, b.Value), budgetValue(b.Metric, b.Limit), b.Key)
	return e.budgetDecision(s, sig, seq, event.ActionNudge, msg, budgetReason(b, "soft"))
}

// BudgetHalt returns the halt for a hard budget signal (D6): at the guard
// level it ends the turn, on any tool event of the session, its subagents
// included (unlike a loop halt: the budget is the session's). Below guard
// it is a would-have. record false: the halt was already recorded in this
// turn (the agent went on), so WouldHave is none.
//
// The wording follows the loop halts' ("stopped this turn", §7.3), but
// says that the next turn stops again while the budget is exceeded, and
// how to raise it.
func (e *Engine) BudgetHalt(s *session.SessionState, sig event.Signal, seq uint64, record bool) event.Decision {
	b := sig.Evidence.Budget
	reason := budgetReason(b, "hard")
	lag := ""
	if b.Metric == config.MetricCost || b.Metric == config.MetricTokens {
		lag = " Usage is read from the transcript a few seconds late, so the total can pass the limit before the stop."
	}
	msg := clipRunes(fmt.Sprintf("%s%s.%s Your next message continues normally, but tool calls stop again while the budget is exceeded. "+
		"To raise it, set %s in config.yaml (loopguard home), or turn LoopGuard off for this session: %s",
		HaltPrefix, reason, lag, b.Key, PauseCommand(ShortSessionID(s.Key))), MaxMessage)
	d := e.budgetDecision(s, sig, seq, event.ActionHalt, msg, reason)
	if !record {
		d.WouldHave = event.ActionNone
	}
	return d
}

func (e *Engine) budgetDecision(s *session.SessionState, sig event.Signal, seq uint64, would event.Action, msg, reason string) event.Decision {
	action := event.ActionNone
	if allows(s, would) {
		action = would
	}
	return event.Decision{Action: action, WouldHave: would, LoopKey: sig.LoopKey, Detector: event.DetectorBudget,
		Message: msg, Reason: reason, Severity: sig.Severity, Seq: seq}
}

// budgetReason is the one-line reason, e.g. "this session's API-equivalent
// cost is $15.40, over the hard budget of $15.00".
func budgetReason(b *event.BudgetEvidence, level string) string {
	word := "past"
	if level == "hard" {
		word = "over"
	}
	return fmt.Sprintf("%s is %s, %s the %s budget of %s", budgetSubject(b), budgetValue(b.Metric, b.Value), word, level,
		budgetValue(b.Metric, b.Limit))
}

// BudgetSummary is a budget passed in a few words, for notifications:
// "session cost $15.40 (API-equivalent), hard limit $15.00".
func BudgetSummary(b *event.BudgetEvidence) string {
	what := map[string]string{config.MetricCost: "cost", config.MetricTokens: "tokens", config.MetricWall: "wall time",
		config.MetricToolCalls: "tool calls"}[b.Metric]
	scope := map[string]string{config.ScopeSession: "session", config.ScopeDaily: "today's", config.ScopeRepo: "repo's daily"}[b.Scope]
	v := budgetValue(b.Metric, b.Value)
	if b.Metric == config.MetricCost {
		v += " (API-equivalent)"
	}
	return fmt.Sprintf("%s %s %s, %s limit %s", scope, what, v, b.Level, budgetValue(b.Metric, b.Limit))
}

func budgetSubject(b *event.BudgetEvidence) string {
	whose := map[string]string{
		config.ScopeSession: "this session's",
		config.ScopeDaily:   "today's",
		config.ScopeRepo:    "this repo's",
	}[b.Scope]
	what := map[string]string{
		config.MetricCost:      "API-equivalent cost",
		config.MetricTokens:    "token count (input, output, and cache)",
		config.MetricWall:      "wall time",
		config.MetricToolCalls: "tool call count",
	}[b.Metric]
	s := whose + " " + what
	switch b.Scope {
	case config.ScopeDaily:
		s += " across all sessions"
	case config.ScopeRepo:
		s += " today"
	}
	return s
}

// budgetValue formats a budget value: "$15.40", "1,234,567 tokens", "182
// min", "512 calls".
func budgetValue(metric string, v float64) string {
	switch metric {
	case config.MetricCost:
		return fmt.Sprintf("$%.2f", v)
	case config.MetricTokens:
		return thousands(int64(v)) + " tokens"
	case config.MetricWall:
		return fmt.Sprintf("%.0f min", v)
	case config.MetricToolCalls:
		return thousands(int64(v)) + " calls"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := 0
	if n < 0 {
		start = 1
	}
	for i := len(s) - 3; i > start; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
