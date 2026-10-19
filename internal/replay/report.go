package replay

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
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
	// TODO(M4): API-equivalent cost of the wasted calls, from transcript usage.
	Lines      LineStats `json:"lines"`
	Errors     []string  `json:"errors"`
	TopLoops   []*Loop   `json:"top_loops"`
	ElapsedSec float64   `json:"elapsed_seconds"`
}

// NewReport builds a report. top limits TopLoops: 0 lists none, a negative
// number lists all.
func NewReport(res *Result, path string, since time.Time, top int, now time.Time, elapsed time.Duration) *Report {
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
	p.f("  Cost                 not computed yet (milestone M4)\n")
	if r.Lines.Malformed > 0 || len(r.Errors) > 0 {
		p.f("  Skipped              %d malformed lines, %d unreadable files\n", r.Lines.Malformed, len(r.Errors))
	}
	if len(r.TopLoops) == 0 {
		return p.err
	}
	p.f("\nTop %d loops\n", len(r.TopLoops))
	for i, l := range r.TopLoops {
		p.f("\n%2d. %s · %s · %d× over %s · %s\n", i+1, l.Detector, subject(l), l.Count,
			humanDuration(l.DurationSeconds), RepoName(l.Repo))
		p.f("    session %s, calls %d-%d (%d calls)\n", shortStream(l), l.FirstCall, l.LastCall, l.WastedToolCalls)
		for _, e := range l.ErrorLines {
			p.f("    │ %s\n", clipText(e, 160))
		}
		p.f("    LoopGuard would have: %s\n", wouldText(l.WouldHave))
	}
	return p.err
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
