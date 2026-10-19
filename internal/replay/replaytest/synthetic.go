// Package replaytest writes synthetic Claude Code transcripts for tests.
// They follow docs/transcript-format.md, with made-up content.
package replaytest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionID is the id of the synthetic main session.
const SessionID = "5e55a0c1-0000-4000-8000-000000000001"

// AgentID is the id of the synthetic subagent.
const AgentID = "a0000000000000001"

// Project is the encoded project directory of the synthetic session.
const Project = "C--Users-dev-work-shop"

// Cwd is the synthetic session's working directory.
const Cwd = `C:\Users\dev\work\shop`

// Secret is planted in a failing output; it must never come out of replay.
const Secret = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"

// PromptText is the first prompt; exports must drop it.
const PromptText = "please make the cart total test pass"

// T0 is when the synthetic session starts.
var T0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// JestFail is a failing jest output (same error every run, varying time).
func JestFail(run int) string {
	return fmt.Sprintf(`FAIL ./cart.test.js
  ● cart › total sums price * qty

    expect(received).toBe(expected) // Object.is equality

    Expected: 7
    Received: 8

      at Object.toBe (cart.test.js:5:65)

Tests:       1 failed, 3 passed, 4 total
Time:        %d.%03d s
token=%s`, 1+run%3, 100+run*37, Secret)
}

// GoFail is a failing go test output.
const GoFail = "--- FAIL: TestParse (0.00s)\n    parse_test.go:9: got 4, want 3\nFAIL\nFAIL\texample.com/shop/parse\t0.412s\nFAIL"

// Builder accumulates transcript lines.
type Builder struct {
	lines   []string
	t       time.Time
	n       int
	session string
	agent   string
}

// NewBuilder starts a transcript at T0.
func NewBuilder(session, agent string) *Builder {
	return &Builder{t: T0, session: session, agent: agent}
}

func (b *Builder) base(typ string) map[string]any {
	m := map[string]any{
		"type": typ, "uuid": fmt.Sprintf("u-%d", len(b.lines)), "sessionId": b.session,
		"timestamp": b.t.Format("2006-01-02T15:04:05.000Z"), "cwd": Cwd, "gitBranch": "main",
		"version": "2.0.0", "userType": "external", "isSidechain": b.agent != "",
	}
	if b.agent != "" {
		m["agentId"] = b.agent
	}
	return m
}

func (b *Builder) add(m map[string]any) {
	out, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	b.lines = append(b.lines, string(out))
}

// Advance moves the clock.
func (b *Builder) Advance(d time.Duration) *Builder { b.t = b.t.Add(d); return b }

// Raw adds a line as is.
func (b *Builder) Raw(line string) *Builder { b.lines = append(b.lines, line); return b }

// Prompt adds a human prompt (newer client: origin.kind = human).
func (b *Builder) Prompt(text string) *Builder {
	m := b.base("user")
	m["origin"] = map[string]string{"kind": "human"}
	m["promptSource"] = "typed"
	m["permissionMode"] = "auto"
	m["message"] = map[string]any{"role": "user", "content": text}
	b.add(m)
	return b.Advance(5 * time.Second)
}

// User adds a user line with string content and extra fields (for the
// lines that are not prompts).
func (b *Builder) User(content string, extra map[string]any) *Builder {
	m := b.base("user")
	for k, v := range extra {
		m[k] = v
	}
	m["message"] = map[string]any{"role": "user", "content": content}
	b.add(m)
	return b
}

// Tool adds a tool_use and its tool_result. It returns the tool use id.
func (b *Builder) Tool(name string, input map[string]any, result string, isError bool) string {
	b.n++
	id := fmt.Sprintf("toolu_%04d", b.n)
	a := b.base("assistant")
	a["message"] = map[string]any{
		"id": fmt.Sprintf("msg_%04d", b.n), "role": "assistant", "model": "claude-test", "stop_reason": "tool_use",
		"content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input, "caller": map[string]string{"type": "direct"}}},
		"usage":   map[string]int{"input_tokens": 3, "output_tokens": 50},
	}
	b.add(a)
	b.Advance(2 * time.Second)
	u := b.base("user")
	block := map[string]any{"type": "tool_result", "tool_use_id": id, "content": result}
	if isError {
		block["is_error"] = true
	}
	u["message"] = map[string]any{"role": "user", "content": []any{block}}
	u["toolUseResult"] = map[string]any{"stdout": result, "stderr": "", "interrupted": false}
	b.add(u)
	b.Advance(20 * time.Second)
	return id
}

