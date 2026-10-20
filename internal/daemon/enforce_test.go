package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// homeWithConfig is a temp home whose config.yaml holds cfg ("" for none).
func homeWithConfig(t *testing.T, cfg string) string {
	t.Helper()
	dir := tempHome(t)
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, config.GlobalFile), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// scenario sends real-shaped Claude Code payloads for one session.
type scenario struct {
	d             *testDaemon
	sid, agent    string
	cwd, permMode string
	n             int
}

func newScenario(d *testDaemon, sid string) *scenario {
	return &scenario{d: d, sid: sid, cwd: d.home, permMode: "auto"}
}

func (s *scenario) send(event, extra string) EventReply {
	s.d.t.Helper()
	m := map[string]any{"session_id": s.sid, "hook_event_name": event, "cwd": s.cwd, "permission_mode": s.permMode,
		"transcript_path": filepath.Join(s.cwd, s.sid+".jsonl")}
	if s.agent != "" {
		m["agent_id"], m["agent_type"] = s.agent, "general-purpose"
	}
	if extra != "" {
		if err := json.Unmarshal([]byte("{"+extra+"}"), &m); err != nil {
			s.d.t.Fatalf("bad extra %q: %v", extra, err)
		}
	}
	b, _ := json.Marshal(m)
	return s.d.event(string(b))
}

func (s *scenario) prompt(text string) EventReply {
	return s.send("UserPromptSubmit", fmt.Sprintf(`"prompt":%q`, text))
}

// bash runs one Bash call: the pre, then (unless the pre was denied or
// halted, as Claude Code would) its post or post-failure.
func (s *scenario) bash(cmd, failure string) (pre, post EventReply) {
	s.n++
	id := fmt.Sprintf(`"tool_name":"Bash","tool_use_id":"toolu_%s_%d","tool_input":{"command":%q,"description":"run"}`, s.sid, s.n, cmd)
	pre = s.send("PreToolUse", id)
	if k := replyKind(s.d.t, pre); k == "deny" || k == "halt" {
		return pre, EventReply{}
	}
	if failure != "" {
		post = s.send("PostToolUseFailure", id+fmt.Sprintf(`,"error":%q,"is_interrupt":false,"duration_ms":900`, failure))
	} else {
		post = s.send("PostToolUse", id+`,"tool_response":{"stdout":"ok","stderr":"","interrupted":false},"duration_ms":40`)
	}
	return pre, post
}

// edit runs one successful Edit of path.
func (s *scenario) edit(path string) EventReply {
	s.n++
	in := fmt.Sprintf(`"tool_name":"Edit","tool_use_id":"toolu_%s_%d","tool_input":{"file_path":%q,"old_string":"a%d","new_string":"b%d"}`,
		s.sid, s.n, path, s.n, s.n)
	s.send("PreToolUse", in)
	return s.send("PostToolUse", in+fmt.Sprintf(`,"tool_response":{"filePath":%q,"oldString":"a","newString":"b","structuredPatch":[],"userModified":false}`, path))
}

const npmFailure = "Exit code 1\nFAIL src/cart.test.ts\n  ● cart › total › adds tax\n    expect(received).toBe(expected)\n    Expected: 110\n    Received: 100\n      at Object.<anonymous> (src/cart.test.ts:14:23)"

