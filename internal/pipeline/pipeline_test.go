package pipeline

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// TestPollingNeverFires is a property test (LLD §16): random sequences of
// polling commands, failing or not, with repeated or varying output, never
// produce a signal.
func TestPollingNeverFires(t *testing.T) {
	cmds := []string{
		"sleep 30", "sleep 5", "gh run watch 123", "gh run watch --exit-status 9", "gh pr checks --watch",
		"watch -n 2 kubectl get pods", "git status", "git status --short", "docker ps", "ls -la", "ls",
		"kubectl get pods -w", "curl -s localhost:8080/health", "cd api && sleep 10",
	}
	outputs := []string{
		"", "in progress", "Error: run failed", "FAILED: 1 check", "connection refused",
		"panic: runtime error\ngoroutine 1 [running]", "Traceback (most recent call last):\nValueError: x",
	}
	rng := rand.New(rand.NewSource(7))
	for run := 0; run < 100; run++ {
		var steps []step
		cmd := cmds[rng.Intn(len(cmds))]
		for i := 0; i < 30; i++ {
			if rng.Intn(4) == 0 { // usually the same command, sometimes another poller
				cmd = cmds[rng.Intn(len(cmds))]
			}
			res := "ok"
			if rng.Intn(2) == 0 {
				res = "fail"
			}
			steps = append(steps, step{Bash: cmd, Result: res, Text: outputs[rng.Intn(len(outputs))]})
		}
		r := runScenario(t, scenario{Name: fmt.Sprintf("poll-%d", run), Steps: steps}, config.Default())
		if len(r.signals) > 0 {
			t.Fatalf("run %d: polling fired %v (first command %q)", run, r.signals, steps[0].Bash)
		}
	}
}

func TestPipelinePhases(t *testing.T) {
	p := New(config.Default())
	s := p.NewSession("cc:x", event.AgentClaudeCode)
	at := t0
	ev := func(phase event.Phase, id string) *event.Event {
		at = at.Add(2 * time.Second)
		return &event.Event{Phase: phase, ToolName: event.ToolBash, ToolUseID: id, Input: json.RawMessage(`{"command":"npm test"}`), ReceivedAt: at}
	}
	p.Handle(s, &event.Event{Phase: event.PhaseSessionStart, ReceivedAt: at}, 0)
	p.Handle(s, ev(event.PhasePre, "a"), 0)
	r := p.Handle(s, ev(event.PhasePost, "a"), 0)
	if r.Call == nil || r.Call.Duration != 2*time.Second || r.Call.Family != "npm test" {
		t.Errorf("post result: %+v", r.Call)
	}
	if len(s.Pending) != 0 {
		t.Error("pending pre event not consumed")
	}
	if s.Seq() != 3 || s.CallCount != 1 {
		t.Errorf("seq=%d calls=%d", s.Seq(), s.CallCount)
	}
}

// benchCalls builds n calls (pre + post events) with a realistic mix:
// failing test runs with real outputs, edits, reads, searches, writes, MCP
// calls, polling, and a human prompt every 250 calls.
func benchCalls(b *testing.B, n int) []*event.Event {
	b.Helper()
	read := func(f string) string {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "outputs", filepath.FromSlash(f)))
		if err != nil {
			b.Fatal(err)
		}
		return string(data)
	}
	jest1, jest2 := read("jest/cart_total_fail_1.txt"), read("jest/cart_total_fail_shifted.txt")
	gofail, pyfail, cargofail := read("gotest/add_fail_1.txt"), read("pytest/add_fail_1.txt"), read("cargo/add_fail_1.txt")
	js := func(v any) json.RawMessage { out, _ := json.Marshal(v); return out }
	type tmpl struct {
		tool       string
		input, out json.RawMessage
		failed     bool
	}
	mix := []tmpl{
		{tool: event.ToolBash, input: js(map[string]string{"command": "npx jest"}), out: js(map[string]string{"stdout": jest1}), failed: true},
		{tool: event.ToolEdit, input: js(map[string]string{"file_path": "src/cart.js", "old_string": "a", "new_string": "b"}), out: js("ok")},
		{tool: event.ToolBash, input: js(map[string]string{"command": "npx jest"}), out: js(map[string]string{"stdout": jest2}), failed: true},
		{tool: event.ToolRead, input: js(map[string]any{"file_path": "src/cart.js", "offset": 1, "limit": 200}), out: js(jest1[:800])},
		{tool: event.ToolBash, input: js(map[string]string{"command": "go test ./..."}), out: js(map[string]string{"stdout": gofail}), failed: true},
		{tool: event.ToolGrep, input: js(map[string]string{"pattern": "total", "path": "src"}), out: js("src/cart.js:2: return total")},
		{tool: event.ToolBash, input: js(map[string]string{"command": "cd py && python -m pytest -x"}), out: js(map[string]string{"stdout": pyfail}), failed: true},
		{tool: event.ToolWrite, input: js(map[string]string{"file_path": "src/new.ts", "content": "export const x = 1;\n"}), out: js("ok")},
		{tool: event.ToolBash, input: js(map[string]string{"command": "cargo test 2>&1 | tail -40"}), out: js(map[string]string{"stdout": cargofail}), failed: true},
		{tool: "mcp:github:get_pr", input: js(map[string]any{"owner": "o", "repo": "r", "number": 12}), out: js(map[string]string{"content": "PR #12 open, 3 checks pending"})},
		{tool: event.ToolBash, input: js(map[string]string{"command": "git status --short"}), out: js(map[string]string{"stdout": " M src/cart.js\n?? src/new.ts"})},
		{tool: event.ToolBash, input: js(map[string]string{"command": "npm run build"}), out: js(map[string]string{"stdout": "built in 1.24s\n12 files, 48213 bytes"})},
	}
	var evs []*event.Event
	at := t0
	for i := 0; i < n; i++ {
		if i > 0 && i%250 == 0 {
			evs = append(evs, &event.Event{Phase: event.PhasePrompt, ReceivedAt: at})
		}
		m := mix[i%len(mix)]
		id := fmt.Sprintf("toolu_%06d", i)
		pre := &event.Event{Agent: event.AgentClaudeCode, Phase: event.PhasePre, ToolName: m.tool, ToolUseID: id, Input: m.input, Cwd: "/repo", PermMode: "auto", ReceivedAt: at}
		phase := event.PhasePost
		if m.failed {
			phase = event.PhasePostFailure
		}
		at = at.Add(3 * time.Second)
		post := &event.Event{Agent: event.AgentClaudeCode, Phase: phase, ToolName: m.tool, ToolUseID: id, Input: m.input, Output: m.out, IsError: m.failed, Cwd: "/repo", PermMode: "auto", ReceivedAt: at}
		evs = append(evs, pre, post)
	}
	return evs
}

// BenchmarkPipeline10kCalls: one op = 10,000 tool calls (20,000 pre/post
// events plus prompts) through the full pipeline. Target: < 1 s per op.
func BenchmarkPipeline10kCalls(b *testing.B) {
	const n = 10000
	evs := benchCalls(b, n)
	p := New(config.Default())
	b.ResetTimer()
	for b.Loop() {
		s := p.NewSession("cc:bench", event.AgentClaudeCode)
		var hash uint64
		for _, ev := range evs {
			if ev.ToolName == event.ToolEdit || ev.ToolName == event.ToolWrite {
				hash++
			}
			p.Handle(s, ev, hash)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/n, "ns/call")
}
