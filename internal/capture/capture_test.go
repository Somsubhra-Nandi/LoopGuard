package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/anon"
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

// TestExportAnonymizes runs the same capture-and-export on Windows-, macOS-,
// and Linux-style payloads, on every OS: the paths come from the data, so
// where the test runs must not matter.
func TestExportAnonymizes(t *testing.T) {
	styles := []struct {
		name, home, cwd string
		otherSpellings  []string // the home and cwd as they also appear in payloads
		otherUserPath   string   // a path below another user's home
		leaks           []string // must not appear in the export
		want            []string // must appear
	}{
		{
			name: "windows", home: `C:\Users\Jane Q-Doe`, cwd: `C:\Users\Jane Q-Doe\desktop\secret-proj`,
			otherSpellings: []string{"/c/Users/Jane Q-Doe/desktop/secret-proj", "C:/Users/Jane Q-Doe/.npmrc",
				`C:\Users\JANEQ-~1\AppData\Local\Temp\out.log`, `C:\Users\Jane Q-Doe\.claude\projects\C--Users-Jane-Q-Doe-desktop-secret-proj\s.jsonl`},
			otherUserPath: `D:\Users\bobby\notes.txt`,
			leaks:         []string{"Jane", "Doe", "JANEQ", "bobby", "example.com"},
			want:          []string{`<HOME>\desktop\project`, "<HOME>/desktop/project", `<HOME>\AppData`, "cd project &&", "<email>", "secret-project-x"},
		},
		{
			name: "macos", home: "/Users/janeqdoe", cwd: "/Users/janeqdoe/code/secret-proj",
			otherSpellings: []string{"/Users/janeqdoe/.npmrc", "/Users/janeqdoe/.claude/projects/-Users-janeqdoe-code-secret-proj/s.jsonl"},
			otherUserPath:  "/Users/bobby/notes.txt",
			leaks:          []string{"janeqdoe", "bobby", "example.com"},
			want:           []string{"<HOME>/code/project", "<HOME>/.npmrc", "cd project &&", "<email>", "secret-project-x"},
		},
		{
			name: "linux", home: "/home/janeqdoe", cwd: "/home/janeqdoe/src/secret-proj",
			otherSpellings: []string{`\home\janeqdoe\src\secret-proj`, "/home/janeqdoe/.claude/projects/-home-janeqdoe-src-secret-proj/s.jsonl"},
			otherUserPath:  "/home/bobby/notes.txt",
			leaks:          []string{"janeqdoe", "bobby", "example.com"},
			want:           []string{"<HOME>/src/project", `<HOME>\src\project`, "cd project &&", "<email>", "secret-project-x"},
		},
	}
	for _, st := range styles {
		t.Run(st.name, func(t *testing.T) {
			c := New(t.TempDir(), st.home)
			user := anonBase(st.home)
			payloads := []map[string]any{
				{"hook_event_name": "PreToolUse", "session_id": "s", "cwd": st.cwd, "tool_name": "Bash",
					"tool_input": map[string]any{"command": "cd secret-proj && git log --author jane.doe@example.com",
						"description": "ask " + user + " about secret-proj; keep secret-project-x"}},
				{"hook_event_name": "Stop", "session_id": "s", "cwd": st.cwd, "paths": st.otherSpellings,
					"note": st.otherUserPath + " belongs to bobby", "key": "sk-ant-api03-" + strings.Repeat("B", 40)},
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
			res, err := Export(filepath.Join(c.dir, "claude-code"), out, ExportOptions{UserHome: st.home,
				Golden: func(b []byte) ([]byte, error) { return []byte("golden\n"), nil }})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != 2 || res.Goldens != 2 {
				t.Fatalf("stats: %+v", res)
			}
			var all strings.Builder
			for _, f := range res.Files {
				b, err := os.ReadFile(filepath.Join(out, f))
				if err != nil {
					t.Fatal(err)
				}
				var v map[string]any
				if err := json.Unmarshal(b, &v); err != nil {
					t.Fatalf("%s is not JSON: %v", f, err)
				}
				// Check the decoded strings, keys included, not the JSON text.
				walkStrings(v, func(s string) string { all.WriteString(s + "\n"); return s })
				if _, err := os.Stat(filepath.Join(out, strings.TrimSuffix(f, ".json")+".golden")); err != nil {
					t.Errorf("no golden for %s", f)
				}
			}
			s := all.String()
			for _, leak := range append(st.leaks, "BBBBBBBBBB") {
				if strings.Contains(s, leak) {
					t.Errorf("export contains %q:\n%s", leak, s)
				}
			}
			if anon.ReplaceWord(s, "secret-proj", "") != s {
				t.Errorf("export contains the project name as a word:\n%s", s)
			}
			for _, want := range append(st.want, "<REDACTED:", "<HOME>", "belongs to user") {
				if !strings.Contains(s, want) {
					t.Errorf("export lacks %q:\n%s", want, s)
				}
			}
			if res.Replaced["secret-proj"] != "project" || res.Replaced["bobby"] != "user" {
				t.Errorf("replaced: %v", res.Replaced)
			}
		})
	}

	if _, err := Export(t.TempDir(), t.TempDir(), ExportOptions{}); err == nil {
		t.Error("exporting an empty capture directory succeeded")
	}
}

// anonBase is the username of a home directory, either style.
func anonBase(home string) string {
	if i := strings.LastIndexAny(home, `/\`); i >= 0 {
		return home[i+1:]
	}
	return home
}
