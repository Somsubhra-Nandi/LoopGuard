package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
)

// Scripted sessions: testdata/sessions/*.yaml. Each file is one session,
// fed through the full pipeline (normalize → session → detect → policy).
//
//	name: what the scenario shows
//	perm_mode: auto            # optional (default "auto")
//	step_seconds: 30           # optional: time between tool calls (default 30)
//	files:                     # optional: initial file contents, so edits
//	  src/a.ts: "x = 1"        #   produce content hashes (and fail when old is missing)
//	steps:
//	  - bash: npx jest
//	    result: fail           # fail | ok (default ok)
//	    output: jest/cart_total_fail_1.txt   # file under testdata/outputs (alias: error)
//	    text: "inline output"  # instead of output
//	  - edit: src/a.ts
//	    old: "x"
//	    new: "y"
//	  - write: src/a.ts
//	    content: "..."         # or hash: H1 (a symbolic content hash)
//	  - read: src/a.ts
//	  - tool: other:TodoWrite  # any normalized tool name
//	    input: {todos: [...]}
//	  - prompt: "try again"    # a human turn boundary (not a call)
//	  - repeat: 3              # repeat a block
//	    steps: [...]
//	expect:
//	  signals: ["7:repeated_failure"]   # exactly the signals emitted, as "<call>:<detector>"
//	  decisions: ["7:nudge"]            # exactly the would-have decisions, as "<call>:<action>"
//	  never: [exact_repeat]             # detectors that must not fire
//
// Calls are numbered from 1 in order, after expanding repeats; prompts are
// not calls. Omitting signals or decisions leaves them unchecked; an empty
// list asserts there are none.
type scenario struct {
	Name        string            `yaml:"name"`
	PermMode    string            `yaml:"perm_mode"`
	StepSeconds int               `yaml:"step_seconds"`
	Files       map[string]string `yaml:"files"`
	Steps       []step            `yaml:"steps"`
	Expect      struct {
		Signals   *[]string `yaml:"signals"`
		Decisions *[]string `yaml:"decisions"`
		Never     []string  `yaml:"never"`
	} `yaml:"expect"`
}

type step struct {
	Bash    string         `yaml:"bash"`
	Result  string         `yaml:"result"`
	Output  string         `yaml:"output"`
	Error   string         `yaml:"error"`
	Text    string         `yaml:"text"`
	Edit    string         `yaml:"edit"`
	Old     string         `yaml:"old"`
	New     string         `yaml:"new"`
	Write   string         `yaml:"write"`
	Content *string        `yaml:"content"`
	Hash    string         `yaml:"hash"`
	Read    string         `yaml:"read"`
	Tool    string         `yaml:"tool"`
	Input   map[string]any `yaml:"input"`
	Prompt  *string        `yaml:"prompt"`
	Repeat  int            `yaml:"repeat"`
	Steps   []step         `yaml:"steps"`
}

const scenarioCwd = "/repo"

var t0 = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

