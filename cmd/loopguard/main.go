// Command loopguard is a circuit breaker for AI coding agents. It watches
// Claude Code and Codex through their hook systems, detects stuck loops, and
// nudges, blocks, or halts the agent.
//
// Only the CLI skeleton exists so far; see plan.md for the milestones.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
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
	root := newRootCmd()
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
		fmt.Fprintf(stderr, "Error: %v\nRun 'loopguard --help' for usage.\n", err)
		return 1
	}
	return 0
}

func isHookInvocation(root *cobra.Command, args []string) bool {
	cmd, _, err := root.Find(args)
	return err == nil && cmd.Name() == "hook"
}

func newRootCmd() *cobra.Command {
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
		newHookCmd(),
		stubCmd("daemon", "Run the local LoopGuard daemon", "M2"),
		stubCmd("init", "Install LoopGuard hooks into your agents' settings", "M2"),
		stubCmd("uninstall", "Remove LoopGuard hooks from your agents' settings", "M2"),
		stubCmd("doctor", "Check that LoopGuard is installed and working", "M2"),
		stubCmd("status", "Show active sessions and recent loop signals", "M2"),
		stubCmd("report [session]", "Show a session timeline: calls, loops, interventions, cost", "M4"),
		stubCmd("replay", "Find loops in past agent transcripts", "M1"),
		stubCmd("pause <session|--repo>", "Pause enforcement for a session or repo", "M3"),
		newVersionCmd(),
	)
	return root
}

func newHookCmd() *cobra.Command {
	var agent string
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Handle one agent hook event (called by the agent, not by you)",
		Args:  cobra.ArbitraryArgs,
		// Tolerate flags from newer installed hook configs.
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// TODO(M2): validate --agent and hand the payload to internal/shim,
			// which forwards it to the daemon and prints the decision (LLD §10).
			// TODO(M3): honor LOOPGUARD_DISABLE=1.
			_, _ = io.Copy(io.Discard, cmd.InOrStdin())
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
