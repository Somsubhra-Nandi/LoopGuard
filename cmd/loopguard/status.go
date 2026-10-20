package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/capture"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// statusDecisions is how many would-have decisions status shows.
const statusDecisions = 10

// statusReport is `loopguard status --json`.
type statusReport struct {
	Daemon    daemonStatus    `json:"daemon"`
	Sessions  []store.Session `json:"active_sessions"`
	Decisions []decisionView  `json:"decisions"`
}

type daemonStatus struct {
	Up      bool    `json:"up"`
	PID     int     `json:"pid,omitempty"`
	Port    int     `json:"port,omitempty"`
	Version string  `json:"version,omitempty"`
	Uptime  float64 `json:"uptime_seconds,omitempty"`
	Mode    string  `json:"mode"`
	Capture bool    `json:"capture"`
	Home    string  `json:"home"`
}

type decisionView struct {
	store.Intervention
	Project string `json:"project,omitempty"` // the session's working directory
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the daemon, active sessions, and recent would-have decisions",
		Long: `Show whether the daemon is up, the active Claude Code sessions (project,
last activity, tool calls, failures), and the last 10 decisions LoopGuard
would have made in enforce mode (nudge, deny, halt) with the detector and
the reason. When the daemon is down, the decisions come from the database.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			r := collectStatus(dir)
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(r)
			}
			printStatus(out, r, time.Now())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")
	return cmd
}

func collectStatus(dir string) statusReport {
	r := statusReport{Daemon: daemonStatus{Mode: "observe", Home: dir, Capture: capture.FlagOn(dir)},
		Sessions: []store.Session{}, Decisions: []decisionView{}}
	cwds := map[string]string{} // session key → cwd
	info, h, up := running(dir)
	if up {
		r.Daemon = daemonStatus{Up: true, PID: h.PID, Port: info.Port, Version: h.Version, Mode: "observe", Home: dir,
			Uptime: time.Since(uptimeStart(info, h)).Seconds(), Capture: h.Capture || r.Daemon.Capture}
		var list struct {
			Sessions []struct {
				store.Session
				Active bool `json:"active"`
			} `json:"sessions"`
		}
		if apiCall(dir, info.Port, http.MethodGet, "/v1/sessions", &list) == nil {
			for _, s := range list.Sessions {
				cwds[s.Key] = s.Cwd
				if s.Active && s.SessionID != doctorSession {
					r.Sessions = append(r.Sessions, s.Session)
				}
			}
		}
		var decs struct {
			Decisions []store.Intervention `json:"decisions"`
		}
		if apiCall(dir, info.Port, http.MethodGet, fmt.Sprintf("/v1/decisions?limit=%d", statusDecisions), &decs) == nil {
			for _, d := range decs.Decisions {
				r.Decisions = append(r.Decisions, decisionView{Intervention: d, Project: cwds[d.SessionKey]})
			}
		}
		return r
	}
	// Daemon down: the database still has the decisions.
	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		return r
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ivs, _ := st.RecentInterventions(ctx, statusDecisions)
	for _, d := range ivs {
		p := ""
		if s, err := st.GetSession(ctx, d.SessionKey); err == nil {
			p = s.Cwd
		}
		r.Decisions = append(r.Decisions, decisionView{Intervention: d, Project: p})
	}
	return r
}

func printStatus(out io.Writer, r statusReport, now time.Time) {
	d := r.Daemon
	capt := "off"
	if d.Capture {
		capt = "on"
	}
	if d.Up {
		fmt.Fprintf(out, "Daemon: up %s (pid %d, port %d, version %s), %s mode, capture %s\n",
			shortDuration(time.Duration(d.Uptime*float64(time.Second))), d.PID, d.Port, d.Version, d.Mode, capt)
	} else {
		fmt.Fprintf(out, "Daemon: down (the next hook event starts it), capture %s\n", capt)
	}

	if d.Up {
		fmt.Fprintf(out, "\nActive sessions (%d)\n", len(r.Sessions))
		if len(r.Sessions) == 0 {
			fmt.Fprintln(out, "  none")
		}
		for _, s := range r.Sessions {
			name := s.Cwd
			if name == "" {
				name = s.SessionID
			}
			if strings.Contains(s.Key, "/") {
				name += " (subagent)"
			}
			fmt.Fprintf(out, "  %s\n    last activity %s ago, %d tool calls, %d failures", name, shortDuration(now.Sub(s.LastSeenAt)), s.ToolCalls, s.Failures)
			if s.LoopsDetected > 0 {
				fmt.Fprintf(out, ", %d %s", s.LoopsDetected, map[bool]string{true: "loop", false: "loops"}[s.LoopsDetected == 1])
			}
			fmt.Fprintln(out)
		}
	}

	fmt.Fprintf(out, "\nWould have (observe mode, last %d)\n", statusDecisions)
	if len(r.Decisions) == 0 {
		fmt.Fprintln(out, "  nothing yet")
	}
	for _, dv := range r.Decisions {
		when := "?"
		if !dv.TS.IsZero() {
			when = shortDuration(now.Sub(dv.TS)) + " ago"
		}
		project := baseName(dv.Project)
		if dv.Project == "" {
			project = dv.SessionKey
		}
		reason := strings.TrimPrefix(firstLine(dv.Reason), dv.Detector+": ")
		if reason == "" {
			reason = firstLine(dv.Message)
		}
		detector := dv.Detector
		if detector == "" {
			detector = "?"
		}
		fmt.Fprintf(out, "  %-8s %-6s %-17s %s: %s\n", when, dv.Action, detector, project, clip(reason, 100))
	}
}

// shortDuration formats d as 45s, 12m, 3h05m, or 2d04h.
func shortDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// baseName is the last element of a path with either separator (a
// session's cwd may come from another OS).
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