func expand(steps []step) []step {
	var out []step
	for _, s := range steps {
		if s.Repeat > 0 {
			for i := 0; i < s.Repeat; i++ {
				out = append(out, expand(s.Steps)...)
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// runResult is what a scenario produced.
type runResult struct {
	signals   []string // "<call>:<detector>"
	keys      []string // LoopKey of each signal, in the same order
	decisions []string // "<call>:<action>"
	details   []string // for failure messages
}

// runScenario feeds a scenario through a pipeline built from th.
func runScenario(t testing.TB, sc scenario, th config.Thresholds) runResult {
	t.Helper()
	p := New(th)
	s := p.NewSession("claude-code:"+sc.Name, event.AgentClaudeCode)
	perm := sc.PermMode
	if perm == "" {
		perm = "auto"
	}
	stepDur := time.Duration(sc.StepSeconds) * time.Second
	if stepDur == 0 {
		stepDur = 30 * time.Second
	}
	files := make(map[string]string, len(sc.Files))
	for k, v := range sc.Files {
		files[k] = v
	}

	var res runResult
	clock := t0
	call := 0
	record := func(r Result) {
		for _, sig := range r.Signals {
			res.signals = append(res.signals, fmt.Sprintf("%d:%s", call, sig.Detector))
			res.keys = append(res.keys, sig.LoopKey)
			res.details = append(res.details, fmt.Sprintf("call %d: signal %s key=%s count=%d sev=%.2f", call, sig.Detector, sig.LoopKey, sig.Count, sig.Severity))
		}
		for _, d := range r.Decisions {
			if d.Action != event.ActionNone {
				t.Fatalf("call %d: observe mode returned action %q", call, d.Action)
			}
			res.decisions = append(res.decisions, fmt.Sprintf("%d:%s", call, d.WouldHave))
			res.details = append(res.details, fmt.Sprintf("call %d: would %s (%s)", call, d.WouldHave, d.Reason))
		}
	}

	for _, st := range expand(sc.Steps) {
		if st.Prompt != nil {
			p.Handle(s, &event.Event{Agent: event.AgentClaudeCode, Phase: event.PhasePrompt, Prompt: *st.Prompt, ReceivedAt: clock, PermMode: perm}, 0)
			clock = clock.Add(stepDur)
			continue
		}
		call++
		tool, input, output, failed, hash := buildCall(t, st, files)
		id := fmt.Sprintf("toolu_%04d", call)
		pre := &event.Event{
			Agent: event.AgentClaudeCode, SessionID: sc.Name, Phase: event.PhasePre, ToolName: tool,
			ToolUseID: id, Input: input, Cwd: scenarioCwd, PermMode: perm, ReceivedAt: clock,
		}
		record(p.Handle(s, pre, 0))
		phase := event.PhasePost
		if failed {
			phase = event.PhasePostFailure
		}
		post := &event.Event{
			Agent: event.AgentClaudeCode, SessionID: sc.Name, Phase: phase, ToolName: tool,
			ToolUseID: id, Input: input, Output: output, IsError: failed, Cwd: scenarioCwd,
			PermMode: perm, ReceivedAt: clock.Add(time.Second),
		}
		record(p.Handle(s, post, hash))
		clock = clock.Add(stepDur)
	}
	return res
}

// buildCall turns a step into a tool name, input, output, failure flag, and
// file content hash after the call.
func buildCall(t testing.TB, st step, files map[string]string) (string, json.RawMessage, json.RawMessage, bool, uint64) {
	t.Helper()
	mustJSON := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	switch {
	case st.Bash != "":
		out := st.Text
		if f := st.Output + st.Error; f != "" {
			b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "outputs", filepath.FromSlash(f)))
			if err != nil {
				t.Fatal(err)
			}
			out = string(b)
		}
		failed := st.Result == "fail"
		return event.ToolBash, mustJSON(map[string]any{"command": st.Bash}), mustJSON(map[string]any{"stdout": out, "stderr": ""}), failed, 0

	case st.Edit != "":
		in := mustJSON(map[string]any{"file_path": st.Edit, "old_string": st.Old, "new_string": st.New})
		content, known := files[st.Edit]
		switch {
		case st.Hash != "":
			return event.ToolEdit, in, mustJSON("ok"), false, normalize.HashString(st.Hash)
		case !known:
			return event.ToolEdit, in, mustJSON("ok"), false, 0
		case !strings.Contains(content, st.Old):
			return event.ToolEdit, in, mustJSON("String to replace not found in file."), true, 0
		}
		content = strings.Replace(content, st.Old, st.New, 1)
		files[st.Edit] = content
		return event.ToolEdit, in, mustJSON("ok"), false, normalize.HashString(content)

	case st.Write != "":
		if st.Content != nil {
			files[st.Write] = *st.Content
			in := mustJSON(map[string]any{"file_path": st.Write, "content": *st.Content})
			return event.ToolWrite, in, mustJSON("ok"), false, normalize.HashString(*st.Content)
		}
		in := mustJSON(map[string]any{"file_path": st.Write, "content": "content-" + st.Hash})
		return event.ToolWrite, in, mustJSON("ok"), false, normalize.HashString(st.Hash)

	case st.Read != "":
		return event.ToolRead, mustJSON(map[string]any{"file_path": st.Read}), mustJSON(files[st.Read]), false, 0

	case st.Tool != "":
		return st.Tool, mustJSON(st.Input), mustJSON(st.Text), st.Result == "fail", 0
	}
	t.Fatalf("step has no action: %+v", st)
	return "", nil, nil, false, 0
}

func loadScenarios(t testing.TB) map[string]scenario {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "testdata", "sessions", "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no scenarios found: %v", err)
	}
	out := make(map[string]scenario, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var sc scenario
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&sc); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out[strings.TrimSuffix(filepath.Base(p), ".yaml")] = sc
	}
	return out
}

func TestScenarios(t *testing.T) {
	for name, sc := range loadScenarios(t) {
		t.Run(name, func(t *testing.T) {
			res := runScenario(t, sc, config.Default())
			trace := "\n  " + strings.Join(res.details, "\n  ")
			if want := sc.Expect.Signals; want != nil && !equalStrings(res.signals, *want) {
				t.Errorf("%s\nsignals = %q\n   want %q%s", sc.Name, res.signals, *want, trace)
			}
			if want := sc.Expect.Decisions; want != nil && !equalStrings(res.decisions, *want) {
				t.Errorf("%s\ndecisions = %q\n     want %q%s", sc.Name, res.decisions, *want, trace)
			}
			for _, d := range sc.Expect.Never {
				for _, s := range res.signals {
					if strings.HasSuffix(s, ":"+d) {
						t.Errorf("%s: detector %s fired (%s)%s", sc.Name, d, s, trace)
					}
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
