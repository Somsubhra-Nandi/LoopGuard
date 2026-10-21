package replay

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
)

// SchemaVersion is the version of the JSON report. Bump it on any
// incompatible change; additive fields keep the version.
const SchemaVersion = 1

// Report is the replay report. The JSON form is a stable interface; see
// docs/replay-json.md.
type Report struct {
	SchemaVersion     int            `json:"schema_version"`
	GeneratedAt       time.Time      `json:"generated_at"`
	Path              string         `json:"path"`
	Since             *time.Time     `json:"since"` // null: all history
	SessionsScanned   int            `json:"sessions_scanned"`
	StreamsScanned    int            `json:"streams_scanned"`
	ToolCalls         int            `json:"tool_calls"`
	SessionsWithLoops int            `json:"sessions_with_loops"`
	TotalLoops        int            `json:"total_loops"`
	LoopsByDetector   map[string]int `json:"loops_by_detector"`
	BelowThreshold    int            `json:"below_threshold_signals"`
	WastedSeconds     float64        `json:"wasted_seconds"`
	WastedToolCalls   int            `json:"wasted_tool_calls"`
	// Cost is the usage and API-equivalent cost (absent without pricing).
	Cost       *ReportCost `json:"cost,omitempty"`
	Lines      LineStats   `json:"lines"`
	Errors     []string    `json:"errors"`
	TopLoops   []*Loop     `json:"top_loops"`
	ElapsedSec float64     `json:"elapsed_seconds"`

	// The shape of real failures (detection-free).
	FailureFamilies []FamilyFailures `json:"failure_families"` // top 10 families by failures
	FailureStreaks  []Streak         `json:"failure_streaks"`  // longest same-family failure streak per stream

	// Only with --near-misses / --soft-failures (absent otherwise, possibly
	// empty when requested).
	NearMisses        *[]NearMiss     `json:"near_misses,omitempty"`
	SoftFailures      *[]SoftFailure  `json:"soft_failures,omitempty"`
	SoftFailureCounts *map[string]int `json:"soft_failure_markers,omitempty"`
}

// ReportOptions selects what a report lists.
type ReportOptions struct {
	// Top limits TopLoops: 0 lists none, a negative number lists all.
	Top int
	// NearMisses lists this many near misses (0: off, negative: all).
	NearMisses int
	// SoftFailures lists the soft-failure calls and counts per marker.
	SoftFailures bool
	// Pricing formats costs (the display currency); nil: USD only.
	Pricing *cost.Pricing
}

// topFamilies is how many command families the failure table shows.
const topFamilies = 10

