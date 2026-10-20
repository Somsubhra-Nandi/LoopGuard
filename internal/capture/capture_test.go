package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSwitchCachesTheFlag(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000, 0)
	s := NewSwitch(dir, false)
	s.now = func() time.Time { return now }
	if s.Enabled() {
		t.Fatal("on without a flag file")
	}
	if err := SetFlag(dir, true); err != nil {
		t.Fatal(err)
	}
	if s.Enabled() {
		t.Error("looked at the flag file again within the TTL")
	}
	now = now.Add(SwitchTTL)
	if !s.Enabled() {
		t.Error("flag file not seen after the TTL")
	}
	if err := SetFlag(dir, false); err != nil || SetFlag(dir, false) != nil {
		t.Fatal("turning off (twice) failed")
	}
	now = now.Add(SwitchTTL)
	if s.Enabled() || FlagOn(dir) {
		t.Error("still on after the flag file was removed")
	}
	if !NewSwitch(dir, true).Enabled() {
		t.Error("forced switch is off")
	}
	var nilSwitch *Switch
	if nilSwitch.Enabled() {
		t.Error("nil switch is on")
	}
}

func TestExportAnonymizes(t *testing.T) {
	userHome := `C:\Users\Jane Q-Doe` // the same string on every OS
	c := New(t.TempDir(), userHome)
	cwd := userHome + `\desktop\secret-proj`
	payloads := []map[string]any{
		{"hook_event_name": "PreToolUse", "session_id": "s", "cwd": cwd, "tool_name": "Bash",
			"tool_input": map[string]any{"command": "cd secret-proj && git log --author jane.doe@example.com",
				"description": "ask Jane about secret-proj; keep secret-project-x"}},
		// A path below another user's home: that username is learned too.
		{"hook_event_name": "Stop", "session_id": "s", "cwd": cwd, "transcript_path": `/home/bobby/.claude/projects/x.jsonl`,
			"note": "bobby and Jane-Doe and JANE", "key": "sk-ant-api03-" + strings.Repeat("B", 40)},
	}
	for _, p := range payloads {
		b, _ := json.Marshal(p)
		if err := c.Save("claude-code", b); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Save("claude-code", []byte("not json")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "testdata", "2.1.0")
	st, err := Export(filepath.Join(c.dir, "claude-code"), out, ExportOptions{UserHome: userHome,
		Golden: func(b []byte) ([]byte, error) { return []byte("golden for " + string(b[:1]) + "\n"), nil }})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Files) != 2 || st.Goldens != 2 {
		t.Fatalf("stats: %+v", st)
	}
	var all strings.Builder
	for _, f := range st.Files {
		b, err := os.ReadFile(filepath.Join(out, f))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("%s is not JSON: %v", f, err)
		}
		all.Write(b)
		if _, err := os.Stat(filepath.Join(out, strings.TrimSuffix(f, ".json")+".golden")); err != nil {
			t.Errorf("no golden for %s", f)
		}
	}
	s := all.String()
	for _, leak := range []string{"Jane", "Doe", "jane", "bobby", "secret-proj\"", "secret-proj ", "example.com", "BBBBBBBBBB"} {
		if strings.Contains(s, leak) {
			t.Errorf("export contains %q:\n%s", leak, s)
		}
	}
	for _, want := range []string{`<HOME>\\desktop\\project`, "cd project &&", "<email>", "secret-project-x", "<REDACTED:", "user and user"} {
		if !strings.Contains(s, want) {
			t.Errorf("export lacks %q:\n%s", want, s)
		}
	}
	if st.Replaced["secret-proj"] != "project" || st.Replaced["bobby"] != "user" {
		t.Errorf("replaced: %v", st.Replaced)
	}

	if _, err := Export(t.TempDir(), out, ExportOptions{}); err == nil {
		t.Error("exporting an empty capture directory succeeded")
	}
}

func TestReplaceWord(t *testing.T) {
	for _, tt := range [][3]string{
		{"loopguard and loopguard-hook", "loopguard", "project and project-hook"},
		{"LoopGuardX loopguard2 xloopguard", "loopguard", "LoopGuardX loopguard2 xloopguard"},
		{"/a/LOOPGUARD/b", "loopguard", "/a/project/b"},
	} {
		if got := replaceWord(tt[0], tt[1], "project"); got != tt[2] {
			t.Errorf("replaceWord(%q, %q) = %q, want %q", tt[0], tt[1], got, tt[2])
		}
	}
}
