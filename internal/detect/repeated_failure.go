package detect

import (
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// RepeatedFailure is D2, the headline detector: the same family keeps
// failing with the same error, whatever is edited in between.
type RepeatedFailure struct {
	Window      int
	MinCount    int
	MaxProgress float64
	MaxDistance int
}

// Name implements Detector.
func (d *RepeatedFailure) Name() string { return event.DetectorRepeatedFailure }

// Observe implements Detector. Walking back from c, it counts failures of
// c's family with the same error as c, stopping at the first success of
// the family. Failures with a different error neither count nor stop the
// walk. It fires when the count reaches MinCount and the progress made
// after the first counted failure stays below MaxProgress.
func (d *RepeatedFailure) Observe(s *session.SessionState, c *event.ToolCall) []event.Signal {
	if !c.Failed() || c.Polling || c.Family == "" {
		return nil
	}
	win := s.Window(d.Window)
	var fails []*event.ToolCall // newest first
	firstIdx := len(win)
	for i := len(win) - 1; i >= 0; i-- {
		w := win[i]
		if w.Family != c.Family {
			continue
		}
		if !w.Failed() {
			break
		}
		if normalize.SameError(w.ErrorSig, c.ErrorSig, d.MaxDistance) {
			fails = append(fails, w)
			firstIdx = i
		}
	}
	count := len(fails)
	if count < d.MinCount {
		return nil
	}
	first := fails[count-1]
	if s.Progress.Since(first.Seq()) >= d.MaxProgress {
		return nil
	}

	seqs := make([]uint64, count)
	var cmds []string
	for i, f := range fails {
		seqs[count-1-i] = f.Seq()
	}
	for i := count - 1; i >= 0; i-- {
		if fails[i].Command != "" {
			cmds = appendUnique(cmds, fails[i].Command)
		}
	}

	// Files edited between attempts, and whether any file was edited in
	// more than one gap between attempts.
	var files []string
	gapsByFile := make(map[string]int)
	gap := make(map[string]bool)
	for _, w := range win[firstIdx:] {
		if w.Family == c.Family && w.Failed() {
			for f := range gap {
				gapsByFile[f]++
			}
			gap = make(map[string]bool)
			continue
		}
		if isWrite(w) && w.FilePath != "" && !w.Failed() {
			gap[w.FilePath] = true
			files = appendUnique(files, w.FilePath)
		}
	}
	sev := 0.55 + 0.1*float64(count-d.MinCount)
	for _, n := range gapsByFile {
		if n >= 2 {
			sev += 0.1
			break
		}
	}

	return []event.Signal{{
		Detector: d.Name(),
		LoopKey:  "rf:" + c.Family + ":" + hex(first.ErrorSig.Exact),
		Severity: clamp01(sev),
		Count:    count,
		Since:    first.Seq(),
		Evidence: event.Evidence{
			Family:     c.Family,
			Commands:   cmds,
			ErrorLines: c.ErrorSig.Lines,
			Files:      files,
			Seqs:       seqs,
			Elapsed:    c.Event.ReceivedAt.Sub(first.Event.ReceivedAt),
		},
	}}
}
