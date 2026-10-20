package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

var update = flag.Bool("update", false, "rewrite the expected *.golden files")

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

// TestGoldenPayloads parses every payload under testdata/hooks/claude-code
// and compares the mapped Event with the .golden file next to it.
func TestGoldenPayloads(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "hooks", "claude-code", "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no golden payloads found")
	}
	for _, f := range files {
		name := filepath.Base(filepath.Dir(f)) + "/" + filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			ev, err := Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ev.ID == "" || ev.ReceivedAt.IsZero() || !bytes.Equal(ev.Raw, raw) {
				t.Errorf("ID %q, ReceivedAt %v, Raw kept %v", ev.ID, ev.ReceivedAt, bytes.Equal(ev.Raw, raw))
			}
			g := goldenEvent{
				Agent: ev.Agent, SessionID: ev.SessionID, AgentID: ev.AgentID, Phase: ev.Phase,
				ToolName: ev.ToolName, ToolUseID: ev.ToolUseID, Input: compact(t, ev.Input), Output: compact(t, ev.Output),
				IsError: ev.IsError, Cwd: ev.Cwd, Transcript: ev.Transcript, PermMode: ev.PermMode,
				Prompt: ev.Prompt, Model: ev.Model, Source: ev.Source,
			}
			got, err := json.MarshalIndent(g, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := strings.TrimSuffix(f, ".json") + ".golden"
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
				t.Errorf("mapped event differs from %s:\n got %s\nwant %s", filepath.Base(golden), got, want)
			}
		})
	}
}

func compact(t *testing.T, v json.RawMessage) json.RawMessage {
	if v == nil {
		return nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, v); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want error
	}{
		{"", ErrNotJSON},
		{"garbage", ErrNotJSON},
		{"[1,2]", ErrNotJSON},
		{"null", ErrNotJSON},
		{`"PreToolUse"`, ErrNotJSON},
		{`{"session_id":"s"}`, ErrNoEventName},
		{`{"session_id":"s","hook_event_name":7}`, ErrNoEventName},
		{`{"hook_event_name":"PreToolUse"}`, ErrNoSessionID},
	} {
		if _, err := Parse([]byte(tc.raw)); !errors.Is(err, tc.want) {
			t.Errorf("Parse(%q) err = %v, want %v", tc.raw, err, tc.want)
		}
	}
}

func TestParseLenient(t *testing.T) {
	ev, err := parseAt([]byte(`{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"Bash",
		"tool_input":"not an object","tool_response":null,"tool_use_id":5}`), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Phase != event.PhasePost || ev.ToolName != event.ToolBash || ev.Input != nil || ev.Output != nil || ev.ToolUseID != "" {
		t.Errorf("lenient parse = %+v", ev)
	}

	// A failure without "error" falls back to tool_response.
	ev, _ = Parse([]byte(`{"session_id":"s","hook_event_name":"PostToolUseFailure","tool_name":"Read","tool_response":"boom"}`))
	if !ev.IsError || string(ev.Output) != `"boom"` {
		t.Errorf("failure fallback = %+v", ev)
	}

	// Prompts are clipped at a rune boundary.
	long := strings.Repeat("é", maxPrompt)
	ev, _ = Parse([]byte(`{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"` + long + `"}`))
	if len(ev.Prompt) > maxPrompt || !strings.HasPrefix(long, ev.Prompt) {
		t.Errorf("prompt clipped to %d bytes, not a prefix of the input", len(ev.Prompt))
	}
}

func TestParseStripsBOM(t *testing.T) {
	ev, err := Parse([]byte("\xef\xbb\xbf" + `{"session_id":"s","hook_event_name":"Stop"}`))
	if err != nil || ev.Phase != event.PhaseStop || bytes.HasPrefix(ev.Raw, utf8BOM) {
		t.Errorf("payload with a UTF-8 BOM: %+v, %v", ev, err)
	}
}
