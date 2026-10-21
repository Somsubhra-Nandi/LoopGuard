package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter/claudecode"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
)

// See docs/transcript-format.md for the format this reads.

// Stream identifies one transcript file: a main session or a subagent.
type Stream struct {
	Path      string
	SessionID string
	AgentID   string // "" for the main session
	Project   string // the encoded project directory name
}

// Key returns "session" or "session/agent".
func (s Stream) Key() string {
	if s.AgentID == "" {
		return s.SessionID
	}
	return s.SessionID + "/" + s.AgentID
}

// LineStats counts what the reader saw.
type LineStats struct {
	Lines       int `json:"lines"`
	Malformed   int `json:"malformed"`
	Ignored     int `json:"ignored"` // valid lines of types replay does not use
	ToolUses    int `json:"tool_uses"`
	ToolResults int `json:"tool_results"`
	Unjoined    int `json:"unjoined_results"` // results with no tool_use earlier in the file
	Prompts     int `json:"prompts"`
}

func (a *LineStats) add(b LineStats) {
	a.Lines += b.Lines
	a.Malformed += b.Malformed
	a.Ignored += b.Ignored
	a.ToolUses += b.ToolUses
	a.ToolResults += b.ToolResults
	a.Unjoined += b.Unjoined
	a.Prompts += b.Prompts
}

// Emit receives each event in order. fileHash is the content hash of the
// file after a Write (0 otherwise).
type Emit func(ev *event.Event, fileHash uint64)

// line is the part of a transcript line replay uses.
type line struct {
	Type             string `json:"type"`
	Timestamp        string `json:"timestamp"`
	SessionID        string `json:"sessionId"`
	AgentID          string `json:"agentId"`
	Cwd              string `json:"cwd"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	PermissionMode   string `json:"permissionMode"`
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Text      string          `json:"text"`
}

type pendingUse struct {
	tool     string
	input    json.RawMessage
	fileHash uint64
}

// reader converts one stream's lines to events.
type reader struct {
	st      Stream
	emit    Emit
	stats   LineStats
	pending map[string]pendingUse
	last    time.Time
	cwd     string
	perm    string
}

// ReadStream reads a transcript and emits pre, post, post_failure, and
// prompt events. Lines can be arbitrarily long. Unknown and malformed lines
// are skipped and counted; an I/O error stops the read and is returned.
func ReadStream(r io.Reader, st Stream, emit Emit) (LineStats, error) {
	return ReadStreamLines(r, st, emit, nil)
}

// ReadStreamLines is ReadStream that also passes every raw line to lines
// (if not nil) before the line's events are emitted, so cost accounting
// reads usage in the same pass (cost.Scanner).
func ReadStreamLines(r io.Reader, st Stream, emit Emit, lines func([]byte)) (LineStats, error) {
	rd := &reader{st: st, emit: emit, pending: make(map[string]pendingUse)}
	br := bufio.NewReaderSize(r, 256<<10)
	for {
		b, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(b)) > 0 {
			rd.stats.Lines++
			if lines != nil {
				lines(b)
			}
			rd.line(b)
		}
		if errors.Is(err, io.EOF) {
			return rd.stats, nil
		}
		if err != nil {
			return rd.stats, err
		}
	}
}

func (rd *reader) line(b []byte) {
	var l line
	if err := json.Unmarshal(b, &l); err != nil {
		rd.stats.Malformed++
		return
	}
	if l.Cwd != "" {
		rd.cwd = l.Cwd
	}
	if l.PermissionMode != "" {
		rd.perm = l.PermissionMode
	}
	switch l.Type {
	case "assistant":
		rd.assistant(&l)
	case "user":
		rd.user(&l)
	default:
		rd.stats.Ignored++
	}
}

// time parses a line timestamp and clamps the stream to non-decreasing
// time: metadata lines carry their own clocks (docs/transcript-format.md).
func (rd *reader) time(ts string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil || t.Before(rd.last) {
		return rd.last
	}
	rd.last = t
	return t
}

func (rd *reader) event(phase event.Phase, ts string) *event.Event {
	return &event.Event{
		Agent:      event.AgentClaudeCode,
		SessionID:  rd.st.SessionID,
		AgentID:    rd.st.AgentID,
		Phase:      phase,
		Cwd:        rd.cwd,
		PermMode:   rd.perm,
		Transcript: rd.st.Path,
		ReceivedAt: rd.time(ts),
	}
}

func (rd *reader) assistant(l *line) {
	blocks := parseBlocks(l)
	used := false
	for _, b := range blocks {
		if b.Type != "tool_use" || b.ID == "" {
			continue
		}
		used = true
		rd.stats.ToolUses++
		ev := rd.event(event.PhasePre, l.Timestamp)
		ev.ToolName = claudecode.ToolName(b.Name)
		ev.ToolUseID = b.ID
		ev.Input = b.Input
		p := pendingUse{tool: ev.ToolName, input: b.Input}
		if ev.ToolName == event.ToolWrite {
			// Replay cannot read the disk as it was: the written content is
			// the file's content after the write.
			var in struct {
				Content *string `json:"content"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.Content != nil {
				p.fileHash = normalize.HashString(*in.Content)
			}
		}
		rd.pending[b.ID] = p
		rd.emit(ev, 0)
	}
	if !used {
		rd.stats.Ignored++
	}
}

