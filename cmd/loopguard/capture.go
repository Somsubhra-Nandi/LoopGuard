package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter/claudecode"
	"github.com/Somsubhra-Nandi/loopguard/internal/capture"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
)

func newCaptureCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capture",
		Short: "Save real hook payloads for LoopGuard's golden tests",
		Long: `Capture mode saves the hook payloads the daemon receives, redacted and with
your home directory replaced by <HOME>, to <home>\capture\claude-code\ (at
most 20 per event type). 'capture on' and 'capture off' take effect within a
few seconds, without restarting the daemon. LOOPGUARD_CAPTURE=1 in the
daemon's environment still turns it on too.

'capture export' copies the payloads into testdata\private (git-ignored)
with a final anonymization pass, plus the expected .golden file for each;
'capture export --structure-only' writes a committable copy with the content
replaced by placeholders.`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "on",
			Short: "Start saving hook payloads",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				dir, err := home.Dir()
				if err != nil {
					return err
				}
				if err := capture.SetFlag(dir, true); err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintln(out, "Capture on.")
				fmt.Fprintf(out, "  Payloads go to %s (redacted; at most %d per event type).\n", filepath.Join(capture.Dir(dir), "claude-code"), capture.MaxPerEvent)
				fmt.Fprintf(out, "  The daemon picks this up within %s; no restart needed.\n", capture.SwitchTTL)
				fmt.Fprintln(out, "  Turn it off with: loopguard capture off")
				return nil
			},
		},
		&cobra.Command{
			Use:   "off",
			Short: "Stop saving hook payloads (keeps what was saved)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				dir, err := home.Dir()
				if err != nil {
					return err
				}
				if err := capture.SetFlag(dir, false); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Capture off. Saved payloads stay in", capture.Dir(dir))
				if _, h, up := running(dir); up && h.Capture {
					fmt.Fprintf(cmd.OutOrStdout(), "Note: if the daemon runs with %s=1 it keeps capturing until restarted without it.\n", capture.EnvCapture)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show whether capture is on and what was saved",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				dir, err := home.Dir()
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				state := "off"
				if capture.FlagOn(dir) {
					state = "on (" + capture.FlagPath(dir) + ")"
				}
				fmt.Fprintln(out, "Capture:", state)
				if _, h, up := running(dir); up {
					fmt.Fprintln(out, "Daemon capturing:", map[bool]string{true: "yes", false: "no"}[h.Capture])
				} else {
					fmt.Fprintln(out, "Daemon: not running")
				}
				src := filepath.Join(capture.Dir(dir), "claude-code")
				counts := map[string]int{}
				files, _ := filepath.Glob(filepath.Join(src, "*.json"))
				for _, f := range files {
					name := strings.TrimSuffix(filepath.Base(f), ".json")
					if i := strings.LastIndexByte(name, '-'); i > 0 {
						name = name[:i]
					}
					counts[name]++
				}
				fmt.Fprintf(out, "Saved: %d payloads in %s\n", len(files), src)
				names := make([]string, 0, len(counts))
				for n := range counts {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					fmt.Fprintf(out, "  %-20s %d\n", n, counts[n])
				}
				return nil
			},
		},
		newCaptureExportCmd(),
	)
	return cmd
}

func newCaptureExportCmd() *cobra.Command {
	var outDir, ccVersion string
	var structureOnly bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Copy captured payloads into testdata, anonymized, with .golden files",
		Long: `Copy the captured Claude Code payloads to --out (default, inside the
LoopGuard repo: testdata\private\hooks\claude-code\<claude --version>\, which
git ignores). On top of capture's redaction it redacts again, replaces
usernames and project names learned from the paths with "user" and "project",
replaces email addresses, and writes the expected .golden file next to each
payload. These are your real prompts and tool output: never commit them.

With --structure-only it also replaces every string in content fields
(prompt, tool_input, tool_response, error, last_assistant_message, ...) and
every unrecognized field with "<str:N>" (N its length), keeping all keys,
nesting and value types, and the enum-like fields (hook_event_name,
tool_name, permission_mode, ...), ids and paths. Its default --out is
testdata\hooks\claude-code\<version>\; those files are safe to commit.

Then run:
  go test ./internal/adapter/claudecode ./internal/capture`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := home.Dir()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if ccVersion == "" {
				ccVersion = claudeVersion()
			}
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			root := gitRoot(wd)
			committed := filepath.Join(root, "testdata", "hooks")
			if outDir == "" {
				if _, err := os.Stat(committed); err != nil {
					return errors.New(`pass --out (the default, testdata\[private\]hooks\claude-code\<version>, needs the LoopGuard repo as the current directory)`)
				}
				base := filepath.Join(root, "testdata", "private", "hooks", "claude-code")
				if structureOnly {
					base = filepath.Join(committed, "claude-code")
				}
				outDir = filepath.Join(base, ccVersion)
			}
			if !structureOnly && within(outDir, committed) {
				return fmt.Errorf(`%s is committed: real payloads go to testdata\private\hooks (the default); use --structure-only for testdata\hooks`, outDir)
			}
			users := []string{os.Getenv("USERNAME"), os.Getenv("USER")}
			st, err := capture.Export(filepath.Join(capture.Dir(dir), "claude-code"), outDir,
				capture.ExportOptions{Usernames: users, Golden: claudecode.Golden, StructureOnly: structureOnly})
			if err != nil {
				return err
			}
			kind := "payloads"
			if structureOnly {
				kind = "structure-only payloads"
			}
			fmt.Fprintf(out, "Exported %d %s (Claude Code %s) to %s\n", len(st.Files), kind, ccVersion, outDir)
			fmt.Fprintf(out, "  with %d .golden files (the events they map to)\n", st.Goldens)
			if len(st.Skipped) > 0 {
				fmt.Fprintf(out, "  skipped (not JSON): %s\n", strings.Join(st.Skipped, ", "))
			}
			if len(st.Replaced) > 0 {
				var names []string
				for w, r := range st.Replaced {
					names = append(names, fmt.Sprintf("%q→%s", w, r))
				}
				sort.Strings(names)
				fmt.Fprintf(out, "  replaced names: %s\n", strings.Join(names, ", "))
			}
			if structureOnly {
				fmt.Fprintln(out, "Review the ids and paths before committing them, then run:")
			} else {
				fmt.Fprintln(out, "These contain your real prompts and tool output: keep them local. Run:")
			}
			fmt.Fprintln(out, "  go test ./internal/adapter/claudecode ./internal/capture")
			return nil
		},
	}
	cmd.Flags().StringVar(&outDir, "out", "", `output directory (default: testdata\private\hooks\claude-code\<version> in this repo, or testdata\hooks\claude-code\<version> with --structure-only)`)
	cmd.Flags().StringVar(&ccVersion, "claude-version", "", "Claude Code version for the default directory (default: from claude --version)")
	cmd.Flags().BoolVar(&structureOnly, "structure-only", false, "replace content strings with <str:N> placeholders, for committing")
	return cmd
}

// within reports whether path is dir or below it.
func within(path, dir string) bool {
	p, err1 := filepath.Abs(path)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(d, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+[0-9A-Za-z.+-]*`)

// claudeVersion returns the version `claude --version` prints, or
// "unknown".
func claudeVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		return "unknown"
	}
	if v := semverRe.FindString(string(b)); v != "" {
		return v
	}
	return "unknown"
}
