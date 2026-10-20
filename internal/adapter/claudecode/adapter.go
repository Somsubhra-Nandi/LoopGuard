package claudecode

import (
	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// Adapter is the Claude Code adapter.
type Adapter struct{}

// New returns the Claude Code adapter.
func New() Adapter { return Adapter{} }

var _ adapter.Adapter = Adapter{}

// Kind returns "claude-code".
func (Adapter) Kind() event.AgentKind { return event.AgentClaudeCode }

// Parse maps a hook payload to an Event; see Parse.
func (Adapter) Parse(raw []byte) (*event.Event, error) { return Parse(raw) }

// Render renders a decision; see Render.
func (Adapter) Render(ev *event.Event, d event.Decision) ([]byte, int) { return Render(ev, d) }

// Install merges LoopGuard's hooks into settings.json (LLD §3.3).
// TODO(M2-B): implement, with backup and idempotence.
func (Adapter) Install(adapter.InstallOpts) (adapter.InstallReport, error) {
	return adapter.InstallReport{}, adapter.ErrNotImplemented
}

// Uninstall removes LoopGuard's hooks from settings.json.
// TODO(M2-B): implement.
func (Adapter) Uninstall() error { return adapter.ErrNotImplemented }

// Doctor checks the Claude Code installation.
// TODO(M2-B): binary on PATH, hooks installed, test event round-trips.
func (Adapter) Doctor() []adapter.Check {
	return []adapter.Check{{Name: "claude-code hooks", OK: false, Detail: "doctor is not implemented yet (M2-B)"}}
}