func (rd *reader) user(l *line) {
	if l.Message == nil {
		rd.stats.Ignored++
		return
	}
	blocks := parseBlocks(l)
	results := false
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		results = true
		rd.stats.ToolResults++
		p, ok := rd.pending[b.ToolUseID]
		if !ok {
			rd.stats.Unjoined++
			continue
		}
		delete(rd.pending, b.ToolUseID)
		phase := event.PhasePost
		if b.IsError {
			phase = event.PhasePostFailure
		}
		ev := rd.event(phase, l.Timestamp)
		ev.ToolName = p.tool
		ev.ToolUseID = b.ToolUseID
		ev.Input = p.input // a post event carries its input, as PostToolUse does
		ev.IsError = b.IsError
		out, _ := json.Marshal(resultText(b.Content))
		ev.Output = out
		hash := uint64(0)
		if !b.IsError {
			hash = p.fileHash
		}
		rd.emit(ev, hash)
		if isInterrupt(b.Content) {
			rd.prompt(l.Timestamp)
		}
	}
	if results {
		return
	}
	if rd.isHumanPrompt(l, blocks) {
		rd.prompt(l.Timestamp)
		return
	}
	rd.stats.Ignored++
}

func (rd *reader) prompt(ts string) {
	rd.stats.Prompts++
	rd.emit(rd.event(event.PhasePrompt, ts), 0)
}

// parseBlocks returns the message's content blocks; string content becomes
// one text block.
func parseBlocks(l *line) []block {
	if l.Message == nil || len(l.Message.Content) == 0 {
		return nil
	}
	c := l.Message.Content
	if c[0] == '"' {
		var s string
		if json.Unmarshal(c, &s) == nil {
			return []block{{Type: "text", Text: s}}
		}
		return nil
	}
	var bs []block
	_ = json.Unmarshal(c, &bs)
	return bs
}

// notPromptPrefixes mark user text that the client injected rather than a
// person typed.
var notPromptPrefixes = []string{
	"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>",
	"<task-notification>", "<system-reminder>", "<bash-stdout>", "<bash-stderr>",
}

// isHumanPrompt applies the rules in docs/transcript-format.md.
func (rd *reader) isHumanPrompt(l *line, blocks []block) bool {
	if l.IsMeta || l.IsCompactSummary {
		return false
	}
	if l.Origin != nil && l.Origin.Kind != "" && l.Origin.Kind != "human" {
		return false
	}
	text := ""
	hasContent := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			hasContent = true
			if text == "" {
				text = strings.TrimSpace(b.Text)
			}
		case "image":
			hasContent = true
		}
	}
	if !hasContent {
		return false
	}
	for _, p := range notPromptPrefixes {
		if strings.HasPrefix(text, p) {
			return false
		}
	}
	return true
}

// resultText returns the text of a tool_result: a string as is, or the
// text blocks of an array joined, with other blocks as placeholders.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var bs []block
	if json.Unmarshal(raw, &bs) != nil {
		return string(raw)
	}
	parts := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// isInterrupt reports whether a tool result says the user interrupted or
// rejected the call: a person stepped in, which is a turn boundary.
func isInterrupt(raw json.RawMessage) bool {
	t := resultText(raw)
	return strings.HasPrefix(t, "[Request interrupted by user") ||
		strings.HasPrefix(t, "The user doesn't want to proceed with this tool use")
}
