package adapter

import (
	"errors"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// Adapter translates between one agent's hook schema and LoopGuard's
// events and decisions, and installs LoopGuard into the agent (LLD §3).
type Adapter interface {
	Kind() event.AgentKind
	// Parse maps a raw hook payload to an Event. Unknown fields are
	// ignored; an unknown hook event parses to an Event with an empty
	// Phase, which callers ignore.
	Parse(raw []byte) (*event.Event, error)
	// Render turns a decision into the agent's hook output: what the hook
	// prints on stdout and its exit code.
	Render(ev *event.Event, d event.Decision) (stdout []byte, exitCode int)
	Install(opts InstallOpts) (InstallReport, error) // merge hooks, backup, idempotent
	Uninstall() error
	Doctor() []Check
}

// InstallOpts configures Install.
type InstallOpts struct {
	Project    bool   // install into the project's settings instead of the user's
	ProjectDir string // project root when Project is set
	Binary     string // absolute path of the loopguard binary
}

// InstallReport says what Install changed.
type InstallReport struct {
	SettingsPath string   // file written
	BackupPath   string   // backup of the previous file ("" if there was none)
	Added        []string // hook events added
	Unchanged    []string // hook events already installed
}

// Check is one doctor check.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// ErrNotImplemented is returned by adapter features that aren't built yet.
var ErrNotImplemented = errors.New("not implemented yet")

// Registry finds adapters by agent kind.
type Registry map[event.AgentKind]Adapter

// NewRegistry returns a registry of the given adapters.
func NewRegistry(as ...Adapter) Registry {
	r := Registry{}
	for _, a := range as {
		r[a.Kind()] = a
	}
	return r
}

// Get returns the adapter for an agent kind ("claude-code", "codex").
func (r Registry) Get(kind string) (Adapter, bool) {
	a, ok := r[event.AgentKind(kind)]
	return a, ok
}
