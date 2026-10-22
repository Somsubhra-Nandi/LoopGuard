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
	"github.com/Somsubhra-Nandi/loopguard/internal/pathid"
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

// printMode prints the level, why, and what changes it.
func printMode(out io.Writer, r modeReport) {
	shown := pathid.Tilde
	fmt.Fprintf(out, "Level: %s (%s)\n", r.Mode.Mode, r.Mode.Why)
	switch {
	case r.Config.Error != "":
		fmt.Fprintf(out, "  %s is invalid, so the defaults are used: %s\n", shown(r.Config.Path), r.Config.Error)
	case !r.Config.Exists:
		fmt.Fprintf(out, "  config: %s (not created yet)\n", shown(r.Config.Path))
	default:
		fmt.Fprintf(out, "  config: %s\n", shown(r.Config.Path))
	}
	if rp := r.Repo; rp != nil {
		switch {
		case rp.File.Error != "":
			fmt.Fprintf(out, "  this repo: %s is invalid and ignored: %s\n", shown(rp.File.Path), rp.File.Error)
		case rp.Mode != r.Mode:
			fmt.Fprintf(out, "  this repo: %s (%s)\n", rp.Mode.Mode, rp.Mode.Why)
		default:
			fmt.Fprintf(out, "  this repo: %s applies\n", shown(rp.File.Path))
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

// levelHelp says what each level does, for the mode command and init.
var levelHelp = map[string]string{
	config.ModeObserve: "LoopGuard records what it would do (loopguard status) and never touches Claude Code or notifies you.",
	config.ModeNotify:  "LoopGuard notifies you about loops (loopguard notify) and never touches Claude Code.",
	config.ModeAssist:  "LoopGuard notifies you and sends looping agents a note; it never blocks or stops anything.",
	config.ModeGuard:   "LoopGuard notifies you, nudges looping agents, blocks the repeated action, and stops the turn if needed.",
}

func newModeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mode [status|observe|notify|assist|guard]",
		Short: "Show or set the level: observe, notify, assist, or guard",
		Long: `Show LoopGuard's level and why, or set it in config.yaml.

  observe  record only: LoopGuard records what it would do (see
           'loopguard status'), never touches Claude Code, never notifies.
  notify   notify you about loops (desktop by default; see 'loopguard
           notify'); never touches Claude Code.
  assist   notify, and nudge a looping agent with a note; never blocks
           or stops anything.
  guard    notify, nudge, block the repeated action, and stop the turn if
           needed. 'enforce' is the old name of guard and still works.

A new install observes for observe_days_after_install days (default 3)
before its level applies; setting a level here ends that period now. A
repo's .loopguard.yaml can raise the level for the repo, never lower it;
'loopguard pause' turns any level into observe for a while. The running
daemon picks up a change within a few seconds.`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: append([]string{"status"}, config.Levels...),
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
			level := config.CanonicalMode(want)
			switch {
			case want == "status":
			case config.ValidMode(want):
				edits := []config.Edit{{Key: "mode", Value: level}}
				if level != config.ModeObserve {
					// An explicit level also ends a new install's observe period.
					edits = append(edits, config.Edit{Key: "observe_until", Remove: true})
				}
				if err := config.EditFile(path, edits...); err != nil {
					return err
				}
				fmt.Fprintf(out, "Set mode: %s in %s\n", level, path)
				if want == config.ModeEnforce {
					fmt.Fprintln(out, "(enforce is the old name of guard.)")
				}
			default:
				return fmt.Errorf("unknown level %q (use status, observe, notify, assist, or guard)", want)
			}
			r := collectMode(dir, time.Now())
			printMode(out, r)
			if want != "status" && r.Mode.Mode == level {
				fmt.Fprintln(out, levelHelp[level])
				if level != config.ModeObserve {
					fmt.Fprintln(out, "Pause it with: loopguard pause --all --for 2h   Record only: loopguard mode observe")
				}
			}
			return nil
		},
	}
}
