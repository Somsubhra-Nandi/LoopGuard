// Command loopguard is a circuit breaker for AI coding agents. It watches
// Claude Code and Codex through their hook systems, detects stuck loops, and
// nudges, blocks, or halts the agent.
//
// Replay (offline detection), the hook, the daemon (levels observe, notify,
// assist, guard), notifications, mode, pause, resume, stop, and stats work; report is a placeholder
// until its milestone, see plan.md.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/shim"
)

// Set at build time via -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process exit code.
//
// The hook path is fail-open: whatever happens (bad flags, read errors,
// panics), `loopguard hook` exits 0 with nothing on stdout or stderr, so a
// LoopGuard bug can never break the agent.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	var hookExit int
	root := newRootCmd(&hookExit)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)

	hook := isHookInvocation(root, args)
	if hook {
		defer func() {
			if recover() != nil {
				code = 0
			}
		}()
	}

	if err := root.Execute(); err != nil {
		if hook {
			return 0
		}
		if errors.Is(err, errSilent) {
			return 1
		}
		fmt.Fprintf(stderr, "Error: %v\nRun 'loopguard --help' for usage.\n", err)
		return 1
	}
	if hook {
		return hookExit
	}
	return 0
}

func isHookInvocation(root *cobra.Command, args []string) bool {
	cmd, _, err := root.Find(args)
	return err == nil && cmd.Name() == "hook"
}

// newRootCmd builds the CLI. The hook command stores its exit code in
// hookExit.
func newRootCmd(hookExit *int) *cobra.Command {
	root := &cobra.Command{
		Use:           "loopguard",
		Short:         "A circuit breaker for AI coding agents",
		Long:          "LoopGuard detects when Claude Code or Codex is stuck in a loop, nudges it out, and halts it if needed.",
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}

	root.AddCommand(
		newHookCmd(hookExit),
		newDaemonCmd(),
		newInitCmd(),
		newUninstallCmd(),
		newDoctorCmd(),
		newStatusCmd(),
		newCaptureCmd(),
		stubCmd("report [session]", "Show a session timeline: calls, loops, interventions, cost", "M4"),
		newReplayCmd(),
		newLabelCmd(),
		newEvalCmd(),
		newModeCmd(),
		newPauseCmd(),
		newResumeCmd(),
		newStopCmd(),
		newStatsCmd(),
		newVersionCmd(),
	)
	return root
}

// hookSpawn starts the daemon for the hook when it isn't running; nil uses
// the shim's default (this executable's `daemon`, detached). Tests replace
// it so they never start a real daemon by accident.
var hookSpawn func(home string) error

func newHookCmd(exit *int) *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Handle one agent hook event (called by the agent, not by you)",
		Long: `Forward one agent hook event (JSON on stdin) to the LoopGuard daemon and
print its decision. Starts the daemon if it isn't running. Always fails
open: on any problem it prints nothing and exits 0.

LOOPGUARD_DISABLE=1 turns it into a no-op; LOOPGUARD_HOOK_TIMEOUT_MS lowers
the 800 ms timeout; LOOPGUARD_NO_AUTOSTART=1 never starts the daemon;
LOOPGUARD_DEBUG=1 explains failures on stderr.`,
		Args: cobra.ArbitraryArgs,
		// Tolerate flags from newer installed hook configs.
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, _ []string) error {
			*exit = shim.Run(shim.Options{
				Agent:  agent,
				Stdin:  cmd.InOrStdin(),
				Stdout: cmd.OutOrStdout(),
				Stderr: cmd.ErrOrStderr(),
				Spawn:  hookSpawn,
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "claude-code", "agent that fired the hook (claude-code|codex)")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "loopguard %s\ncommit: %s\nbuilt:  %s\n", version, commit, date)
			return err
		},
	}
}

// stubCmd returns a placeholder subcommand for work scheduled in a later milestone.
func stubCmd(use, short, milestone string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short + " (milestone " + milestone + ")",
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "not implemented yet (milestone %s)\n", milestone)
			return err
		},
	}
}