// NewReport builds a report.
func NewReport(res *Result, path string, since time.Time, opts ReportOptions, now time.Time, elapsed time.Duration) *Report {
	top := opts.Top
	r := &Report{
		SchemaVersion:     SchemaVersion,
		GeneratedAt:       now.UTC(),
		Path:              path,
		SessionsScanned:   res.Sessions,
		StreamsScanned:    res.Streams,
		ToolCalls:         res.ToolCalls,
		SessionsWithLoops: res.SessionsWithLoops,
		TotalLoops:        len(res.Loops),
		LoopsByDetector:   map[string]int{},
		BelowThreshold:    res.BelowThreshold,
		WastedSeconds:     res.WastedSeconds,
		WastedToolCalls:   res.WastedToolCalls,
		Lines:             res.Lines,
		Errors:            res.Errors,
		ElapsedSec:        elapsed.Seconds(),
	}
	if r.Errors == nil {
		r.Errors = []string{}
	}
	if !since.IsZero() {
		s := since.UTC()
		r.Since = &s
	}
	for _, l := range res.Loops {
		r.LoopsByDetector[l.Detector]++
	}
	loops := append([]*Loop(nil), res.Loops...)
	SortLoops(loops)
	if top >= 0 && len(loops) > top {
		loops = loops[:top]
	}
	r.TopLoops = loops

	r.FailureFamilies = []FamilyFailures{}
	for _, f := range res.Families {
		if f.Failures > 0 && len(r.FailureFamilies) < topFamilies {
			r.FailureFamilies = append(r.FailureFamilies, f)
		}
	}
	r.FailureStreaks = append([]Streak{}, res.Streaks...)
	if res.Costs != nil {
		r.Cost = newReportCost(res.Costs, opts.Pricing)
	}

	if opts.NearMisses != 0 {
		ns := append([]NearMiss{}, res.NearMisses...)
		if opts.NearMisses > 0 && len(ns) > opts.NearMisses {
			ns = ns[:opts.NearMisses]
		}
		r.NearMisses = &ns
	}
	if opts.SoftFailures {
		sf := append([]SoftFailure{}, res.SoftFailures...)
		counts := MarkerCounts(res.SoftFailures)
		r.SoftFailures, r.SoftFailureCounts = &sf, &counts
	}
	return r
}

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText writes the terminal report.
func (r *Report) WriteText(w io.Writer) error {
	p := &printer{w: w}
	window := "all history"
	if r.Since != nil {
		window = "since " + r.Since.Local().Format("2006-01-02 15:04")
	}
	p.f("LoopGuard replay: %s, %s\n", window, r.Path)
	p.f("Scanned %d sessions (%d streams, %s tool calls) in %.1fs\n\n",
		r.SessionsScanned, r.StreamsScanned, thousands(r.ToolCalls), r.ElapsedSec)
	p.f("  Sessions with loops  %d of %d\n", r.SessionsWithLoops, r.SessionsScanned)
	p.f("  Loops found          %d%s\n", r.TotalLoops, byDetector(r.LoopsByDetector))
	p.f("  Wasted time          %s in loops\n", humanDuration(r.WastedSeconds))
	p.f("  Wasted tool calls    %s\n", thousands(r.WastedToolCalls))
	r.writeCost(p)
	if r.Lines.Malformed > 0 || len(r.Errors) > 0 {
		p.f("  Skipped              %d malformed lines, %d unreadable files\n", r.Lines.Malformed, len(r.Errors))
	}
	if len(r.TopLoops) > 0 {
		p.f("\nTop %d loops\n", len(r.TopLoops))
		for i, l := range r.TopLoops {
			p.f("\n%2d. %s · %s · %d× over %s · %s\n", i+1, l.Detector, subject(l), l.Count,
				humanDuration(l.DurationSeconds), RepoName(l.Repo))
			p.f("    session %s, calls %d-%d (%d calls)\n", shortStream(l), l.FirstCall, l.LastCall, l.WastedToolCalls)
			for _, e := range l.ErrorLines {
				p.f("    │ %s\n", clipText(e, 160))
			}
			p.f("    LoopGuard would have: %s\n", wouldText(l.WouldHave))
			if l.CostUSD != nil && r.Cost != nil {
				p.f("    model calls in the loop: %s API-equivalent\n", r.Cost.pricing.Format(*l.CostUSD))
			}
		}
	}
	r.writeSessionCosts(p)
	r.writeFailureShape(p)
	if r.NearMisses != nil {
		r.writeNearMisses(p)
	}
	if r.SoftFailures != nil {
		r.writeSoftFailures(p)
	}
	return p.err
}

// maxStreakLines caps the per-session streak list in the terminal report.
const maxStreakLines = 10

func (r *Report) writeFailureShape(p *printer) {
	if len(r.FailureFamilies) > 0 {
		p.f("\nFailures by command family (top %d)\n", len(r.FailureFamilies))
		p.f("  %-36s %8s %6s %6s\n", "family", "failures", "runs", "soft")
		for _, f := range r.FailureFamilies {
			name := f.Family
			if f.Polling {
				name += " (polling)"
			}
			p.f("  %-36s %8d %6d %6d\n", clipText(name, 36), f.Failures, f.Runs, f.Soft)
		}
	}
	if len(r.FailureStreaks) > 0 {
		p.f("\nLongest same-family failure streak per session (any error, no success in between)\n")
		for i, s := range r.FailureStreaks {
			if i == maxStreakLines {
				p.f("  … %d more sessions\n", len(r.FailureStreaks)-maxStreakLines)
				break
			}
			p.f("  %-22s %3d× `%s`, calls %d-%d, %d distinct errors\n",
				shortID(s.SessionID, s.AgentID), s.Length, clipText(s.Family, 40), s.FromCall, s.ToCall, s.Errors)
		}
	}
}

func (r *Report) writeNearMisses(p *printer) {
	ns := *r.NearMisses
	p.f("\nNear misses (%d): closest to triggering a detector without firing\n", len(ns))
	if len(ns) == 0 {
		p.f("  none\n")
	}
	for i, n := range ns {
		p.f("\n%2d. %s · %d/%d · session %s, calls %d-%d · %s\n", i+1, n.Detector, n.Count, n.Threshold,
			shortID(n.SessionID, n.AgentID), n.FromCall, n.ToCall, RepoName(n.Repo))
		p.f("    %s\n", n.Reason)
		for _, e := range n.Evidence {
			p.f("    │ %s\n", clipText(e, 160))
		}
	}
	if len(ns) > 0 {
		p.f("\nTo record one as a missed loop: loopguard label missed <session-id> <from-call> <to-call> [note]\n")
	}
}

