package detect

import (
	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// Budget levels and their signal severities (LLD §6 D6): a soft limit
// notifies (and nudges once at the assist and guard levels), a hard one
// halts at the guard level.
const (
	BudgetSoft         = "soft"
	BudgetHard         = "hard"
	BudgetSoftSeverity = 0.5
	BudgetHardSeverity = 1.0
)

// BudgetTotals are the current values of one budget scope: what a session
// (with its subagents), the day, or a repo's day has used so far. Period
// names what they are counted over (see event.BudgetEvidence).
type BudgetTotals struct {
	Scope  string
	Period string
	Values map[string]float64 // by config.Metric*
}

// Budget is detector D6. Unlike D1-D5 it doesn't look at one session's
// calls: its inputs are totals across sessions (the cost accountant's
// usage, wall time, tool calls), which the daemon keeps. It is pure, like
// the other detectors.
//
// For each scope and metric over a limit it returns one signal: the hard
// one when the hard limit is passed, else the soft one. Values at the
// limit are not over it.
func Budget(b config.Budgets, totals []BudgetTotals) []event.Signal {
	var out []event.Signal
	for _, t := range totals {
		limits := b.Scope(t.Scope)
		for _, m := range config.Metrics {
			l := limits.Get(m)
			v := t.Values[m]
			level, limit, sev := "", 0.0, 0.0
			switch {
			case l.Hard > 0 && v > l.Hard:
				level, limit, sev = BudgetHard, l.Hard, BudgetHardSeverity
			case l.Soft > 0 && v > l.Soft:
				level, limit, sev = BudgetSoft, l.Soft, BudgetSoftSeverity
			default:
				continue
			}
			out = append(out, event.Signal{
				Detector: event.DetectorBudget,
				LoopKey:  BudgetLoopKey(t.Scope, m),
				Severity: sev,
				Count:    int(v),
				Evidence: event.Evidence{Budget: &event.BudgetEvidence{
					Scope: t.Scope, Metric: m, Level: level, Value: v, Limit: limit,
					Key: config.BudgetKey(t.Scope, m, level), Period: t.Period,
				}},
			})
		}
	}
	return out
}

// BudgetLoopKey is the LoopKey of a budget: bg:<scope>:<metric>.
func BudgetLoopKey(scope, metric string) string { return "bg:" + scope + ":" + metric }
