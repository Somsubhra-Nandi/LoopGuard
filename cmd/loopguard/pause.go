package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// scopeFlags pick what pause and resume apply to.
type scopeFlags struct {
	session string
	repo    string
	all     bool
}

func (f *scopeFlags) register(cmd *cobra.Command, verb string) {
	cmd.Flags().StringVar(&f.session, "session", "", verb+" one session (its id, or the first characters of it)")
	cmd.Flags().StringVar(&f.repo, "repo", "", verb+" every session in a repo (a path inside it; --repo alone: this one)")
	cmd.Flags().Lookup("repo").NoOptDefVal = "."
	cmd.Flags().BoolVar(&f.all, "all", false, verb+" every session")
}

func (f *scopeFlags) given() int {
	n := 0
	for _, b := range []bool{f.session != "", f.repo != "", f.all} {
		if b {
			n++
		}
	}
	return n
}

// resolve turns the flags into a scope and target. A session prefix is
// looked up in the database.
func (f *scopeFlags) resolve(ctx context.Context, st *store.Store) (scope, target string, err error) {
	switch {
	case f.all:
		return store.PauseAll, "", nil
	case f.repo != "":
		abs, err := filepath.Abs(f.repo)
		if err != nil {
			return "", "", err
		}
		return store.PauseRepo, gitRoot(abs), nil
	}
	id := strings.TrimPrefix(f.session, "claude-code:")
	if i := strings.IndexByte(id, '/'); i >= 0 {
		id = id[:i] // a subagent's key: pause the whole session
	}
	ids, err := st.SessionIDs(ctx, id, 10)
	if err != nil {
		return "", "", err
	}
	for _, x := range ids {
		if x == id {
			return store.PauseSession, id, nil
		}
	}
	switch len(ids) {
	case 0:
		return "", "", fmt.Errorf("no session id starts with %q; 'loopguard status' lists the active ones", id)
	case 1:
		return store.PauseSession, ids[0], nil
	}
	return "", "", fmt.Errorf("%q matches several sessions (%s); give more of the id", id, strings.Join(ids, ", "))
}

func openStore(dir string) (*store.Store, error) {
	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	return st, nil
}

// notifyDaemon makes a running daemon re-read the pauses now (otherwise it
// does within 30 s). It reports whether a daemon was told.
func notifyDaemon(dir string) bool {
	info, _, up := running(dir)
	return up && apiCall(dir, info.Port, http.MethodPost, "/v1/pauses/reload", nil) == nil
}

func newPauseCmd() *cobra.Command {
	var f scopeFlags
	var forDur string
	cmd := &cobra.Command{
		Use:   "pause [--session ID | --repo [PATH] | --all] [--for 30m]",
		Short: "Observe only (no notifications, nudges, blocks, or stops) for a session, a repo, or everything",
		Long: `Pause LoopGuard: the paused sessions are handled as at the observe level
(LoopGuard records what it would do, and never notifies, nudges, blocks, or
stops),
until 'loopguard resume' or until --for runs out. Loop detection goes on,
so resuming continues where it was. Pauses are kept in the database, so
they survive daemon restarts.

Without a scope, it lists the pauses in effect.

  loopguard pause --session 1a2b3c4d          the session a halt message names
  loopguard pause --repo --for 2h             the repo of the current directory
  loopguard pause --all --for 30m`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if f.given() == 0 {
				if forDur != "" {
					return errors.New("--for needs --session, --repo, or --all")
				}
				r := collectMode(dir, time.Now())
				if len(r.Pauses) == 0 {
					fmt.Fprintln(out, "Nothing is paused.")
				}
				for _, p := range r.Pauses {
					fmt.Fprintf(out, "Paused: %s\n", pauseText(p))
				}
				fmt.Fprintln(out, "Pause with: loopguard pause --session ID | --repo [PATH] | --all [--for 30m]")
				return nil
			}
			if f.given() > 1 {
				return errors.New("give one of --session, --repo, --all")
			}
			now := time.Now()
			var until time.Time
			if forDur != "" {
				since, err := parseSince(forDur, now)
				if err != nil || since.IsZero() {
					return fmt.Errorf("invalid --for %q (use e.g. 30m, 2h, 1d)", forDur)
				}
				until = now.Add(now.Sub(since))
			}
			st, err := openStore(dir)
			if err != nil {
				return err
			}
			defer st.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			scope, target, err := f.resolve(ctx, st)
			if err != nil {
				return err
			}
			p := store.Pause{Scope: scope, Target: target, Until: until, CreatedAt: now}
			if err := st.SetPause(ctx, p); err != nil {
				return err
			}
			fmt.Fprintf(out, "Paused %s: observe only, nothing is nudged, blocked, or halted.\n", pauseText(p))
			if !notifyDaemon(dir) {
				fmt.Fprintln(out, "(The daemon isn't running; it applies the pause when it starts.)")
			}
			fmt.Fprintf(out, "Undo with: loopguard resume %s\n", f.flagText(scope, target))
			return nil
		},
	}
	f.register(cmd, "pause")
	cmd.Flags().StringVar(&forDur, "for", "", "how long (e.g. 30m, 2h, 1d); default: until resumed")
	return cmd
}

// flagText is the scope as command-line flags.
func (f *scopeFlags) flagText(scope, target string) string {
	switch scope {
	case store.PauseAll:
		return "--all"
	case store.PauseRepo:
		return "--repo " + quoteArg(target)
	}
	return "--session " + target
}

func newResumeCmd() *cobra.Command {
	var f scopeFlags
	cmd := &cobra.Command{
		Use:   "resume [--session ID | --repo [PATH] | --all]",
		Short: "Undo 'loopguard pause'",
		Long: `Remove a pause. --all removes every pause (of any scope). A session also
stays paused while a pause of its repo, or of everything, is in effect.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.given() != 1 {
				return errors.New("give one of --session, --repo, --all")
			}
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			st, err := openStore(dir)
			if err != nil {
				return err
			}
			defer st.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			scope, target, err := f.resolve(ctx, st)
			if err != nil {
				return err
			}
			if scope == store.PauseAll {
				scope = "" // every pause
			}
			n, err := st.DeletePauses(ctx, scope, target)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if n == 0 {
				fmt.Fprintln(out, "Nothing to resume: no such pause.")
			} else {
				fmt.Fprintf(out, "Resumed (%d %s removed).\n", n, map[bool]string{true: "pause", false: "pauses"}[n == 1])
				notifyDaemon(dir)
			}
			left, _ := st.Pauses(ctx, time.Now())
			for _, p := range left {
				fmt.Fprintf(out, "Still paused: %s\n", pauseText(p))
			}
			return nil
		},
	}
	f.register(cmd, "resume")
	return cmd
}
