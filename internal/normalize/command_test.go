package normalize

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

func TestFamily(t *testing.T) {
	tests := []struct {
		cmd      string
		families []string
	}{
		{"npm test", []string{"npm test"}},
		{"npm  test", []string{"npm test"}},
		{"npx jest src/a.test.ts", []string{"npx jest"}},
		{"pytest tests/test_calc.py -x", []string{"pytest"}},
		{"pytest -k test_add", []string{"pytest"}},
		{"python -m pytest -q", []string{"pytest"}},
		{"python3 -m pip install -e .", []string{"pip install"}},
		{"uv run pytest", []string{"pytest"}},
		{"go test ./...", []string{"go test"}},
		{"cargo build --release", []string{"cargo build"}},
		{"npm run build", []string{"npm run build"}},
		{"npm run test:unit", []string{"npm run test:unit"}},
		{"cd web && npm test", []string{"npm test"}},
		{"export CI=1; npm test", []string{"npm test"}},
		{"source .venv/bin/activate && pytest", []string{"pytest"}},
		{"time go test ./...", []string{"go test"}},
		{"env CI=1 FOO=bar npm test", []string{"npm test"}},
		{"CI=1 npm test", []string{"npm test"}},
		{"timeout 60 cargo test", []string{"cargo test"}},
		{"npm test 2>&1 | tail -20", []string{"npm test"}},
		{"npm run build && npm test", []string{"npm run build", "npm test"}},
		{"make lint || true", []string{"make lint"}},
		{"bash -c 'cd x && go vet ./...'", []string{"go vet"}},
		{"for i in 1 2 3; do go test ./...; done", []string{"go test"}},
		{"(cd api && go build ./...)", []string{"go build"}},
		{"./node_modules/.bin/jest --ci", []string{"jest"}},
		{`C:\Python312\python.exe -m pytest`, []string{"pytest"}},
		{"sleep 30", []string{"sleep"}},
		{"git -C repo status", []string{"git"}},
		{"Get-ChildItem -Recurse | Select-Object -First 5", []string{"get-childitem"}},
	}
	for _, tt := range tests {
		c := ParseCommand(tt.cmd)
		if !reflect.DeepEqual(c.Families, tt.families) {
			t.Errorf("ParseCommand(%q).Families = %q, want %q", tt.cmd, c.Families, tt.families)
		}
		if want := tt.families[len(tt.families)-1]; c.Primary() != want {
			t.Errorf("ParseCommand(%q).Primary() = %q, want %q", tt.cmd, c.Primary(), want)
		}
	}
}

func TestParseCommandFallback(t *testing.T) {
	// Unbalanced quote: not valid bash, still gets a family.
	c := ParseCommand(`echo "unterminated && npm test`)
	if c.Primary() != "echo" || c.Canonical == "" {
		t.Errorf("fallback = %+v", c)
	}
}

func TestCanonicalCommand(t *testing.T) {
	same := [][2]string{
		{"npm  test", "npm test"},
		{"npm test  2>&1|tail -5", "npm test 2>&1 | tail -5"},
		{"cd a&&make", "cd a && make"},
		{"go test -run 'TestAdd' ./...", "go test -run 'TestAdd' ./..."},
	}
	for _, p := range same {
		if a, b := ParseCommand(p[0]).Canonical, ParseCommand(p[1]).Canonical; a != b {
			t.Errorf("canonical differs: %q → %q vs %q → %q", p[0], a, p[1], b)
		}
	}
	diff := [][2]string{
		{"npm test", "npm run test"},
		{"CI=1 npm test", "npm test"}, // env prefixes are kept
		{"go test ./a", "go test ./b"},
	}
	for _, p := range diff {
		if a, b := ParseCommand(p[0]).Canonical, ParseCommand(p[1]).Canonical; a == b {
			t.Errorf("canonical equal for %q and %q: %q", p[0], p[1], a)
		}
	}
}

