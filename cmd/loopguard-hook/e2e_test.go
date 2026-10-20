package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/daemon"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/scenarios/stuck-npm-test.jsonl")

// scenarioFile is the scripted session the end-to-end test and
// scripts/demo-intervention.ps1 both run: an agent "fixing" a test that
// keeps failing the same way. "<CWD>" stands for the project directory
// (JSON-escaped when substituted).
const scenarioFile = "../../testdata/scenarios/stuck-npm-test.jsonl"

// scenarioSession is the scripted session's id; halt messages name its
// first 8 characters.
const scenarioSession = "e2e0c0de-0000-4000-8000-000000000001"

// step is one line of the scenario file.
type step struct {
	Note string `json:"note"`
	// Enforce is the reply expected in enforce mode: none, nudge, deny, or
	// halt. In observe mode every reply is empty.
	Enforce string          `json:"enforce"`
	Payload json.RawMessage `json:"payload"`
}

// buildScenario writes the scenario. The payloads have the fields and
// shapes of real Claude Code 2.1.x payloads (testdata/hooks/claude-code/).
// Posts of actions that enforce mode denies are left out, as Claude Code
// sends none for a denied tool call.
func buildScenario() []step {
	var steps []step
	n := 0
	promptID := "11111111-0000-4000-8000-000000000001"
	add := func(note, expect string, fields map[string]any) {
		p := map[string]any{
			"session_id":      scenarioSession,
			"transcript_path": "<CWD>/.transcript.jsonl",
			"cwd":             "<CWD>",
			"permission_mode": "auto",
			"effort":          map[string]any{"level": "medium"},
			"prompt_id":       promptID,
		}
		for k, v := range fields {
			p[k] = v
		}
		steps = append(steps, step{Note: note, Enforce: expect, Payload: marshal(p)})
	}
	bash := func(cmd string) map[string]any {
		return map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": cmd, "description": "Run " + cmd}}
	}
	with := func(m map[string]any, kv ...any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	const failure = "Exit code 1\n> shop@1.0.0 test\n> jest\n\nFAIL src/cart.test.ts\n  ● cart › total › adds tax\n\n" +
		"    expect(received).toBe(expected) // Object.is equality\n\n    Expected: 110\n    Received: 100\n\n" +
		"      12 |   const cart = new Cart([item(100)]);\n    > 13 |   expect(cart.total()).toBe(110);\n" +
		"         |                        ^\n\n      at Object.<anonymous> (src/cart.test.ts:13:24)\n\n" +
		"Tests:       1 failed, 7 passed, 8 total"
	edit := func(attempt int) {
		n++
		in := map[string]any{"tool_name": "Edit", "tool_use_id": fmt.Sprintf("toolu_e2e_%03d", n), "tool_input": map[string]any{
			"file_path": "<CWD>/src/cart.ts", "old_string": fmt.Sprintf("return subtotal * %d.0;", attempt),
			"new_string": fmt.Sprintf("return subtotal * %d.0;", attempt+1), "replace_all": false}}
		note := fmt.Sprintf("attempt %d: edit src/cart.ts", attempt)
		add(note+" (pre)", "none", with(in, "hook_event_name", "PreToolUse"))
		add(note, "none", with(in, "hook_event_name", "PostToolUse", "duration_ms", 64, "tool_response", map[string]any{
			"filePath": "<CWD>/src/cart.ts", "oldString": "x", "newString": "y", "originalFile": "z",
			"replaceAll": false, "structuredPatch": []any{}, "userModified": false}))
	}
	npmTest := func(attempt int, pre, post string) {
		n++
		in := with(bash("npm test"), "tool_use_id", fmt.Sprintf("toolu_e2e_%03d", n))
		note := fmt.Sprintf("attempt %d: npm test", attempt)
		add(note+" (pre)", pre, with(in, "hook_event_name", "PreToolUse"))
		if post != "" {
			add(note+" fails the same way", post, with(in, "hook_event_name", "PostToolUseFailure",
				"error", failure, "is_interrupt", false, "duration_ms", 2380))
		}
	}

	add("session starts", "none", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "model": "claude-opus-5-5"})
	add("the human asks for a fix", "none", map[string]any{"hook_event_name": "UserPromptSubmit",
		"prompt": "The cart total test fails. Please fix it."})
	for a := 1; a <= 7; a++ {
		edit(a)
		post := "none"
		if a == 4 {
			post = "nudge" // the 4th failure with the same error
		}
		npmTest(a, "none", post)
	}
	// Three more occurrences after the nudge armed the deny.
	edit(8)
	npmTest(8, "deny", "")
	n++
	cat := with(bash("cat README.md"), "tool_use_id", fmt.Sprintf("toolu_e2e_%03d", n))
	add("cat README.md (pre): not the looping action", "none", with(cat, "hook_event_name", "PreToolUse"))
	add("cat README.md", "none", with(cat, "hook_event_name", "PostToolUse", "duration_ms", 40,
		"tool_response", map[string]any{"stdout": "# shop\n", "stderr": "", "interrupted": false, "isImage": false}))
	edit(9)
	npmTest(9, "deny", "")
	edit(10)
	npmTest(10, "halt", "")
	add("the turn stops", "none", map[string]any{"hook_event_name": "Stop", "stop_hook_active": false,
		"last_assistant_message": "Stopped.", "background_tasks": []any{}, "session_crons": []any{}})

	promptID = "11111111-0000-4000-8000-000000000002"
	add("the human reads the reason and replies", "none", map[string]any{"hook_event_name": "UserPromptSubmit",
		"prompt": "Stop editing cart.ts. Check where the tax rate comes from (config/tax.json)."})
	npmTest(11, "none", "none")
	return steps
}

