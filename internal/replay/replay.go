// Package replay reconstructs agent sessions from transcripts and runs them
// through the same detection pipeline as the daemon, in observe mode, to
// find the loops in past sessions (LLD §14).
package replay

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/pipeline"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
)

// Loop is one detected loop: one LoopKey within one human turn of one
// stream. Calls are numbered from 1 per stream, in completion order.
type Loop struct {
	SessionID       string    `json:"session_id"`
	AgentID         string    `json:"agent_id,omitempty"`
	Project         string    `json:"project"`
	Repo            string    `json:"repo"`
	Detector        string    `json:"detector"`
	LoopKey         string    `json:"loop_key"`
	Family          string    `json:"family,omitempty"`
	File            string    `json:"file,omitempty"`
	Count           int       `json:"count"`
	FirstCall       int       `json:"first_call"`
	LastCall        int       `json:"last_call"`
	FirstSignalCall int       `json:"first_signal_call"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	DurationSeconds float64   `json:"duration_seconds"`
	WastedToolCalls int       `json:"wasted_tool_calls"`
	MaxSeverity     float64   `json:"max_severity"`
	ErrorLines      []string  `json:"error_lines"`
	WouldHave       []Would   `json:"would_have"`
	// CostUSD is the API-equivalent cost of the model calls from the one
	// that issued the loop's first call to the one that issued its last
	// (with Options.Pricing; models without a price count 0).
	CostUSD *float64 `json:"cost_usd,omitempty"`

	seqs map[uint64]bool
	gen  int
}

// Would is one decision enforce mode would have made.
type Would struct {
	Action string `json:"action"` // nudge | deny | halt
	Call   int    `json:"call"`
	Reason string `json:"reason"`
}

// Stream returns "session" or "session/agent".
func (l *Loop) Stream() string {
	if l.AgentID == "" {
		return l.SessionID
	}
	return l.SessionID + "/" + l.AgentID
}

// Call is one completed tool call, for timelines.
type Call struct {
	N       int       `json:"call"`
	At      time.Time `json:"at"`
	Tool    string    `json:"tool"`
	Family  string    `json:"family,omitempty"`
	Command string    `json:"command,omitempty"` // normalized and redacted
	File    string    `json:"file,omitempty"`
	Failed  bool      `json:"failed"`
	Turn    int       `json:"turn"` // human turns since the stream's start (0: before the first prompt)
}

// Options configures a replay.
type Options struct {
	Thresholds config.Thresholds
	// Workers is the number of streams replayed in parallel (default:
	// GOMAXPROCS).
	Workers int
	// Timeline, if set, collects every completed call of every stream.
	Timeline bool
	// Pricing, if set, accounts for usage and API-equivalent cost (M4):
	// per stream, per loop, and in the time spent inside loops.
	Pricing *cost.Pricing
}

// Costs is the usage and API-equivalent cost of a replay (with
// Options.Pricing).
type Costs struct {
	Usage    cost.Usage `json:"usage"`
	CostUSD  float64    `json:"cost_usd"`       // priced messages only
	Unpriced cost.Usage `json:"unpriced_usage"` // models without a verified price
	// UnknownModels are the models without a verified price.
	UnknownModels []string `json:"unknown_models"`
	// WastedCostUSD is the cost of the model calls inside loops (the union
	// of the loops' spans, per stream).
	WastedCostUSD float64 `json:"wasted_cost_usd"`
	// Streams holds each stream's summary, by stream key.
	Streams map[string]*StreamCost `json:"-"`
}

// StreamCost is one stream's usage summary, with the issuing message of
// each call (by call number) for loop costs.
type StreamCost struct {
	Summary cost.Summary
	// CallMessage[n] is the index of the message that issued call n (-1:
	// unknown).
	CallMessage []int
	// WastedUSD is the cost of the model calls inside the stream's loops.
	WastedUSD float64
	// Project is the encoded project directory, for display.
	Project string
}

// Result is the outcome of replaying a set of streams.
type Result struct {
	Sessions          int
	Streams           int
	ToolCalls         int
	Lines             LineStats
	Loops             []*Loop // loops at or above the nudge threshold, by stream then first call
	BelowThreshold    int     // loops that only ever signaled below the nudge threshold
	SessionsWithLoops int
	WastedSeconds     float64 // union of loop spans per stream
	WastedToolCalls   int     // union of loop call ranges per stream
	Errors            []string
	Timelines         map[string][]Call // by stream key, with Options.Timeline
	Costs             *Costs            // with Options.Pricing

	// Visibility for tuning (detection-free).
	NearMisses   []NearMiss       // closest first
	SoftFailures []SoftFailure    // by stream, then call
	Families     []FamilyFailures // by failures, then runs, then name
	Streaks      []Streak         // longest same-family failure streak per stream, longest first
}

type streamResult struct {
	st       Stream
	stats    LineStats
	calls    int
	loops    []*Loop
	below    int
	wasted   time.Duration
	wastedN  int
	timeline []Call
	in       *insights
	err      error
	cost     *StreamCost
	wasteUSD float64
}

// Run replays streams (in parallel across streams; each stream in order).
func Run(streams []Stream, opts Options) *Result {
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	p := pipeline.New(opts.Thresholds)
	results := make([]streamResult, len(streams))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				results[i] = replayStream(p, streams[i], opts)
			}
		}()
	}
	for i := range streams {
		next <- i
	}
	close(next)
	wg.Wait()

	res := &Result{Streams: len(streams)}
	if opts.Timeline {
		res.Timelines = make(map[string][]Call)
	}
	if opts.Pricing != nil {
		res.Costs = &Costs{UnknownModels: []string{}, Streams: map[string]*StreamCost{}}
	}
	unknown := map[string]bool{}
	sessions := map[string]bool{}
	withLoops := map[string]bool{}
	for _, r := range results {
		if c := res.Costs; c != nil && r.cost != nil {
			sum := r.cost.Summary
			c.Usage = c.Usage.Add(sum.Usage)
			c.CostUSD += sum.CostUSD
			c.Unpriced = c.Unpriced.Add(sum.Unpriced)
			c.WastedCostUSD += r.wasteUSD
			c.Streams[r.st.Key()] = r.cost
			for _, m := range sum.Models {
				if !m.Priced && m.Model != cost.SyntheticModel && !unknown[m.Model] {
					unknown[m.Model] = true
					c.UnknownModels = append(c.UnknownModels, m.Model)
				}
			}
		}
		sessions[r.st.SessionID] = true
		res.Lines.add(r.stats)
		res.ToolCalls += r.calls
		res.Loops = append(res.Loops, r.loops...)
		res.BelowThreshold += r.below
		res.WastedSeconds += r.wasted.Seconds()
		res.WastedToolCalls += r.wastedN
		if len(r.loops) > 0 {
			withLoops[r.st.SessionID] = true
		}
		if r.err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", r.st.Path, r.err))
		}
		if opts.Timeline {
			res.Timelines[r.st.Key()] = r.timeline
		}
		res.mergeInsights(r.in)
	}
	res.Sessions = len(sessions)
	res.SessionsWithLoops = len(withLoops)
	if res.Costs != nil {
		sort.Strings(res.Costs.UnknownModels)
	}
	res.finishInsights()
	return res
}

func (res *Result) mergeInsights(in *insights) {
	if in == nil {
		return
	}
	res.NearMisses = append(res.NearMisses, in.near...)
	res.SoftFailures = append(res.SoftFailures, in.soft...)
	if in.best.Length > 0 {
		res.Streaks = append(res.Streaks, in.best)
	}
	for _, f := range in.fams {
		found := false
		for i := range res.Families {
			if res.Families[i].Family == f.Family {
				res.Families[i].Runs += f.Runs
				res.Families[i].Failures += f.Failures
				res.Families[i].Soft += f.Soft
				found = true
				break
			}
		}
		if !found {
			res.Families = append(res.Families, *f)
		}
	}
}

func (res *Result) finishInsights() {
	SortNearMisses(res.NearMisses)
	sort.SliceStable(res.Families, func(i, j int) bool {
		a, b := res.Families[i], res.Families[j]
		if a.Failures != b.Failures {
			return a.Failures > b.Failures
		}
		if a.Runs != b.Runs {
			return a.Runs > b.Runs
		}
		return a.Family < b.Family
	})
	sort.SliceStable(res.Streaks, func(i, j int) bool {
		a, b := res.Streaks[i], res.Streaks[j]
		if a.Length != b.Length {
			return a.Length > b.Length
		}
		return a.SessionID+a.AgentID < b.SessionID+b.AgentID
	})
}

func replayStream(p *pipeline.Pipeline, st Stream, opts Options) streamResult {
	out := streamResult{st: st}
	f, err := os.Open(st.Path)
	if err != nil {
		out.err = err
		return out
	}
	defer f.Close()

	s := p.NewSession(string(event.AgentClaudeCode)+":"+st.Key(), event.AgentClaudeCode)
	in := newInsights(opts.Thresholds, st)
	out.in = in
	nudgeAt := opts.Thresholds.Policy.NudgeThreshold
	var (
		times   = []time.Time{{}} // by seq (seq 0 unused)
		callOf  = []int{0}        // call number by seq
		done    = 0
		gen     = 0
		loops   = map[string]*Loop{}
		order   []*Loop
		sc      *cost.Scanner
		callMsg = []int{-1} // issuing message by call number (call 0 unused)
		lines   func([]byte)
	)
	if opts.Pricing != nil {
		sc = cost.NewScanner()
		lines = sc.Line
	}
	loopFor := func(key, detector string) *Loop {
		k := fmt.Sprintf("%d\x00%s", gen, key)
		l := loops[k]
		if l == nil {
			l = &Loop{SessionID: st.SessionID, AgentID: st.AgentID, Project: st.Project, Detector: detector, LoopKey: key, seqs: map[uint64]bool{}, gen: gen}
			loops[k] = l
			order = append(order, l)
		}
		return l
	}
	emit := func(ev *event.Event, hash uint64) {
		r := p.Handle(s, ev, hash)
		for uint64(len(times)) <= ev.Seq {
			times = append(times, time.Time{})
			callOf = append(callOf, 0)
		}
		times[ev.Seq] = ev.ReceivedAt
		switch ev.Phase {
		case event.PhasePre:
			callOf[ev.Seq] = done + 1
		case event.PhasePost, event.PhasePostFailure:
			done++
			callOf[ev.Seq] = done
			if r.Call != nil {
				in.call(r.Call, done)
			}
			if sc != nil {
				msg := -1
				if r.Call != nil {
					sc.Label(ev.ToolUseID, CostGroup(r.Call, ev.Cwd), done)
					msg = sc.IssuingMessage(ev.ToolUseID)
				}
				callMsg = append(callMsg, msg)
			}
			if opts.Timeline && r.Call != nil {
				out.timeline = append(out.timeline, Call{
					N: done, At: ev.ReceivedAt, Tool: ev.ToolName, Family: r.Call.Family,
					Command: clipText(r.Call.Command, 160), File: relPath(r.Call.FilePath, ev.Cwd), Failed: r.Call.Failed(), Turn: gen,
				})
			}
		case event.PhasePrompt:
			gen++
			callOf[ev.Seq] = done
			in.prompt()
			if sc != nil {
				sc.MarkTurn()
			}
		}
		for _, sig := range r.Signals {
			l := loopFor(sig.LoopKey, sig.Detector)
			if l.FirstSignalCall == 0 {
				l.FirstSignalCall = done
				l.Repo = ev.Cwd
			}
			for _, q := range sig.Evidence.Seqs {
				l.seqs[q] = true
			}
			if sig.Evidence.Family != "" {
				l.Family = sig.Evidence.Family
			}
			if sig.Evidence.Path != "" {
				l.File = relPath(sig.Evidence.Path, ev.Cwd)
			}
			if len(sig.Evidence.ErrorLines) > 0 {
				l.ErrorLines = redact.Lines(firstN(sig.Evidence.ErrorLines, 2))
			}
			if sig.Severity > l.MaxSeverity {
				l.MaxSeverity = sig.Severity
			}
		}
		for _, d := range r.Decisions {
			l := loopFor(d.LoopKey, d.Detector)
			l.WouldHave = append(l.WouldHave, Would{Action: string(d.WouldHave), Call: callOf[d.Seq], Reason: d.Reason})
		}
	}
	out.stats, out.err = ReadStreamLines(f, st, emit, lines)
	out.calls = done

	var spans []span
	for _, l := range order {
		if l.MaxSeverity < nudgeAt || len(l.seqs) == 0 {
			out.below++
			continue
		}
		lo, hi := minMax(l.seqs)
		l.Count = len(l.seqs)
		l.FirstCall, l.LastCall = callOf[lo], callOf[hi]
		l.StartedAt, l.EndedAt = times[lo], times[hi]
		l.DurationSeconds = l.EndedAt.Sub(l.StartedAt).Seconds()
		l.WastedToolCalls = l.LastCall - l.FirstCall + 1
		out.loops = append(out.loops, l)
		spans = append(spans, span{l.StartedAt, l.EndedAt, l.FirstCall, l.LastCall})
	}
	out.wasted, out.wastedN = union(spans)
	in.finish(out.loops)
	if sc != nil {
		sum := sc.Summarize(opts.Pricing)
		out.cost = &StreamCost{Summary: sum, CallMessage: callMsg, Project: st.Project}
		// The model calls inside loops: from the one that issued a loop's
		// first call to the one that issued its last; overlaps count once.
		inLoop := map[int]bool{}
		msgOf := func(n int) int {
			if n > 0 && n < len(callMsg) {
				return callMsg[n]
			}
			return -1
		}
		for _, l := range out.loops {
			from, to := msgOf(l.FirstCall), msgOf(l.LastCall)
			c := sum.CostBetween(from, to)
			l.CostUSD = &c
			for i := from; from >= 0 && i <= to; i++ {
				inLoop[i] = true
			}
		}
		for i := range inLoop {
			if i < len(sum.MessageCost) {
				out.wasteUSD += sum.MessageCost[i]
			}
		}
		out.cost.WastedUSD = out.wasteUSD
	}
	return out
}

// CostGroup is how cost estimates group a call: a shell command by its
// family ("bash: npm test"), a file tool by the file ("read src/a.go",
// relative to the working directory, redacted), anything else by its tool.
func CostGroup(c *event.ToolCall, cwd string) string {
	tool := c.Event.ToolName
	switch tool {
	case event.ToolBash:
		if c.Family != "" {
			return "bash: " + c.Family
		}
	case event.ToolRead, event.ToolEdit, event.ToolWrite, event.ToolPatch:
		if c.FilePath != "" {
			return tool + " " + redact.String(relPath(c.FilePath, cwd))
		}
	}
	return tool
}

type span struct {
	start, end  time.Time
	first, last int
}

// union returns the total time and the number of calls covered by spans,
// counting overlaps once.
func union(spans []span) (time.Duration, int) {
	if len(spans) == 0 {
		return 0, 0
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].first < spans[j].first })
	var total time.Duration
	calls := 0
	cur := spans[0]
	flush := func() {
		total += cur.end.Sub(cur.start)
		calls += cur.last - cur.first + 1
	}
	for _, s := range spans[1:] {
		if s.first <= cur.last {
			if s.last > cur.last {
				cur.last = s.last
			}
			if s.end.After(cur.end) {
				cur.end = s.end
			}
			if s.start.Before(cur.start) {
				cur.start = s.start
			}
			continue
		}
		flush()
		cur = s
	}
	flush()
	return total, calls
}

func minMax(set map[uint64]bool) (uint64, uint64) {
	var lo, hi uint64
	first := true
	for s := range set {
		if first || s < lo {
			lo = s
		}
		if first || s > hi {
			hi = s
		}
		first = false
	}
	return lo, hi
}

func firstN(xs []string, n int) []string {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// relPath shows a normalized path relative to cwd when it is inside it.
func relPath(p, cwd string) string {
	if p == "" || cwd == "" {
		return p
	}
	base := strings.ToLower(strings.TrimSuffix(strings.ReplaceAll(cwd, `\`, "/"), "/"))
	if strings.HasPrefix(strings.ToLower(p), base+"/") {
		return p[len(base)+1:]
	}
	return p
}

// RepoName shortens a working directory for display: its last two path
// elements.
func RepoName(cwd string) string {
	parts := strings.FieldsFunc(cwd, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

// SortLoops orders loops by wasted time, then count, then stream and call.
func SortLoops(loops []*Loop) {
	sort.SliceStable(loops, func(i, j int) bool {
		a, b := loops[i], loops[j]
		if a.DurationSeconds != b.DurationSeconds {
			return a.DurationSeconds > b.DurationSeconds
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Stream() != b.Stream() {
			return a.Stream() < b.Stream()
		}
		return a.FirstCall < b.FirstCall
	})
}
