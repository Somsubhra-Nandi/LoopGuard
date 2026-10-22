package config

import (
	"strings"
	"testing"
)

func TestBudgets(t *testing.T) {
	d := Defaults().Budgets
	if d != DefaultBudgets() || d.Any() {
		t.Errorf("defaults %+v: all off", d)
	}
	for key, want := range map[string]string{"daily.soft_usd": "budgets.daily.soft_usd", "budgets.session.hard_wall_minutes": "budgets.session.hard_wall_minutes",
		"Repo.Hard_Tool_Calls": "budgets.repo.hard_tool_calls"} {
		if s, m, l, err := ParseBudgetKey(key); err != nil || BudgetKey(s, m, l) != want {
			t.Errorf("ParseBudgetKey(%q) = %s %s %s %v", key, s, m, l, err)
		}
	}
	for _, key := range []string{"daily", "weekly.soft_usd", "daily.max_usd", "daily.soft_dollars", "daily.soft"} {
		if _, _, _, err := ParseBudgetKey(key); err == nil {
			t.Errorf("ParseBudgetKey(%q) accepted", key)
		}
	}
	if v, err := ParseBudgetValue(MetricCost, "$12.5"); err != nil || v != 12.5 {
		t.Errorf("12.5: %v %v", v, err)
	}
	for _, s := range []string{"0", "-1", "x", "NaN", "Inf"} {
		if _, err := ParseBudgetValue(MetricCost, s); err == nil {
			t.Errorf("value %q accepted", s)
		}
	}
	if _, err := ParseBudgetValue(MetricToolCalls, "10.5"); err == nil {
		t.Error("fractional tool calls accepted")
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