// marshal encodes v as one line of JSON, keeping "<CWD>" readable (and
// replaceable) rather than escaping it as <CWD>.
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

func encodeScenario(steps []step) []byte {
	var b bytes.Buffer
	for _, s := range steps {
		b.Write(marshal(s))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func TestScenarioFileUpToDate(t *testing.T) {
	want := encodeScenario(buildScenario())
	if *update {
		if err := os.MkdirAll(filepath.Dir(scenarioFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(scenarioFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(scenarioFile)
	if err != nil {
		t.Fatalf("%v (run: go test ./cmd/loopguard-hook -run ScenarioFile -update)", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Errorf("%s is out of date: go test ./cmd/loopguard-hook -run ScenarioFile -update", scenarioFile)
	}
}

func loadScenario(t *testing.T, cwd string) []step {
	t.Helper()
	f, err := os.Open(scenarioFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	esc, _ := json.Marshal(cwd)
	cwdJSON := string(esc[1 : len(esc)-1])
	var steps []step
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var s step
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("scenario line %q: %v", sc.Text(), err)
		}
		s.Payload = json.RawMessage(strings.ReplaceAll(string(s.Payload), "<CWD>", cwdJSON))
		steps = append(steps, s)
	}
	return steps
}

// TestInterventionsThroughTheHookBinary runs the scripted session through
// the real loopguard-hook binary and a real daemon (in a temp home), in
// enforce and in observe mode.
func TestInterventionsThroughTheHookBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds loopguard-hook and runs it once per event")
	}
	hook := filepath.Join(t.TempDir(), "loopguard-hook")
	if runtime.GOOS == "windows" {
		hook += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", hook, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, mode := range []string{config.ModeEnforce, config.ModeObserve} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "lg home")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(dir, config.GlobalFile), []byte("mode: "+mode+"\n"), 0o600)
			project := filepath.Join(t.TempDir(), "shop")
			os.MkdirAll(filepath.Join(project, "src"), 0o700)
			info := startDaemon(t, dir)

			for i, s := range loadScenario(t, project) {
				stdout := runHook(t, hook, dir, s.Payload)
				want := s.Enforce
				if mode == config.ModeObserve {
					want = "none"
				}
				got, detail := classify(t, stdout)
				if got != want {
					t.Fatalf("step %d (%s): %s %s, want %s", i+1, s.Note, got, stdout, want)
				}
				switch got {
				case "nudge":
					if !strings.Contains(detail, "`npm test` has failed 4 times") || !strings.Contains(detail, "FAIL src/cart.test.ts") ||
						!strings.Contains(detail, "these files were edited: src/cart.ts") || len([]rune(detail)) > 1200 {
						t.Errorf("nudge text: %s", detail)
					}
				case "deny":
					if !strings.HasPrefix(detail, "LoopGuard blocked this command") {
						t.Errorf("deny reason: %s", detail)
					}
				case "halt":
					if !strings.HasSuffix(detail, "To let it continue, run: loopguard pause --session "+scenarioSession[:8]) {
						t.Errorf("halt stopReason: %s", detail)
					}
				}
			}

			ivs := interventions(t, dir, info.Port)
			var got []string
			wantMode := store.ModeEnforce
			if mode == config.ModeObserve {
				wantMode = store.ModeObserve
			}
			for _, iv := range ivs {
				if iv.Mode != wantMode {
					t.Errorf("intervention %s recorded as %s, want %s", iv.Action, iv.Mode, wantMode)
				}
				got = append(got, iv.Action)
			}
			if strings.Join(got, " ") != "nudge deny deny halt" {
				t.Errorf("interventions = %v", got)
			}
		})
	}
}

func startDaemon(t *testing.T, dir string) home.DaemonInfo {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan home.DaemonInfo, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- daemon.Run(ctx, daemon.Options{Home: dir, Version: "e2e", Port: -1, Ready: func(i home.DaemonInfo) { ready <- i }})
	}()
	t.Cleanup(func() { cancel(); <-errc })
	select {
	case info := <-ready:
		return info
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not start")
	}
	return home.DaemonInfo{}
}

// runHook runs loopguard-hook once with a payload, as Claude Code does,
// and returns its stdout. It must exit 0 and write nothing to stderr.
func runHook(t *testing.T, hook, dir string, payload []byte) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, hook, "--agent", "claude-code")
	// A generous timeout: CI machines can be slow, and a late reply is
	// dropped (fail open), which would look like a missing decision.
	cmd.Env = append(os.Environ(), home.EnvHome+"="+dir, "LOOPGUARD_NO_AUTOSTART=1", "LOOPGUARD_HOOK_TIMEOUT_MS=5000", "LOOPGUARD_DEBUG=1")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || stderr.Len() > 0 {
		t.Fatalf("loopguard-hook: %v, stderr %q", err, stderr.String())
	}
	return stdout.String()
}

