package config

import (
	"strings"
	"testing"
)

func TestBudgets(t *testing.T) {
	d := Defaults().Budgets
	if d != DefaultBudgets() || d.Daily.CostUSD != (Limit{Soft: 20}) || d.Session.CostUSD.On() || d.Repo.ToolCalls.On() {
		t.Errorf("defaults %+v: only a soft daily cost warning", d)
	}
	f := mustParse(t, `
budgets:
  session: { soft_usd: 5, hard_usd: 15, hard_tool_calls: 400 }
  daily:   { soft_usd: 0, hard_tokens: 50000000 }
  repo:    { soft_wall_minutes: 120 }
`)
	c := Defaults().Apply(f, GlobalFile)
	b := c.Budgets
	if b.Session.CostUSD != (Limit{5, 15}) || b.Session.ToolCalls.Hard != 400 || b.Daily.CostUSD.On() ||
		b.Daily.Tokens.Hard != 50e6 || b.Repo.WallMinutes.Soft != 120 {
		t.Errorf("applied %+v", b)
	}
	if BudgetKey(ScopeSession, MetricCost, "hard") != "budgets.session.hard_usd" || BudgetKey(ScopeRepo, MetricWall, "soft") != "budgets.repo.soft_wall_minutes" {
		t.Error("budget keys")
	}

	// A repo file may set a limit config.yaml leaves off, or lower one;
	// raising or turning one off is ignored with a note.
	repo := mustParse(t, `
budgets:
  session: { hard_usd: 10, soft_usd: 8, hard_tool_calls: 0 }
  repo: { hard_tool_calls: 100 }
`)
	tc, notes := c.Tighten(repo, ".loopguard.yaml")
	if tc.Budgets.Session.CostUSD != (Limit{5, 10}) || tc.Budgets.Session.ToolCalls.Hard != 400 || tc.Budgets.Repo.ToolCalls.Hard != 100 {
		t.Errorf("tightened %+v", tc.Budgets)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "budgets.session.soft_usd: 8") || !strings.Contains(joined, "budgets.session.hard_tool_calls: 0") || len(notes) != 2 {
		t.Errorf("notes %v", notes)
	}

	for _, bad := range []string{
		"budgets: { session: { hard_usd: -1 } }",
		"budgets: { daily: { soft_tokens: 10, hard_tokens: 5 } }",
		"budgets: { session: { soft_usd: x } }",
	} {
		if _, _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	// An unknown key in budgets is a warning, as anywhere else.
	if _, w, err := Parse([]byte("budgets: { session: { wall_minutes: 180 } }")); err != nil || len(w) != 1 {
		t.Errorf("unknown budget key: %v %v", w, err)
	}
}
