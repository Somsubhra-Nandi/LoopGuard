package claudecode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
)

// Doctor checks the LoopGuard hooks in a Claude Code settings file: every
// event in Hooks has one, it runs an existing loopguard-hook binary, and
// the file doesn't disable all hooks.
func (Adapter) Doctor(settings string) []adapter.Check {
	const name = "hooks installed"
	b, err := os.ReadFile(settings)
	if errors.Is(err, fs.ErrNotExist) {
		return []adapter.Check{{Name: name, Status: adapter.StatusFail, Detail: settings + " does not exist",
			Fix: "run: loopguard init"}}
	}
	if err != nil {
		return []adapter.Check{{Name: name, Status: adapter.StatusFail, Detail: err.Error()}}
	}
	hooks, disableAll, err := FindHooks(b)
	if err != nil {
		return []adapter.Check{{Name: name, Status: adapter.StatusFail, Detail: settings + " is not valid JSON: " + err.Error(),
			Fix: "fix the JSON in " + settings + " (a backup may be at " + settings + BackupSuffix + ")"}}
	}

	var checks []adapter.Check
	have := map[string]bool{}
	for _, h := range hooks {
		have[h.Event] = true
	}
	var missing []string
	for _, h := range Hooks {
		if !have[h.Event] {
			missing = append(missing, h.Event)
		}
	}
	switch {
	case len(hooks) == 0:
		checks = append(checks, adapter.Check{Name: name, Status: adapter.StatusFail,
			Detail: "no LoopGuard hooks in " + settings, Fix: "run: loopguard init"})
	case len(missing) > 0:
		checks = append(checks, adapter.Check{Name: name, Status: adapter.StatusWarn,
			Detail: fmt.Sprintf("%d of %d events hooked in %s; missing %s", len(Hooks)-len(missing), len(Hooks), settings, strings.Join(missing, ", ")),
			Fix:    "run: loopguard init (it adds the missing ones)"})
	default:
		checks = append(checks, adapter.Check{Name: name, Status: adapter.StatusOK,
			Detail: fmt.Sprintf("%d events hooked in %s", len(Hooks), settings)})
	}

	if len(hooks) > 0 {
		paths := map[string]bool{}
		var bad []string
		for _, h := range hooks {
			if paths[h.Command] {
				continue
			}
			paths[h.Command] = true
			if fi, err := os.Stat(h.Command); err != nil || fi.IsDir() {
				bad = append(bad, h.Command)
			}
		}
		const hb = "hook binary"
		switch {
		case len(bad) > 0:
			checks = append(checks, adapter.Check{Name: hb, Status: adapter.StatusFail,
				Detail: "hooks run a missing program: " + strings.Join(bad, ", "),
				Fix:    "rebuild loopguard-hook there, or run loopguard init again from the new location"})
		case len(paths) > 1:
			checks = append(checks, adapter.Check{Name: hb, Status: adapter.StatusWarn,
				Detail: fmt.Sprintf("hooks run %d different loopguard-hook binaries", len(paths)), Fix: "run: loopguard init"})
		default:
			checks = append(checks, adapter.Check{Name: hb, Status: adapter.StatusOK, Detail: hooks[0].Command})
		}
	}
	if disableAll {
		checks = append(checks, adapter.Check{Name: "hooks enabled", Status: adapter.StatusWarn,
			Detail: `"disableAllHooks": true in ` + settings + `: Claude Code runs no hooks`,
			Fix:    "remove disableAllHooks from " + settings})
	}
	return checks
}
