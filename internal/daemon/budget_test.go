package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/notify"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// fastTail polls transcripts often, so tests don't wait 2 s.
func fastTail(o *Options) { o.TailEvery = 20 * time.Millisecond }

// spend appends an API message to a transcript: output tokens of Claude
// Opus 5.5 ($20 per million), so 50,000 tokens cost $1.00. Each message is
// written on two lines, as Claude Code does, to check deduplication.
func spend(t *testing.T, path, id string, usd float64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	out := int64(usd * 50_000)
	line := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"claude-opus-5-5","role":"assistant",`+
		`"content":[{"type":"text","text":"x"}],"usage":{"input_tokens":0,"output_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano), id, out)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + line); err != nil {
		t.Fatal(err)
	}
}

func (s *scenario) transcript() string { return filepath.Join(s.cwd, s.sid+".jsonl") }

// usage fetches GET /v1/usage.
func (d *testDaemon) usage(t *testing.T) UsageReply {
	t.Helper()
	code, b := d.do("GET", "/v1/usage", nil, nil)
	if code != 200 {
		t.Fatalf("usage: %d %s", code, b)
	}
	var r UsageReply
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// waitCost waits until the tailer has counted a session's cost.
func (d *testDaemon) waitCost(t *testing.T, sid string, want float64) UsageView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		u := d.usage(t).Sessions[sid]
		if u.CostUSD >= want-1e-9 {
			return u
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s cost %v, want %v", sid, u.CostUSD, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func budgetIVs(t *testing.T, d *testDaemon, key string) (nudges, halts []store.Intervention) {
	t.Helper()
	for _, iv := range d.interventions(t, key) {
		if iv.Detector != event.DetectorBudget {
			continue
		}
		switch iv.Action {
		case "nudge":
			nudges = append(nudges, iv)
		case "halt":
			halts = append(halts, iv)
		}
	}
	return nudges, halts
}

func stopReason(t *testing.T, r EventReply) string {
	t.Helper()
	var o struct {
		StopReason string `json:"stopReason"`
	}
	json.Unmarshal([]byte(r.Stdout), &o)
	return o.StopReason
}

// TestBudgetSessionGuard: at the guard level a soft session budget gives
// one note with a tool result; a hard one stops the turn at the next tool
// call after the usage arrives, every turn while over it.
func TestBudgetSessionGuard(t *testing.T) {
	d := startDaemon(t, homeWithConfig(t, "mode: guard\nbudgets:\n  session: { soft_usd: 0.5, hard_usd: 1 }\n"), fastTail)
	s := newScenario(d, "b1")
	s.prompt("build the cart")
	if pre, post := s.bash("npm run build", ""); pre.Stdout != "" || post.Stdout != "" {
		t.Fatalf("under budget: %s %s", pre.Stdout, post.Stdout)
	}

	spend(t, s.transcript(), "msg_1", 0.6)
	d.waitCost(t, "b1", 0.6)
	pre, post := s.bash("npm run build", "")
	if pre.Stdout != "" || replyKind(t, post) != "nudge" || !strings.Contains(post.Stdout, "soft budget of $0.50") ||
		!strings.Contains(post.Stdout, "budgets.session.soft_usd") || !strings.Contains(post.Stdout, "Nothing is blocked") {
		t.Fatalf("soft budget: pre %s, post %s", pre.Stdout, post.Stdout)
	}
	if _, post := s.bash("npm run build", ""); post.Stdout != "" {
		t.Errorf("the soft note came twice: %s", post.Stdout)
	}

	spend(t, s.transcript(), "msg_2", 0.5)
	d.waitCost(t, "b1", 1.1)
	for i := 0; i < 2; i++ {
		pre, _ := s.bash("npm run build", "")
		r := stopReason(t, pre)
		if replyKind(t, pre) != "halt" || !strings.HasPrefix(r, "LoopGuard stopped this turn: this session's API-equivalent cost is $1.10, over the hard budget of $1.00") ||
			!strings.Contains(r, "budgets.session.hard_usd") || !strings.Contains(r, "a few seconds late") {
			t.Fatalf("hard budget, attempt %d: %s", i, pre.Stdout)
		}
	}
	nudges, halts := budgetIVs(t, d, "claude-code:b1")
	if len(nudges) != 1 || len(halts) != 1 || halts[0].Mode != store.ModeEnforce || halts[0].Outcome != "n/a" {
		t.Errorf("recorded: %d nudges, halts %+v", len(nudges), halts)
	}

	// A new turn: no "commands are not blocked" note (they still are), and
	// the first tool call stops again.
	if r := s.prompt("go on"); r.Stdout != "" {
		t.Errorf("prompt after a budget halt: %s", r.Stdout)
	}
	if pre, _ := s.bash("ls", ""); replyKind(t, pre) != "halt" {
		t.Errorf("next turn while over the hard budget: %s", pre.Stdout)
	}
	if _, halts := budgetIVs(t, d, "claude-code:b1"); len(halts) != 2 {
		t.Errorf("%d halts recorded, want one per turn (2)", len(halts))
	}
	u := d.usage(t).Sessions["b1"]
	if u.Usage.Output != 55_000 || u.ToolCalls != 3 {
		t.Errorf("usage %+v (each message is on two lines; counted once)", u)
	}
}

// TestBudgetLevels: below guard, budgets never touch the agent beyond the
// soft note at assist; notify sends budget_soft/budget_hard once, and the
// would-have halt is recorded once.
func TestBudgetLevels(t *testing.T) {
	for _, level := range []string{config.ModeObserve, config.ModeNotify, config.ModeAssist} {
		t.Run(level, func(t *testing.T) {
			rec := &sent{got: map[string][]notify.Notification{}}
			d := startDaemon(t, homeWithConfig(t, "mode: "+level+"\nbudgets:\n  session: { soft_usd: 0.5, hard_usd: 1 }\n"), func(o *Options) {
				fastTail(o)
				o.Senders = rec.senders()
			})
			s := newScenario(d, "lv-"+level)
			s.prompt("go")
			spend(t, s.transcript(), "msg_1", 0.6)
			d.waitCost(t, s.sid, 0.6)
			_, post := s.bash("ls", "")
			wantNudge := level == config.ModeAssist
			if got := replyKind(t, post) == "nudge"; got != wantNudge {
				t.Errorf("soft note reached the agent: %v, want %v (%s)", got, wantNudge, post.Stdout)
			}
			spend(t, s.transcript(), "msg_2", 0.6)
			d.waitCost(t, s.sid, 1.2)
			for i := 0; i < 3; i++ {
				if pre, _ := s.bash("ls", ""); pre.Stdout != "" {
					t.Fatalf("hard budget below guard touched the agent: %s", pre.Stdout)
				}
			}
			_, halts := budgetIVs(t, d, "claude-code:"+s.sid)
			if len(halts) != 1 || halts[0].Mode != store.ModeObserve {
				t.Errorf("would-have halts %+v, want one", halts)
			}
			want := 0
			if level != config.ModeObserve {
				want = 2
			}
			evs := rec.wait(t, config.ChannelDesktop, want)
			if len(evs) != want || (want == 2 && (evs[0] != config.EventBudgetSoft || evs[1] != config.EventBudgetHard)) {
				t.Errorf("notifications %v", evs)
			}
			if want == 2 && len(evs) == 2 {
				rec.mu.Lock()
				body := rec.got[config.ChannelDesktop][1].Body()
				rec.mu.Unlock()
				if !strings.Contains(body, "session cost $1.20 (API-equivalent), hard limit $1.00") {
					t.Errorf("budget_hard body %q", body)
				}
			}
		})
	}
}

// TestBudgetSubagentRollup: a subagent's usage counts toward its
// session's budget, and the halt reaches the parent's next tool call
// (unlike a loop halt). /v1/usage shows the streams apart.
func TestBudgetSubagentRollup(t *testing.T) {
	d := startDaemon(t, homeWithConfig(t, "mode: guard\nbudgets:\n  session: { hard_usd: 1 }\n"), fastTail)
	s := newScenario(d, "r1")
	s.prompt("investigate")
	s.bash("ls", "")
	sub := *s
	sub.agent = "a77"
	sub.bash("go test ./...", "")
	spend(t, s.transcript(), "msg_main", 0.3)
	spend(t, filepath.Join(s.cwd, s.sid, "subagents", "agent-a77.jsonl"), "msg_sub", 0.9)
	d.waitCost(t, "r1", 1.2)
	if pre, _ := s.bash("ls", ""); replyKind(t, pre) != "halt" {
		t.Fatalf("parent after the subagent's spending: %s", pre.Stdout)
	}
	if pre, _ := sub.bash("go test ./...", ""); replyKind(t, pre) != "halt" {
		t.Errorf("subagent stream over the session budget: %s", pre.Stdout)
	}
	u := d.usage(t)
	main, subU := u.Streams["claude-code:r1"], u.Streams["claude-code:r1/a77"]
	if fmt.Sprintf("%.2f %.2f", main.CostUSD, subU.CostUSD) != "0.30 0.90" {
		t.Errorf("streams: main %+v, subagent %+v", main, subU)
	}
}

// TestBudgetDailyAndRepo: the daily scope adds up every session; the repo
// scope only the sessions in the repo.
func TestBudgetDailyAndRepo(t *testing.T) {
	repo := gitRepo(t, "")
	d := startDaemon(t, homeWithConfig(t, "mode: guard\nbudgets:\n  daily: { hard_tool_calls: 5 }\n  repo: { hard_tool_calls: 3 }\n"), fastTail)
	a, b, out := newScenario(d, "d-a"), newScenario(d, "d-b"), newScenario(d, "d-out")
	a.cwd, b.cwd = repo, repo
	for _, s := range []*scenario{a, b, out} {
		s.prompt("go")
	}
	a.bash("ls", "")
	b.bash("ls", "")
	a.bash("ls", "") // the repo's 3rd call: at the limit, not over it
	if pre, _ := out.bash("ls", ""); pre.Stdout != "" {
		t.Fatalf("outside the repo, under the daily limit: %s", pre.Stdout)
	}
	b.bash("ls", "") // the repo's 4th
	if pre, _ := a.bash("ls", ""); replyKind(t, pre) != "halt" || !strings.Contains(stopReason(t, pre), "this repo's tool call count today is 4 calls") {
		t.Fatalf("repo budget: %s", pre.Stdout)
	}
	// The 6th call of the day completes (its pre saw 5, at the limit).
	if pre, _ := out.bash("ls", ""); pre.Stdout != "" {
		t.Fatalf("a session outside the repo was stopped by the repo budget: %s", pre.Stdout)
	}
	// 6 calls today in all.
	if pre, _ := out.bash("ls", ""); replyKind(t, pre) != "halt" || !strings.Contains(stopReason(t, pre), "today's tool call count across all sessions") {
		t.Fatalf("daily budget: %s", pre.Stdout)
	}
}

// TestUsageSurvivesRestart: offsets and usage are stored; a restarted
// daemon reads only what is new and counts each message once.
func TestUsageSurvivesRestart(t *testing.T) {
	dir := homeWithConfig(t, "mode: observe\n")
	d := startDaemon(t, dir, fastTail)
	s := newScenario(d, "rs")
	s.prompt("go")
	spend(t, s.transcript(), "msg_1", 0.4)
	// msg_2's first line only: its second line comes after the restart.
	spend(t, s.transcript(), "msg_2", 0.2)
	d.waitCost(t, "rs", 0.6)
	if err := d.stop(); err != nil {
		t.Fatal(err)
	}

	d2 := startDaemon(t, dir, fastTail)
	s.d = d2
	s.bash("ls", "")
	spend(t, s.transcript(), "msg_2", 0.2) // the same message again
	spend(t, s.transcript(), "msg_3", 0.1)
	u := d2.waitCost(t, "rs", 0.7)
	time.Sleep(100 * time.Millisecond)
	if u = d2.usage(t).Sessions["rs"]; fmt.Sprintf("%.2f", u.CostUSD) != "0.70" || u.ToolCalls != 1 {
		t.Errorf("after restart: %+v, want $0.70 and 1 call", u)
	}
	if today := d2.usage(t).Today; fmt.Sprintf("%.2f", today.CostUSD) != "0.70" {
		t.Errorf("today after restart: %+v", today)
	}
}

// TestStopSurvivesRestart: a pending `loopguard stop` is kept in the
// store, so a daemon restart before the next tool call doesn't lose it.
func TestStopSurvivesRestart(t *testing.T) {
	dir := homeWithConfig(t, "mode: assist\n")
	d := startDaemon(t, dir, nil)
	s := newScenario(d, "stop-rs")
	s.prompt("go")
	s.bash("ls", "")
	if code, r := postStop(t, d, `{"session":"stop-rs"}`); code != 200 || len(r.Stopped) != 1 {
		t.Fatalf("stop: %d %+v", code, r)
	}
	if err := d.stop(); err != nil {
		t.Fatal(err)
	}
	d2 := startDaemon(t, dir, nil)
	s.d = d2
	if pre, _ := s.bash("ls", ""); replyKind(t, pre) != "halt" {
		t.Fatalf("after a restart the stop was lost: %s", pre.Stdout)
	}
	// The turn ends: the stored request is gone too.
	s.send("Stop", `"stop_hook_active":false`)
	time.Sleep(200 * time.Millisecond) // the delete runs off the hook path
	if err := d2.stop(); err != nil {
		t.Fatal(err)
	}
	d3 := startDaemon(t, dir, nil)
	s.d = d3
	if pre, _ := s.bash("ls", ""); pre.Stdout != "" {
		t.Errorf("a finished stop came back after a restart: %s", pre.Stdout)
	}
}
