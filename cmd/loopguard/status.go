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
	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// statusDecisions is how many decisions status shows.
const statusDecisions = 10

// statusReport is `loopguard status --json`.
type statusReport struct {
	Daemon    daemonStatus    `json:"daemon"`
	Mode      modeReport      `json:"mode"`
	Sessions  []sessionStatus `json:"active_sessions"`
	Decisions []decisionView  `json:"decisions"`
	Today     todayUsage      `json:"today"`
}

type daemonStatus struct {
	Up      bool    `json:"up"`
	PID     int     `json:"pid,omitempty"`
	Port    int     `json:"port,omitempty"`
	Version string  `json:"version,omitempty"`
	Uptime  float64 `json:"uptime_seconds,omitempty"`
	Capture bool    `json:"capture"`
	Home    string  `json:"home"`
}

// sessionStatus is an active session, with the mode at its latest event.
type sessionStatus struct {
	store.Session
	Mode    string `json:"mode,omitempty"`
	ModeWhy string `json:"mode_why,omitempty"`
	// Usage so far of the whole session (subagents included), API-equivalent.
	Usage *daemon.UsageView `json:"usage,omitempty"`
}

type decisionView struct {
	store.Intervention
	Project string `json:"project,omitempty"` // the session's working directory
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the level, the daemon, active sessions, and recent decisions",
		Long: `Show the level (observe, notify, assist, guard) and why, the pauses, whether
the daemon is up, the active Claude Code sessions (project, last activity,
tool calls, failures, level and why, and the command that stops each), and
the last 10 decisions: real ones (they reached the agent) and the ones
LoopGuard only recorded (a lower level, or a pause), with the detector, the reason,
and the outcome 10 tool calls later. When the daemon is down, the
decisions come from the database.`,
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
	r := statusReport{Daemon: daemonStatus{Home: dir, Capture: capture.FlagOn(dir)},
		Sessions: []sessionStatus{}, Decisions: []decisionView{}}
	cwds := map[string]string{} // session key → cwd
	info, h, up := running(dir)
	r.Mode = collectMode(dir, time.Now())
	if up {
		r.Daemon = daemonStatus{Up: true, PID: h.PID, Port: info.Port, Version: h.Version, Home: dir,
			Uptime: time.Since(uptimeStart(info, h)).Seconds(), Capture: h.Capture || r.Daemon.Capture}
		var list struct {
			Sessions []struct {
				sessionStatus
				Active bool `json:"active"`
			} `json:"sessions"`
		}
		if apiCall(dir, info.Port, http.MethodGet, "/v1/sessions", &list) == nil {
			for _, s := range list.Sessions {
				cwds[s.Key] = s.Cwd
				if s.Active && s.SessionID != doctorSession {
					r.Sessions = append(r.Sessions, s.sessionStatus)
				}
			}
		}
		var usage daemon.UsageReply
		if apiCall(dir, info.Port, http.MethodGet, "/v1/usage", &usage) == nil {
			for i := range r.Sessions {
				if u, ok := usage.Sessions[r.Sessions[i].SessionID]; ok {
					r.Sessions[i].Usage = &u
				}
			}
			r.Today = todayUsage{Source: "daemon", Tokens: usage.Today.Tokens, CostUSD: usage.Today.CostUSD,
				UnpricedTokens: usage.Today.UnpricedTokens, Budget: usage.Budgets.Daily.CostUSD}
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
	if !fileExists(filepath.Join(dir, home.DBFile)) {
		return r
	}
	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		return r
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.Today = todayFromStore(ctx, st, dir)
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
	printMode(out, r.Mode)
	d := r.Daemon
	capt := "off"
	if d.Capture {
		capt = "on"
	}
	if d.Up {
		fmt.Fprintf(out, "Daemon: up %s (pid %d, port %d, version %s), capture %s\n",
			shortDuration(time.Duration(d.Uptime*float64(time.Second))), d.PID, d.Port, d.Version, capt)
	} else {
		fmt.Fprintf(out, "Daemon: down (the next hook event starts it), capture %s\n", capt)
	}
	printToday(out, r.Today)

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
			fmt.Fprintf(out, "  %s  [%s]\n    last activity %s ago, %d tool calls, %d failures", name, shortSID(s.SessionID),
				shortDuration(now.Sub(s.LastSeenAt)), s.ToolCalls, s.Failures)
			if s.LoopsDetected > 0 {
				fmt.Fprintf(out, ", %d %s", s.LoopsDetected, map[bool]string{true: "loop", false: "loops"}[s.LoopsDetected == 1])
			}
			if u := s.Usage; u != nil && !strings.Contains(s.Key, "/") {
				fmt.Fprintf(out, "\n    %s tokens, %s API-equivalent so far (subagents included)", compactCount(u.Tokens), cost.FormatUSD(u.CostUSD))
			}
			if s.Mode != "" {
				fmt.Fprintf(out, "\n    level %s (%s)", s.Mode, s.ModeWhy)
			}
			if !strings.Contains(s.Key, "/") && config.LevelRank(s.Mode) > config.LevelRank(config.ModeObserve) {
				fmt.Fprintf(out, "; stop its turn: loopguard stop %s", shortSID(s.SessionID))
			}
			fmt.Fprintln(out)
		}
	}

	fmt.Fprintf(out, "\nRecent decisions (last %d; real = reached the agent, would = recorded only)\n", statusDecisions)
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
		kind := "would"
		if dv.Mode == store.ModeEnforce {
			kind = "real"
		}
		outcome := dv.Outcome
		if outcome == "" {
			outcome = "pending"
		}
		fmt.Fprintf(out, "  %-8s %-5s %-5s %-17s %-14s %s: %s\n", when, kind, dv.Action, detector, outcome, project, clip(reason, 100))
	}
}

