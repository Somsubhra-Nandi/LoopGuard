package capture

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStructureOnly(t *testing.T) {
	in := `{
  "session_id": "s-1", "cwd": "/home/user/project", "hook_event_name": "PostToolBatch",
  "permission_mode": "default", "model": "claude-opus", "effort": {"level": "high"},
  "prompt": "my secret plan", "duration_ms": 12, "stop_hook_active": false, "new_field": "héllo",
  "error": "boom", "last_assistant_message": "done",
  "tool_calls": [{"tool_name": "Bash", "tool_use_id": "t1",
    "tool_input": {"command": "cat .env", "tool_name": "nested", "lines": [1, "two"]},
    "tool_response": {"stdout": "", "interrupted": true, "session_id": "x"}}]
}`
	want := `{
  "session_id": "s-1", "cwd": "/home/user/project", "hook_event_name": "PostToolBatch",
  "permission_mode": "default", "model": "claude-opus", "effort": {"level": "high"},
  "prompt": "<str:14>", "duration_ms": 12, "stop_hook_active": false, "new_field": "<str:5>",
  "error": "<str:4>", "last_assistant_message": "<str:4>",
  "tool_calls": [{"tool_name": "Bash", "tool_use_id": "t1",
    "tool_input": {"command": "<str:8>", "tool_name": "<str:6>", "lines": [1, "<str:3>"]},
    "tool_response": {"stdout": "<str:0>", "interrupted": true, "session_id": "<str:1>"}}]
}`
	var v, w any
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if got := StructureOnly(v); !reflect.DeepEqual(got, w) {
		b, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("got:\n%s", b)
	}
}

func TestCheckNoContent(t *testing.T) {
	long := strings.Repeat("x", MaxContentString+1)
	bad, err := CheckNoContent([]byte(`{"cwd":"` + long + `","prompt":"short","tool_input":{"a":["ok","` + long + `"]},"output":{"stdout":"` + long + `"}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".output.stdout: 41-character string", ".tool_input.a[1]: 41-character string"}
	if !reflect.DeepEqual(bad, want) {
		t.Errorf("got %q, want %q", bad, want)
	}
	if _, err := CheckNoContent([]byte("not json")); err == nil {
		t.Error("no error for invalid JSON")
	}
}

// TestCommittedHookPayloadsHaveNoContent is the safety net against
// committing real payloads: no file under testdata/hooks may hold a long
// string in a content field. Real payloads belong in testdata/private;
// committed ones come from `loopguard capture export --structure-only`.
func TestCommittedHookPayloadsHaveNoContent(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "hooks")
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".json") || strings.HasSuffix(p, ".golden")) {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n++
		bad, err := CheckNoContent(b)
		if err != nil {
			t.Errorf("%s: %v", p, err)
		}
		for _, s := range bad {
			t.Errorf("%s: %s in a content field (over %d): real content? Re-export with `loopguard capture export --structure-only`", filepath.ToSlash(p), s, MaxContentString)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no payloads under " + root)
	}
}
