package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
)

func newStopCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "stop SESSION | --all",
		Short: "Stop a session's current turn at its next tool call",
		Long: `Ask LoopGuard to stop a Claude Code session's current turn: the next tool
call of that session (or of its subagents) is blocked and the turn ends,
with the stop reason "Stopped by the user via loopguard stop." It works
whether or not LoopGuard has seen a loop, at the notify, assist, and guard
levels (not in observe, which never touches the agent).

The session stays usable: your next message continues normally. A
message from you before the next tool call cancels the request, and so
does the turn ending on its own.

SESSION is the session id or its first characters, as notifications and
'loopguard status' show it. --all stops every active session.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if (all && len(args) == 0) || (!all && len(args) == 1) {
				return nil
			}
			return errors.New("give a session id (or its first characters) or --all")
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			info, _, up := running(dir)
			if !up {
				return errors.New("the LoopGuard daemon isn't running, so no session is being watched; nothing to stop")
			}
			req := daemon.StopRequest{All: all}
			if !all {
				req.Session = strings.TrimPrefix(args[0], "claude-code:")
				if i := strings.IndexByte(req.Session, '/'); i >= 0 {
					req.Session = req.Session[:i] // a subagent's key: its session
				}
			}
			var r daemon.StopReply
			code, err := apiRequest(dir, info.Port, http.MethodPost, "/v1/stop", req, &r)
			if err != nil {
				return err
			}
			switch code {
			case http.StatusOK:
			case http.StatusConflict:
				return fmt.Errorf("%q matches several active sessions (%s); give more of the id", req.Session, strings.Join(r.Matches, ", "))
			case http.StatusNotFound:
				return fmt.Errorf("no active session id starts with %q; 'loopguard status' lists them", req.Session)
			default:
				return fmt.Errorf("the daemon answered %d %s", code, http.StatusText(code))
			}
			printStop(cmd.OutOrStdout(), r)
			if len(r.Stopped) == 0 && len(r.Refused) > 0 {
				return errSilent
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every active session")
	return cmd
}

func printStop(out io.Writer, r daemon.StopReply) {
	name := func(t daemon.StopTarget) string {
		if t.Project == "" {
			return "[" + shortSID(t.SessionID) + "]"
		}
		return baseName(t.Project) + " [" + shortSID(t.SessionID) + "]"
	}
	for _, t := range r.Stopped {
		fmt.Fprintf(out, "Stopping %s: its turn ends at its next tool call.\n", name(t))
	}
	for _, t := range r.Refused {
		fmt.Fprintf(out, "Not stopping %s: it observes only (%s), and LoopGuard never touches the agent in observe.\n", name(t), t.ModeWhy)
	}
	switch {
	case len(r.Stopped) > 0:
		fmt.Fprintln(out, "Your next message to it continues normally.")
	case len(r.Refused) == 0:
		fmt.Fprintln(out, "No active session to stop.")
	default:
		fmt.Fprintln(out, "To let LoopGuard stop sessions: loopguard mode notify|assist|guard (or resume a paused one).")
	}
}