// shortSID is the session id as `loopguard pause --session` takes it.
func shortSID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
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

// todayUsage is the day's usage of all sessions, against the daily cost
// budget.
type todayUsage struct {
	Source         string       `json:"source,omitempty"` // daemon | database ("": nothing recorded)
	Tokens         int64        `json:"tokens"`
	CostUSD        float64      `json:"cost_usd"` // API-equivalent
	UnpricedTokens int64        `json:"unpriced_tokens,omitempty"`
	Budget         config.Limit `json:"daily_cost_budget"`
}

// todayFromStore adds up today's usage from the database (daemon down).
func todayFromStore(ctx context.Context, st *store.Store, dir string) todayUsage {
	t := todayUsage{Source: "database", Budget: config.NewLoader(dir, config.LoaderOptions{}).For("").Budgets.Daily.CostUSD}
	n := time.Now()
	rows, err := st.UsageRows(ctx, store.UsageFilter{Since: time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())})
	if err != nil {
		return t
	}
	pricing, _, _ := cost.LoadPricing(dir)
	for _, u := range rows {
		m := usageMessage(u)
		t.Tokens += m.Usage.Total()
		if usd, ok := pricing.Cost(m); ok {
			t.CostUSD += usd
		} else {
			t.UnpricedTokens += m.Usage.Total()
		}
	}
	return t
}

func printToday(out io.Writer, t todayUsage) {
	if t.Source == "" {
		return
	}
	fmt.Fprintf(out, "Today: %s API-equivalent, %s tokens", cost.FormatUSD(t.CostUSD), compactCount(t.Tokens))
	if t.UnpricedTokens > 0 {
		fmt.Fprintf(out, " (%s not priced)", compactCount(t.UnpricedTokens))
	}
	var lim []string
	if t.Budget.Soft > 0 {
		lim = append(lim, fmt.Sprintf("soft %s", cost.FormatUSD(t.Budget.Soft)))
	}
	if t.Budget.Hard > 0 {
		lim = append(lim, fmt.Sprintf("hard %s", cost.FormatUSD(t.Budget.Hard)))
	}
	if len(lim) == 0 {
		fmt.Fprintln(out, "; no daily cost budget")
		return
	}
	fmt.Fprintf(out, "; daily budget %s", strings.Join(lim, ", "))
	switch {
	case t.Budget.Hard > 0 && t.CostUSD > t.Budget.Hard:
		fmt.Fprint(out, " (over the hard limit)")
	case t.Budget.Soft > 0 && t.CostUSD > t.Budget.Soft:
		fmt.Fprint(out, " (past the soft limit)")
	}
	fmt.Fprintln(out)
}
