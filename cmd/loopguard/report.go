package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// topEstimates is how many groups the "by estimated cost" tables list.
const topEstimates = 10

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

// estimateReport ranks tool calls by estimated cost.
type estimateReport struct {
	Window  string                  `json:"window"`
	Streams int                     `json:"streams"`
	CostUSD float64                 `json:"cost_usd"` // the real cost the shares are of
	Top     []replay.GroupEstimate  `json:"top"`
	Largest []replay.OutputEstimate `json:"largest_outputs"`
}

// summaryReport is `loopguard report` without a session.
type summaryReport struct {
	Basis     string          `json:"basis"`
	Database  bool            `json:"database"` // false: LoopGuard has recorded nothing yet
	Periods   []periodReport  `json:"periods"`
	Estimates *estimateReport `json:"estimates,omitempty"` // last 7 days, from the transcripts
	Errors    []string        `json:"errors,omitempty"`
}

type periodReport struct {
	Name           string     `json:"name"` // today | 7d | 30d
	Since          time.Time  `json:"since"`
	Sessions       int        `json:"sessions"`
	ToolCalls      int64      `json:"tool_calls"`
	Tokens         int64      `json:"tokens"`
	Usage          cost.Usage `json:"usage"`
	CostUSD        float64    `json:"cost_usd"`
	UnpricedTokens int64      `json:"unpriced_tokens"`
	UnknownModels  []string   `json:"unknown_models"`
	Loops          int        `json:"loops"`
	Interventions  struct {
		Real  int64 `json:"real"`
		Would int64 `json:"would_have"`
	} `json:"interventions"`
	// LoopSeconds and LoopCostUSD estimate the time and cost inside loops:
	// from each loop's first call to its last (overlaps once), and the
	// usage recorded in that time.
	LoopSeconds float64 `json:"est_loop_seconds"`
	LoopCostUSD float64 `json:"est_loop_cost_usd"`
}

func buildSummaryReport(dir, path string, pricing *cost.Pricing, at time.Time) summaryReport {
	r := summaryReport{Basis: replay.CostBasis, Periods: []periodReport{}}
	today := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	periods := []periodReport{
		{Name: "today", Since: today},
		{Name: "7d", Since: at.Add(-7 * 24 * time.Hour)},
		{Name: "30d", Since: at.Add(-30 * 24 * time.Hour)},
	}
	if db := filepath.Join(dir, home.DBFile); fileExists(db) {
		if st, err := store.Open(db); err != nil {
			r.Errors = append(r.Errors, "database: "+err.Error())
		} else {
			defer st.Close()
			r.Database = true
			nudgeAt := config.Default().Policy.NudgeThreshold
			for i := range periods {
				if err := fillPeriod(st, &periods[i], pricing, nudgeAt); err != nil {
					r.Errors = append(r.Errors, err.Error())
				}
			}
		}
	}
	r.Periods = periods

	// The estimates come from the transcripts: the database keeps no
	// output sizes.
	streams, err := replay.Discover(path, replay.Filter{Since: at.Add(-7 * 24 * time.Hour)})
	if err != nil {
		r.Errors = append(r.Errors, "transcripts: "+err.Error())
		return r
	}
	res := replay.Run(streams, replay.Options{Thresholds: config.Default(), Pricing: pricing})
	top, largest := res.Costs.Estimates(topEstimates, nil)
	r.Estimates = &estimateReport{Window: "transcripts changed in the last 7 days", Streams: len(streams),
		CostUSD: res.Costs.CostUSD, Top: top, Largest: largest}
	return r
}

