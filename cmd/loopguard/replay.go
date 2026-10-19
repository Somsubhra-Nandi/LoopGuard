package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay"
)

// now is the clock for --since; tests replace it.
var now = time.Now

type replayFlags struct {
	since   string
	path    string
	repo    string
	session string
	json    bool
	top     int
	calls   bool
	label   bool
	labels  string
	export  string
	out     string
	workers int
	near    int
	soft    bool
}

func newReplayCmd() *cobra.Command {
	var f replayFlags
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Find loops in past agent transcripts",
		Long: `Replay past Claude Code sessions through LoopGuard's detectors (observe mode)
and report the loops it finds: how many, how much time and how many tool calls
they took, and what LoopGuard would have done.

Transcripts are read from ~/.claude/projects (or $CLAUDE_CONFIG_DIR/projects).
Nothing leaves your machine.`,
		Example: `  loopguard replay
  loopguard replay --since 7d --repo .
  loopguard replay --json --top -1 > loops.json
  loopguard replay --label
  loopguard replay --session 1d3a0fe0 --calls
  loopguard replay --near-misses          # the 10 stretches closest to a detection
  loopguard replay --near-misses=25 --soft-failures
  loopguard replay --export-anonymized 1d3a0fe0 --out session.jsonl`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runReplay(cmd, f) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.since, "since", "30d", `how far back to look (e.g. 30d, 12h, 2w; "all" for everything)`)
	fl.StringVar(&f.path, "path", "", "transcripts directory (default ~/.claude/projects)")
	fl.StringVar(&f.repo, "repo", "", "only sessions started in this directory or below it")
	fl.StringVar(&f.session, "session", "", "only this session (id or id prefix)")
	fl.BoolVar(&f.json, "json", false, "print the report as JSON (schema: docs/replay-json.md)")
	fl.IntVar(&f.top, "top", 5, "loops to list (0: none, -1: all)")
	fl.BoolVar(&f.calls, "calls", false, "with --session: list the session's tool calls with their numbers")
	fl.BoolVar(&f.label, "label", false, "label each detected loop as real or a false positive")
	fl.StringVar(&f.labels, "labels", "", "labels file (default ~/.loopguard/labels.jsonl)")
	fl.StringVar(&f.export, "export-anonymized", "", "write an anonymized copy of this session (id or id prefix)")
	fl.StringVar(&f.out, "out", "", "output file for --export-anonymized")
	fl.IntVar(&f.workers, "workers", 0, "sessions replayed in parallel (default: number of CPUs)")
	fl.IntVar(&f.near, "near-misses", 0, "list the stretches that came closest to a detection without firing (--near-misses alone: 10; --near-misses=N; -1: all)")
	fl.Lookup("near-misses").NoOptDefVal = "10"
	fl.BoolVar(&f.soft, "soft-failures", false, "list calls counted as soft failures (exit 0 + failure marker), with counts per marker")
	return cmd
}

func runReplay(cmd *cobra.Command, f replayFlags) error {
	since, err := parseSince(f.since, now())
	if err != nil {
		return err
	}
	path := f.path
	if path == "" {
		if path, err = replay.DefaultPath(); err != nil {
			return err
		}
	}
	if f.export != "" {
		if f.out == "" {
			return errors.New("--export-anonymized needs --out")
		}
		return runExport(cmd, path, f.export, f.out)
	}
	if f.calls && f.session == "" {
		return errors.New("--calls needs --session")
	}

	filter := replay.Filter{Since: since, Repo: f.repo, Session: f.session}
	streams, err := replay.Discover(path, filter)
	if err != nil {
		return fmt.Errorf("reading transcripts: %w", err)
	}
	start := time.Now()
	res := replay.Run(streams, replay.Options{Thresholds: config.Default(), Workers: f.workers, Timeline: f.calls})
	elapsed := time.Since(start)

	out := cmd.OutOrStdout()
	switch {
	case f.calls:
		return writeCalls(out, res)
	case f.label:
		return runLabel(cmd, res, f.labels)
	}
	ropts := replay.ReportOptions{Top: f.top, NearMisses: f.near, SoftFailures: f.soft}
	report := replay.NewReport(res, path, since, ropts, now(), elapsed)
	if f.json {
		return report.WriteJSON(out)
	}
	return report.WriteText(out)
}

// parseSince parses "30d", "12h", "2w", "90m", Go durations, or "all"/"0".
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "all" || s == "0" || s == "" {
		return time.Time{}, nil
	}
	unit := map[byte]time.Duration{'d': 24 * time.Hour, 'w': 7 * 24 * time.Hour}
	if u, ok := unit[s[len(s)-1]]; ok {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil || n <= 0 {
			return time.Time{}, fmt.Errorf("invalid --since %q (use e.g. 30d, 12h, 2w, all)", s)
		}
		return now.Add(-time.Duration(n * float64(u))), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("invalid --since %q (use e.g. 30d, 12h, 2w, all)", s)
	}
	return now.Add(-d), nil
}
