package replay

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/replay/replaytest"
)

func readAll(t *testing.T, content string) ([]*event.Event, []uint64, LineStats) {
	t.Helper()
	var evs []*event.Event
	var hashes []uint64
	st, err := ReadStream(strings.NewReader(content), Stream{SessionID: replaytest.SessionID}, func(ev *event.Event, h uint64) {
		evs = append(evs, ev)
		hashes = append(hashes, h)
	})
	if err != nil {
		t.Fatal(err)
	}
	return evs, hashes, st
}

func TestReadStream(t *testing.T) {
	evs, hashes, st := readAll(t, replaytest.MainSession())

	// 5 jest + 4 edits + 1 read + 4 writes.
	if st.ToolUses != 14 || st.ToolResults != 15 || st.Unjoined != 1 {
		t.Errorf("tool uses/results/unjoined = %d/%d/%d, want 14/15/1", st.ToolUses, st.ToolResults, st.Unjoined)
	}
	if st.Malformed != 1 {
		t.Errorf("malformed = %d, want 1", st.Malformed)
	}
	// The human prompt and the slash command; not the task notification,
	// the isMeta caveat, or the compaction summary.
	if st.Prompts != 2 {
		t.Errorf("prompts = %d, want 2", st.Prompts)
	}

	var last time.Time
	pres, posts := 0, 0
	for i, ev := range evs {
		if ev.ReceivedAt.Before(last) {
			t.Fatalf("event %d goes back in time", i)
		}
		last = ev.ReceivedAt
		switch ev.Phase {
		case event.PhasePre:
			pres++
		case event.PhasePost, event.PhasePostFailure:
			posts++
			if len(ev.Input) == 0 {
				t.Errorf("post event %s has no input", ev.ToolUseID)
			}
			if ev.ToolName == event.ToolBash && ev.Phase != event.PhasePostFailure {
				t.Errorf("failed jest run not marked post_failure")
			}
			if ev.ToolName == event.ToolWrite && hashes[i] == 0 {
				t.Error("write without a content hash")
			}
			if ev.ToolName == event.ToolEdit && hashes[i] != 0 {
				t.Error("edit with a content hash: replay cannot know the file after an edit")
			}
		}
		if ev.Cwd != replaytest.Cwd || ev.PermMode != "auto" {
			t.Errorf("event %d: cwd %q perm %q", i, ev.Cwd, ev.PermMode)
		}
	}
	if pres != 14 || posts != 14 {
		t.Errorf("pre/post events = %d/%d, want 14/14", pres, posts)
	}
	// The 100 KB read result is read whole.
	for _, ev := range evs {
		if ev.ToolName == event.ToolRead && ev.Phase == event.PhasePost && len(ev.Output) < 100_000 {
			t.Errorf("big result truncated to %d bytes", len(ev.Output))
		}
	}
}

func TestReadStreamInterrupt(t *testing.T) {
	b := replaytest.NewBuilder("s", "")
	b.Tool("Bash", map[string]any{"command": "npm test"}, "The user doesn't want to proceed with this tool use.", true)
	b.Tool("Bash", map[string]any{"command": "ls"}, "a\nb", false)
	_, _, st := readAll(t, b.String())
	if st.Prompts != 1 {
		t.Errorf("a rejected tool call should be a turn boundary: prompts = %d", st.Prompts)
	}
}

