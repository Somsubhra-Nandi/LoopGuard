package report

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// TopEstimates is how many groups the "by estimated cost" tables list.
const TopEstimates = 10

// DoctorSession is the session id of `loopguard doctor`'s synthetic
// events; totals and lists leave it out.
const DoctorSession = "loopguard-doctor"

// EstimateReport ranks tool calls by estimated cost.
type EstimateReport struct {
	Window  string                  `json:"window"`
	Streams int                     `json:"streams"`
	CostUSD float64                 `json:"cost_usd"` // the real cost the shares are of
	Top     []replay.GroupEstimate  `json:"top"`
	Largest []replay.OutputEstimate `json:"largest_outputs"`
}

// Summary is `loopguard report` without a session.
type Summary struct {
	Basis     string          `json:"basis"`
	Database  bool            `json:"database"` // false: LoopGuard has recorded nothing yet
	Periods   []Period        `json:"periods"`
	Estimates *EstimateReport `json:"estimates,omitempty"` // last 7 days, from the transcripts
	Errors    []string        `json:"errors,omitempty"`
}

// Period is one column of the summary: today, the last 7 days, or the
// last 30 days.
type Period struct {
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
	// Recovery rates of the period's interventions, as `loopguard stats`
	// computes them (nil: no outcome decided yet).
	RecoveryReal *float64 `json:"recovery_rate_real,omitempty"`
	RecoveryWH   *float64 `json:"recovery_rate_would_have,omitempty"`
	// LoopSeconds and LoopCostUSD estimate the time and cost inside loops:
	// from each loop's first call to its last (overlaps once), and the
	// usage recorded in that time.
	LoopSeconds float64 `json:"est_loop_seconds"`
	LoopCostUSD float64 `json:"est_loop_cost_usd"`
}

// Day is one day's usage for the dashboard's chart.
type Day struct {
	Date        time.Time `json:"date"` // local midnight
	Tokens      int64     `json:"tokens"`
	CostUSD     float64   `json:"cost_usd"`
	LoopCostUSD float64   `json:"est_loop_cost_usd"`
}

// NudgeAt is the severity from which a stored signal counts as a loop:
// the default nudge threshold.
func NudgeAt() float64 { return config.Default().Policy.NudgeThreshold }

// Midnight is the start of t's day in t's location.
func Midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// BuildSummary fills today, the last 7 days, and the last 30 days (before
// at) from the database; st nil means there is none yet (all zero). The
// estimates are separate (TopCosts): they come from the transcripts.
func BuildSummary(ctx context.Context, st *store.Store, pricing *cost.Pricing, at time.Time, nudgeAt float64) Summary {
	s, _ := Overview(ctx, st, pricing, at, 0, nudgeAt)
	return s
}

// Overview is BuildSummary plus the last n days (today included, in at's
// location) by day, all from one read of the database: the days use the
// same priced rows and loop spans as the periods, so over the days the
// costs add up to a period's that starts at the first day's midnight.
func Overview(ctx context.Context, st *store.Store, pricing *cost.Pricing, at time.Time, n int, nudgeAt float64) (Summary, []Day) {
	r := Summary{Basis: replay.CostBasis, Database: st != nil}
	r.Periods = []Period{
		{Name: "today", Since: Midnight(at)},
		{Name: "7d", Since: at.Add(-7 * 24 * time.Hour)},
		{Name: "30d", Since: at.Add(-30 * 24 * time.Hour)},
	}
	today := Midnight(at)
	days := make([]Day, n)
	since := r.Periods[2].Since
	for i := range days {
		days[i].Date = time.Date(today.Year(), today.Month(), today.Day()-(n-1-i), 0, 0, 0, 0, at.Location())
		if days[i].Date.Before(since) {
			since = days[i].Date
		}
	}
	if st == nil {
		return r, days
	}
	data, err := loadSummary(ctx, st, since, pricing, nudgeAt)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r, days
	}
	for i := range r.Periods {
		if err := fillPeriod(ctx, st, &r.Periods[i], data); err != nil {
			r.Errors = append(r.Errors, err.Error())
		}
	}
	if n > 0 {
		fillDays(days, data)
	}
	return r, days
}

