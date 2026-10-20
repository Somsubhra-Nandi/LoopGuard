package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

var update = flag.Bool("update", false, "rewrite the expected *.golden files")

// TestGoldenPayloads parses every payload under testdata/hooks/claude-code
// (committed: synthetic/ and one structure-only directory per captured
// Claude Code version) and, when it exists, testdata/private/hooks/claude-code
// (local only: real payloads), at any depth, and compares the mapped Event
// with the .golden file next to it. Run with -update to write the .golden
// files, then review the diff.
func TestGoldenPayloads(t *testing.T) {
	testdata := filepath.Join("..", "..", "..", "testdata")
	roots := []string{filepath.Join(testdata, "hooks", "claude-code")}
	if private := filepath.Join(testdata, "private", "hooks", "claude-code"); dirExists(private) {
		roots = append(roots, private)
	}
	var files []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".json") {
				files = append(files, p)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(files) == 0 {
		t.Fatal("no golden payloads found")
	}
	installed := map[string]bool{}
	for _, h := range Hooks {
		installed[h.Event] = true
	}
	for _, f := range files {
		rel, _ := filepath.Rel(testdata, f)
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
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
			// A real payload for an event LoopGuard hooks must map to a phase:
			// an empty one means Claude Code renamed or reshaped it.
			var name struct {
				Event string `json:"hook_event_name"`
			}
			_ = json.Unmarshal(raw, &name)
			if installed[name.Event] && ev.Phase == "" {
				t.Errorf("%s parsed to no phase", name.Event)
			}
			got, err := Golden(raw)
			if err != nil {
				t.Fatal(err)
			}
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

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func TestPromptTurnBoundaries(t *testing.T) {
	for prompt, want := range map[string]event.Phase{
		"fix the failing test":                   event.PhasePrompt,
		"/review":                                event.PhasePrompt,
		"<str:230>":                              event.PhasePrompt, // structure-only golden payloads
		"":                                       event.PhaseAutoPrompt,
		"   ":                                    event.PhaseAutoPrompt,
		"<task-notification>agent done":          event.PhaseAutoPrompt,
		"  <system-reminder>x</system-reminder>": event.PhaseAutoPrompt,
		"<local-command-stdout>ok</local-command-stdout>": event.PhaseAutoPrompt,
	} {
		b, _ := json.Marshal(map[string]string{"session_id": "s", "hook_event_name": "UserPromptSubmit", "prompt": prompt})
		ev, err := Parse(b)
		if err != nil || ev.Phase != want {
			t.Errorf("prompt %q: phase %q, %v; want %q", prompt, ev.Phase, err, want)
		}
	}
}
