package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
	"github.com/Somsubhra-Nandi/loopguard/internal/report"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

func newReportCmd() *cobra.Command {
	var asJSON bool
	var path string
	cmd := &cobra.Command{
		Use:   "report [session-id-prefix]",
		Short: "Report usage, API-equivalent cost, loops, and interventions",
		Long: `Without a session: today, the last 7 days, and the last 30 days from
LoopGuard's database (sessions, tool calls, tokens, API-equivalent cost, loops
caught, interventions, and the estimated time and cost spent inside loops),
and the tool calls with the highest estimated cost over the last 7 days,
from the transcripts.

With a session id (or its first characters): the session's timeline from its
transcript: turns, tool calls grouped, failures, loops (start and end,
detector, what LoopGuard did, outcome), tokens and cost per turn, and the
total, plus its tool calls by estimated cost and its largest outputs.

Costs are API-equivalent: token usage priced at API rates (pricing.yaml in
the LoopGuard home). On a subscription you don't pay per token. Estimates
per tool call are marked "est.": a call's share of the output of the model
call that issued it, plus its result (about 4 characters per token) carried
as input by every later model call until the next compaction. Outputs and
prompts are never shown; commands are normalized and redacted.`,
		Example: `  loopguard report
  loopguard report 1d3a0fe0
  loopguard report 1d3a0fe0 --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			if path == "" {
				if path, err = replay.DefaultPath(); err != nil {
					return err
				}
			}
			pricing := loadPricing(cmd)
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				r, err := buildSessionReport(dir, path, args[0], pricing)
				if err != nil {
					return err
				}
				if asJSON {
					return writeIndented(out, r)
				}
				printSessionReport(out, r, pricing)
				return nil
			}
			r := buildSummaryReport(dir, path, pricing, now())
			if asJSON {
				return writeIndented(out, r)
			}
			printSummaryReport(out, r, pricing)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	cmd.Flags().StringVar(&path, "path", "", "transcripts directory (default ~/.claude/projects)")
	return cmd
}

func writeIndented(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// buildSummaryReport is `loopguard report` without a session: the
// periods from the database (if there is one) and the last 7 days'
// estimates from the transcripts.
func buildSummaryReport(dir, path string, pricing *cost.Pricing, at time.Time) report.Summary {
	var st *store.Store
	var dbErr error
	if db := filepath.Join(dir, home.DBFile); fileExists(db) {
		if st, dbErr = store.Open(db); dbErr == nil {
			defer st.Close()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := report.BuildSummary(ctx, st, pricing, at, report.NudgeAt())
	if dbErr != nil {
		r.Errors = append([]string{"database: " + dbErr.Error()}, r.Errors...)
	}
	est, err := report.TopCosts(ctx, st, path, pricing, at)
	if err != nil {
		r.Errors = append(r.Errors, "transcripts: "+err.Error())
		return r
	}
	r.Estimates = est
	return r
}

func printSummaryReport(out io.Writer, r report.Summary, p *cost.Pricing) {
	fmt.Fprintln(out, "LoopGuard report (costs are API-equivalent: token usage at API prices)")
	if !r.Database {
		fmt.Fprintln(out, "\nLoopGuard has recorded nothing yet (no database). Live totals start once the hooks run (loopguard init).")
	} else {
		fmt.Fprintf(out, "\n%-28s %14s %14s %14s\n", "", "today", "last 7 days", "last 30 days")
		row := func(label string, f func(report.Period) string) {
			fmt.Fprintf(out, "%-28s", label)
			for _, pr := range r.Periods {
				fmt.Fprintf(out, " %14s", f(pr))
			}
			fmt.Fprintln(out)
		}
		row("sessions", func(pr report.Period) string { return fmt.Sprint(pr.Sessions) })
		row("tool calls", func(pr report.Period) string { return fmt.Sprint(pr.ToolCalls) })
		row("tokens", func(pr report.Period) string { return compactCount(pr.Tokens) })
		row("cost (API-equivalent)", func(pr report.Period) string { return cost.FormatUSD(pr.CostUSD) })
		row("loops caught", func(pr report.Period) string { return fmt.Sprint(pr.Loops) })
		row("interventions (real/would)", func(pr report.Period) string {
			return fmt.Sprintf("%d/%d", pr.Interventions.Real, pr.Interventions.Would)
		})
		row("recovery rate (real/would)", func(pr report.Period) string {
			return ratePct(pr.RecoveryReal) + "/" + ratePct(pr.RecoveryWH)
		})
		row("time inside loops (est.)", func(pr report.Period) string {
			return shortDuration(time.Duration(pr.LoopSeconds * float64(time.Second)))
		})
		row("cost inside loops (est.)", func(pr report.Period) string { return cost.FormatUSD(pr.LoopCostUSD) })
		if last := r.Periods[len(r.Periods)-1]; last.UnpricedTokens > 0 {
			fmt.Fprintf(out, "Not priced (30 days): %s tokens of %s; their cost is unknown (add them to pricing.yaml).\n",
				compactCount(last.UnpricedTokens), strings.Join(last.UnknownModels, ", "))
		}
		if p != nil && p.CurrencyDisplay != "" && p.FXRate > 0 {
			fmt.Fprintf(out, "30-day cost in %s: %s\n", p.CurrencyDisplay, p.Format(r.Periods[len(r.Periods)-1].CostUSD))
		}
	}
	if e := r.Estimates; e != nil {
		fmt.Fprintf(out, "\nTop %d by estimated cost (est.), %s (%d streams, %s in all)\n", len(e.Top), e.Window, e.Streams, p.Format(e.CostUSD))
		printEstimates(out, e)
	}
	for _, err := range r.Errors {
		fmt.Fprintf(out, "\nWarning: %s\n", err)
	}
}

func ratePct(r *float64) string { return report.RatePct(r) }

func printEstimates(out io.Writer, e *report.EstimateReport) {
	if len(e.Top) == 0 {
		fmt.Fprintln(out, "  no tool calls")
		return
	}
	fmt.Fprintf(out, "  %-44s %6s %14s %11s %11s\n", "family or tool + target", "runs", "avg output", "est. cost", "est. share")
	for _, g := range e.Top {
		fmt.Fprintf(out, "  %-44s %6d %14s %11s %10.1f%%\n", clip(g.Group, 44), g.Runs, compactCount(int64(g.AvgResultTokens+0.5))+" tok",
			cost.FormatUSD(g.EstUSD), 100*g.EstShare)
	}
	if len(e.Largest) == 0 {
		return
	}
	fmt.Fprintln(out, "\n  Largest outputs (est. cost of a single result)")
	for _, o := range e.Largest {
		where := shortSID(o.SessionID)
		if o.AgentID != "" {
			where += "/" + clip(o.AgentID, 12)
		}
		fmt.Fprintf(out, "  %-22s call %-5d %-36s %10s tok, carried %4d×  est. %s (%.1f%%)\n", where, o.Call, clip(o.Group, 36),
			compactCount(o.ResultTokens), o.LaterCalls, cost.FormatUSD(o.EstUSD), 100*o.EstShare)
	}
}

// compactCount shows a count as 950, 12.3k, 4.56M.
func compactCount(n int64) string { return report.CompactCount(n) }

// buildSessionReport is `loopguard report <session>`: the timeline from
// the transcript, with what LoopGuard did live from the database.
func buildSessionReport(dir, path, prefix string, pricing *cost.Pricing) (*report.Session, error) {
	var st *store.Store
	if db := filepath.Join(dir, home.DBFile); fileExists(db) {
		if s, err := store.Open(db); err == nil {
			st = s
			defer st.Close()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return report.BuildSession(ctx, st, path, prefix, pricing)
}

func printSessionReport(out io.Writer, r *report.Session, p *cost.Pricing) {
	t := r.Totals
	fmt.Fprintf(out, "LoopGuard report: session %s (%s)\n", r.SessionID, r.Project)
	src := "replayed from the transcript (LoopGuard did not watch it live)"
	if r.Live {
		src = "from the transcript, with what LoopGuard did live"
	}
	fmt.Fprintf(out, "%s; costs are API-equivalent\n\n", src)
	fmt.Fprintf(out, "  Turns %d, tool calls %d (%d failed), loops %d\n", t.Turns, t.Calls, t.Failures, t.Loops)
	fmt.Fprintf(out, "  Tokens %s: input %s, output %s, cache write %s, cache read %s\n", compactCount(t.Tokens), compactCount(t.Usage.Input),
		compactCount(t.Usage.Output), compactCount(t.Usage.CacheWrite), compactCount(t.Usage.CacheRead))
	fmt.Fprintf(out, "  Cost %s API-equivalent; inside loops %s over %s\n", p.Format(t.CostUSD), p.Format(t.LoopCostUSD),
		shortDuration(time.Duration(t.LoopSeconds*float64(time.Second))))
	if t.UnpricedTokens > 0 {
		fmt.Fprintf(out, "  Not priced: %s tokens of %s (cost unknown)\n", compactCount(t.UnpricedTokens), strings.Join(t.UnknownModels, ", "))
	}
	for _, s := range r.Streams {
		name := "Main session"
		if s.Stream != "main" {
			name = "Subagent " + s.Stream
		}
		fmt.Fprintf(out, "\n%s: %d tool calls (%d failed), %s tokens, %s\n", name, s.Calls, s.Failures, compactCount(s.Tokens), p.Format(s.CostUSD))
		for _, tr := range s.Turns {
			head := fmt.Sprintf("Turn %d", tr.Turn)
			if tr.Turn == 0 {
				head = "Before the first prompt"
			}
			fmt.Fprintf(out, "  %s  +%s  %d calls, %d failed, %s tokens, %s", head, offset(tr.StartSeconds), tr.Calls, tr.Failures,
				compactCount(tr.Tokens), p.Format(tr.CostUSD))
			if tr.UnpricedTokens > 0 {
				fmt.Fprintf(out, " (+%s tokens not priced)", compactCount(tr.UnpricedTokens))
			}
			fmt.Fprintln(out)
			for _, g := range tr.Groups {
				calls := fmt.Sprintf("call %d", g.From)
				if g.To > g.From {
					calls = fmt.Sprintf("calls %d-%d", g.From, g.To)
				}
				failed := ""
				if g.Failed > 0 {
					failed = fmt.Sprintf(", %d failed", g.Failed)
				}
				fmt.Fprintf(out, "    %-14s %s ×%d%s\n", calls, clip(g.Label, 70), g.Count, failed)
			}
			for _, l := range tr.Loops {
				c := ""
				if l.CostUSD != nil {
					c = ", " + p.Format(*l.CostUSD)
				}
				fmt.Fprintf(out, "    LOOP %s %s: calls %d-%d (%d calls, +%s to +%s%s)\n", l.Detector, l.Subject, l.FirstCall, l.LastCall,
					l.Count, offset(l.StartSeconds), offset(l.EndSeconds), c)
				fmt.Fprintf(out, "      LoopGuard: %s; outcome %s\n", actionsText(l.Actions), l.Outcome)
			}
		}
		for _, a := range s.Other {
			fmt.Fprintf(out, "  Other: %s %s (%s) at +%s, outcome %s: %s\n", a.Kind, a.Action, a.Detector, offset(a.AtSecond), a.Outcome, a.Reason)
		}
	}
	fmt.Fprintf(out, "\nTop %d by estimated cost (est.), %s (%s in all)\n", len(r.Estimates.Top), r.Estimates.Window, p.Format(r.Estimates.CostUSD))
	printEstimates(out, &r.Estimates)
}

func actionsText(as []report.Action) string {
	if len(as) == 0 {
		return "nothing (cooldown or per-turn nudge limit)"
	}
	parts := make([]string, 0, len(as))
	for _, a := range as {
		switch {
		case a.Kind == "replay":
			parts = append(parts, fmt.Sprintf("would %s at call %d", a.Action, a.Call))
		default:
			parts = append(parts, fmt.Sprintf("%s %s at +%s", a.Kind, a.Action, offset(a.AtSecond)))
		}
	}
	return strings.Join(parts, ", ")
}

// offset formats seconds since the session's first call: 0:42, 12:05,
// 1:02:03.
func offset(sec float64) string { return report.Offset(sec) }
