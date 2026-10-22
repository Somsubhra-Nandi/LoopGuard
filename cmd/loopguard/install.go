package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
	"github.com/Somsubhra-Nandi/loopguard/internal/adapter/claudecode"
	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/textdiff"
)

// settingsFlags pick the Claude Code settings file init, uninstall, and
// doctor work on.
type settingsFlags struct {
	project  bool
	settings string
}

func (f *settingsFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.project, "project", false, `use this repo's .claude\settings.local.json instead of your user settings`)
	cmd.Flags().StringVar(&f.settings, "settings", "", "use this settings file")
}

// path resolves the settings file: --settings, else --project (the git
// root above the current directory, or the current directory), else the
// user settings.
func (f *settingsFlags) path() (string, error) {
	switch {
	case f.settings != "":
		return filepath.Abs(f.settings)
	case f.project:
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return claudecode.ProjectSettingsPath(gitRoot(wd)), nil
	default:
		return claudecode.UserSettingsPath()
	}
}

func (f *settingsFlags) undo() string {
	switch {
	case f.settings != "":
		return "loopguard uninstall --settings " + quoteArg(f.settings)
	case f.project:
		return "loopguard uninstall --project"
	}
	return "loopguard uninstall"
}

// gitRoot returns the nearest directory at or above dir with a .git, or
// dir itself if there is none.
func gitRoot(dir string) string {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t'\"") {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return s
}

// exeName adds .exe on Windows.
func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// defaultHookPath is loopguard-hook next to this executable.
func defaultHookPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), exeName(claudecode.HookBinary)), nil
}

