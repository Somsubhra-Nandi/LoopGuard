package detect

import (
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// ExactRepeat is D1: the same action with the same result, over and over,
// with nothing changed in between.
type ExactRepeat struct {
	Window   int
	MinCount int
}

// Name implements Detector.
func (d *ExactRepeat) Name() string { return event.DetectorExactRepeat }

// Observe implements Detector. It fires when c's (ActionFP, ResultFP) pair
// occurs at least MinCount times in the window since the last file
// modification. A write in between means the repeat is a re-check, not a
// no-op; repeated failures across edits are D2's job. Reads and searches
// are left to D4, which never escalates past a nudge, and polling commands
// are exempt.
func (d *ExactRepeat) Observe(s *session.SessionState, c *event.ToolCall) []event.Signal {
	if c.Polling || isWrite(c) {
		return nil
	}
	switch c.Event.ToolName {
	case event.ToolRead, event.ToolGrep, event.ToolGlob:
		return nil
	}
	win := s.Window(d.Window)
	var seqs []uint64
	var first *event.ToolCall
	for i := len(win) - 1; i >= 0; i-- {
		w := win[i]
		if isWrite(w) && !w.Failed() {
			break
		}
		if w.ActionFP == c.ActionFP && w.ResultFP == c.ResultFP {
			seqs = append(seqs, w.Seq())
			first = w
		}
	}
	count := len(seqs)
	if count < d.MinCount {
		return nil
	}
	reverse(seqs)
	ev := event.Evidence{
		Family:   c.Family,
		ActionFP: c.ActionFP,
		Seqs:     seqs,
		Elapsed:  c.Event.ReceivedAt.Sub(first.Event.ReceivedAt),
	}
	if c.Command != "" {
		ev.Commands = []string{c.Command}
	}
	if c.ErrorSig != nil {
		ev.ErrorLines = c.ErrorSig.Lines
	}
	return []event.Signal{{
		Detector: d.Name(),
		LoopKey:  "er:" + hex(c.ActionFP),
		Severity: clamp01(0.5 + float64(0.15*float64(count-d.MinCount))),
		Count:    count,
		Since:    seqs[0],
		Evidence: ev,
	}}
}

func reverse[T any](xs []T) {
	for i, j := 0, len(xs)-1; i < j; i, j = i+1, j-1 {
		xs[i], xs[j] = xs[j], xs[i]
	}
}