func fillPeriod(st *store.Store, p *periodReport, pricing *cost.Pricing, nudgeAt float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessions, err := st.ListSessions(ctx, store.ListOptions{Since: p.Since, Limit: 100000})
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, s := range sessions {
		if s.SessionID != doctorSession {
			ids[s.SessionID] = true
		}
	}
	p.Sessions = len(ids)
	calls, err := st.CallCounts(ctx, p.Since)
	if err != nil {
		return err
	}
	for _, n := range calls {
		p.ToolCalls += n
	}
	rows, err := st.UsageRows(ctx, store.UsageFilter{Since: p.Since})
	if err != nil {
		return err
	}
	unknown := map[string]bool{}
	for _, u := range rows {
		m := usageMessage(u)
		p.Usage = p.Usage.Add(m.Usage)
		if usd, ok := pricing.Cost(m); ok {
			p.CostUSD += usd
		} else {
			p.UnpricedTokens += m.Usage.Total()
			unknown[m.Model] = true
		}
	}
	p.Tokens = p.Usage.Total()
	p.UnknownModels = sortedKeys(unknown)
	ivs, err := st.InterventionStats(ctx, p.Since)
	if err != nil {
		return err
	}
	for _, c := range ivs {
		if c.Mode == store.ModeEnforce {
			p.Interventions.Real += c.Count
		} else {
			p.Interventions.Would += c.Count
		}
	}
	spans, err := st.LoopSpans(ctx, p.Since, nudgeAt)
	if err != nil {
		return err
	}
	p.Loops = len(spans)
	// Time inside loops: per stream, the union of the loops' spans; cost:
	// the stream's usage in that time.
	byKey := map[string][]store.LoopSpan{}
	for _, sp := range spans {
		byKey[sp.SessionKey] = append(byKey[sp.SessionKey], sp)
	}
	for key, sps := range byKey {
		merged := mergeSpans(sps)
		for _, m := range merged {
			p.LoopSeconds += m[1].Sub(m[0]).Seconds()
		}
		for _, u := range rows {
			if u.SessionKey != key {
				continue
			}
			for _, m := range merged {
				if !u.TS.Before(m[0]) && !u.TS.After(m[1]) {
					if usd, ok := pricing.Cost(usageMessage(u)); ok {
						p.LoopCostUSD += usd
					}
					break
				}
			}
		}
	}
	return nil
}

// mergeSpans returns the union of loop spans as [start, end] pairs.
func mergeSpans(sps []store.LoopSpan) [][2]time.Time {
	sort.Slice(sps, func(i, j int) bool { return sps[i].Start.Before(sps[j].Start) })
	var out [][2]time.Time
	for _, sp := range sps {
		if sp.Start.IsZero() || sp.End.Before(sp.Start) {
			continue
		}
		if n := len(out); n > 0 && !sp.Start.After(out[n-1][1]) {
			if sp.End.After(out[n-1][1]) {
				out[n-1][1] = sp.End
			}
			continue
		}
		out = append(out, [2]time.Time{sp.Start, sp.End})
	}
	return out
}

