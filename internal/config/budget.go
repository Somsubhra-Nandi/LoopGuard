package config

import "fmt"

// Budget scopes (LLD §6 D6): one session (with its subagents), all
// sessions of the local day, and one repo's sessions of the local day.
const (
	ScopeSession = "session"
	ScopeDaily   = "daily"
	ScopeRepo    = "repo"
)

// Budget metrics. Cost is API-equivalent USD; tokens are all four kinds
// (input, output, cache writes, cache reads); wall minutes run from a
// session's first event to its latest (summed over sessions for daily and
// repo); tool calls are completed calls.
const (
	MetricCost      = "cost_usd"
	MetricTokens    = "tokens"
	MetricWall      = "wall_minutes"
	MetricToolCalls = "tool_calls"
)

// Metrics lists the budget metrics in display order.
var Metrics = []string{MetricCost, MetricTokens, MetricWall, MetricToolCalls}

// Scopes lists the budget scopes in display order.
var Scopes = []string{ScopeSession, ScopeDaily, ScopeRepo}

// Limit is a soft and a hard limit; 0 is off.
type Limit struct {
	Soft float64 `json:"soft,omitempty"`
	Hard float64 `json:"hard,omitempty"`
}

// On reports whether either limit is set.
func (l Limit) On() bool { return l.Soft > 0 || l.Hard > 0 }

// ScopeBudget holds a scope's limits.
type ScopeBudget struct {
	CostUSD     Limit `json:"cost_usd,omitzero"`
	Tokens      Limit `json:"tokens,omitzero"`
	WallMinutes Limit `json:"wall_minutes,omitzero"`
	ToolCalls   Limit `json:"tool_calls,omitzero"`
}

// Get returns the limit of a metric.
func (b ScopeBudget) Get(metric string) Limit {
	switch metric {
	case MetricCost:
		return b.CostUSD
	case MetricTokens:
		return b.Tokens
	case MetricWall:
		return b.WallMinutes
	case MetricToolCalls:
		return b.ToolCalls
	}
	return Limit{}
}

func (b *ScopeBudget) ptr(metric string) *Limit {
	switch metric {
	case MetricCost:
		return &b.CostUSD
	case MetricTokens:
		return &b.Tokens
	case MetricWall:
		return &b.WallMinutes
	case MetricToolCalls:
		return &b.ToolCalls
	}
	return nil
}

// Budgets are the D6 limits per scope.
type Budgets struct {
	Session ScopeBudget `json:"session,omitzero"`
	Daily   ScopeBudget `json:"daily,omitzero"`
	Repo    ScopeBudget `json:"repo,omitzero"`
}

// Scope returns a scope's limits.
func (b Budgets) Scope(scope string) ScopeBudget {
	switch scope {
	case ScopeSession:
		return b.Session
	case ScopeDaily:
		return b.Daily
	case ScopeRepo:
		return b.Repo
	}
	return ScopeBudget{}
}

func (b *Budgets) scopePtr(scope string) *ScopeBudget {
	switch scope {
	case ScopeSession:
		return &b.Session
	case ScopeDaily:
		return &b.Daily
	case ScopeRepo:
		return &b.Repo
	}
	return nil
}

// Any reports whether any limit is set.
func (b Budgets) Any() bool {
	for _, s := range Scopes {
		for _, m := range Metrics {
			if b.Scope(s).Get(m).On() {
				return true
			}
		}
	}
	return false
}

// DefaultBudgets returns the default limits: everything off except a soft
// warning when the day's API-equivalent cost passes $20.
func DefaultBudgets() Budgets {
	return Budgets{Daily: ScopeBudget{CostUSD: Limit{Soft: 20}}}
}

// BudgetKey is a limit's name in config.yaml, e.g. "budgets.session.hard_usd".
func BudgetKey(scope, metric, level string) string {
	suffix := map[string]string{MetricCost: "usd", MetricTokens: "tokens", MetricWall: "wall_minutes", MetricToolCalls: "tool_calls"}[metric]
	return "budgets." + scope + "." + level + "_" + suffix
}

