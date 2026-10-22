package report

import (
	"sort"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/policy"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// Stats is `loopguard stats --json`.
type Stats struct {
	Since     time.Time       `json:"since,omitzero"`
	Total     int64           `json:"total"`
	ByKind    []KindStats     `json:"by_kind"`
	Detectors []DetectorStats `json:"detectors"`
	// Real and WouldHave add up the detectors' outcomes, with their
	// recovery rates.
	Real         OutcomeCounts `json:"real"`
	WouldHave    OutcomeCounts `json:"would_have"`
	RecoveryReal *float64      `json:"recovery_rate_real,omitempty"`
	RecoveryWH   *float64      `json:"recovery_rate_would_have,omitempty"`
}

// KindStats counts one action, real (it reached the agent) and would-have.
type KindStats struct {
	Action    string `json:"action"`
	Real      int64  `json:"real"`
	WouldHave int64  `json:"would_have"`
}

// DetectorStats is one detector's interventions and outcomes. Recovery
// rates are recovered / (recovered + moved_on + continued_loop + halted),
// apart for real and would-have interventions: outcomes still pending or
// unknown don't count, and moved_on (the loop went quiet without the
// command recovering) counts against.
type DetectorStats struct {
	Detector     string           `json:"detector"`
	Actions      map[string]int64 `json:"actions"`
	Real         OutcomeCounts    `json:"real"`
	WouldHave    OutcomeCounts    `json:"would_have"`
	RecoveryReal *float64         `json:"recovery_rate_real,omitempty"`
	RecoveryWH   *float64         `json:"recovery_rate_would_have,omitempty"`
}

// OutcomeCounts counts interventions by outcome.
type OutcomeCounts struct {
	Recovered int64 `json:"recovered"`
	MovedOn   int64 `json:"moved_on"`
	Continued int64 `json:"continued_loop"`
	Halted    int64 `json:"halted"`
	Unknown   int64 `json:"unknown"`
	Pending   int64 `json:"pending"`
	NA        int64 `json:"not_applicable,omitempty"` // self-protection: no loop to recover from
}

func (o *OutcomeCounts) add(outcome string, n int64) {
	switch outcome {
	case policy.OutcomeRecovered:
		o.Recovered += n
	case policy.OutcomeMovedOn:
		o.MovedOn += n
	case policy.OutcomeContinued:
		o.Continued += n
	case policy.OutcomeNA:
		o.NA += n
	case policy.OutcomeHalted:
		o.Halted += n
	case policy.OutcomeUnknown:
		o.Unknown += n
	default:
		o.Pending += n
	}
}

// Rate is the recovery rate, nil while no outcome is decided.
func (o OutcomeCounts) Rate() *float64 {
	decided := o.Recovered + o.MovedOn + o.Continued + o.Halted
	if decided == 0 {
		return nil
	}
	r := float64(o.Recovered) / float64(decided)
	return &r
}

// BuildStats adds up intervention counts (store.InterventionStats).
func BuildStats(rows []store.InterventionCount, since time.Time) Stats {
	r := Stats{Since: since, ByKind: []KindStats{}, Detectors: []DetectorStats{}}
	kinds := map[string]*KindStats{}
	dets := map[string]*DetectorStats{}
	for _, c := range rows {
		r.Total += c.Count
		k := kinds[c.Action]
		if k == nil {
			k = &KindStats{Action: c.Action}
			kinds[c.Action] = k
		}
		name := c.Detector
		if name == "" {
			name = "?"
		}
		d := dets[name]
		if d == nil {
			d = &DetectorStats{Detector: name, Actions: map[string]int64{}}
			dets[name] = d
		}
		d.Actions[c.Action] += c.Count
		if c.Mode == store.ModeEnforce {
			k.Real += c.Count
			d.Real.add(c.Outcome, c.Count)
			r.Real.add(c.Outcome, c.Count)
		} else {
			k.WouldHave += c.Count
			d.WouldHave.add(c.Outcome, c.Count)
			r.WouldHave.add(c.Outcome, c.Count)
		}
	}
	for _, a := range []string{"nudge", "deny", "halt"} {
		if k := kinds[a]; k != nil {
			r.ByKind = append(r.ByKind, *k)
		} else {
			r.ByKind = append(r.ByKind, KindStats{Action: a})
		}
	}
	for _, d := range dets {
		d.RecoveryReal, d.RecoveryWH = d.Real.Rate(), d.WouldHave.Rate()
		r.Detectors = append(r.Detectors, *d)
	}
	sort.Slice(r.Detectors, func(i, j int) bool { return r.Detectors[i].Detector < r.Detectors[j].Detector })
	r.RecoveryReal, r.RecoveryWH = r.Real.Rate(), r.WouldHave.Rate()
	return r
}