func usageMessage(u store.Usage) cost.Message {
	return cost.Message{ID: u.MessageID, Model: u.Model, TS: u.TS, Speed: u.Speed, Geo: u.Geo,
		Usage: cost.Usage{Input: u.Input, Output: u.Output, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, CacheWrite1h: u.CacheWrite1h}}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func printSummaryReport(out io.Writer, r summaryReport, p *cost.Pricing) {
	fmt.Fprintln(out, "LoopGuard report (costs are API-equivalent: token usage at API prices)")
	if !r.Database {
		fmt.Fprintln(out, "\nLoopGuard has recorded nothing yet (no database). Live totals start once the hooks run (loopguard init).")
	} else {
		fmt.Fprintf(out, "\n%-28s %14s %14s %14s\n", "", "today", "last 7 days", "last 30 days")
		row := func(label string, f func(periodReport) string) {
			fmt.Fprintf(out, "%-28s", label)
			for _, pr := range r.Periods {
				fmt.Fprintf(out, " %14s", f(pr))
			}
			fmt.Fprintln(out)
		}
		row("sessions", func(pr periodReport) string { return fmt.Sprint(pr.Sessions) })
		row("tool calls", func(pr periodReport) string { return fmt.Sprint(pr.ToolCalls) })
		row("tokens", func(pr periodReport) string { return compactCount(pr.Tokens) })
		row("cost (API-equivalent)", func(pr periodReport) string { return cost.FormatUSD(pr.CostUSD) })
		row("loops caught", func(pr periodReport) string { return fmt.Sprint(pr.Loops) })
		row("interventions (real/would)", func(pr periodReport) string {
			return fmt.Sprintf("%d/%d", pr.Interventions.Real, pr.Interventions.Would)
		})
		row("time inside loops (est.)", func(pr periodReport) string {
			return shortDuration(time.Duration(pr.LoopSeconds * float64(time.Second)))
		})
		row("cost inside loops (est.)", func(pr periodReport) string { return cost.FormatUSD(pr.LoopCostUSD) })
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

func printEstimates(out io.Writer, e *estimateReport) {
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
func compactCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fG", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// sessionReport is `loopguard report <session>`.
type sessionReport struct {
	Basis     string         `json:"basis"`
	SessionID string         `json:"session_id"`
	Project   string         `json:"project"`
	Live      bool           `json:"live_data"` // LoopGuard's database has the session (real interventions)
	Totals    sessionTotals  `json:"totals"`
	Streams   []streamReport `json:"streams"`
	Estimates estimateReport `json:"estimates"`
}

type sessionTotals struct {
	Calls          int        `json:"tool_calls"`
	Failures       int        `json:"failures"`
	Turns          int        `json:"turns"`
	Tokens         int64      `json:"tokens"`
	Usage          cost.Usage `json:"usage"`
	CostUSD        float64    `json:"cost_usd"`
	UnpricedTokens int64      `json:"unpriced_tokens"`
	UnknownModels  []string   `json:"unknown_models"`
	Loops          int        `json:"loops"`
	LoopSeconds    float64    `json:"loop_seconds"`
	LoopCostUSD    float64    `json:"loop_cost_usd"`
}

type streamReport struct {
	Stream   string       `json:"stream"` // "main" or the subagent id
	Calls    int          `json:"tool_calls"`
	Failures int          `json:"failures"`
	Tokens   int64        `json:"tokens"`
	CostUSD  float64      `json:"cost_usd"`
	Turns    []turnReport `json:"turns"`
	// Other are LoopGuard's actions not tied to a loop found here (budget
	// limits, the user's stop, loops under other thresholds).
	Other []actionReport `json:"other_actions,omitempty"`
}

type turnReport struct {
	Turn           int          `json:"turn"` // 0: before the first prompt
	StartSeconds   float64      `json:"start_seconds"`
	Calls          int          `json:"tool_calls"`
	Failures       int          `json:"failures"`
	Tokens         int64        `json:"tokens"`
	CostUSD        float64      `json:"cost_usd"`
	UnpricedTokens int64        `json:"unpriced_tokens,omitempty"`
	Groups         []callGroup  `json:"calls"`
	Loops          []loopReport `json:"loops"`
}

// callGroup is a run of consecutive calls of one family or tool + target.
type callGroup struct {
	From   int    `json:"from_call"`
	To     int    `json:"to_call"`
	Label  string `json:"label"`
	Count  int    `json:"count"`
	Failed int    `json:"failed"`
}

type loopReport struct {
	Detector     string         `json:"detector"`
	LoopKey      string         `json:"loop_key"`
	Subject      string         `json:"subject"`
	FirstCall    int            `json:"first_call"`
	LastCall     int            `json:"last_call"`
	Count        int            `json:"count"`
	StartSeconds float64        `json:"start_seconds"`
	EndSeconds   float64        `json:"end_seconds"`
	CostUSD      *float64       `json:"cost_usd,omitempty"`
	Actions      []actionReport `json:"actions"`
	Outcome      string         `json:"outcome"`
}

// actionReport is one thing LoopGuard did (or would have done) about a
// loop: from the database when it watched the session live ("real" or
// "would"), else from the replay ("replay").
type actionReport struct {
	Action   string  `json:"action"`
	Kind     string  `json:"kind"` // real | would | replay
	Call     int     `json:"call,omitempty"`
	AtSecond float64 `json:"at_seconds,omitempty"`
	Detector string  `json:"detector,omitempty"`
	Outcome  string  `json:"outcome,omitempty"`
	Reason   string  `json:"reason,omitempty"`
}

func buildSessionReport(dir, path, prefix string, pricing *cost.Pricing) (*sessionReport, error) {
	streams, err := replay.Discover(path, replay.Filter{Session: prefix})
	if err != nil {
		return nil, fmt.Errorf("reading transcripts: %w", err)
	}
	ids := map[string]bool{}
	for _, s := range streams {
		ids[s.SessionID] = true
	}
	switch len(ids) {
	case 0:
		return nil, fmt.Errorf("no transcript for a session starting with %q under %s", prefix, path)
	case 1:
	default:
		return nil, fmt.Errorf("%q matches %d sessions (%s); give more of the id", prefix, len(ids), strings.Join(sortedKeys(ids), ", "))
	}
	res := replay.Run(streams, replay.Options{Thresholds: config.Default(), Timeline: true, Pricing: pricing})
	if len(res.Errors) > 0 {
		return nil, errors.New(res.Errors[0])
	}
	r := &sessionReport{Basis: replay.CostBasis, SessionID: streams[0].SessionID, Project: streams[0].Project}

	// What LoopGuard did live, if it watched the session.
	live := map[string][]store.Intervention{} // by stream key ("" main, else the agent id)
	if db := filepath.Join(dir, home.DBFile); fileExists(db) {
		if st, err := store.Open(db); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			rows, _ := st.SessionStreams(ctx, r.SessionID)
			for _, s := range rows {
				r.Live = true
				_, agent, _ := strings.Cut(s.Key[strings.IndexByte(s.Key, ':')+1:], "/")
				ivs, _ := st.Interventions(ctx, s.Key, 10000)
				live[agent] = ivs
			}
			cancel()
			st.Close()
		}
	}

	var t0 time.Time
	for _, tl := range res.Timelines {
		if len(tl) > 0 && (t0.IsZero() || tl[0].At.Before(t0)) {
			t0 = tl[0].At
		}
	}
	since := func(t time.Time) float64 {
		if t.IsZero() || t0.IsZero() {
			return 0
		}
		return t.Sub(t0).Seconds()
	}
	unknown := map[string]bool{}
	sort.Slice(streams, func(i, j int) bool { return streams[i].AgentID < streams[j].AgentID }) // main first
	for _, st := range streams {
		key := st.Key()
		sc := res.Costs.Streams[key]
		if sc == nil {
			continue
		}
		sum := sc.Summary
		sr := streamReport{Stream: "main", Tokens: sum.Usage.Total(), CostUSD: sum.CostUSD, Turns: []turnReport{}}
		if st.AgentID != "" {
			sr.Stream = st.AgentID
		}
		for _, m := range sum.Models {
			if !m.Priced && m.Model != cost.SyntheticModel {
				unknown[m.Model] = true
			}
		}
		turns := map[int]*turnReport{}
		turnOf := func(n int) *turnReport {
			t := turns[n]
			if t == nil {
				t = &turnReport{Turn: n, Groups: []callGroup{}, Loops: []loopReport{}}
				turns[n] = t
			}
			return t
		}
		for _, tc := range sum.Turns {
			t := turnOf(tc.Turn)
			t.Tokens, t.CostUSD, t.UnpricedTokens = tc.Usage.Total(), tc.CostUSD, tc.Unpriced
		}
		callTurn := map[int]int{}
		for _, c := range res.Timelines[key] {
			t := turnOf(c.Turn)
			callTurn[c.N] = c.Turn
			if t.Calls == 0 {
				t.StartSeconds = since(c.At)
			}
			t.Calls++
			sr.Calls++
			label := callLabel(c)
			g := len(t.Groups) - 1
			if g < 0 || t.Groups[g].Label != label || t.Groups[g].To != c.N-1 {
				t.Groups = append(t.Groups, callGroup{From: c.N, Label: label})
				g++
			}
			t.Groups[g].To = c.N
			t.Groups[g].Count++
			if c.Failed {
				t.Groups[g].Failed++
				t.Failures++
				sr.Failures++
			}
		}
		ivs := live[st.AgentID]
		used := map[int]bool{}
		for _, l := range res.Loops {
			if l.SessionID != st.SessionID || l.AgentID != st.AgentID {
				continue
			}
			lr := loopReport{Detector: l.Detector, LoopKey: l.LoopKey, Subject: reportSubject(l), FirstCall: l.FirstCall,
				LastCall: l.LastCall, Count: l.Count, StartSeconds: since(l.StartedAt), EndSeconds: since(l.EndedAt),
				CostUSD: l.CostUSD, Actions: []actionReport{}}
			if r.Live {
				for i, iv := range ivs {
					if iv.LoopKey == l.LoopKey && !used[i] {
						used[i] = true
						lr.Actions = append(lr.Actions, liveAction(iv, since))
					}
				}
			} else {
				for _, w := range l.WouldHave {
					lr.Actions = append(lr.Actions, actionReport{Action: w.Action, Kind: "replay", Call: w.Call})
				}
			}
			lr.Outcome = loopOutcome(lr, l)
			turnOf(callTurn[l.FirstCall]).Loops = append(turnOf(callTurn[l.FirstCall]).Loops, lr)
			r.Totals.Loops++
			r.Totals.LoopSeconds += l.DurationSeconds
		}
		r.Totals.LoopCostUSD += sc.WastedUSD
		for i, iv := range ivs {
			if !used[i] {
				sr.Other = append(sr.Other, liveAction(iv, since))
			}
		}
		for _, t := range turns {
			sr.Turns = append(sr.Turns, *t)
		}
		sort.Slice(sr.Turns, func(i, j int) bool { return sr.Turns[i].Turn < sr.Turns[j].Turn })
		r.Streams = append(r.Streams, sr)
		r.Totals.Calls += sr.Calls
		r.Totals.Failures += sr.Failures
		r.Totals.Usage = r.Totals.Usage.Add(sum.Usage)
		r.Totals.CostUSD += sum.CostUSD
		r.Totals.UnpricedTokens += sum.Unpriced.Total()
		if st.AgentID == "" {
			r.Totals.Turns = len(sr.Turns)
		}
	}
	r.Totals.Tokens = r.Totals.Usage.Total()
	r.Totals.UnknownModels = sortedKeys(unknown)
	top, largest := res.Costs.Estimates(topEstimates, nil)
	r.Estimates = estimateReport{Window: "this session", Streams: len(streams), CostUSD: res.Costs.CostUSD, Top: top, Largest: largest}
	return r, nil
}

// callLabel names a call for the timeline: the command family, or the
// tool and its file (relative, redacted).
func callLabel(c replay.Call) string {
	switch {
	case c.Family != "" && c.Tool == "bash":
		return "bash: " + c.Family
	case c.File != "":
		return c.Tool + " " + redact.String(c.File)
	}
	return c.Tool
}

func reportSubject(l *replay.Loop) string {
	switch {
	case l.File != "":
		return redact.String(l.File)
	case l.Family != "":
		return "`" + l.Family + "`"
	}
	return l.LoopKey
}

func liveAction(iv store.Intervention, since func(time.Time) float64) actionReport {
	kind := "would"
	if iv.Mode == store.ModeEnforce {
		kind = "real"
	}
	outcome := iv.Outcome
	if outcome == "" {
		outcome = "pending"
	}
	return actionReport{Action: iv.Action, Kind: kind, AtSecond: since(iv.TS), Detector: iv.Detector, Outcome: outcome,
		Reason: clip(firstLine(redact.String(iv.Reason)), 120)}
}

// loopOutcome sums up how a loop ended: the outcome LoopGuard recorded
// for its last action, or, from the replay alone, "halted" / "went on".
func loopOutcome(lr loopReport, l *replay.Loop) string {
	for i := len(lr.Actions) - 1; i >= 0; i-- {
		if o := lr.Actions[i].Outcome; o != "" && o != "pending" {
			return o
		}
	}
	for _, w := range l.WouldHave {
		if w.Action == "halt" {
			return "would have halted"
		}
	}
	switch {
	case len(lr.Actions) == 0:
		return "no action"
	case lr.Actions[0].Kind == "replay":
		return "not tracked (replay)"
	}
	return "pending"
}

func printSessionReport(out io.Writer, r *sessionReport, p *cost.Pricing) {
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

func actionsText(as []actionReport) string {
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
func offset(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	if d < 0 {
		d = 0
	}
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
