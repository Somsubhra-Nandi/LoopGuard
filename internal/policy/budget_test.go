package policy

import (
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

func budgetSignal(scope, metric, level string, v, limit float64) event.Signal {
	return event.Signal{Detector: event.DetectorBudget, LoopKey: "bg:" + scope + ":" + metric, Severity: 1,
		Evidence: event.Evidence{Budget: &event.BudgetEvidence{Scope: scope, Metric: metric, Level: level, Value: v, Limit: limit,
			Key: config.BudgetKey(scope, metric, level)}}}
}

func TestBudgetDecisions(t *testing.T) {
	e := New(config.Default())
	s := session.New("claude-code:1a2b3c4d-x", event.AgentClaudeCode, config.Default())
	banned := []string{"SYSTEM", "you must", "MUST", "IMPORTANT", "ignore previous"}

	for _, allow := range []event.Action{event.ActionNone, event.ActionNudge, event.ActionHalt} {
		s.Allow = allow
		n := e.BudgetNudge(s, budgetSignal(config.ScopeSession, config.MetricCost, "soft", 5.2, 5), 7)
		h := e.BudgetHalt(s, budgetSignal(config.ScopeDaily, config.MetricTokens, "hard", 2_500_000, 2_000_000), 8, true)
		if n.WouldHave != event.ActionNudge || h.WouldHave != event.ActionHalt || n.Detector != event.DetectorBudget || h.Seq != 8 {
			t.Errorf("allow %s: %+v %+v", allow, n, h)
		}
		if (n.Action == event.ActionNudge) != (allow != event.ActionNone) || (h.Action == event.ActionHalt) != (allow == event.ActionHalt) {
			t.Errorf("allow %s: actions %s %s", allow, n.Action, h.Action)
		}
		for _, msg := range []string{n.Message, h.Message} {
			for _, b := range banned {
				if strings.Contains(msg, b) {
					t.Errorf("%q in %q", b, msg)
				}
			}
		}
	}
	n := e.BudgetNudge(s, budgetSignal(config.ScopeSession, config.MetricCost, "soft", 5.2, 5), 7)
	if !strings.Contains(n.Message, "this session's API-equivalent cost is $5.20, past the soft budget of $5.00 (budgets.session.soft_usd)") {
		t.Errorf("nudge %q", n.Message)
	}
	h := e.BudgetHalt(s, budgetSignal(config.ScopeDaily, config.MetricTokens, "hard", 2_500_000, 2_000_000), 8, true)
	for _, want := range []string{"LoopGuard stopped this turn: today's token count (input, output, and cache) across all sessions is 2,500,000 tokens, over the hard budget of 2,000,000 tokens.",
		"a few seconds late", "Your next message continues normally, but tool calls stop again", "budgets.daily.hard_tokens", "loopguard pause --session 1a2b3c4d"} {
		if !strings.Contains(h.Message, want) {
			t.Errorf("halt message lacks %q: %q", want, h.Message)
		}
	}
	if h.Reason != "today's token count (input, output, and cache) across all sessions is 2,500,000 tokens, over the hard budget of 2,000,000 tokens" {
		t.Errorf("reason %q", h.Reason)
	}
	if s.HaltNote {
		t.Error("a budget halt set the 'stop is over' note: the next turn is still over budget")
	}
	if again := e.BudgetHalt(s, budgetSignal(config.ScopeSession, config.MetricToolCalls, "hard", 101, 100), 9, false); again.WouldHave != event.ActionNone ||
		strings.Contains(again.Message, "seconds late") {
		t.Errorf("repeat halt %+v", again)
	}
	if got := BudgetSummary(budgetSignal(config.ScopeSession, config.MetricCost, "hard", 15.4, 15).Evidence.Budget); got != "session cost $15.40 (API-equivalent), hard limit $15.00" {
		t.Errorf("summary %q", got)
	}
}
