package detect

import (
	"strconv"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// Stall is D5: a long stretch of calls with no visible progress, many of
// them failures or repeated reads. It is lower-confidence than the loop
// detectors: the policy nudges it once per turn, and it can notify the
// human, but it never denies or halts.
type Stall struct {
	Window int
}

// D5 constants (LLD §6 D5).
const (
	stallMinBad      = 8   // failures or repeated reads among the window's calls
	stallMaxProgress = 0.5 // fires only below this
	// exploreScore is what a call that looked at something new adds to the
	// stall detector's progress: research (reading new files, fetching new
	// URLs, new searches) is not a stall, but one or two new reads in a
	// stuck stretch don't end it either.
	exploreScore = 0.2
)

// Name implements Detector.
func (d *Stall) Name() string { return event.DetectorStall }

// Observe implements Detector. It fires when the last Window calls of the
// turn (it needs that many) made less than stallMaxProgress progress,
// counting exploration (ToolCall.Explore) as progress, and at least
// stallMinBad of them failed or repeated an earlier read, fetch, or
// search. While a loop detector's loop is being handled (nudged or past),
// it stays quiet: that loop's message is more specific. A stall that goes
// on signals again only after another Window calls.
func (d *Stall) Observe(s *session.SessionState, c *event.ToolCall) []event.Signal {
	win := s.Window(d.Window)
	if len(win) < d.Window {
		return nil
	}
	key := "st:" + strconv.FormatUint(s.TurnSeq, 10)
	for _, l := range s.Loops {
		switch {
		case l.Key == key:
			if s.CallCount-l.LastSignalCall < uint64(d.Window) {
				return nil
			}
		case l.Stage != session.StageObserving && canEscalate(l.Detector):
			return nil
		}
	}
	progress := 0.0
	var fails, repeats int
	for _, w := range win {
		progress += w.Progress
		if w.Explore {
			progress += exploreScore
		}
		switch {
		case w.Polling:
		case w.Failed():
			fails++
		case Looks(w) && !w.Explore:
			repeats++
		}
	}
	if progress >= stallMaxProgress || fails+repeats < stallMinBad {
		return nil
	}
	seqs := make([]uint64, len(win))
	for i, w := range win {
		seqs[i] = w.Seq()
	}
	return []event.Signal{{
		Detector: d.Name(),
		LoopKey:  key,
		Severity: 0.55,
		Count:    len(win),
		Since:    seqs[0],
		Evidence: event.Evidence{
			Seqs:     seqs,
			Elapsed:  c.Event.ReceivedAt.Sub(win[0].Event.ReceivedAt),
			Failures: fails,
			Repeats:  repeats,
		},
	}}
}

// Looks reports whether a call only looks at something: a read, a search,
// a fetch, or a read-only command.
func Looks(c *event.ToolCall) bool {
	switch c.Event.ToolName {
	case event.ToolRead, event.ToolGrep, event.ToolGlob, "other:WebFetch", "other:WebSearch":
		return true
	case event.ToolBash:
		return c.ReadOnly
	}
	return false
}

// canEscalate reports whether a detector's loops can be denied and halted
// (D1-D3): those have their own, more specific messages.
func canEscalate(detector string) bool {
	switch detector {
	case event.DetectorExactRepeat, event.DetectorRepeatedFailure, event.DetectorOscillation:
		return true
	}
	return false
}