func writeSynthetic(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := replaytest.WriteTree(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDiscover(t *testing.T) {
	root := writeSynthetic(t)
	streams, err := Discover(root, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 2 || streams[0].AgentID != "" || streams[1].AgentID != replaytest.AgentID ||
		streams[1].SessionID != replaytest.SessionID || streams[0].Project != replaytest.Project {
		t.Fatalf("streams = %+v", streams)
	}
	if s, _ := Discover(root, Filter{Session: "5e55"}); len(s) != 2 {
		t.Errorf("session prefix filter: %d streams", len(s))
	}
	if s, _ := Discover(root, Filter{Session: "nope"}); len(s) != 0 {
		t.Errorf("session filter kept %d streams", len(s))
	}
	if s, _ := Discover(root, Filter{Since: time.Now().Add(time.Hour)}); len(s) != 0 {
		t.Errorf("since filter kept %d streams", len(s))
	}
	if s, _ := Discover(root, Filter{Repo: replaytest.Cwd}); len(s) != 2 {
		t.Errorf("repo filter: %d streams", len(s))
	}
	if s, _ := Discover(root, Filter{Repo: `C:\Users\dev\work\other`}); len(s) != 0 {
		t.Errorf("repo filter kept %d streams of another repo", len(s))
	}
}

func TestEncodeProjectDir(t *testing.T) {
	if got := EncodeProjectDir(`C:\Users\Jane Doe\desktop\myproject`); got != "C--Users-Jane-Doe-desktop-myproject" {
		t.Errorf("got %q", got)
	}
	if got := EncodeProjectDir("/home/dev/my.app"); got != "-home-dev-my-app" {
		t.Errorf("got %q", got)
	}
}

func runSynthetic(t *testing.T) *Result {
	t.Helper()
	streams, err := Discover(writeSynthetic(t), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return Run(streams, Options{Thresholds: config.Default(), Workers: 2})
}

func TestRunFindsLoops(t *testing.T) {
	res := runSynthetic(t)
	if res.Sessions != 1 || res.Streams != 2 || res.SessionsWithLoops != 1 {
		t.Errorf("sessions/streams/with loops = %d/%d/%d", res.Sessions, res.Streams, res.SessionsWithLoops)
	}
	if len(res.Loops) != 3 {
		for _, l := range res.Loops {
			t.Logf("%+v", *l)
		}
		t.Fatalf("loops = %d, want 3", len(res.Loops))
	}
	SortLoops(res.Loops)
	byDet := map[string]*Loop{}
	for _, l := range res.Loops {
		byDet[l.Detector+"/"+l.AgentID] = l
	}

	rf := byDet[event.DetectorRepeatedFailure+"/"]
	if rf == nil || rf.Family != "jest" || rf.Count != 5 || rf.FirstCall != 1 || rf.LastCall != 9 || rf.FirstSignalCall != 7 {
		t.Fatalf("repeated failure loop = %+v", rf)
	}
	if rf.WastedToolCalls != 9 || rf.DurationSeconds != (8*22) {
		t.Errorf("wasted calls %d, duration %v", rf.WastedToolCalls, rf.DurationSeconds)
	}
	if len(rf.WouldHave) == 0 || rf.WouldHave[0].Action != "nudge" || rf.WouldHave[0].Call != 7 {
		t.Errorf("would have = %+v", rf.WouldHave)
	}
	for _, l := range rf.ErrorLines {
		if strings.Contains(l, replaytest.Secret) {
			t.Errorf("secret in error lines: %q", l)
		}
	}
	if len(rf.ErrorLines) != 2 {
		t.Errorf("error lines = %q", rf.ErrorLines)
	}

	os := byDet[event.DetectorOscillation+"/"]
	if os == nil || os.File != "src/flags.ts" || os.FirstCall != 11 || os.LastCall != 14 {
		t.Errorf("oscillation loop = %+v", os)
	}

	sub := byDet[event.DetectorRepeatedFailure+"/"+replaytest.AgentID]
	if sub == nil || sub.Family != "go test" || sub.Count != 4 || sub.Stream() != replaytest.SessionID+"/"+replaytest.AgentID {
		t.Errorf("subagent loop = %+v", sub)
	}
	if res.WastedToolCalls != 9+4+4 {
		t.Errorf("wasted tool calls = %d", res.WastedToolCalls)
	}
	if res.Lines.Malformed != 1 || res.ToolCalls != 14+4 {
		t.Errorf("lines %+v, tool calls %d", res.Lines, res.ToolCalls)
	}
}

func TestUnion(t *testing.T) {
	t0 := time.Unix(0, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	d, n := union([]span{{at(0), at(10), 1, 5}, {at(5), at(20), 3, 8}, {at(100), at(110), 20, 21}})
	if d != 30*time.Second || n != 10 {
		t.Errorf("union = %v, %d calls", d, n)
	}
}

func TestReport(t *testing.T) {
	res := runSynthetic(t)
	r := NewReport(res, "/x", time.Time{}, ReportOptions{Top: 2}, time.Unix(0, 0), time.Second)
	if r.TotalLoops != 3 || len(r.TopLoops) != 2 || r.LoopsByDetector[event.DetectorRepeatedFailure] != 2 {
		t.Errorf("report = %+v", r)
	}
	var text bytes.Buffer
	if err := r.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Loops found          3", "repeated_failure", "`jest`", "would have: nudged at call 7"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report missing %q:\n%s", want, text.String())
		}
	}
	if strings.Contains(text.String(), "Cost") {
		t.Errorf("cost lines without pricing:\n%s", text.String())
	}
	var js bytes.Buffer
	if err := r.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_version", "sessions_scanned", "total_loops", "wasted_seconds", "wasted_tool_calls", "top_loops", "loops_by_detector"} {
		if _, ok := back[k]; !ok {
			t.Errorf("JSON report missing %q", k)
		}
	}
	if strings.Contains(js.String(), replaytest.Secret) || strings.Contains(text.String(), replaytest.Secret) {
		t.Error("secret in the report")
	}
	if r := NewReport(res, "/x", time.Time{}, ReportOptions{Top: -1}, time.Unix(0, 0), 0); len(r.TopLoops) != 3 {
		t.Errorf("top -1 lists %d loops", len(r.TopLoops))
	}
}

func TestLabelsAndEval(t *testing.T) {
	res := runSynthetic(t)
	path := filepath.Join(t.TempDir(), "sub", "labels.jsonl")
	if ls, err := ReadLabels(path); err != nil || ls != nil {
		t.Fatalf("missing file: %v %v", ls, err)
	}
	SortLoops(res.Loops)
	var rf, osc, sub *Loop
	for _, l := range res.Loops {
		switch {
		case l.AgentID != "":
			sub = l
		case l.Detector == event.DetectorRepeatedFailure:
			rf = l
		default:
			osc = l
		}
	}
	now := time.Unix(100, 0)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(AppendLabel(path, DetectedLabel(rf, VerdictReal, now)))
	must(AppendLabel(path, DetectedLabel(osc, VerdictFalsePositive, now)))
	// A loop the detectors missed: calls 1-3 of the subagent... is caught
	// by the subagent's D2 loop, which overlaps it.
	must(AppendLabel(path, Label{Kind: "missed", SessionID: replaytest.SessionID[:8], AgentID: replaytest.AgentID, FromCall: 1, ToCall: 3, Verdict: VerdictMissed}))
	// And one nothing overlaps.
	must(AppendLabel(path, Label{Kind: "missed", SessionID: replaytest.SessionID, FromCall: 100, ToCall: 120, Verdict: VerdictMissed}))
	labels, err := ReadLabels(path)
	must(err)
	if len(labels) != 4 {
		t.Fatalf("labels = %d", len(labels))
	}
	if _, ok := LabelFor(labels, rf); !ok {
		t.Error("labeled loop not found")
	}
	if _, ok := LabelFor(labels, sub); ok {
		t.Error("unlabeled loop matched a label")
	}

	ev := Evaluate(res.Loops, labels)
	if ev.RealLoops != 3 || ev.Caught != 2 || ev.Missed != 1 || ev.Unlabeled != 1 {
		t.Errorf("eval = %+v", ev)
	}
	got := map[string]DetectorMetrics{}
	for _, d := range ev.Detectors {
		got[d.Detector] = d
	}
	rfm, osm := got[event.DetectorRepeatedFailure], got[event.DetectorOscillation]
	if rfm.TP != 1 || rfm.FP != 0 || rfm.Precision != 1 || rfm.Caught != 2 || rfm.Unlabeled != 1 {
		t.Errorf("repeated_failure metrics = %+v", rfm)
	}
	// Delay: first signal at call 7, loop starts at call 1 (main); 4 - 1
	// for the subagent's missed loop.
	if rfm.MeanDelay != (6+3)/2.0 {
		t.Errorf("mean delay = %v", rfm.MeanDelay)
	}
	if osm.FP != 1 || osm.Precision != 0 {
		t.Errorf("oscillation metrics = %+v", osm)
	}
	var out bytes.Buffer
	if err := ev.WriteText(&out); err != nil || !strings.Contains(out.String(), "real loops 3") {
		t.Errorf("eval text: %v\n%s", err, out.String())
	}
}

// TestAnonymizeTextAllStyles runs the same anonymization on Windows-,
// macOS-, and Linux-style sessions, on every OS: the paths come from the
// transcripts, so where the test runs must not matter.
func TestAnonymizeTextAllStyles(t *testing.T) {
	styles := []struct {
		name, cwd, home, user, sep string
	}{
		{"windows", `C:\Users\Jane Q Doe\Desktop\acmeshop`, `C:\Users\Jane Q Doe`, "Jane Q Doe", `\`},
		{"macos", "/Users/janeqdoe/Desktop/acmeshop", "/Users/janeqdoe", "janeqdoe", "/"},
		{"linux", "/home/janeqdoe/src/acmeshop", "/home/janeqdoe", "janeqdoe", "/"},
	}
	for _, st := range styles {
		t.Run(st.name, func(t *testing.T) {
			x := &exporter{paths: map[string]string{}, cwds: map[string]bool{}, words: map[string]string{}}
			x.cwds[st.cwd] = true
			x.learn(st.cwd)
			x.fakePath("src"+st.sep+"pay.ts", st.cwd)
			fwd := strings.ReplaceAll(st.cwd, `\`, "/")
			enc := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(strings.ReplaceAll(st.home, "Desktop", ""), "-")
			tests := []struct{ in, want string }{
				{`cd "` + st.cwd + `" && npm test`, `cd "/repo" && npm test`},
				{"node " + fwd + "/src/pay.ts", "node /repo/src/file_1.ts"},
				{"cat " + st.home + st.sep + ".npmrc", "cat /home/user/p_"},
				{"ls ~/Desktop/acmeshop-api", "ls ~/Desktop/repo-api"},
				{`git config user.name "` + st.user + `"`, `git config user.name "user"`},
				{"cat ~/.claude/projects/" + enc + "-other/x.jsonl", "cat ~/.claude/projects/<project-dir>/x.jsonl"},
				{"mail jane.doe@acme.example", "mail <email>"},
			}
			for _, tt := range tests {
				if got := x.text(tt.in); !strings.HasPrefix(got, tt.want) {
					t.Errorf("text(%q)\n got %q\nwant prefix %q", tt.in, got, tt.want)
				}
			}
		})
	}
}

func TestAnonymizeText(t *testing.T) {
	x := &exporter{paths: map[string]string{}, cwds: map[string]bool{}, words: map[string]string{}}
	cwd := `C:\Users\Jane Q Doe\Desktop\acmeshop`
	x.cwds[cwd] = true
	x.learn(cwd)
	x.fakePath(`src\pay.ts`, cwd)
	tests := []struct{ in, want string }{
		{`cd "C:\Users\Jane Q Doe\Desktop\acmeshop" && npm test`, `cd "/repo" && npm test`},
		{`cd /c/Users/Jane\ Q\ Doe/Desktop/acmeshop`, `cd /c/Users/Jane\ Q\ Doe/Desktop/acmeshop`}, // shell-escaped spaces: see below
		{`node /c/Users/Jane Q Doe/Desktop/acmeshop/src/pay.ts`, `node /repo/src/file_1.ts`},
		{`cat C:/Users/Jane Q Doe/.npmrc`, `cat /home/user/p_`},
		{`ls ~/Desktop/acmeshop-api`, `ls ~/Desktop/repo-api`},
		{`git config user.name "Jane Q Doe"`, `git config user.name "user"`},
		{`curl https://internal.acme.example/api?id=7`, `curl <url:`},
		{`cat ~/.claude/projects/C--Users-Jane-Q-Doe-Desktop-other/x.jsonl`, `cat ~/.claude/projects/<project-dir>/x.jsonl`},
		{`mail jane.doe@acme.example`, `mail <email>`},
		{`type C:\Users\JANEQD~1\AppData\Local\Temp\out.log`, `type /home/user/p_`}, // 8.3 short name
	}
	for _, tt := range tests {
		got := x.text(tt.in)
		if tt.in == tests[1].in {
			// Escaped spaces are not a known spelling; the learned names
			// still remove the username and repo.
			for _, leak := range []string{"Jane", "Doe", "acmeshop"} {
				if strings.Contains(got, leak) {
					t.Errorf("text(%q) = %q leaks %q", tt.in, got, leak)
				}
			}
			continue
		}
		if !strings.HasPrefix(got, tt.want) {
			t.Errorf("text(%q)\n got %q\nwant prefix %q", tt.in, got, tt.want)
		}
	}

	if got := anonCommand(`git commit -m "fix the thing that broke checkout"`); got == `git commit -m "fix the thing that broke checkout"` ||
		!strings.HasPrefix(got, `git commit -m "q_`) {
		t.Errorf("anonCommand kept a commit message: %q", got)
	}
	if got := anonCommand("cat <<EOF > a.txt\nsecret plans\nEOF"); strings.Contains(got, "secret plans") || !strings.HasPrefix(got, "cat <<EOF > a.txt <body:") {
		t.Errorf("anonCommand kept a heredoc body: %q", got)
	}
	if a, b := anonCommand(`python -c "print('hello world from here')"`), anonCommand(`python -c "print('hello world from here')"`); a != b {
		t.Error("anonCommand is not deterministic")
	}
}

func TestExportRoundTrip(t *testing.T) {
	root := writeSynthetic(t)
	streams, _ := Discover(root, Filter{})
	var buf bytes.Buffer
	st, err := Export(streams[0], &buf, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	if st.ToolCalls != 14 || st.Prompts != 2 || st.Files != 3 {
		t.Errorf("export stats = %+v", st)
	}
	out := buf.String()
	for _, leak := range []string{
		replaytest.PromptText, replaytest.Secret, "cart.js", "flags.ts", "big.log", `Users\\dev`, "Users/dev",
		"export const cache", "log line without errors", "v1", replaytest.SessionID, "task-notification",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("export contains %q", leak)
		}
	}
	for _, keep := range []string{"npx jest", "Expected: 7", "Received: 8", "/repo/src/file_1.js", "/repo/src/file_3.ts", "1970-01-01T00:00:00.000Z"} {
		if !strings.Contains(out, keep) {
			t.Errorf("export lost %q", keep)
		}
	}

	// Replaying the export finds the same loops in the main stream.
	dir := filepath.Join(t.TempDir(), "anon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "anon-session.jsonl"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	again, _ := Discover(filepath.Dir(dir), Filter{})
	res := Run(again, Options{Thresholds: config.Default()})
	orig := Run(streams[:1], Options{Thresholds: config.Default()})
	if len(res.Loops) != len(orig.Loops) {
		t.Fatalf("export replays to %d loops, original %d", len(res.Loops), len(orig.Loops))
	}
	SortLoops(res.Loops)
	SortLoops(orig.Loops)
	for i := range res.Loops {
		a, b := res.Loops[i], orig.Loops[i]
		if a.Detector != b.Detector || a.Count != b.Count || a.FirstCall != b.FirstCall || a.LastCall != b.LastCall ||
			a.DurationSeconds != b.DurationSeconds || len(a.WouldHave) != len(b.WouldHave) {
			t.Errorf("loop %d differs after export:\n got %+v\nwant %+v", i, *a, *b)
		}
	}
}
