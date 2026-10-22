package report

import (
	"os"
	"sort"
	"time"
)

// IdleGap is the longest gap between two events of a session that counts
// as active time; a longer one is idle (the user away, the laptop shut).
const IdleGap = 30 * time.Minute

// ActiveDuration is the time a session was active: the gaps between its
// events (tool calls, model calls, first and last hook events), each up to
// IdleGap, a longer gap counting nothing. times need not be sorted.
func ActiveDuration(times []time.Time) time.Duration {
	ts := make([]time.Time, 0, len(times))
	for _, t := range times {
		if !t.IsZero() {
			ts = append(ts, t)
		}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	var d time.Duration
	for i := 1; i < len(ts); i++ {
		if g := ts[i].Sub(ts[i-1]); g <= IdleGap {
			d += g
		}
	}
	return d
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// span returns the earliest and latest of times (zero ones ignored).
func span(times []time.Time) (first, last time.Time) {
	for _, t := range times {
		if t.IsZero() {
			continue
		}
		if first.IsZero() || t.Before(first) {
			first = t
		}
		if t.After(last) {
			last = t
		}
	}
	return first, last
}
