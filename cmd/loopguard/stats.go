package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/report"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

func newStatsCmd() *cobra.Command {
	var since string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Totals: interventions by kind and detector, and how often the agent recovered",
		Long: `Count LoopGuard's interventions (real ones that reached the agent, would-have
ones recorded at a lower level) by kind and by detector, with their outcomes 10 tool calls
later: recovered (the looping command itself later succeeded, or its error
changed), moved_on (the loop just went quiet: the agent did something else),
continued_loop, halted, or unknown (the turn or session ended first). The
recovery rate is recovered / (recovered + moved_on + continued_loop + halted).`,
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
			r := report.BuildStats(rows, from)
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

func printStats(out io.Writer, r report.Stats, since string) {
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
	fmt.Fprintf(out, "  %-17s %5s %4s %4s   %-34s %-34s\n", "", "nudge", "deny", "halt", "real: rec/moved/cont/halt/unk", "would-have: rec/moved/cont/halt/unk")
	for _, d := range r.Detectors {
		fmt.Fprintf(out, "  %-17s %5d %4d %4d   %-34s %-34s\n", d.Detector, d.Actions["nudge"], d.Actions["deny"], d.Actions["halt"],
			outcomeText(d.Real, d.RecoveryReal), outcomeText(d.WouldHave, d.RecoveryWH))
	}
}

func outcomeText(o report.OutcomeCounts, rate *float64) string {
	s := fmt.Sprintf("%d/%d/%d/%d/%d", o.Recovered, o.MovedOn, o.Continued, o.Halted, o.Unknown)
	if rate != nil {
		s += fmt.Sprintf(" (%.0f%%)", *rate*100)
	} else if o.Recovered+o.MovedOn+o.Continued+o.Halted+o.Unknown+o.Pending > 0 {
		s += " (-)"
	}
	if o.Pending > 0 {
		s += fmt.Sprintf(" +%d pending", o.Pending)
	}
	return s
}
