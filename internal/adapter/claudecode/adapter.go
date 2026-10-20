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