// BudgetsFile is the budgets: section.
type BudgetsFile struct {
	Session *ScopeBudgetFile `yaml:"session"`
	Daily   *ScopeBudgetFile `yaml:"daily"`
	Repo    *ScopeBudgetFile `yaml:"repo"`
}

// ScopeBudgetFile is one scope's limits; 0 turns a limit off.
type ScopeBudgetFile struct {
	SoftUSD         *float64 `yaml:"soft_usd"`
	HardUSD         *float64 `yaml:"hard_usd"`
	SoftTokens      *float64 `yaml:"soft_tokens"`
	HardTokens      *float64 `yaml:"hard_tokens"`
	SoftWallMinutes *float64 `yaml:"soft_wall_minutes"`
	HardWallMinutes *float64 `yaml:"hard_wall_minutes"`
	SoftToolCalls   *float64 `yaml:"soft_tool_calls"`
	HardToolCalls   *float64 `yaml:"hard_tool_calls"`
}

func (f *BudgetsFile) scope(scope string) *ScopeBudgetFile {
	if f == nil {
		return nil
	}
	switch scope {
	case ScopeSession:
		return f.Session
	case ScopeDaily:
		return f.Daily
	case ScopeRepo:
		return f.Repo
	}
	return nil
}

// values returns the file's (soft, hard) pointers of a metric.
func (s *ScopeBudgetFile) values(metric string) (soft, hard *float64) {
	if s == nil {
		return nil, nil
	}
	switch metric {
	case MetricCost:
		return s.SoftUSD, s.HardUSD
	case MetricTokens:
		return s.SoftTokens, s.HardTokens
	case MetricWall:
		return s.SoftWallMinutes, s.HardWallMinutes
	case MetricToolCalls:
		return s.SoftToolCalls, s.HardToolCalls
	}
	return nil, nil
}

func (f *BudgetsFile) validate(bad func(string, ...any)) {
	for _, scope := range Scopes {
		s := f.scope(scope)
		for _, m := range Metrics {
			soft, hard := s.values(m)
			for _, x := range []struct {
				v     *float64
				level string
			}{{soft, "soft"}, {hard, "hard"}} {
				if x.v != nil && *x.v < 0 {
					bad("%s: %g is negative", BudgetKey(scope, m, x.level), *x.v)
				}
			}
			if soft != nil && hard != nil && *soft > 0 && *hard > 0 && *soft > *hard {
				bad("%s: %g is above %s %g", BudgetKey(scope, m, "soft"), *soft, BudgetKey(scope, m, "hard"), *hard)
			}
		}
	}
}

// apply sets every limit the file sets (config.yaml).
func (b Budgets) apply(f *BudgetsFile) Budgets {
	for _, scope := range Scopes {
		for _, m := range Metrics {
			soft, hard := f.scope(scope).values(m)
			l := b.scopePtr(scope).ptr(m)
			if soft != nil {
				l.Soft = *soft
			}
			if hard != nil {
				l.Hard = *hard
			}
		}
	}
	return b
}

// tighten applies a repo file's limits: only a limit that makes LoopGuard
// more protective is used (set where config.yaml has none, or lower);
// others become notes.
func (b Budgets) tighten(f *BudgetsFile, ignore func(key, why string)) Budgets {
	for _, scope := range Scopes {
		for _, m := range Metrics {
			soft, hard := f.scope(scope).values(m)
			l := b.scopePtr(scope).ptr(m)
			for _, x := range []struct {
				v     *float64
				dst   *float64
				level string
			}{{soft, &l.Soft, "soft"}, {hard, &l.Hard, "hard"}} {
				switch {
				case x.v == nil:
				case *x.v > 0 && (*x.dst == 0 || *x.v <= *x.dst):
					*x.dst = *x.v
				case *x.v == *x.dst:
				default:
					ignore(fmt.Sprintf("%s: %g", BudgetKey(scope, m, x.level), *x.v),
						fmt.Sprintf("a repo file can only set or lower a budget (config: %g)", *x.dst))
				}
			}
		}
	}
	return b
}
