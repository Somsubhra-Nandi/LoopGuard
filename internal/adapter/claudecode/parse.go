package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// Hook event names (code.claude.com/docs/en/hooks), mapped to phases as in
// LLD §3.1. Events not listed parse to an empty phase and are ignored.
var phases = map[string]event.Phase{
	"SessionStart":       event.PhaseSessionStart,
	"UserPromptSubmit":   event.PhasePrompt,
	"PreToolUse":         event.PhasePre,
	"PostToolUse":        event.PhasePost,
	"PostToolUseFailure": event.PhasePostFailure,
	"PostToolBatch":      event.PhaseBatchEnd,
	"Stop":               event.PhaseStop,
	"SessionEnd":         event.PhaseSessionEnd,
}

// maxPrompt caps the prompt text kept in memory. Nothing downstream needs
// more than a turn boundary.
const maxPrompt = 1024

// Parse errors.
var (
	ErrNotJSON     = errors.New("payload is not a JSON object")
	ErrNoEventName = errors.New("payload has no hook_event_name")
	ErrNoSessionID = errors.New("payload has no session_id")
)

// Parse maps a Claude Code hook payload to an Event.
//
// It is lenient: unknown fields are ignored, missing optional fields are
// fine, and a field of an unexpected type is treated as missing rather
// than failing the payload. Only a non-object payload, or one without
// hook_event_name or session_id, is an error. An unknown hook event parses
// with an empty Phase.
func Parse(raw []byte) (*event.Event, error) {
	return parseAt(raw, time.Now())
}

// utf8BOM is stripped from payloads: Windows tools (PowerShell's pipe and
// Set-Content) add one, and encoding/json rejects it.
var utf8BOM = []byte("\xef\xbb\xbf")

func parseAt(raw []byte, now time.Time) (*event.Event, error) {
	raw = bytes.TrimPrefix(raw, utf8BOM)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, ErrNotJSON
	}
	name := str(m, "hook_event_name")
	if name == "" {
		return nil, ErrNoEventName
	}
	sid := str(m, "session_id")
	if sid == "" {
		return nil, ErrNoSessionID
	}
	ev := &event.Event{
		ID:         event.NewID(now),
		Agent:      event.AgentClaudeCode,
		SessionID:  sid,
		AgentID:    str(m, "agent_id"),
		Phase:      phases[name],
		Cwd:        str(m, "cwd"),
		Transcript: str(m, "transcript_path"),
		PermMode:   str(m, "permission_mode"),
		ReceivedAt: now,
		Raw:        json.RawMessage(raw),
	}
	switch ev.Phase {
	case event.PhaseSessionStart:
		ev.Source = str(m, "source")
		ev.Model = model(m["model"])
	case event.PhasePrompt:
		ev.Prompt = clip(str(m, "prompt"), maxPrompt)
		if !humanPrompt(ev.Prompt) {
			ev.Phase = event.PhaseAutoPrompt
		}
	case event.PhasePre, event.PhasePost, event.PhasePostFailure:
		ev.ToolName = ToolName(str(m, "tool_name"))
		ev.ToolUseID = str(m, "tool_use_id")
		ev.Input = object(m["tool_input"])
		if ev.Phase == event.PhasePost {
			ev.Output = present(m["tool_response"])
		}
		if ev.Phase == event.PhasePostFailure {
			ev.IsError = true
			// The failure text is in "error"; fall back to a response if a
			// version sends one instead.
			ev.Output = present(m["error"])
			if ev.Output == nil {
				ev.Output = present(m["tool_response"])
			}
		}
	}
	return ev, nil
}

// injectedPrefixes start prompt text that Claude Code submits on its own
// rather than the human typing it (the same shapes replay ignores as turn
// boundaries, LLD §14).
var injectedPrefixes = []string{"<task-notification>", "<system-reminder>", "<local-command-"}

// humanPrompt reports whether a UserPromptSubmit prompt is a human turn:
// non-empty and not injected content. Slash commands count; the human
// typed them.
func humanPrompt(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	for _, pre := range injectedPrefixes {
		if strings.HasPrefix(p, pre) {
			return false
		}
	}
	return true
}

// str returns m[key] if it is a JSON string, else "".
func str(m map[string]json.RawMessage, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// present returns v unless it is missing or null.
func present(v json.RawMessage) json.RawMessage {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return v
}

// object returns v if it is a JSON object, else nil: tool inputs are
// objects, and the normalizer expects one.
func object(v json.RawMessage) json.RawMessage {
	v = present(v)
	if v == nil {
		return nil
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(v, &probe) != nil {
		return nil
	}
	return v
}

// model reads SessionStart's model: a string, or an object with an id or a
// display name.
func model(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var o struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	if json.Unmarshal(v, &o) == nil {
		if o.ID != "" {
			return o.ID
		}
		return o.DisplayName
	}
	return ""
}

// clip cuts s to at most n bytes at a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
