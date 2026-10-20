package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/capture"
	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/proc"
)

// startWait bounds how long `daemon --detach` and `daemon stop` wait.
const startWait = 10 * time.Second

func newDaemonCmd() *cobra.Command {
	var detach bool
	var port int
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the local LoopGuard daemon (started automatically by the hook)",
		Long: `Run the local LoopGuard daemon in the foreground (Ctrl+C stops it), or in
the background with --detach. The hook starts it automatically when needed,
so you rarely run this yourself.

It listens on 127.0.0.1 (port 47821 or the first free one above it) and keeps
its files in ~/.loopguard (%USERPROFILE%\.loopguard on Windows; override with
LOOPGUARD_HOME): daemon.json, token, loopguard.db, logs\daemon.log.

Capture mode saves redacted hook payloads to <home>/capture/ for golden
tests (at most 20 per event type). Turn it on and off with 'loopguard
capture on|off' (no restart needed), or force it on with LOOPGUARD_CAPTURE=1.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			if detach {
				return startDetached(cmd.OutOrStdout(), dir)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = daemon.Run(ctx, daemon.Options{
				Home:    dir,
				Version: version,
				Port:    port,
				Capture: os.Getenv(capture.EnvCapture) == "1",
				Log:     slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
			})
			var already *daemon.AlreadyRunningError
			if errors.As(err, &already) {
				fmt.Fprintln(cmd.OutOrStdout(), already.Error())
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&detach, "detach", false, "run in the background and return once it is ready")
	cmd.Flags().IntVar(&port, "port", daemon.DefaultPort, "first port to try")
	cmd.AddCommand(newDaemonStopCmd(), newDaemonStatusCmd())
	return cmd
}

// running returns the daemon serving dir, if one is.
func running(dir string) (home.DaemonInfo, daemon.Health, bool) {
	info, err := home.ReadDaemonInfo(dir)
	if err != nil {
		return info, daemon.Health{}, false
	}
	h, ok := daemon.Healthy(info.Port, dir)
	return info, h, ok
}

func startDetached(out io.Writer, dir string) error {
	if info, _, ok := running(dir); ok {
		fmt.Fprintf(out, "LoopGuard daemon already running (pid %d, port %d)\n", info.PID, info.Port)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := proc.SpawnDetached(dir, exe, "daemon"); err != nil {
		return fmt.Errorf("starting the daemon: %w", err)
	}
	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		if info, _, ok := running(dir); ok {
			fmt.Fprintf(out, "LoopGuard daemon started (pid %d, port %d)\n", info.PID, info.Port)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the daemon did not come up within %v; see %s", startWait, filepath.Join(dir, home.LogDir, "daemon.log"))
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			info, h, ok := running(dir)
			if !ok {
				if info.Port != 0 && !proc.Alive(info.PID) {
					// daemon.json from a daemon that is gone.
					os.Remove(filepath.Join(dir, home.DaemonFile))
				}
				fmt.Fprintln(out, "LoopGuard daemon is not running")
				return nil
			}
			if err := apiCall(dir, info.Port, http.MethodPost, "/v1/shutdown", nil); err != nil {
				return fmt.Errorf("asking the daemon to stop: %w", err)
			}
			deadline := time.Now().Add(startWait)
			for time.Now().Before(deadline) {
				if !proc.Alive(h.PID) {
					fmt.Fprintf(out, "LoopGuard daemon stopped (pid %d)\n", h.PID)
					return nil
				}
				time.Sleep(50 * time.Millisecond)
			}
			// It confirmed its pid over HTTP, so this is the right process.
			if err := proc.Kill(h.PID); err != nil {
				return fmt.Errorf("daemon (pid %d) did not stop: %w", h.PID, err)
			}
			fmt.Fprintf(out, "LoopGuard daemon killed (pid %d) after not stopping within %v\n", h.PID, startWait)
			return nil
		},
	}
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the daemon is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			info, h, ok := running(dir)
			if !ok {
				fmt.Fprintln(out, "LoopGuard daemon: not running")
				fmt.Fprintf(out, "  home: %s\n", dir)
				return nil
			}
			var list struct {
				Sessions []struct {
					Active bool `json:"active"`
				} `json:"sessions"`
			}
			active := "?"
			if err := apiCall(dir, info.Port, http.MethodGet, "/v1/sessions", &list); err == nil {
				n := 0
				for _, s := range list.Sessions {
					if s.Active {
						n++
					}
				}
				active = fmt.Sprint(n)
			}
			store := h.Store
			if store == "sqlite" {
				store += " (" + filepath.Join(dir, home.DBFile) + ")"
			}
			fmt.Fprintln(out, "LoopGuard daemon: running")
			fmt.Fprintf(out, "  pid:      %d\n", h.PID)
			fmt.Fprintf(out, "  port:     %d\n", info.Port)
			fmt.Fprintf(out, "  version:  %s\n", h.Version)
			fmt.Fprintf(out, "  uptime:   %s\n", time.Since(info.StartedAt).Round(time.Second))
			fmt.Fprintf(out, "  store:    %s\n", store)
			fmt.Fprintf(out, "  sessions: %s active\n", active)
			fmt.Fprintf(out, "  home:     %s\n", dir)
			fmt.Fprintln(out, "  mode:     observe (decisions are recorded, never enforced)")
			return nil
		},
	}
}

// apiCall calls an authenticated daemon route and decodes the JSON reply
// into out (if not nil).
func apiCall(dir string, port int, method, path string, out any) error {
	tok, err := home.ReadToken(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