// replyKind classifies a reply: none, nudge, deny, or halt. It fails the
// test on output LoopGuard must never produce.
func replyKind(t *testing.T, r EventReply) string {
	t.Helper()
	if r.Exit != 0 {
		t.Fatalf("exit code %d", r.Exit)
	}
	if r.Stdout == "" {
		return "none"
	}
	var o struct {
		Continue           *bool  `json:"continue"`
		StopReason         string `json:"stopReason"`
		HookSpecificOutput *struct {
			HookEventName      string `json:"hookEventName"`
			AdditionalContext  string `json:"additionalContext"`
			PermissionDecision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &o); err != nil {
		t.Fatalf("reply %q: %v", r.Stdout, err)
	}
	h := o.HookSpecificOutput
	switch {
	case h != nil && (h.PermissionDecision == "allow" || h.PermissionDecision == "ask"):
		t.Fatalf("permissionDecision %q: %s", h.PermissionDecision, r.Stdout)
	case o.Continue != nil && !*o.Continue && o.StopReason != "":
		return "halt"
	case h != nil && h.PermissionDecision == "deny":
		return "deny"
	case h != nil && h.AdditionalContext != "":
		return "nudge"
	}
	t.Fatalf("unrecognized reply %s", r.Stdout)
	return ""
}

// attempt is one failing `npm test`, after an edit (as an agent "fixing"
// the test does). It returns the kinds of the pre and post replies.
func (s *scenario) attempt() (string, string) {
	s.d.t.Helper()
	s.edit("src/cart.ts")
	pre, post := s.bash("npm test", npmFailure)
	pk := replyKind(s.d.t, pre)
	if pk == "deny" || pk == "halt" {
		return pk, ""
	}
	return pk, replyKind(s.d.t, post)
}

func (d *testDaemon) interventions(t *testing.T, key string) []store.Intervention {
	t.Helper()
	var detail struct {
		Interventions []store.Intervention `json:"interventions"`
	}
	code, b := d.do("GET", "/v1/sessions/"+key, nil, nil)
	if code != 200 {
		t.Fatalf("session %s: %d %s", key, code, b)
	}
	if err := json.Unmarshal(b, &detail); err != nil {
		t.Fatal(err)
	}
	return detail.Interventions
}

func TestEnforceNudgeDenyHalt(t *testing.T) {
	d := startDaemon(t, homeWithConfig(t, "mode: enforce\n"), nil)
	s := newScenario(d, "e1")
	s.send("SessionStart", `"source":"startup","model":"claude-opus-5-5"`)
	s.prompt("make the cart tests pass")

	for i := 1; i <= 3; i++ {
		if pre, post := s.attempt(); pre != "none" || post != "none" {
			t.Fatalf("attempt %d: %s/%s, want none", i, pre, post)
		}
	}
	// The 4th identical failure: a nudge in the PostToolUseFailure reply.
	s.edit("src/cart.ts")
	_, post := s.bash("npm test", npmFailure)
	if replyKind(t, post) != "nudge" || !strings.Contains(post.Stdout, `"hookEventName":"PostToolUseFailure"`) ||
		!strings.Contains(post.Stdout, "has failed 4 times") || !strings.Contains(post.Stdout, `FAIL src/cart.test.ts\n`) ||
		!strings.Contains(post.Stdout, "these files were edited: src/cart.ts.") {
		t.Fatalf("4th failure: %s", post.Stdout)
	}
	for i := 5; i <= 7; i++ {
		if pre, post := s.attempt(); pre != "none" || post != "none" {
			t.Fatalf("attempt %d: %s/%s, want none (escalating)", i, pre, post)
		}
	}
	if pre, _ := s.attempt(); pre != "deny" {
		t.Fatalf("attempt 8: %s, want deny", pre)
	}
	// Only the looping action is denied.
	if pre, post := s.bash("cat README.md", ""); replyKind(t, pre) != "none" || replyKind(t, post) != "none" {
		t.Fatalf("cat README.md: %s / %s", pre.Stdout, post.Stdout)
	}
	if pre, _ := s.attempt(); pre != "deny" {
		t.Fatalf("attempt 9: %s, want deny", pre)
	}
	s.edit("src/cart.ts")
	pre, _ := s.bash("npm test", npmFailure)
	if replyKind(t, pre) != "halt" || !strings.Contains(pre.Stdout, "loopguard pause --session e1") {
		t.Fatalf("attempt 10: %s, want halt", pre.Stdout)
	}
	s.send("Stop", `"stop_hook_active":false`)

	// The human reads the reason and replies: the loop state resets.
	s.prompt("try mocking the tax service instead")
	if pre, post := s.attempt(); pre != "none" || post != "none" {
		t.Fatalf("after the human prompt: %s/%s, want none", pre, post)
	}

	ivs := d.interventions(t, "claude-code:e1")
	var got []string
	for _, iv := range ivs {
		if iv.Mode != store.ModeEnforce {
			t.Errorf("intervention %+v: mode %s, want enforce", iv, iv.Mode)
		}
		got = append(got, iv.Action+"="+iv.Outcome)
	}
	// The nudge's outcome is decided 10 completed calls later, when the loop
	// was still going; the denies were followed by the halt.
	if strings.Join(got, " ") != "nudge=continued_loop deny=halted deny=halted halt=halted" {
		t.Errorf("interventions = %v", got)
	}
}

func TestObserveModeRecordsWouldHave(t *testing.T) {
	for _, cfg := range []string{"", "mode: observe\n", "mode: enforce\nobserve_until: 2999-01-01T00:00:00Z\n", "mode: [\n"} {
		t.Run(strings.TrimSpace(cfg), func(t *testing.T) {
			d := startDaemon(t, homeWithConfig(t, cfg), nil)
			s := newScenario(d, "o1")
			s.prompt("make the cart tests pass")
			for i := 1; i <= 11; i++ {
				if pre, post := s.attempt(); pre != "none" || post != "none" {
					t.Fatalf("attempt %d: %s/%s, want none in observe mode", i, pre, post)
				}
			}
			ivs := d.interventions(t, "claude-code:o1")
			var got []string
			for _, iv := range ivs {
				if iv.Mode != store.ModeObserve || iv.Message == "" {
					t.Errorf("intervention %+v, want a would-have with a message", iv)
				}
				got = append(got, iv.Action)
			}
			if strings.Join(got, " ") != "nudge deny deny halt" {
				t.Errorf("would have = %v", got)
			}
		})
	}
}

func TestPauseAndResume(t *testing.T) {
	dir := homeWithConfig(t, "mode: enforce\n")
	d := startDaemon(t, dir, nil)
	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	reload := func() {
		if code, b := d.do("POST", "/v1/pauses/reload", nil, nil); code != 200 {
			t.Fatalf("reload: %d %s", code, b)
		}
	}
	if err := st.SetPause(ctx, store.Pause{Scope: store.PauseSession, Target: "p1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	reload()

	s := newScenario(d, "p1")
	other := newScenario(d, "p2")
	s.prompt("go")
	other.prompt("go")
	for i := 1; i <= 7; i++ {
		if pre, post := s.attempt(); pre != "none" || post != "none" {
			t.Fatalf("paused attempt %d: %s/%s", i, pre, post)
		}
		other.attempt()
	}
	var detail struct {
		Session sessionView `json:"session"`
	}
	_, b := d.do("GET", "/v1/sessions/claude-code:p1", nil, nil)
	json.Unmarshal(b, &detail)
	if detail.Session.Mode != config.ModeObserve || !strings.Contains(detail.Session.ModeWhy, "paused") {
		t.Errorf("paused session mode %q (%s)", detail.Session.Mode, detail.Session.ModeWhy)
	}
	// The other session isn't paused: it is denied by now.
	if pre, _ := other.attempt(); pre != "deny" {
		t.Errorf("unpaused session: %s, want deny", pre)
	}

	// Resume: the loop kept its state while paused, so the next attempt is
	// denied at once.
	if _, err := st.DeletePauses(ctx, store.PauseSession, "p1"); err != nil {
		t.Fatal(err)
	}
	reload()
	if pre, _ := s.attempt(); pre != "deny" {
		t.Errorf("after resume: %s, want deny", pre)
	}

	// --all, with an expiry.
	st.SetPause(ctx, store.Pause{Scope: store.PauseAll, Until: time.Now().Add(time.Hour), CreatedAt: time.Now()})
	reload()
	if pre, _ := s.attempt(); pre != "none" {
		t.Errorf("paused --all: %s, want none", pre)
	}
	var ps struct {
		Pauses []store.Pause `json:"pauses"`
	}
	_, b = d.do("GET", "/v1/pauses", nil, nil)
	if json.Unmarshal(b, &ps); len(ps.Pauses) != 1 || ps.Pauses[0].Scope != store.PauseAll {
		t.Errorf("pauses = %s", b)
	}
}

func TestPauseMatching(t *testing.T) {
	now := time.Now()
	repo := filepath.Join(t.TempDir(), "Repo")
	p := &pauses{}
	p.set([]store.Pause{
		{Scope: store.PauseSession, Target: "s1"},
		{Scope: store.PauseRepo, Target: repo, Until: now.Add(time.Minute)},
		{Scope: store.PauseSession, Target: "old", Until: now.Add(-time.Second)},
	})
	for _, tt := range []struct {
		sid, repo, cwd string
		want           bool
	}{
		{"s1", "", "", true},
		{"s2", "", "", false},
		{"s2", repo, repo, true},
		{"s2", "", filepath.Join(repo, "sub"), true},
		{"s2", "", repo + "2", false},
		{"old", "", "", false}, // expired
	} {
		if _, ok := p.match(tt.sid, tt.repo, tt.cwd, now); ok != tt.want {
			t.Errorf("match(%q, %q, %q) = %v", tt.sid, tt.repo, tt.cwd, ok)
		}
	}
	if _, ok := p.match("s2", repo, repo, now.Add(2*time.Minute)); ok {
		t.Error("an expired repo pause still matches")
	}
}

// gitRepo makes a directory that looks like a git repo, with an optional
// .loopguard.yaml.
func gitRepo(t *testing.T, repoCfg string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if repoCfg != "" {
		if err := os.WriteFile(filepath.Join(dir, config.RepoFile), []byte(repoCfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRepoFileCannotLoosen(t *testing.T) {
	t.Run("can't turn enforcement on", func(t *testing.T) {
		d := startDaemon(t, homeWithConfig(t, ""), nil)
		s := newScenario(d, "r1")
		s.cwd = gitRepo(t, "mode: enforce\npolicy: { nudge_threshold: 0.1 }\n")
		s.prompt("go")
		for i := 1; i <= 11; i++ {
			if pre, post := s.attempt(); pre != "none" || post != "none" {
				t.Fatalf("attempt %d: %s/%s, want none", i, pre, post)
			}
		}
	})
	t.Run("can't raise thresholds, can lower them", func(t *testing.T) {
		d := startDaemon(t, homeWithConfig(t, "mode: enforce\n"), nil)
		high := newScenario(d, "r2")
		high.cwd = gitRepo(t, "detectors: { repeated_failure: { min_count: 50 } }\n")
		high.prompt("go")
		low := newScenario(d, "r3")
		low.cwd = gitRepo(t, "detectors: { repeated_failure: { min_count: 2 } }\n")
		low.prompt("go")
		var highAt, lowAt int
		for i := 1; i <= 4; i++ {
			if _, post := high.attempt(); post == "nudge" && highAt == 0 {
				highAt = i
			}
			if _, post := low.attempt(); post == "nudge" && lowAt == 0 {
				lowAt = i
			}
		}
		if highAt != 4 || lowAt != 2 {
			t.Errorf("first nudge at attempt %d (min_count 50 in the repo) and %d (min_count 2); want 4 and 2", highAt, lowAt)
		}
	})
	t.Run("enabled: false turns it off", func(t *testing.T) {
		d := startDaemon(t, homeWithConfig(t, "mode: enforce\n"), nil)
		s := newScenario(d, "r4")
		s.cwd = gitRepo(t, "enabled: false\n")
		s.prompt("go")
		for i := 1; i <= 11; i++ {
			if pre, post := s.attempt(); pre != "none" || post != "none" {
				t.Fatalf("attempt %d: %s/%s, want none", i, pre, post)
			}
		}
		// Nothing about the session is stored.
		if code, b := d.do("GET", "/v1/sessions/claude-code:r4", nil, nil); code != 404 {
			t.Errorf("a disabled repo's session was recorded: %d %s", code, b)
		}
	})
}

func TestSubagentLoopNeverHaltsTheParent(t *testing.T) {
	d := startDaemon(t, homeWithConfig(t, "mode: enforce\n"), nil)
	parent := newScenario(d, "sa1")
	parent.prompt("research and fix the cart tests")
	sub := newScenario(d, "sa1")
	sub.agent = "agent-1"
	var kinds []string
	for i := 1; i <= 14; i++ {
		pre, post := sub.attempt()
		if pre == "halt" || post == "halt" {
			t.Fatalf("attempt %d halted the turn because of a subagent loop", i)
		}
		kinds = append(kinds, pre+"/"+post)
	}
	if kinds[3] != "none/nudge" || kinds[7] != "deny/" || kinds[13] != "deny/" {
		t.Errorf("subagent replies: %v", kinds)
	}
	// The parent is unaffected...
	if pre, post := parent.bash("npm test", npmFailure); replyKind(t, pre) != "none" || replyKind(t, post) != "none" {
		t.Errorf("parent: %s / %s", pre.Stdout, post.Stdout)
	}
	// ...and its next human prompt starts a new turn for the subagent too.
	parent.prompt("ok, try a different approach")
	if pre, _ := sub.attempt(); pre != "none" {
		t.Errorf("subagent after the parent's human prompt: %s, want none", pre)
	}
}

func TestOutcomesClosedAtStart(t *testing.T) {
	dir := homeWithConfig(t, "mode: enforce\n")
	d := startDaemon(t, dir, nil)
	s := newScenario(d, "oc1")
	s.prompt("go")
	for i := 1; i <= 4; i++ {
		s.attempt()
	}
	if err := d.stop(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ivs, _ := st.Interventions(context.Background(), "claude-code:oc1", 10)
	if len(ivs) != 1 || ivs[0].Action != "nudge" || ivs[0].Outcome != "unknown" {
		t.Errorf("after shutdown: %+v, want the nudge with outcome unknown", ivs)
	}
}