// summaryData is what the periods and days are computed from, read once
// for the earliest of them: the sessions last seen since then, the usage
// rows (priced), and the loop spans.
type summaryData struct {
	sessions []store.Session
	rows     []store.Usage
	priced   []PricedRow
	spans    []store.LoopSpan
}

func loadSummary(ctx context.Context, st *store.Store, since time.Time, pricing *cost.Pricing, nudgeAt float64) (summaryData, error) {
	var d summaryData
	var err error
	if d.sessions, err = st.ListSessions(ctx, store.ListOptions{Since: since, Limit: 1_000_000}); err != nil {
		return d, err
	}
	if d.rows, err = st.UsageRows(ctx, store.UsageFilter{Since: since}); err != nil {
		return d, err
	}
	d.priced = Price(d.rows, pricing)
	d.spans, err = st.LoopSpans(ctx, since, nudgeAt)
	return d, err
}

// since returns the rows at or after t (rows are oldest first) and the
// spans of streams last seen at or after t.
func (d summaryData) since(t time.Time) (rows []store.Usage, priced []PricedRow, spans []store.LoopSpan) {
	i := sort.Search(len(d.rows), func(i int) bool { return !d.rows[i].TS.Before(t) })
	for _, sp := range d.spans {
		if !sp.LastSeen.Before(t) {
			spans = append(spans, sp)
		}
	}
	return d.rows[i:], d.priced[i:], spans
}

func fillPeriod(ctx context.Context, st *store.Store, p *Period, d summaryData) error {
	ids := map[string]bool{}
	for _, s := range d.sessions {
		if s.SessionID != DoctorSession && !s.LastSeenAt.Before(p.Since) {
			ids[s.SessionID] = true
		}
	}
	p.Sessions = len(ids)
	calls, err := st.CallCounts(ctx, p.Since)
	if err != nil {
		return err
	}
	for key, n := range calls {
		if _, id, _ := strings.Cut(key, ":"); id != DoctorSession {
			p.ToolCalls += n
		}
	}
	rows, priced, spans := d.since(p.Since)
	unknown := map[string]bool{}
	for i, u := range rows {
		p.Usage = p.Usage.Add(priced[i].Usage)
		if priced[i].Priced {
			p.CostUSD += priced[i].USD
		} else {
			p.UnpricedTokens += priced[i].Usage.Total()
			unknown[u.Model] = true
		}
	}
	p.Tokens = p.Usage.Total()
	p.UnknownModels = SortedKeys(unknown)
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
	s := BuildStats(ivs, p.Since)
	p.RecoveryReal, p.RecoveryWH = s.RecoveryReal, s.RecoveryWH
	p.Loops = len(spans)
	merged := MergeSpansByStream(spans)
	for _, m := range merged {
		for _, sp := range m {
			p.LoopSeconds += sp[1].Sub(sp[0]).Seconds()
		}
	}
	for i, in := range InLoops(rows, merged) {
		if in && priced[i].Priced {
			p.LoopCostUSD += priced[i].USD
		}
	}
	return nil
}

// fillDays splits the usage since the first day by day.
func fillDays(days []Day, data summaryData) {
	rows, priced, spans := data.since(days[0].Date)
	in := InLoops(rows, MergeSpansByStream(spans))
	d := 0
	for i, u := range rows { // oldest first
		for d+1 < len(days) && !u.TS.Before(days[d+1].Date) {
			d++
		}
		days[d].Tokens += priced[i].Usage.Total()
		if priced[i].Priced {
			days[d].CostUSD += priced[i].USD
			if in[i] {
				days[d].LoopCostUSD += priced[i].USD
			}
		}
	}
}

// PricedRow is a usage row priced with the current table.
type PricedRow struct {
	Usage  cost.Usage
	USD    float64
	Priced bool
}