// classify returns the kind of a hook reply and its text (additionalContext,
// deny reason, or stopReason). Output LoopGuard must never produce fails
// the test.
func classify(t *testing.T, stdout string) (kind, text string) {
	t.Helper()
	if stdout == "" {
		return "none", ""
	}
	var o struct {
		Continue           *bool  `json:"continue"`
		StopReason         string `json:"stopReason"`
		HookSpecificOutput *struct {
			HookEventName            string `json:"hookEventName"`
			AdditionalContext        string `json:"additionalContext"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(stdout), &o); err != nil {
		t.Fatalf("hook output is not one JSON object: %q", stdout)
	}
	h := o.HookSpecificOutput
	switch {
	case h != nil && h.PermissionDecision != "" && h.PermissionDecision != "deny":
		t.Fatalf("permissionDecision %q", h.PermissionDecision)
	case o.Continue != nil && !*o.Continue:
		return "halt", o.StopReason
	case h != nil && h.PermissionDecision == "deny":
		return "deny", h.PermissionDecisionReason
	case h != nil && h.AdditionalContext != "":
		if h.HookEventName != "PostToolUseFailure" {
			t.Errorf("nudge on %s, want PostToolUseFailure", h.HookEventName)
		}
		return "nudge", h.AdditionalContext
	}
	t.Fatalf("unrecognized hook output %s", stdout)
	return "", ""
}

func interventions(t *testing.T, dir string, port int) []store.Intervention {
	t.Helper()
	tok, err := home.ReadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/v1/sessions/%s", port, scenarioSession), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var detail struct {
		Interventions []store.Intervention `json:"interventions"`
	}
	if err := json.Unmarshal(b, &detail); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return detail.Interventions
}
