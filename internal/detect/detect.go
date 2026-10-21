package detect

import (
	"sort"
	"strconv"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// Detector looks for one kind of loop. Detectors are pure: they read the
// session state and the call, and never do I/O or read the clock.
type Detector interface {
	Name() string
	// Observe is called after each completed call, once the call has been
	// recorded in s.
	Observe(s *session.SessionState, c *event.ToolCall) []event.Signal
}

// Engine runs the enabled detectors and deduplicates their signals.
type Engine struct {
	detectors []Detector
}

// New returns an engine with the detectors enabled in th.
func New(th config.Thresholds) *Engine {
	d := th.Detectors
	e := &Engine{}
	if d.ExactRepeat.Enabled {
		e.detectors = append(e.detectors, &ExactRepeat{Window: d.Window, MinCount: d.ExactRepeat.MinCount})
	}
	if d.RepeatedFailure.Enabled {
		e.detectors = append(e.detectors, &RepeatedFailure{
			Window: d.Window, MinCount: d.RepeatedFailure.MinCount,
			MaxProgress: d.RepeatedFailure.MaxProgress, MaxDistance: d.SimHashMaxDistance,
		})
	}
	if d.Oscillation.Enabled {
		e.detectors = append(e.detectors, &Oscillation{
			Window: d.Window, MinRevisits: d.Oscillation.MinRevisits, RevisitWindow: d.Oscillation.RevisitWindow,
		})
	}
	if d.ReadThrash.Enabled {
		e.detectors = append(e.detectors, &ReadThrash{Window: d.Window, MinReads: d.ReadThrash.MinReads})
	}
	if d.Stall.Enabled {
		e.detectors = append(e.detectors, &Stall{Window: d.Stall.Window})
	}
	// D6 (Budget) runs apart: its inputs are totals across sessions.
	return e
}

// Detectors returns the enabled detectors.
func (e *Engine) Detectors() []Detector { return e.detectors }

// Observe runs every detector on c and returns the deduplicated signals,
// most severe first.
func (e *Engine) Observe(s *session.SessionState, c *event.ToolCall) []event.Signal {
	var sigs []event.Signal
	for _, d := range e.detectors {
		sigs = append(sigs, d.Observe(s, c)...)
	}
	return Dedup(sigs)
}

// Dedup applies LLD §6.1. Signals with the same LoopKey collapse into the
// most severe one. When D1 (exact repeat) and D2 (repeated failure) fire on
// overlapping calls, only D2 is kept, with the higher of the two
// severities: D2 has the better message, and keeping one detector's
// LoopKey stops the loop's escalation state from flipping between keys as
// the two severities grow at different rates. The result is sorted by
// severity, descending (ties by LoopKey, for determinism).
func Dedup(sigs []event.Signal) []event.Signal {
	if len(sigs) < 2 {
		return sigs
	}
	byKey := make(map[string]int, len(sigs))
	var out []event.Signal
	for _, s := range sigs {
		if i, ok := byKey[s.LoopKey]; ok {
			if s.Severity > out[i].Severity {
				out[i] = s
			}
			continue
		}
		byKey[s.LoopKey] = len(out)
		out = append(out, s)
	}
	for i := range out {
		if out[i].Detector != event.DetectorRepeatedFailure {
			continue
		}
		for j := range out {
			if out[j].Detector != event.DetectorExactRepeat || !overlap(out[i].Evidence.Seqs, out[j].Evidence.Seqs) {
				continue
			}
			if out[j].Severity > out[i].Severity {
				out[i].Severity = out[j].Severity
			}
			out[j].Detector = "" // dropped below
		}
	}
	kept := out[:0]
	for _, s := range out {
		if s.Detector != "" {
			kept = append(kept, s)
		}
	}
	sort.SliceStable(kept, func(a, b int) bool {
		if kept[a].Severity != kept[b].Severity {
			return kept[a].Severity > kept[b].Severity
		}
		return kept[a].LoopKey < kept[b].LoopKey
	})
	return kept
}

func overlap(a, b []uint64) bool {
	set := make(map[uint64]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	for _, y := range b {
		if _, ok := set[y]; ok {
			return true
		}
	}
	return false
}

func hex(v uint64) string { return strconv.FormatUint(v, 16) }

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

// appendUnique appends s to xs if it is not already there.
func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

func isWrite(c *event.ToolCall) bool {
	switch c.Event.ToolName {
	case event.ToolEdit, event.ToolWrite, event.ToolPatch:
		return true
	}
	return false
}
