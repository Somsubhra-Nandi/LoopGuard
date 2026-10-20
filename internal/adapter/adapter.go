package adapter

import (
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
	// Install merges LoopGuard's hooks into the agent's settings: idempotent,
	// keeping everything else, with a backup before the first change.
	Install(opts InstallOpts) (InstallReport, error)
	// Uninstall removes LoopGuard's hooks (and nothing else).
	Uninstall(opts InstallOpts) (InstallReport, error)
	// Doctor checks the hooks installed in a settings file.
	Doctor(settings string) []Check
}

// InstallOpts configures Install and Uninstall.
type InstallOpts struct {
	Settings string // settings file to change
	Hook     string // absolute path of the loopguard-hook binary (Install)
	DryRun   bool   // compute the change, write nothing
}

// InstallReport says what Install or Uninstall changed.
type InstallReport struct {
	SettingsPath  string
	BackupPath    string   // backup of the file before LoopGuard's first change ("" if none)
	BackupCreated bool     // the backup was written by this call
	Added         []string // hook events installed
	Updated       []string // hook events whose LoopGuard hook was replaced (new path, timeout, ...)
	Unchanged     []string // hook events already installed as wanted
	Removed       []string // hook events LoopGuard's hooks were removed from
	Before, After []byte   // the file's content (Before is nil if it didn't exist)
	Changed       bool     // After differs from Before (and was written, unless DryRun)
	Warnings      []string // things the user should know (e.g. disableAllHooks)
}

// Status is a doctor check's outcome.
type Status string

// Check outcomes.
const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Check is one doctor check.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"` // one-line suggestion when not ok
}

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