func TestPolling(t *testing.T) {
	th := config.Default()
	m := NewCommandMatcher(th.PollingFamilies, th.PollingPatterns)
	polling := []string{
		"sleep 30",
		"gh run watch 123456",
		"gh run watch --exit-status 42",
		"gh pr checks --watch",
		"watch -n 5 kubectl get pods",
		"git status",
		"git status --short",
		"docker ps -a",
		"ls -la",
		"kubectl get pods -w",
		"kubectl get deploy api --watch",
		"curl -s http://localhost:8080/health",
		"curl -fsS localhost:3000/healthz",
		"cd x && sleep 10",
	}
	for _, cmd := range polling {
		if !m.Matches(ParseCommand(cmd)) {
			t.Errorf("polling(%q) = false, want true", cmd)
		}
	}
	notPolling := []string{
		"npm test",
		"gh run view 123",
		"gh pr create",
		"git commit -m wip",
		"kubectl get pods",
		"curl -s http://localhost:8080/api/users",
		"sleep 5 && npm test", // the primary command is what counts
		"git status && go test ./...",
	}
	for _, cmd := range notPolling {
		if m.Matches(ParseCommand(cmd)) {
			t.Errorf("polling(%q) = true, want false", cmd)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	tests := []struct{ p, cwd, want string }{
		{"src/a.ts", "/repo", "/repo/src/a.ts"},
		{"./src/../src/a.ts", "/repo", "/repo/src/a.ts"},
		{"/repo/src/a.ts", "/elsewhere", "/repo/src/a.ts"},
		{`src\a.ts`, `C:\Users\dev\repo`, "c:/Users/dev/repo/src/a.ts"},
		{`C:\Users\dev\repo\src\a.ts`, "", "c:/Users/dev/repo/src/a.ts"},
		{`c:/Users/dev/repo/src/a.ts`, "", "c:/Users/dev/repo/src/a.ts"},
		{"", "/repo", ""},
	}
	for _, tt := range tests {
		if got := NormalizePath(tt.p, tt.cwd); got != tt.want {
			t.Errorf("NormalizePath(%q, %q) = %q, want %q", tt.p, tt.cwd, got, tt.want)
		}
	}
}

func fp(tool, input string) uint64 { return ActionFP(tool, json.RawMessage(input), "/repo") }

func TestActionFP(t *testing.T) {
	same := []struct {
		tool, a, b string
	}{
		{event.ToolBash, `{"command":"npm  test"}`, `{"command":"npm test","description":"run tests","timeout":60000}`},
		{event.ToolEdit, `{"file_path":"/repo/a.ts","old_string":"x","new_string":"y"}`, `{"file_path":"a.ts","old_string":"x","new_string":"y"}`},
		{event.ToolEdit, `{"file_path":"C:\\repo\\a.ts","old_string":"x","new_string":"y"}`, `{"file_path":"c:/repo/a.ts","old_string":"x","new_string":"y"}`},
		{event.ToolWrite, `{"file_path":"/repo/a.ts","content":"abc"}`, `{"content":"abc","file_path":"/repo/a.ts"}`},
		{event.ToolRead, `{"file_path":"/repo/a.ts","offset":10,"limit":50}`, `{"file_path":"/repo/a.ts","limit":50,"offset":10}`},
		{event.ToolGrep, `{"pattern":"TODO","path":"src"}`, `{"pattern":"TODO","path":"/repo/src","output_mode":"content"}`},
		{"mcp:github:create_issue", `{"title":"t","body":"b"}`, `{"body":"b", "title":"t"}`},
		{event.ToolPatch, "{\"input\":\"*** Begin Patch\\n*** Update File: b.go\\n@@\\n-x\\n+y\\n*** Update File: a.go\\n@@\\n-p\\n+q\\n*** End Patch\"}",
			"{\"input\":\"*** Begin Patch\\n*** Update File: a.go\\n@@\\n-p\\n+q\\n*** Update File: b.go\\n@@\\n-x\\n+y\\n*** End Patch\"}"},
	}
	for _, tt := range same {
		if fp(tt.tool, tt.a) != fp(tt.tool, tt.b) {
			t.Errorf("%s: ActionFP(%s) != ActionFP(%s)", tt.tool, tt.a, tt.b)
		}
	}
	diff := []struct {
		tool, a, b string
	}{
		{event.ToolBash, `{"command":"npm test"}`, `{"command":"npm test -- -u"}`},
		{event.ToolEdit, `{"file_path":"a.ts","old_string":"x","new_string":"y"}`, `{"file_path":"a.ts","old_string":"y","new_string":"x"}`},
		{event.ToolEdit, `{"file_path":"a.ts","old_string":"x","new_string":"y"}`, `{"file_path":"b.ts","old_string":"x","new_string":"y"}`},
		{event.ToolWrite, `{"file_path":"a.ts","content":"abc"}`, `{"file_path":"a.ts","content":"abd"}`},
		{event.ToolRead, `{"file_path":"a.ts","offset":10}`, `{"file_path":"a.ts","offset":20}`},
		{"mcp:github:create_issue", `{"title":"t"}`, `{"title":"u"}`},
	}
	for _, tt := range diff {
		if fp(tt.tool, tt.a) == fp(tt.tool, tt.b) {
			t.Errorf("%s: ActionFP(%s) == ActionFP(%s)", tt.tool, tt.a, tt.b)
		}
	}
	if fp(event.ToolBash, `{"command":"ls"}`) == fp("other:Bash2", `{"command":"ls"}`) {
		t.Error("tool name not part of the fingerprint")
	}
}

func TestOutputText(t *testing.T) {
	tests := []struct{ raw, want string }{
		{`"plain"`, "plain"},
		{`{"stdout":"out","stderr":"err","interrupted":false}`, "out\nerr"},
		{`{"stdout":"","stderr":"err"}`, "err"},
		{`{"content":[{"type":"text","text":"x"}]}`, `[{"text":"x","type":"text"}]`},
		{`{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{``, ""},
	}
	for _, tt := range tests {
		if got := OutputText(json.RawMessage(tt.raw)); got != tt.want {
			t.Errorf("OutputText(%s) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestBuild(t *testing.T) {
	th := config.Default()
	b := BuilderFor(th)

	c := b.Build(&event.Event{
		Phase: event.PhasePostFailure, ToolName: event.ToolBash, Cwd: "/repo", IsError: true,
		Input:  json.RawMessage(`{"command":"cd web && npx jest"}`),
		Output: json.RawMessage(`{"stdout":"Tests: 2 failed, 4 passed, 6 total","stderr":""}`),
	})
	if c.Family != "npx jest" || !c.Failed() || c.FailingTests != 2 || c.Polling {
		t.Errorf("bash failure: %+v", c)
	}

	soft := b.Build(&event.Event{
		Phase: event.PhasePost, ToolName: event.ToolBash,
		Input:  json.RawMessage(`{"command":"python app.py"}`),
		Output: json.RawMessage(`{"stdout":"Traceback (most recent call last):\nKeyError: 'X'"}`),
	})
	if !soft.Failed() {
		t.Error("soft failure (exit 0 + Traceback) not detected")
	}

	// Read/search commands quote other text: failure markers in their
	// output are not a failure.
	for _, cmd := range []string{
		`grep -rn "Error:" src`,
		`rg "panic:" --type go`,
		`cat build.log`,
		`tail -50 test-output.txt`,
		`git log --oneline | head`,
		`git show HEAD~1`,
		`Select-String -Path *.log -Pattern "FAILED"`,
	} {
		c := b.Build(&event.Event{
			Phase: event.PhasePost, ToolName: event.ToolBash,
			Input:  json.RawMessage(fmt.Sprintf(`{"command":%q}`, cmd)),
			Output: json.RawMessage(`{"stdout":"src/api.ts:12:    throw new Error: bad input\nsrc/x.go:3: panic: unreachable\n1 FAILED\nTraceback (most recent call last)"}`),
		})
		if c.Failed() {
			t.Errorf("%s with matching output counted as a failure", cmd)
		}
	}
	// ...but a real error still is one.
	grepErr := b.Build(&event.Event{
		Phase: event.PhasePostFailure, ToolName: event.ToolBash, IsError: true,
		Input:  json.RawMessage(`{"command":"grep -rn x missing-dir"}`),
		Output: json.RawMessage(`"grep: missing-dir: No such file or directory"`),
	})
	if !grepErr.Failed() {
		t.Error("a failing grep (non-zero exit) must still be a failure")
	}

	secret := b.Build(&event.Event{
		Phase: event.PhasePostFailure, ToolName: event.ToolBash, IsError: true,
		Input:  json.RawMessage(`{"command":"curl -H 'Authorization: Bearer abc123def456ghi789jkl' https://api.x"}`),
		Output: json.RawMessage(`"Error: invalid password=hunter2!x for user"`),
	})
	if strings.Contains(secret.Command, "abc123def456ghi789jkl") || strings.Contains(strings.Join(secret.ErrorSig.Lines, "\n"), "hunter2") {
		t.Errorf("secret not redacted: command %q, lines %q", secret.Command, secret.ErrorSig.Lines)
	}

	readErr := b.Build(&event.Event{
		Phase: event.PhasePost, ToolName: event.ToolRead,
		Input:  json.RawMessage(`{"file_path":"/repo/notes.md"}`),
		Output: json.RawMessage(`"Error: this file talks about errors"`),
	})
	if readErr.Failed() {
		t.Error("soft-failure markers must only apply to bash output")
	}

	edit := b.Build(&event.Event{
		Phase: event.PhasePost, ToolName: event.ToolEdit, Cwd: `C:\repo`,
		Input: json.RawMessage(`{"file_path":"src\\a.ts","edits":[{"old_string":"a","new_string":"b"},{"old_string":"c","new_string":"d"}]}`),
	})
	if edit.FilePath != "c:/repo/src/a.ts" || len(edit.Edits) != 2 || edit.Family != event.ToolEdit {
		t.Errorf("multi-edit: %+v", edit)
	}

	poll := b.Build(&event.Event{Phase: event.PhasePost, ToolName: event.ToolBash, Input: json.RawMessage(`{"command":"gh run watch 1"}`)})
	if !poll.Polling {
		t.Error("gh run watch not flagged as polling")
	}

	todo := b.Build(&event.Event{
		Phase: event.PhasePost, ToolName: "other:TodoWrite",
		Input: json.RawMessage(`{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"}]}`),
	})
	if todo.TodosDone != 1 {
		t.Errorf("TodosDone = %d, want 1", todo.TodosDone)
	}

	pre := b.Build(&event.Event{Phase: event.PhasePre, ToolName: event.ToolBash, Input: json.RawMessage(`{"command":"npm test"}`)})
	post := b.Build(&event.Event{Phase: event.PhasePost, ToolName: event.ToolBash, Input: json.RawMessage(`{"command":"npm test"}`)})
	if pre.ActionFP != post.ActionFP || pre.Family != post.Family {
		t.Error("pre and post of the same action fingerprint differently")
	}
}
