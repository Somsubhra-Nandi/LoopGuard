package report

import (
	"testing"
	"time"
)

func TestActiveDuration(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	cases := []struct {
		times []time.Time
		want  time.Duration
	}{
		{nil, 0},
		{[]time.Time{at(0)}, 0},
		{[]time.Time{at(10), at(0), at(5)}, 10 * time.Minute},            // unsorted
		{[]time.Time{at(0), at(30), at(31)}, 31 * time.Minute},           // 30 min exactly is active
		{[]time.Time{at(0), at(10), at(200), at(215)}, 25 * time.Minute}, // 190 min away: idle
		{[]time.Time{{}, at(0), at(2)}, 2 * time.Minute},                 // no time: ignored
	}
	for i, c := range cases {
		if got := ActiveDuration(c.times); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}
