package detect

import (
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// ReadThrash is D4: the same file read again and again with no change to
// it, or the same search giving the same result, while nothing else
// progresses. It is a weak signal: the policy only ever nudges it.
type ReadThrash struct {
	Window   int
	MinReads int
}

// D4 constants (LLD §6 D4).
const (
	thrashProgressCalls = 15  // Progress.Recent window
	thrashMaxProgress   = 0.5 // fires only below this
)

// Name implements Detector.
func (d *ReadThrash) Name() string { return event.DetectorReadThrash }

// minSearches is how many identical searches fire: one fewer than reads
// (4 by default), so a repo that lowers min_reads lowers both.
func (d *ReadThrash) minSearches() int { return max(2, d.MinReads-1) }

// Observe implements Detector. On a successful read, it counts the reads
// of the same file in the window since the file was last written, minus
// the distinct ranges among them plus one: paging through a long file in
// chunks is not thrashing, reading the same part again is. On a
// successful search (grep, glob), it counts the same search with the same
// result. Both need little progress in the last 15 calls.
func (d *ReadThrash) Observe(s *session.SessionState, c *event.ToolCall) []event.Signal {
	if c.Failed() {
		return nil
	}
	var read bool
	switch c.Event.ToolName {
	case event.ToolRead:
		if c.FilePath == "" {
			return nil
		}
		read = true
	case event.ToolGrep, event.ToolGlob:
	default:
		return nil
	}
	if s.Progress.Recent(thrashProgressCalls) >= thrashMaxProgress {
		return nil
	}
	win := s.Window(d.Window)
	var seqs []uint64
	var first *event.ToolCall
	ranges := map[uint64]bool{}
	for i := len(win) - 1; i >= 0; i-- {
		w := win[i]
		if read && isWrite(w) && !w.Failed() && w.FilePath == c.FilePath {
			break
		}
		if w.Failed() || w.Event.ToolName != c.Event.ToolName {
			continue
		}
		if read && w.FilePath == c.FilePath {
			ranges[w.ActionFP] = true
		} else if read || w.ActionFP != c.ActionFP || w.ResultFP != c.ResultFP {
			continue
		}
		seqs = append(seqs, w.Seq())
		first = w
	}
	count, need, key := len(seqs), d.minSearches(), "rt:"+hex(c.ActionFP)
	if read {
		count, need, key = len(seqs)-len(ranges)+1, d.MinReads, "rt:"+c.FilePath
	}
	if count < need || first == nil {
		return nil
	}
	reverse(seqs)
	ev := event.Evidence{Family: c.Family, Seqs: seqs, Elapsed: c.Event.ReceivedAt.Sub(first.Event.ReceivedAt)}
	if read {
		ev.Path = c.FilePath
	} else {
		ev.ActionFP = c.ActionFP
	}
	return []event.Signal{{
		Detector: d.Name(),
		LoopKey:  key,
		Severity: min(0.6, 0.55+float64(0.05*float64(count-need))),
		Count:    count,
		Since:    seqs[0],
		Evidence: ev,
	}}
}