// Bash runs a shell command.
func (b *Builder) Bash(cmd, out string, failed bool) *Builder {
	res := out
	if failed {
		res = "Exit code 1\n" + out
	}
	b.Tool("Bash", map[string]any{"command": cmd, "description": "run"}, res, failed)
	return b
}

// Edit edits a file.
func (b *Builder) Edit(path, old, new string) *Builder {
	b.Tool("Edit", map[string]any{"file_path": path, "old_string": old, "new_string": new, "replace_all": false},
		"The file "+path+" has been updated.", false)
	return b
}

// Write writes a file.
func (b *Builder) Write(path, content string) *Builder {
	b.Tool("Write", map[string]any{"file_path": path, "content": content}, "File created successfully at: "+path, false)
	return b
}

// String returns the transcript.
func (b *Builder) String() string { return strings.Join(b.lines, "\n") + "\n" }

// MainSession returns the synthetic main session transcript:
//
//   - metadata lines (no timestamp, or a clock that goes backwards),
//     a task notification, an isMeta line, a compaction, a malformed line,
//     a 100 KB tool result, and a result whose tool_use is missing;
//   - prompt 1: `npx jest` fails 5 times with the same error, an edit to
//     src/cart.js between attempts (D2 fires at the 4th failure, call 7);
//   - prompt 2 (a slash command): src/flags.ts written with content A, B,
//     A, B (D3 state revisit at the 4th write).
func MainSession() string {
	b := NewBuilder(SessionID, "")
	b.Raw(`{"type":"file-history-snapshot","messageId":"m-0","snapshot":{},"isSnapshotUpdate":false}`)
	b.Raw(`{"type":"permission-mode","permissionMode":"auto","sessionId":"` + SessionID + `"}`)
	b.Prompt(PromptText)
	for i := 0; i < 5; i++ {
		b.Bash("npx jest", JestFail(i), true)
		if i < 4 {
			b.Edit(Cwd+`\src\cart.js`, fmt.Sprintf("v%d", i), fmt.Sprintf("v%d", i+1))
		}
		if i == 1 {
			// Metadata written with its own clock, an hour behind.
			late := b.base("attachment")
			late["timestamp"] = b.t.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
			late["attachment"] = map[string]any{"type": "total_tokens_reminder"}
			b.add(late)
			b.User("<task-notification>build finished</task-notification>", map[string]any{"origin": map[string]string{"kind": "task-notification"}})
			b.User("<local-command-caveat>caveat</local-command-caveat>", map[string]any{"isMeta": true})
			b.Raw(`{"type":"user","message":` /* truncated: malformed */)
		}
	}
	b.Tool("Read", map[string]any{"file_path": Cwd + `\big.log`}, strings.Repeat("log line without errors\n", 4500), false)
	b.Raw(`{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto"},"timestamp":"` + b.t.Format("2006-01-02T15:04:05.000Z") + `"}`)
	b.User("This session is being continued from a previous conversation.", map[string]any{"isCompactSummary": true, "isVisibleInTranscriptOnly": true})
	orphan := b.base("user")
	orphan["message"] = map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_missing", "content": "x"}}}
	b.add(orphan)
	b.User("<command-name>/review</command-name>", nil)
	for _, c := range []string{"export const cache = true;\n", "export const cache = false;\n", "export const cache = true;\n", "export const cache = false;\n"} {
		b.Write(Cwd+`\src\flags.ts`, c)
	}
	return b.String()
}

// Subagent returns the synthetic subagent transcript: `go test ./...`
// fails 4 times with the same error (D2 in the subagent's own stream).
func Subagent() string {
	b := NewBuilder(SessionID, AgentID)
	b.User("Investigate the parse failure", nil)
	for i := 0; i < 4; i++ {
		b.Bash("go test ./...", GoFail, true)
	}
	return b.String()
}

// WriteTree writes the synthetic session under root/<Project>/ and returns
// the main transcript's path.
func WriteTree(root string) (string, error) {
	dir := filepath.Join(root, Project)
	sub := filepath.Join(dir, SessionID, "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return "", err
	}
	main := filepath.Join(dir, SessionID+".jsonl")
	if err := os.WriteFile(main, []byte(MainSession()), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(sub, "agent-"+AgentID+".jsonl"), []byte(Subagent()), 0o644); err != nil {
		return "", err
	}
	meta := `{"agentType":"Explore","description":"x","spawnDepth":1,"toolUseId":"toolu_0001"}`
	return main, os.WriteFile(filepath.Join(sub, "agent-"+AgentID+".meta.json"), []byte(meta), 0o644)
}
