package claudecode

import (
	"bytes"
	"encoding/json"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// goldenEvent is the part of an Event the golden files pin: everything the
// adapter maps, nothing that varies per run (ID, ReceivedAt, Raw).
type goldenEvent struct {
	Agent      event.AgentKind `json:"agent"`
	SessionID  string          `json:"session_id"`
	AgentID    string          `json:"agent_id,omitempty"`
	Phase      event.Phase     `json:"phase"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Cwd        string          `json:"cwd,omitempty"`
	Transcript string          `json:"transcript,omitempty"`
	PermMode   string          `json:"perm_mode,omitempty"`
	Prompt     string          `json:"prompt,omitempty"`
	Model      string          `json:"model,omitempty"`
	Source     string          `json:"source,omitempty"`
}

// Golden parses a hook payload and returns the content of its golden file
// (testdata/hooks/claude-code/**/<name>.golden): the mapped Event as
// indented JSON. `loopguard capture export` writes one per payload.
func Golden(raw []byte) ([]byte, error) {
	ev, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	in, err := compactJSON(ev.Input)
	if err != nil {
		return nil, err
	}
	out, err := compactJSON(ev.Output)
	if err != nil {
		return nil, err
	}
	g := goldenEvent{
		Agent: ev.Agent, SessionID: ev.SessionID, AgentID: ev.AgentID, Phase: ev.Phase,
		ToolName: ev.ToolName, ToolUseID: ev.ToolUseID, Input: in, Output: out,
		IsError: ev.IsError, Cwd: ev.Cwd, Transcript: ev.Transcript, PermMode: ev.PermMode,
		Prompt: ev.Prompt, Model: ev.Model, Source: ev.Source,
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func compactJSON(v json.RawMessage) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
