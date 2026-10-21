package detect

import (
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

func TestBudget(t *testing.T) {
	b := config.Budgets{
		Session: config.ScopeBudget{CostUSD: config.Limit{Soft: 5, Hard: 15}, ToolCalls: config.Limit{Hard: 100}},
		Daily:   config.ScopeBudget{Tokens: config.Limit{Soft: 1000}},
		Repo:    config.ScopeBudget{WallMinutes: config.Limit{Soft: 60}},
	}
	totals := []BudgetTotals{
		{Scope: config.ScopeSession, Period: "s1", Values: map[string]float64{config.MetricCost: 16, config.MetricToolCalls: 100}},
		{Scope: config.ScopeDaily, Period: "2026-10-21", Values: map[string]float64{config.MetricTokens: 1001}},
		{Scope: config.ScopeRepo, Period: "2026-10-21 /r", Values: map[string]float64{config.MetricWall: 59}},
	}
	sigs := Budget(b, totals)
	if len(sigs) != 2 {
		t.Fatalf("signals %+v", sigs)
	}
	got := map[string]*event.BudgetEvidence{}
	for _, s := range sigs {
		got[s.LoopKey] = s.Evidence.Budget
		if s.Detector != event.DetectorBudget {
			t.Errorf("detector %q", s.Detector)
		}
	}
	// Over the hard limit: only the hard signal. Tool calls at the limit
	// are not over it.
	if e := got["bg:session:cost_usd"]; e == nil || e.Level != BudgetHard || e.Limit != 15 || e.Key != "budgets.session.hard_usd" || e.Period != "s1" {
		t.Errorf("session cost: %+v", e)
	}
	if e := got["bg:daily:tokens"]; e == nil || e.Level != BudgetSoft || e.Value != 1001 {
		t.Errorf("daily tokens: %+v", e)
	}
	if len(Budget(config.Budgets{}, totals)) != 0 {
		t.Error("signals with no limits set")
	}
}
