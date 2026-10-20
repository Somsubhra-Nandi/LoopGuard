package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/policy"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// statsReport is `loopguard stats --json`.
type statsReport struct {
	Since     time.Time       `json:"since,omitzero"`
	Total     int64           `json:"total"`
	ByKind    []kindStats     `json:"by_kind"`
	Detectors []detectorStats `json:"detectors"`
}

// kindStats counts one action, real (enforce) and would-have (observe).
type kindStats struct {
	Action    string `json:"action"`
	Real      int64  `json:"real"`
	WouldHave int64  `json:"would_have"`
}

// detectorStats is one detector's interventions and outcomes. Recovery
// rates are recovered / (recovered + continued_loop + halted), apart for
// real and would-have interventions: outcomes still pending or unknown
// don't count.
type detectorStats struct {
	Detector     string           `json:"detector"`
	Actions      map[string]int64 `json:"actions"`
	Real         outcomeCounts    `json:"real"`
	WouldHave    outcomeCounts    `json:"would_have"`
	RecoveryReal *float64         `json:"recovery_rate_real,omitempty"`
	RecoveryWH   *float64         `json:"recovery_rate_would_have,omitempty"`
}

type outcomeCounts struct {
	Recovered int64 `json:"recovered"`
	Continued int64 `json:"continued_loop"`
	Halted    int64 `json:"halted"`
	Unknown   int64 `json:"unknown"`
	Pending   int64 `json:"pending"`
}

func (o *outcomeCounts) add(outcome string, n int64) {
	switch outcome {
	case policy.OutcomeRecovered:
		o.Recovered += n
	case policy.OutcomeContinued:
		o.Continued += n
	case policy.OutcomeHalted:
		o.Halted += n
	case policy.OutcomeUnknown:
		o.Unknown += n
	default:
		o.Pending += n
	}
}

func (o outcomeCounts) rate() *float64 {
	decided := o.Recovered + o.Continued + o.Halted
	if decided == 0 {
		return nil
	}
	r := float64(o.Recovered) / float64(decided)
	return &r
}

func buildStats(rows []store.InterventionCount, since time.Time) statsReport {
	r := statsReport{Since: since, ByKind: []kindStats{}, Detectors: []detectorStats{}}
	kinds := map[string]*kindStats{}
	dets := map[string]*detectorStats{}
	for _, c := range rows {
		r.Total += c.Count
		k := kinds[c.Action]
		if k == nil {
			k = &kindStats{Action: c.Action}
			kinds[c.Action] = k
		}
		name := c.Detector
		if name == "" {
			name = "?"
		}
		d := dets[name]
		if d == nil {
			d = &detectorStats{Detector: name, Actions: map[string]int64{}}
			dets[name] = d
		}
		d.Actions[c.Action] += c.Count
		if c.Mode == store.ModeEnforce {
			k.Real += c.Count
			d.Real.add(c.Outcome, c.Count)
		} else {
			k.WouldHave += c.Count
			d.WouldHave.add(c.Outcome, c.Count)
		}
	}
	for _, a := range []string{"nudge", "deny", "halt"} {
		if k := kinds[a]; k != nil {
			r.ByKind = append(r.ByKind, *k)
		} else {
			r.ByKind = append(r.ByKind, kindStats{Action: a})
		}
	}
	for _, d := range dets {
		d.RecoveryReal, d.RecoveryWH = d.Real.rate(), d.WouldHave.rate()
		r.Detectors = append(r.Detectors, *d)
	}
	sort.Slice(r.Detectors, func(i, j int) bool { return r.Detectors[i].Detector < r.Detectors[j].Detector })
	return r
}

func newStatsCmd() *cobra.Command {
	var since string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Totals: interventions by kind and detector, and how often the agent recovered",
		Long: `Count LoopGuard's interventions (real ones in enforce mode, would-have ones
in observe mode) by kind and by detector, with their outcomes 10 tool calls
later: recovered (the command succeeded, the error changed, or the loop
stopped), continued_loop, halted, or unknown (the turn or session ended
first). The recovery rate is recovered / (recovered + continued_loop +
halted).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			from, err := parseSince(since, time.Now())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			var rows []store.InterventionCount
			if fileExists(filepath.Join(dir, home.DBFile)) {
				st, err := openStore(dir)
				if err != nil {
					return err
				}
				defer st.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if rows, err = st.InterventionStats(ctx, from); err != nil {
					return err
				}
			}
			r := buildStats(rows, from)
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(r)
			}
			printStats(out, r, since)
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "all", `how far back (e.g. 7d, 12h; "all" for everything)`)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")
	return cmd
}

func printStats(out io.Writer, r statsReport, since string) {
	window := "all time"
	if !r.Since.IsZero() {
		window = "last " + since
	}
	fmt.Fprintf(out, "Interventions (%s): %d\n", window, r.Total)
	if r.Total == 0 {
		fmt.Fprintln(out, "  none yet")
		return
	}
	fmt.Fprintf(out, "  %-6s %6s %11s\n", "", "real", "would-have")
	for _, k := range r.ByKind {
		fmt.Fprintf(out, "  %-6s %6d %11d\n", k.Action, k.Real, k.WouldHave)
	}
	fmt.Fprintln(out, "\nBy detector (outcome 10 calls later; recovery = recovered / decided)")
	fmt.Fprintf(out, "  %-17s %5s %4s %4s   %-28s %-28s\n", "", "nudge", "deny", "halt", "real: rec/cont/halt/unk", "would-have: rec/cont/halt/unk")
	for _, d := range r.Detectors {
		fmt.Fprintf(out, "  %-17s %5d %4d %4d   %-28s %-28s\n", d.Detector, d.Actions["nudge"], d.Actions["deny"], d.Actions["halt"],
			outcomeText(d.Real, d.RecoveryReal), outcomeText(d.WouldHave, d.RecoveryWH))
	}
}

func outcomeText(o outcomeCounts, rate *float64) string {
	s := fmt.Sprintf("%d/%d/%d/%d", o.Recovered, o.Continued, o.Halted, o.Unknown)
	if rate != nil {
		s += fmt.Sprintf(" (%.0f%%)", *rate*100)
	} else if o.Recovered+o.Continued+o.Halted+o.Unknown+o.Pending > 0 {
		s += " (-)"
	}
	if o.Pending > 0 {
		s += fmt.Sprintf(" +%d pending", o.Pending)
	}
	return s
}
