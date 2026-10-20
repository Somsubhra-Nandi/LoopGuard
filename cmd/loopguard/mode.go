package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// modeReport is LoopGuard's mode as the CLI sees it: config.yaml, the
// current directory's repo file, and the pauses.
type modeReport struct {
	Mode   config.ModeInfo `json:"mode"`
	Config fileReport      `json:"config"`
	Repo   *repoReport     `json:"repo,omitempty"`
	Pauses []store.Pause   `json:"pauses"`
}

type fileReport struct {
	Path     string   `json:"path"`
	Exists   bool     `json:"exists"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// repoReport is the mode for sessions in the current directory's repo,
// when its .loopguard.yaml changes anything.
type repoReport struct {
	Root  string          `json:"root"`
	File  fileReport      `json:"file"`
	Mode  config.ModeInfo `json:"mode"`
	Notes []string        `json:"notes,omitempty"`
}

func reportFile(l config.Loaded) fileReport {
	r := fileReport{Path: l.Path, Exists: l.Exists, Warnings: l.Warnings}
	if l.Err != nil {
		r.Error = l.Err.Error()
	}
	return r
}

// collectMode reads the config files and the pauses (from the database;
// nothing else is opened or started).
func collectMode(dir string, now time.Time) modeReport {
	l := config.NewLoader(dir, config.LoaderOptions{})
	g := l.For("")
	r := modeReport{Mode: g.ModeAt(now), Config: reportFile(g.Global), Pauses: []store.Pause{}}
	if wd, err := os.Getwd(); err == nil {
		if root := gitRoot(wd); fileExists(filepath.Join(root, ".git")) {
			e := l.For(root)
			if e.Repo.Exists {
				r.Repo = &repoReport{Root: root, File: reportFile(e.Repo), Mode: e.ModeAt(now), Notes: e.Notes}
			}
		}
	}
	if fileExists(filepath.Join(dir, home.DBFile)) {
		if st, err := store.Open(filepath.Join(dir, home.DBFile)); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if ps, err := st.Pauses(ctx, now); err == nil {
				r.Pauses = ps
			}
			cancel()
			st.Close()
		}
	}
	return r
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// printMode prints the mode, why, and what changes it.
func printMode(out io.Writer, r modeReport) {
	fmt.Fprintf(out, "Mode: %s (%s)\n", r.Mode.Mode, r.Mode.Why)
	switch {
	case r.Config.Error != "":
		fmt.Fprintf(out, "  %s is invalid, so the defaults are used: %s\n", r.Config.Path, r.Config.Error)
	case !r.Config.Exists:
		fmt.Fprintf(out, "  config: %s (not created yet)\n", r.Config.Path)
	default:
		fmt.Fprintf(out, "  config: %s\n", r.Config.Path)
	}
	if rp := r.Repo; rp != nil {
		switch {
		case rp.File.Error != "":
			fmt.Fprintf(out, "  this repo: %s is invalid and ignored: %s\n", rp.File.Path, rp.File.Error)
		case rp.Mode != r.Mode:
			fmt.Fprintf(out, "  this repo: %s (%s)\n", rp.Mode.Mode, rp.Mode.Why)
		default:
			fmt.Fprintf(out, "  this repo: %s applies\n", rp.File.Path)
		}
		for _, n := range rp.Notes {
			fmt.Fprintf(out, "    %s\n", n)
		}
	}
	for _, p := range r.Pauses {
		fmt.Fprintf(out, "  paused: %s\n", pauseText(p))
	}
}

// pauseText describes a pause in one line.
func pauseText(p store.Pause) string {
	s := "everything"
	switch p.Scope {
	case store.PauseSession:
		s = "session " + p.Target
	case store.PauseRepo:
		s = "repo " + p.Target
	}
	if p.Until.IsZero() {
		return s + " (until resumed)"
	}
	return s + " (until " + p.Until.Local().Format("2006-01-02 15:04") + ")"
}

func newModeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mode [status|observe|enforce]",
		Short: "Show or set the mode: observe (record only) or enforce (nudge, block, halt)",
		Long: `Show LoopGuard's mode and why, or set it in config.yaml.

  observe  LoopGuard records what it would do (see 'loopguard status') and
           never changes what Claude Code does.
  enforce  LoopGuard nudges a looping agent, blocks the repeated action,
           and halts the turn if needed.

A new install observes for observe_days_after_install days (default 3)
before enforcing; 'loopguard mode enforce' ends that period now. A repo's
.loopguard.yaml can turn enforcement off for the repo (mode: observe, or
enabled: false), never on, and 'loopguard pause' turns it off for a while.
The running daemon picks up a change within a few seconds.`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"status", config.ModeObserve, config.ModeEnforce},
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			path := filepath.Join(dir, config.GlobalFile)
			want := "status"
			if len(args) == 1 {
				want = args[0]
			}
			switch want {
			case "status":
			case config.ModeEnforce:
				// An explicit enforce also ends a new install's observe period.
				if err := config.EditFile(path, config.Edit{Key: "mode", Value: config.ModeEnforce},
					config.Edit{Key: "observe_until", Remove: true}); err != nil {
					return err
				}
				fmt.Fprintf(out, "Set mode: enforce in %s\n", path)
			case config.ModeObserve:
				if err := config.EditFile(path, config.Edit{Key: "mode", Value: config.ModeObserve}); err != nil {
					return err
				}
				fmt.Fprintf(out, "Set mode: observe in %s\n", path)
			default:
				return fmt.Errorf("unknown mode %q (use status, observe, or enforce)", want)
			}
			r := collectMode(dir, time.Now())
			printMode(out, r)
			if want == config.ModeEnforce && r.Mode.Mode == config.ModeEnforce {
				fmt.Fprintln(out, "LoopGuard now nudges looping agents, blocks the repeated action, and halts them if needed.")
				fmt.Fprintln(out, "Pause it with: loopguard pause --all --for 2h   Back to observe: loopguard mode observe")
			}
			return nil
		},
	}
}