func newInitCmd() *cobra.Command {
	var sf settingsFlags
	var hook string
	var dryRun, enforce bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Install LoopGuard's hooks into Claude Code's settings",
		Long: `Add LoopGuard's hooks to Claude Code's user settings
(~/.claude/settings.json, %USERPROFILE%\.claude\settings.json on Windows, or
$CLAUDE_CONFIG_DIR/settings.json), or with --project to this repo's
.claude\settings.local.json.

It merges: every other setting and every other tool's hooks stay as they
are. Running it again changes nothing (or updates LoopGuard's hooks, e.g.
after you move the binaries). Before the first change it saves a backup
next to the file (settings.json.loopguard.bak). The hooks run
loopguard-hook (by default the one next to this executable) by absolute
path, so it needn't be on PATH. Undo with 'loopguard uninstall'.

On a new install it also writes config.yaml in the LoopGuard home: mode
guard, but observe only for the first 3 days (observe_until), so you can
see in 'loopguard status' what it would do before it does anything. With
--enforce it guards at once. An existing install keeps its level
('loopguard mode' changes it).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			settings, err := sf.path()
			if err != nil {
				return err
			}
			if hook == "" {
				if hook, err = defaultHookPath(); err != nil {
					return err
				}
			}
			if hook, err = filepath.Abs(hook); err != nil {
				return err
			}
			if fi, err := os.Stat(hook); err != nil || fi.IsDir() {
				return fmt.Errorf("no hook binary at %s: build it there (go build -o %s ./cmd/loopguard-hook) or pass --hook", hook, quoteArg(hook))
			}
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			// Decided before anything is written: a new install has no
			// config.yaml, no database, and no LoopGuard hooks yet.
			existing := fileExists(filepath.Join(dir, config.GlobalFile)) || fileExists(filepath.Join(dir, home.DBFile))
			r, err := claudecode.New().Install(adapter.InstallOpts{Settings: settings, Hook: hook, DryRun: dryRun})
			if err != nil {
				return err
			}
			newInstall := !existing && r.Changed && len(r.Updated) == 0 && len(r.Added) > 0
			out := cmd.OutOrStdout()
			if dryRun {
				printDryRun(out, r)
				return nil
			}
			printWarnings(out, r)
			if !r.Changed {
				fmt.Fprintf(out, "LoopGuard hooks are already installed in %s; nothing changed.\n", settings)
				fmt.Fprintf(out, "  hook: %s\n", hook)
				return initMode(out, enforce, false)
			}
			fmt.Fprintf(out, "Installed LoopGuard hooks in %s\n", settings)
			if len(r.Added) > 0 {
				fmt.Fprintf(out, "  added:   %s\n", strings.Join(r.Added, ", "))
			}
			if len(r.Updated) > 0 {
				fmt.Fprintf(out, "  updated: %s\n", strings.Join(r.Updated, ", "))
			}
			if len(r.Removed) > 0 {
				fmt.Fprintf(out, "  removed: %s (no longer used)\n", strings.Join(r.Removed, ", "))
			}
			fmt.Fprintf(out, "  hook:    %s\n", hook)
			switch {
			case r.BackupCreated:
				fmt.Fprintf(out, "  backup:  %s\n", r.BackupPath)
			case r.BackupPath != "":
				fmt.Fprintf(out, "  backup:  %s (from an earlier install, kept)\n", r.BackupPath)
			default:
				fmt.Fprintln(out, "  backup:  none needed (the file is new)")
			}
			if err := initMode(out, enforce, newInstall); err != nil {
				return err
			}
			fmt.Fprintln(out, "New Claude Code sessions use the hooks; restart sessions that are already open.")
			fmt.Fprintln(out, "Check it with: loopguard doctor")
			fmt.Fprintf(out, "Undo with:     %s\n", sf.undo())
			return nil
		},
	}
	sf.register(cmd)
	cmd.Flags().StringVar(&hook, "hook", "", "path of the loopguard-hook binary (default: next to this executable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the change as a diff and write nothing")
	cmd.Flags().BoolVar(&enforce, "enforce", false, "guard (the highest level) from now on, skipping a new install's observe-only days")
	return cmd
}

// initMode sets the level on init (LLD §12, TDD D-8): --enforce guards at
// once; a new install guards after observe_days_after_install days of
// observing; an existing install keeps its level. It prints the result.
func initMode(out io.Writer, enforce, newInstall bool) error {
	dir, err := home.Dir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, config.GlobalFile)
	switch {
	case enforce:
		if err := config.EditFile(path, config.Edit{Key: "mode", Value: config.ModeGuard},
			config.Edit{Key: "observe_until", Remove: true}); err != nil {
			return err
		}
	case newInstall:
		days := config.Defaults().ObserveDaysAfterInstall
		until := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Truncate(time.Second)
		if err := config.EditFile(path, config.Edit{Key: "mode", Value: config.ModeGuard},
			config.Edit{Key: "observe_until", Value: until.Format(time.RFC3339)}); err != nil {
			return err
		}
	}
	m := collectMode(dir, time.Now())
	fmt.Fprintf(out, "Level: %s (%s)\n", m.Mode.Mode, m.Mode.Why)
	if h := levelHelp[m.Mode.Mode]; h != "" {
		fmt.Fprintln(out, "  "+h)
	}
	switch m.Mode.Mode {
	case config.ModeOff:
	case config.ModeObserve:
		fmt.Fprintln(out, "  Pick a level now with: loopguard mode notify|assist|guard")
	default:
		fmt.Fprintln(out, "  Pause it with: loopguard pause --all --for 2h   Record only: loopguard mode observe")
	}
	if !config.NewLoader(dir, config.LoaderOptions{}).For("").Budgets.Any() {
		fmt.Fprintln(out, "No budget set. Example: "+config.ExampleBudget)
	}
	return nil
}

func newUninstallCmd() *cobra.Command {
	var sf settingsFlags
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove LoopGuard's hooks from Claude Code's settings",
		Long: `Remove LoopGuard's hooks (those that run loopguard-hook) from Claude Code's
settings. Other settings and other tools' hooks stay as they are. The backup
from 'loopguard init' is kept. The daemon keeps running until you stop it
with 'loopguard daemon stop'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			settings, err := sf.path()
			if err != nil {
				return err
			}
			r, err := claudecode.New().Uninstall(adapter.InstallOpts{Settings: settings, DryRun: dryRun})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if dryRun {
				printDryRun(out, r)
				return nil
			}
			if !r.Changed {
				fmt.Fprintf(out, "No LoopGuard hooks in %s; nothing changed.\n", settings)
				return nil
			}
			fmt.Fprintf(out, "Removed LoopGuard hooks from %s\n", settings)
			fmt.Fprintf(out, "  events: %s\n", strings.Join(r.Removed, ", "))
			fmt.Fprintln(out, "  other settings and hooks: untouched")
			if r.BackupPath != "" {
				fmt.Fprintf(out, "  backup from before install: %s (kept; delete it when you no longer need it)\n", r.BackupPath)
			}
			fmt.Fprintln(out, "Stop the daemon with: loopguard daemon stop")
			return nil
		},
	}
	sf.register(cmd)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the change as a diff and write nothing")
	return cmd
}

func printDryRun(out io.Writer, r adapter.InstallReport) {
	printWarnings(out, r)
	if !r.Changed {
		fmt.Fprintf(out, "Dry run: %s would not change.\n", r.SettingsPath)
		return
	}
	before := r.SettingsPath + " (now)"
	if r.Before == nil {
		before = r.SettingsPath + " (does not exist yet)"
	}
	fmt.Fprint(out, textdiff.Unified(string(r.Before), string(r.After), before, r.SettingsPath+" (after)", 3))
	fmt.Fprintln(out, "Dry run: nothing written.")
}

func printWarnings(out io.Writer, r adapter.InstallReport) {
	for _, w := range r.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", w)
	}
}

// errSilent is returned by commands that already printed their failure
// (doctor): the exit code is 1, with no extra error line.
var errSilent = errors.New("")
