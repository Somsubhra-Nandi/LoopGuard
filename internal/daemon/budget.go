package daemon

import (
	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/detect"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/notify"
	"github.com/Somsubhra-Nandi/loopguard/internal/pipeline"
	"github.com/Somsubhra-Nandi/loopguard/internal/policy"
)

// budget checks the session's budgets (D6, LLD §6) on a tool event: the
// totals of the session (subagents included), the day, and the repo's
// day, against the limits in effect for the repo.
//
//   - Soft: one factual nudge per session and period, given with a tool
//     result (it reaches the agent at assist and guard, is recorded below),
//     and one budget_soft notification per period.
//   - Hard: at guard, a halt on every tool event while over the limit,
//     subagent streams included (a budget is the session's, unlike a loop),
//     recorded once per turn; below guard, recorded once per period as a
//     would-have. One budget_hard notification per period.
//
// Usage arrives from the transcript a few seconds after the calls that
// used it, so a hard limit stops the turn at the first tool event after
// the tailer has seen the crossing.
func (a *actor) budget(pipe *pipeline.Pipeline, ev *event.Event, eff config.Effective, mode config.ModeInfo, repo string) ([]event.Decision, []notify.Notification) {
	switch ev.Phase {
	case event.PhasePre, event.PhasePost, event.PhasePostFailure, event.PhaseBatchEnd:
	default:
		return nil, nil
	}
	if ev.SessionID == doctorSessionID || !eff.Budgets.Any() {
		return nil, nil
	}
	book := a.m.book
	sigs := detect.Budget(eff.Budgets, book.budgetTotals(ev.SessionID, repo, ev.ReceivedAt))
	post := ev.Phase == event.PhasePost || ev.Phase == event.PhasePostFailure
	var ds []event.Decision
	var notes []notify.Notification
	for _, sig := range sigs {
		b := sig.Evidence.Budget
		period := sig.LoopKey + "|" + b.Level + "|" + b.Period
		recordedBefore := book.marked("restart|" + a.key + "|" + sig.LoopKey) // a restarted daemon already recorded it today
		switch b.Level {
		case detect.BudgetSoft:
			// A nudge goes with a tool result.
			if !post || recordedBefore || !book.markOnce("nudge|"+ev.SessionID+"|"+period) {
				continue
			}
			ds = append(ds, pipe.BudgetNudge(a.state, sig, ev.Seq))
		case detect.BudgetHard:
			if a.state.Allow == event.ActionHalt {
				record := a.budgetTurn[period] != a.state.TurnSeq+1
				a.budgetTurn[period] = a.state.TurnSeq + 1
				ds = append(ds, pipe.BudgetHalt(a.state, sig, ev.Seq, record))
			} else if !recordedBefore && book.markOnce("halt|"+ev.SessionID+"|"+period) {
				ds = append(ds, pipe.BudgetHalt(a.state, sig, ev.Seq, true))
			}
		}
		if config.Notifies(mode.Mode) && book.markOnce("notify|"+period) {
			kind := config.EventBudgetSoft
			if b.Level == detect.BudgetHard {
				kind = config.EventBudgetHard
			}
			project := baseName(repo)
			if project == "" {
				project = baseName(ev.Cwd)
			}
			// Keyed by level too: the rate limit allows one notification per
			// loop key per 10 minutes, and the hard one must not wait on the soft.
			n := notify.New(kind, project, a.key, sig.LoopKey+":"+b.Level, event.DetectorBudget, "", sig.Count, 0, ev.ReceivedAt)
			n.Budget = policy.BudgetSummary(b)
			notes = append(notes, n)
		}
	}
	return ds, notes
}