func (r *Report) writeSoftFailures(p *printer) {
	sf := *r.SoftFailures
	p.f("\nSoft failures (%d): exit 0, but the output has a failure marker\n", len(sf))
	if len(sf) == 0 {
		p.f("  none\n")
		return
	}
	counts := *r.SoftFailureCounts
	markers := make([]string, 0, len(counts))
	for m := range counts {
		markers = append(markers, m)
	}
	sort.Slice(markers, func(i, j int) bool {
		if counts[markers[i]] != counts[markers[j]] {
			return counts[markers[i]] > counts[markers[j]]
		}
		return markers[i] < markers[j]
	})
	p.f("  by marker:")
	for _, m := range markers {
		p.f("  %q %d", m, counts[m])
	}
	p.f("\n\n")
	for _, s := range sf {
		p.f("  %-22s call %-4d `%s`  [%s]\n", shortID(s.SessionID, s.AgentID), s.Call, clipText(s.Family, 40), strings.Join(s.Markers, ", "))
		p.f("    │ %s\n", s.Line)
	}
}

func shortID(session, agent string) string {
	if len(session) > 8 {
		session = session[:8]
	}
	if agent != "" {
		session += "/" + agent
	}
	return session
}

type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

func subject(l *Loop) string {
	switch {
	case l.Family != "" && l.File == "":
		return "`" + l.Family + "`"
	case l.File != "":
		return l.File
	}
	return l.LoopKey
}

func shortStream(l *Loop) string {
	s := l.SessionID
	if len(s) > 8 {
		s = s[:8]
	}
	if l.AgentID != "" {
		s += "/" + l.AgentID
	}
	return s
}

func wouldText(ws []Would) string {
	if len(ws) == 0 {
		return "nothing (cooldown or per-turn nudge limit)"
	}
	var parts []string
	verbs := map[string]string{"nudge": "nudged", "deny": "denied", "halt": "halted"}
	for i := 0; i < len(ws); {
		j := i
		var calls []string
		for j < len(ws) && ws[j].Action == ws[i].Action {
			calls = append(calls, fmt.Sprint(ws[j].Call))
			j++
		}
		word := "call"
		if len(calls) > 1 {
			word = "calls"
		}
		parts = append(parts, fmt.Sprintf("%s at %s %s", verbs[ws[i].Action], word, strings.Join(calls, ", ")))
		i = j
	}
	return strings.Join(parts, ", ")
}

func byDetector(m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return "   (" + strings.Join(parts, ", ") + ")"
}

func humanDuration(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// ReportCost is the usage and API-equivalent cost of a replay.
type ReportCost struct {
	Basis          string        `json:"basis"` // "API-equivalent USD"
	Tokens         int64         `json:"tokens"`
	Usage          cost.Usage    `json:"usage"`
	CostUSD        float64       `json:"cost_usd"`
	UnpricedTokens int64         `json:"unpriced_tokens"`
	UnknownModels  []string      `json:"unknown_models"`
	WastedCostUSD  float64       `json:"wasted_cost_usd"` // model calls inside loops
	Sessions       []SessionCost `json:"sessions"`        // most expensive first

	pricing *cost.Pricing
}

// maxCostSessions caps the per-session cost list in the terminal report.
const maxCostSessions = 5

func newReportCost(c *Costs, p *cost.Pricing) *ReportCost {
	return &ReportCost{Basis: CostBasis, Tokens: c.Usage.Total(), Usage: c.Usage, CostUSD: c.CostUSD,
		UnpricedTokens: c.Unpriced.Total(), UnknownModels: c.UnknownModels, WastedCostUSD: c.WastedCostUSD,
		Sessions: c.SessionCosts(), pricing: p}
}

func (r *Report) writeCost(p *printer) {
	c := r.Cost
	if c == nil {
		return
	}
	p.f("  Cost                 %s API-equivalent, %s tokens\n", c.pricing.Format(c.CostUSD), thousands(int(c.Tokens)))
	p.f("  Cost inside loops    %s\n", c.pricing.Format(c.WastedCostUSD))
	if c.UnpricedTokens > 0 {
		p.f("  Not priced           %s tokens of %s (cost unknown; add them to pricing.yaml)\n",
			thousands(int(c.UnpricedTokens)), strings.Join(c.UnknownModels, ", "))
	}
}

func (r *Report) writeSessionCosts(p *printer) {
	c := r.Cost
	if c == nil || len(c.Sessions) == 0 {
		return
	}
	n := min(len(c.Sessions), maxCostSessions)
	p.f("\nCost by session (top %d of %d, API-equivalent)\n", n, len(c.Sessions))
	for _, s := range c.Sessions[:n] {
		extra := ""
		if s.WastedCostUSD > 0 {
			extra = ", " + c.pricing.Format(s.WastedCostUSD) + " inside loops"
		}
		if s.UnpricedTokens > 0 {
			extra += fmt.Sprintf(", %s tokens not priced", thousands(int(s.UnpricedTokens)))
		}
		p.f("  %-10s %12s  %14s tokens  %s%s\n", shortID(s.SessionID, ""), c.pricing.Format(s.CostUSD), thousands(int(s.Tokens)),
			clipLeft(s.Project, 40), extra)
	}
}

// clipLeft keeps the last n characters of s, with "…" in front if cut.
func clipLeft(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}