// Price prices usage rows with the current table (costs are re-priced
// when read, LLD §8.2).
func Price(rows []store.Usage, pricing *cost.Pricing) []PricedRow {
	out := make([]PricedRow, len(rows))
	for i, u := range rows {
		m := UsageMessage(u)
		usd, ok := pricing.Cost(m)
		out[i] = PricedRow{Usage: m.Usage, USD: usd, Priced: ok}
	}
	return out
}

// MergeSpansByStream returns, per stream (session key), the union of its
// loops' spans as [start, end] pairs.
func MergeSpansByStream(spans []store.LoopSpan) map[string][][2]time.Time {
	byKey := map[string][]store.LoopSpan{}
	for _, sp := range spans {
		byKey[sp.SessionKey] = append(byKey[sp.SessionKey], sp)
	}
	out := make(map[string][][2]time.Time, len(byKey))
	for k, sps := range byKey {
		out[k] = MergeSpans(sps)
	}
	return out
}

// InLoops reports for each usage row whether it was recorded inside one of
// its stream's merged loop spans.
func InLoops(rows []store.Usage, merged map[string][][2]time.Time) []bool {
	out := make([]bool, len(rows))
	for i, u := range rows {
		for _, m := range merged[u.SessionKey] {
			if !u.TS.Before(m[0]) && !u.TS.After(m[1]) {
				out[i] = true
				break
			}
		}
	}
	return out
}

// MergeSpans returns the union of loop spans as [start, end] pairs.
func MergeSpans(sps []store.LoopSpan) [][2]time.Time {
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

// TopCosts ranks the tool calls of the last 7 days (before at) by
// estimated cost, from the transcripts (the database keeps no output
// sizes). With a database it counts what the summary's 7 days count: the
// sessions with usage recorded since then, and only their model calls made
// since then; its total is the summary's 7-day cost. Without one, every
// transcript changed in the 7 days counts, whole.
func TopCosts(ctx context.Context, st *store.Store, path string, pricing *cost.Pricing, at time.Time) (*EstimateReport, error) {
	since := at.Add(-7 * 24 * time.Hour)
	streams, err := replay.Discover(path, replay.Filter{Since: since})
	if err != nil {
		return nil, err
	}
	if st == nil {
		res := replay.Run(streams, replay.Options{Thresholds: config.Default(), Pricing: pricing})
		top, largest := res.Costs.Estimates(TopEstimates, nil)
		return &EstimateReport{Window: "transcripts changed in the last 7 days", Streams: len(streams),
			CostUSD: res.Costs.CostUSD, Top: top, Largest: largest}, nil
	}
	rows, err := st.UsageRows(ctx, store.UsageFilter{Since: since})
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	var total float64
	for i, p := range Price(rows, pricing) {
		known[rows[i].SessionID] = true
		if p.Priced {
			total += p.USD
		}
	}
	kept := streams[:0]
	for _, s := range streams {
		if known[s.SessionID] {
			kept = append(kept, s)
		}
	}
	res := replay.Run(kept, replay.Options{Thresholds: config.Default(), Pricing: pricing, CostFrom: since})
	top, largest := res.Costs.Estimates(TopEstimates, nil)
	// Shares of the recorded total (a session whose transcript is gone
	// counts in it, not in the estimates).
	for i := range top {
		top[i].EstShare = share(top[i].EstUSD, total)
	}
	for i := range largest {
		largest[i].EstShare = share(largest[i].EstUSD, total)
	}
	return &EstimateReport{Window: "last 7 days", Streams: len(kept), CostUSD: total, Top: top, Largest: largest}, nil
}

func share(part, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return part / total
}

// UsageMessage turns a stored usage row back into a message to price.
func UsageMessage(u store.Usage) cost.Message {
	return cost.Message{ID: u.MessageID, Model: u.Model, TS: u.TS, Speed: u.Speed, Geo: u.Geo,
		Usage: cost.Usage{Input: u.Input, Output: u.Output, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, CacheWrite1h: u.CacheWrite1h}}
}

// SortedKeys returns a set's keys, sorted.
func SortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
