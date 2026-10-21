package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
	"github.com/Somsubhra-Nandi/loopguard/internal/adapter/claudecode"
	"github.com/Somsubhra-Nandi/loopguard/internal/capture"
	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/notify"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// doctorSession is the session id of doctor's synthetic events; status
// leaves it out.
const doctorSession = "loopguard-doctor"

// roundTripLimit is how long the synthetic event may take end to end.
const roundTripLimit = time.Second

func newDoctorCmd() *cobra.Command {
	var sf settingsFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that LoopGuard is installed and working",
		Long: `Check the LoopGuard installation: both binaries and their versions, the
hooks in Claude Code's settings, the daemon, a synthetic event sent through
loopguard-hook to the daemon and back, the database, and capture mode.
Each check prints ok, warn, or fail, with a fix for anything not ok. Starts
the daemon if it isn't running (as the next hook would). Exits 1 if a check
fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			settings, err := sf.path()
			if err != nil {
				return err
			}
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			checks := runDoctor(dir, settings)
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(map[string]any{"checks": checks}); err != nil {
					return err
				}
			} else {
				printChecks(out, checks)
			}
			for _, c := range checks {
				if c.Status == adapter.StatusFail {
					return errSilent
				}
			}
			return nil
		},
	}
	sf.register(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the checks as JSON")
	return cmd
}

func printChecks(out io.Writer, checks []adapter.Check) {
	width := 0
	for _, c := range checks {
		width = max(width, len(c.Name))
	}
	var warns, fails int
	fmt.Fprintln(out, "LoopGuard doctor")
	for _, c := range checks {
		fmt.Fprintf(out, "  %-4s  %-*s  %s\n", c.Status, width, c.Name, c.Detail)
		if c.Fix != "" && c.Status != adapter.StatusOK {
			fmt.Fprintf(out, "        %-*s  fix: %s\n", width, "", c.Fix)
		}
		switch c.Status {
		case adapter.StatusWarn:
			warns++
		case adapter.StatusFail:
			fails++
		}
	}
	if warns+fails == 0 {
		fmt.Fprintln(out, "All checks passed.")
	} else {
		fmt.Fprintf(out, "%d failed, %d warnings.\n", fails, warns)
	}
}

func runDoctor(dir, settings string) []adapter.Check {
	var checks []adapter.Check
	add := func(name string, st adapter.Status, detail, fix string) {
		checks = append(checks, adapter.Check{Name: name, Status: st, Detail: detail, Fix: fix})
	}

	// The hook binary: the one the settings run, else the one next to us.
	hook := ""
	if b, err := os.ReadFile(settings); err == nil {
		if hs, _, err := claudecode.FindHooks(b); err == nil && len(hs) > 0 {
			hook = hs[0].Command
		}
	}
	if hook == "" {
		hook, _ = defaultHookPath()
	}
	hookVersion, hookErr := binaryVersion(hook)
	switch {
	case hookErr != nil:
		add("binaries", adapter.StatusFail, "loopguard "+version+"; no working loopguard-hook at "+hook+" ("+hookErr.Error()+")",
			"build it: go build -o "+quoteArg(hook)+" ./cmd/loopguard-hook")
	case hookVersion != version:
		add("binaries", adapter.StatusWarn, fmt.Sprintf("loopguard %s but loopguard-hook %s (%s)", version, hookVersion, hook),
			"rebuild both from the same checkout")
	default:
		add("binaries", adapter.StatusOK, fmt.Sprintf("loopguard and loopguard-hook %s (%s)", version, filepath.Dir(hook)), "")
	}

	checks = append(checks, claudecode.New().Doctor(settings)...)
	checks = append(checks, disableAllHooksElsewhere(settings)...)

	// The daemon.
	info, h, up := running(dir)
	started := false
	if !up {
		if err := startDetached(io.Discard, dir); err == nil {
			info, h, up = running(dir)
			started = up
		}
	}
	switch {
	case !up:
		add("daemon", adapter.StatusFail, "not running and could not be started",
			"run 'loopguard daemon' in a terminal to see why, or read "+filepath.Join(dir, home.LogDir, "daemon.log"))
	case h.Version != version:
		add("daemon", adapter.StatusWarn, fmt.Sprintf("running version %s (pid %d), this is %s", h.Version, h.PID, version),
			"restart it: loopguard daemon stop (the next hook starts the new one)")
	default:
		detail := fmt.Sprintf("healthy on 127.0.0.1:%d (pid %d, up %s)", info.Port, h.PID, time.Since(uptimeStart(info, h)).Round(time.Second))
		if started {
			detail = fmt.Sprintf("was not running; started it (pid %d, port %d)", h.PID, info.Port)
		}
		add("daemon", adapter.StatusOK, detail, "")
	}
	if _, err := home.ReadToken(dir); err != nil {
		add("token", adapter.StatusFail, err.Error(), "delete "+filepath.Join(dir, home.TokenFile)+" and restart the daemon (loopguard daemon stop)")
	} else {
		add("token", adapter.StatusOK, "readable ("+filepath.Join(dir, home.TokenFile)+")", "")
	}

	// A synthetic event through the real hook binary.
	switch {
	case !up:
		add("round trip", adapter.StatusFail, "skipped: the daemon is not running", "fix the daemon check first")
	case hookErr != nil:
		add("round trip", adapter.StatusFail, "skipped: no loopguard-hook binary", "fix the binaries check first")
	default:
		took, err := roundTrip(dir, info.Port, hook)
		switch {
		case err != nil:
			add("round trip", adapter.StatusFail, err.Error(), "run loopguard-hook with LOOPGUARD_DEBUG=1 to see why")
		case took > roundTripLimit:
			add("round trip", adapter.StatusFail, fmt.Sprintf("took %s (limit %s)", took.Round(time.Millisecond), roundTripLimit),
				"check the daemon log for slow events: "+filepath.Join(dir, home.LogDir, "daemon.log"))
		default:
			add("round trip", adapter.StatusOK, fmt.Sprintf("loopguard-hook -> daemon -> \"none\" in %s", took.Round(time.Millisecond)), "")
		}
	}

	// The database (opening it applies pending migrations).
	dbPath := filepath.Join(dir, home.DBFile)
	if st, err := store.Open(dbPath); err != nil {
		add("database", adapter.StatusFail, err.Error(), "move "+dbPath+" aside; the daemon creates a new one")
	} else {
		v, err := st.SchemaVersion(context.Background())
		st.Close()
		if err != nil || v != store.SchemaLatest {
			add("database", adapter.StatusFail, fmt.Sprintf("schema version %d, want %d (%v)", v, store.SchemaLatest, err), "move "+dbPath+" aside")
		} else {
			add("database", adapter.StatusOK, fmt.Sprintf("opens, schema v%d (%s)", v, dbPath), "")
		}
	}

	checks = append(checks, costChecks(dir, dbPath)...)

	// The config files and the mode they give.
	m := collectMode(dir, time.Now())
	switch c := m.Config; {
	case c.Error != "":
		add("config", adapter.StatusWarn, c.Path+" is invalid, so the defaults (observe level) are used: "+c.Error,
			"fix the file, or move it aside; 'loopguard mode' writes a new one")
	case !c.Exists:
		add("config", adapter.StatusOK, "no "+config.GlobalFile+" yet; the defaults apply ("+c.Path+")", "")
	case len(c.Warnings) > 0:
		add("config", adapter.StatusWarn, c.Path+": "+strings.Join(c.Warnings, "; "), "remove or fix the keys it names")
	default:
		add("config", adapter.StatusOK, "valid ("+c.Path+")", "")
	}
	if rp := m.Repo; rp != nil {
		switch {
		case rp.File.Error != "":
			add("repo config", adapter.StatusWarn, rp.File.Path+" is invalid and ignored: "+rp.File.Error, "fix or remove it")
		case len(rp.Notes) > 0 || len(rp.File.Warnings) > 0:
			add("repo config", adapter.StatusWarn, strings.Join(append(rp.File.Warnings, rp.Notes...), "; "),
				"a repo file can only raise the level and tighten thresholds; remove the keys it ignores")
		default:
			add("repo config", adapter.StatusOK, "valid ("+rp.File.Path+"): "+rp.Mode.Mode+" for this repo", "")
		}
	}
	modeDetail := m.Mode.Mode + " (" + m.Mode.Why + ")"
	if n := len(m.Pauses); n > 0 {
		modeDetail += fmt.Sprintf("; %d %s in effect (loopguard pause)", n, map[bool]string{true: "pause", false: "pauses"}[n == 1])
	}
	add("level", adapter.StatusOK, modeDetail, "")

	// Each active session's level, and why (a repo file, a pause, ...).
	if up {
		var list struct {
			Sessions []struct {
				sessionStatus
				Active bool `json:"active"`
			} `json:"sessions"`
		}
		if err := apiCall(dir, info.Port, http.MethodGet, "/v1/sessions", &list); err == nil {
			var lines []string
			for _, s := range list.Sessions {
				if s.Active && s.SessionID != doctorSession && s.Mode != "" {
					lines = append(lines, fmt.Sprintf("%s [%s]: %s (%s)", baseName(s.Cwd), shortSID(s.SessionID), s.Mode, s.ModeWhy))
				}
			}
			if len(lines) == 0 {
				add("sessions", adapter.StatusOK, "none active", "")
			} else {
				add("sessions", adapter.StatusOK, strings.Join(lines, "; "), "")
			}
		}
	}

	// Notifications: the channels on, and whether they can work here.
	if n, mode, err := notifyConfig(dir); err == nil {
		var on, problems []string
		for _, name := range []string{config.ChannelDesktop, config.ChannelTerminal, config.ChannelNtfy, config.ChannelWebhook} {
			ch := n.Channels()[name]
			if !ch.Enabled {
				continue
			}
			on = append(on, name)
			switch {
			case name == config.ChannelDesktop:
				if ok, how := notify.DesktopAvailable(); !ok {
					problems = append(problems, "desktop: "+how)
				}
			case name == config.ChannelNtfy && ch.Topic == "":
				problems = append(problems, "ntfy has no topic")
			case name == config.ChannelWebhook && ch.URL == "":
				problems = append(problems, "webhook has no url")
			}
		}
		detail := "channels on: " + strings.Join(on, ", ")
		if len(on) == 0 {
			detail = "no channel on"
		}
		if !config.Notifies(mode.Mode) {
			detail += " (nothing is sent at the " + mode.Mode + " level)"
		}
		if len(problems) > 0 {
			add("notifications", adapter.StatusWarn, detail+"; "+strings.Join(problems, "; "), "loopguard notify setup ntfy|webhook, then loopguard notify test")
		} else {
			add("notifications", adapter.StatusOK, detail+" (test them: loopguard notify test)", "")
		}
	}

	// Capture mode.
	flag := capture.FlagOn(dir)
	switch {
	case up && h.Capture && !flag:
		add("capture", adapter.StatusOK, "on (LOOPGUARD_CAPTURE=1 in the daemon's environment)", "")
	case flag:
		add("capture", adapter.StatusOK, "on: payloads are saved to "+capture.Dir(dir)+" (turn off: loopguard capture off)", "")
	default:
		add("capture", adapter.StatusOK, "off", "")
	}
	return checks
}

// disableAllHooksElsewhere warns about disableAllHooks in the other
// settings files Claude Code merges for the current project.
func disableAllHooksElsewhere(checked string) []adapter.Check {
	var files []string
	if p, err := claudecode.UserSettingsPath(); err == nil {
		files = append(files, p)
	}
	if wd, err := os.Getwd(); err == nil {
		root := gitRoot(wd)
		files = append(files, filepath.Join(root, ".claude", "settings.json"), claudecode.ProjectSettingsPath(root))
	}
	var checks []adapter.Check
	for _, f := range files {
		if sameFile(f, checked) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if _, off, err := claudecode.FindHooks(b); err == nil && off {
			checks = append(checks, adapter.Check{Name: "hooks enabled", Status: adapter.StatusWarn,
				Detail: `"disableAllHooks": true in ` + f + `: Claude Code runs no hooks`, Fix: "remove disableAllHooks from " + f})
		}
	}
	return checks
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func uptimeStart(info home.DaemonInfo, h daemon.Health) time.Time {
	if !h.Started.IsZero() {
		return h.Started
	}
	return info.StartedAt
}

// binaryVersion runs `<hook> --version` and returns the version it prints.
func binaryVersion(hook string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, hook, "--version").Output()
	if err != nil {
		return "", err
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(out)), claudecode.HookBinary+" ")
	if !ok {
		return "", fmt.Errorf("unexpected --version output %q", out)
	}
	return v, nil
}

// roundTrip sends a synthetic Stop event through the hook binary and
// checks that the daemon saw it and the decision was "none" (no output,
// exit 0). LOOPGUARD_DEBUG=1 makes the shim explain any failure on
// stderr, so a silent fail-open can't pass for success.
func roundTrip(dir string, port int, hook string) (time.Duration, error) {
	payload, _ := json.Marshal(map[string]any{
		"session_id": doctorSession, "hook_event_name": "Stop", "cwd": dir, "stop_hook_active": false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, hook, "--agent", "claude-code")
	cmd.Env = append(os.Environ(), home.EnvHome+"="+dir, "LOOPGUARD_DEBUG=1", "LOOPGUARD_NO_AUTOSTART=1")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	sent := time.Now()
	err := cmd.Run()
	took := time.Since(sent)
	switch {
	case err != nil:
		return took, fmt.Errorf("loopguard-hook failed: %v %s", err, strings.TrimSpace(stderr.String()))
	case stderr.Len() > 0:
		return took, errors.New(strings.TrimSpace(stderr.String()))
	case stdout.Len() > 0:
		return took, fmt.Errorf("expected no output (decision none), got %q", stdout.String())
	}
	var detail struct {
		Session store.Session `json:"session"`
	}
	if err := apiCall(dir, port, http.MethodGet, "/v1/sessions/"+url.PathEscape(doctorSession), &detail); err != nil {
		return took, fmt.Errorf("the daemon has no record of the event: %v", err)
	}
	if detail.Session.LastSeenAt.Before(sent.Add(-time.Second)) {
		return took, errors.New("the daemon did not record the event")
	}
	return took, nil
}

// costChecks covers cost accounting (M4): pricing.yaml, prices nobody
// verified, models seen in the last 30 days without a price, and the
// budgets in effect.
func costChecks(dir, dbPath string) []adapter.Check {
	var checks []adapter.Check
	add := func(name string, st adapter.Status, detail, fix string) {
		checks = append(checks, adapter.Check{Name: name, Status: st, Detail: detail, Fix: fix})
	}
	path := filepath.Join(dir, cost.PricingFile)
	pricing, exists, err := cost.LoadPricing(dir)
	switch {
	case err != nil:
		add("pricing", adapter.StatusWarn, path+" is invalid, so the shipped prices are used: "+err.Error(),
			"fix the file, or move it aside (the daemon writes a fresh one at start)")
	case !exists:
		add("pricing", adapter.StatusWarn, "no "+path+"; the shipped prices are used",
			"restart the daemon (loopguard daemon stop; the next hook starts it), which writes it")
	default:
		if u := pricing.Unverified(); len(u) > 0 {
			names := make([]string, len(u))
			for i, m := range u {
				names[i] = m.Match
			}
			add("pricing", adapter.StatusWarn, "unverified prices (their cost shows as unknown): "+strings.Join(names, ", "),
				"fill them in from the provider's pricing page in "+path+" and set checked: <date>")
		} else {
			checked := map[string]bool{}
			for _, m := range pricing.Models {
				checked[m.Checked] = true
			}
			add("pricing", adapter.StatusOK, fmt.Sprintf("%d model prices, checked %s (%s); costs are API-equivalent",
				len(pricing.Models), strings.Join(sortedKeys(checked), ", "), path), "")
		}
	}

	if fileExists(dbPath) {
		if st, err := store.Open(dbPath); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			rows, err := st.UsageRows(ctx, store.UsageFilter{Since: time.Now().Add(-30 * 24 * time.Hour)})
			cancel()
			st.Close()
			if err == nil {
				unknown := map[string]int64{}
				for _, u := range rows {
					if u.Model != cost.SyntheticModel && !pricing.Priced(u.Model) {
						unknown[u.Model] += usageMessage(u).Usage.Total()
					}
				}
				if len(unknown) > 0 {
					var parts []string
					for _, m := range sortedKeys(boolKeys(unknown)) {
						parts = append(parts, fmt.Sprintf("%s (%s tokens)", m, compactCount(unknown[m])))
					}
					add("unknown models", adapter.StatusWarn, "seen in the last 30 days without a price, cost unknown: "+strings.Join(parts, ", "),
						"add them to "+path+" from the provider's pricing page")
				}
			}
		}
	}

	b := config.NewLoader(dir, config.LoaderOptions{}).For("").Budgets
	add("budgets", adapter.StatusOK, describeBudgets(b)+" (soft: notify and one note to the agent; hard: stop the turn at the guard level)", "")
	return checks
}

func boolKeys[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// describeBudgets lists the limits set, e.g. "daily cost soft $20.00;
// session tool calls hard 500".
func describeBudgets(b config.Budgets) string {
	var parts []string
	for _, scope := range config.Scopes {
		for _, m := range config.Metrics {
			l := b.Scope(scope).Get(m)
			if !l.On() {
				continue
			}
			var lv []string
			for _, x := range []struct {
				name string
				v    float64
			}{{"soft", l.Soft}, {"hard", l.Hard}} {
				if x.v <= 0 {
					continue
				}
				val := fmt.Sprintf("%g", x.v)
				if m == config.MetricCost {
					val = cost.FormatUSD(x.v)
				}
				lv = append(lv, x.name+" "+val)
			}
			parts = append(parts, fmt.Sprintf("%s %s %s", scope, strings.ReplaceAll(m, "_", " "), strings.Join(lv, ", ")))
		}
	}
	if len(parts) == 0 {
		return "none set"
	}
	return strings.Join(parts, "; ")
}
