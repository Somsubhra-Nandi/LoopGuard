package detect

import (
	"slices"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// Oscillation is D3: a file flip-flops between versions.
type Oscillation struct {
	Window        int
	MinRevisits   int
	RevisitWindow int
}

// Name implements Detector.
func (d *Oscillation) Name() string { return event.DetectorOscillation }

// Observe implements Detector. It runs on successful writes and checks two
// patterns in the file's history inside the detector window, firing only
// when c itself is part of the pattern:
//
//   - inverse edits: an edit (old=A, new=B) that undoes an earlier
//     (old=B, new=A), at least MinRevisits times;
//   - state revisits: a content hash after a write that equals one seen at
//     least two writes earlier (A→B→A), at least MinRevisits times within
//     the last RevisitWindow writes.
//
// When both patterns fire, the more severe signal is returned.
func (d *Oscillation) Observe(s *session.SessionState, c *event.ToolCall) []event.Signal {
	if !isWrite(c) || c.Failed() || c.FilePath == "" {
		return nil
	}
	fh := s.Files[c.FilePath]
	if fh == nil {
		return nil
	}
	start := s.WindowStart(d.Window)
	inv, invSeqs := inverseEdits(fh.Edits, start, c.Seq())
	rev, revSeqs := revisits(fh.Writes, start, c.Seq(), d.RevisitWindow)

	var best *event.Signal
	consider := func(count int, seqs []uint64, base float64) {
		if count < d.MinRevisits {
			return
		}
		sig := event.Signal{
			Detector: d.Name(),
			LoopKey:  "os:" + c.FilePath,
			Severity: clamp01(base + 0.1*float64(count-d.MinRevisits)),
			Count:    count,
			Since:    seqs[0],
			Evidence: event.Evidence{Path: c.FilePath, Files: []string{c.FilePath}, Seqs: seqs},
		}
		if best == nil || sig.Severity > best.Severity {
			best = &sig
		}
	}
	consider(inv, invSeqs, 0.7)
	consider(rev, revSeqs, 0.6)
	if best == nil {
		return nil
	}
	if first := callAt(s, best.Since); first != nil {
		best.Evidence.Elapsed = c.Event.ReceivedAt.Sub(first.Event.ReceivedAt)
	}
	return []event.Signal{*best}
}

// inverseEdits counts edits since start that undo an earlier edit, and
// returns 0 unless the edit at cur is one of them. seqs lists the seqs
// involved (undone edits and their inverses), oldest first.
func inverseEdits(edits []session.EditRecord, start, cur uint64) (int, []uint64) {
	count := 0
	curIsInverse := false
	involved := make(map[uint64]bool)
	for j := range edits {
		if edits[j].Seq < start {
			continue
		}
		for i := 0; i < j; i++ {
			if edits[i].Seq < start || edits[i].Seq == edits[j].Seq {
				continue
			}
			if edits[i].Pair.Old == edits[j].Pair.New && edits[i].Pair.New == edits[j].Pair.Old {
				count++
				involved[edits[i].Seq] = true
				involved[edits[j].Seq] = true
				if edits[j].Seq == cur {
					curIsInverse = true
				}
				break
			}
		}
	}
	if !curIsInverse {
		return 0, nil
	}
	return count, sortedSeqs(involved)
}

// revisits counts writes among the last max (and since start) whose content
// hash equals one at least two writes earlier, and returns 0 unless the
// write at cur is one of them.
func revisits(writes []session.HashRecord, start, cur uint64, max int) (int, []uint64) {
	if len(writes) > max {
		writes = writes[len(writes)-max:]
	}
	lo := 0
	for lo < len(writes) && writes[lo].Seq < start {
		lo++
	}
	writes = writes[lo:]
	count := 0
	curIsRevisit := false
	involved := make(map[uint64]bool)
	for j := 2; j < len(writes); j++ {
		if writes[j].Hash == writes[j-1].Hash {
			continue // rewriting the same content is not a revisit
		}
		for i := j - 2; i >= 0; i-- {
			if writes[i].Hash == writes[j].Hash {
				count++
				involved[writes[i].Seq] = true
				involved[writes[j].Seq] = true
				if writes[j].Seq == cur {
					curIsRevisit = true
				}
				break
			}
		}
	}
	if !curIsRevisit {
		return 0, nil
	}
	return count, sortedSeqs(involved)
}

func sortedSeqs(m map[uint64]bool) []uint64 {
	out := make([]uint64, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// callAt returns the recorded call with the given seq, if still held.
func callAt(s *session.SessionState, seq uint64) *event.ToolCall {
	for i := s.Calls.Len() - 1; i >= 0; i-- {
		c := s.Calls.At(i)
		if c.Seq() == seq {
			return c
		}
		if c.Seq() < seq {
			break
		}
	}
	return nil
}
