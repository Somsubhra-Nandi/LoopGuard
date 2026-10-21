package replay

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/detect"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
)

// Visibility for tuning: near misses (stretches that came close to a
// detector but didn't fire), soft failures, and the shape of failures.
// Nothing here changes detection.

// NearMiss is a stretch of calls that came close to a detector.
type NearMiss struct {
	SessionID string   `json:"session_id"`
	AgentID   string   `json:"agent_id,omitempty"`
	Repo      string   `json:"repo"`
	Detector  string   `json:"detector"`
	FromCall  int      `json:"from_call"`
	ToCall    int      `json:"to_call"`
	Count     int      `json:"count"`
	Threshold int      `json:"threshold"`
	Closeness float64  `json:"closeness"` // 0..1, how close it got; ranks near misses
	Reason    string   `json:"reason"`    // why it didn't fire
	Family    string   `json:"family,omitempty"`
	File      string   `json:"file,omitempty"`
	Evidence  []string `json:"evidence"` // normalized, redacted
}

// Stream returns "session" or "session/agent".
func (n *NearMiss) Stream() string {
	if n.AgentID == "" {
		return n.SessionID
	}
	return n.SessionID + "/" + n.AgentID
}

// SoftFailure is a call that exited 0 but whose output has a failure
// marker, so the pipeline counted it as failed.
type SoftFailure struct {
	SessionID string   `json:"session_id"`
	AgentID   string   `json:"agent_id,omitempty"`
	Call      int      `json:"call"`
	Family    string   `json:"family"`
	Command   string   `json:"command"`
	Markers   []string `json:"markers"`
	Line      string   `json:"line"` // the line with the first marker, normalized and redacted
}

// FamilyFailures counts runs and failures of one command family.
type FamilyFailures struct {
	Family   string `json:"family"`
	Runs     int    `json:"runs"`
	Failures int    `json:"failures"`
	Soft     int    `json:"soft_failures"`
	Polling  bool   `json:"polling"`
}

// Streak is the longest run of failures of one family in a stream (any
// error, no success of that family in between).
type Streak struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id,omitempty"`
	Family    string `json:"family"`
	Length    int    `json:"length"`
	FromCall  int    `json:"from_call"`
	ToCall    int    `json:"to_call"`
	Errors    int    `json:"distinct_errors"`
}

// insights collects near misses, soft failures, and failure stats for one
// stream.
type insights struct {
	th   config.Thresholds
	errM *normalize.FamilyMatcher // families whose "Error:" lines are soft failures
	st   Stream
	cwd  string
	near []NearMiss
	soft []SoftFailure
	fams map[string]*FamilyFailures

	// Longest streak per stream.
	streaks map[string]*streakState
	best    Streak

	// Per-turn state for near misses.
	rf     map[string]*rfRun    // family → current run (no success of the family in it)
	er     map[uint64]*erGroup  // ActionFP → repeats since the last write
	files  map[string]*fileTurn // path → writes this turn
	stall  stallRun
	stallB stallRun // longest this turn
}

type streakState struct {
	from, to, n int
	sigs        []*event.ErrorSignature
}

type rfCluster struct {
	anchor *event.ErrorSignature
	calls  []int
	lines  []string
	cmds   []string
}

type rfRun struct {
	clusters []*rfCluster
	polling  bool
	progress []progAt // family progress, by call
}

type progAt struct {
	call int
	p    float64
}

type erGroup struct {
	calls   []int
	results map[uint64]int
	tool    string
	command string
	file    string
	polling bool
	maxSame int
}

type fileTurn struct {
	calls    []int
	hashes   []uint64
	edits    []event.EditPair
	revisits int
	inverses int
}

type stallRun struct {
	from, to, n, bad int
}

func newInsights(th config.Thresholds, st Stream) *insights {
	in := &insights{th: th, errM: normalize.NewFamilyMatcher(th.ErrorMarkerFamilies), st: st, fams: map[string]*FamilyFailures{}, streaks: map[string]*streakState{}}
	in.resetTurn()
	return in
}

func (in *insights) resetTurn() {
	in.rf = map[string]*rfRun{}
	in.er = map[uint64]*erGroup{}
	in.files = map[string]*fileTurn{}
	in.stall, in.stallB = stallRun{}, stallRun{}
}

// prompt ends a turn: windows restart, so near misses are cut here.
func (in *insights) prompt() {
	in.flushTurn()
	in.resetTurn()
}

// call records one completed call, numbered n in its stream.
func (in *insights) call(c *event.ToolCall, n int) {
	ev := c.Event
	if ev.Cwd != "" {
		in.cwd = ev.Cwd
	}
	in.familyStats(c, n)
	if ev.ToolName == event.ToolBash && !ev.IsError && ev.Phase != event.PhasePostFailure && c.Failed() {
		in.softFailure(c, n)
	}
	in.trackRF(c, n)
	in.trackER(c, n)
	in.trackFile(c, n)
	in.trackStall(c, n)
}

func (in *insights) familyStats(c *event.ToolCall, n int) {
	if c.Family == "" {
		return
	}
	f := in.fams[c.Family]
	if f == nil {
		f = &FamilyFailures{Family: c.Family, Polling: c.Polling}
		in.fams[c.Family] = f
	}
	f.Runs++
	s := in.streaks[c.Family]
	if !c.Failed() {
		delete(in.streaks, c.Family)
		return
	}
	f.Failures++
	if c.Event.ToolName == event.ToolBash && !c.Event.IsError {
		f.Soft++
	}
	if s == nil {
		s = &streakState{from: n}
		in.streaks[c.Family] = s
	}
	s.n++
	s.to = n
	known := false
	for _, k := range s.sigs {
		if normalize.SameError(k, c.ErrorSig, in.th.Detectors.SimHashMaxDistance) {
			known = true
			break
		}
	}
	if !known {
		s.sigs = append(s.sigs, c.ErrorSig)
	}
	if s.n > in.best.Length {
		in.best = Streak{SessionID: in.st.SessionID, AgentID: in.st.AgentID, Family: c.Family,
			Length: s.n, FromCall: s.from, ToCall: s.to, Errors: len(s.sigs)}
	}
}

func (in *insights) softFailure(c *event.ToolCall, n int) {
	out := redact.String(normalize.OutputText(c.Event.Output))
	markers, line := normalize.SoftFailureMarkers(out, in.errM.Matches(c.Family))
	in.soft = append(in.soft, SoftFailure{
		SessionID: in.st.SessionID, AgentID: in.st.AgentID, Call: n, Family: c.Family,
		Command: clipText(c.Command, 160), Markers: markers, Line: clipText(normalize.Text(line), 200),
	})
}

// trackRF follows same-error failure clusters per family until the family
// succeeds (D2's "no success in between").
func (in *insights) trackRF(c *event.ToolCall, n int) {
	if c.Family == "" {
		return
	}
	run := in.rf[c.Family]
	if !c.Failed() {
		if run != nil {
			in.flushRF(c.Family, run, "ended by a success of the family")
			delete(in.rf, c.Family)
		}
		return
	}
	if run == nil {
		run = &rfRun{polling: c.Polling}
		in.rf[c.Family] = run
	}
	run.progress = append(run.progress, progAt{n, c.FamilyProgress})
	var cl *rfCluster
	for _, k := range run.clusters {
		if normalize.SameError(k.anchor, c.ErrorSig, in.th.Detectors.SimHashMaxDistance) {
			cl = k
			break
		}
	}
	if cl == nil {
		cl = &rfCluster{anchor: c.ErrorSig, lines: redact.Lines(firstN(c.ErrorSig.Lines, 2))}
		run.clusters = append(run.clusters, cl)
	}
	cl.calls = append(cl.calls, n)
	if c.Command != "" {
		cl.cmds = appendUniq(cl.cmds, clipText(c.Command, 120))
	}
}

func (in *insights) flushRF(family string, run *rfRun, end string) {
	d := in.th.Detectors
	min := d.RepeatedFailure.MinCount
	for _, cl := range run.clusters {
		count := len(cl.calls)
		if count < 2 {
			continue
		}
		from, to := cl.calls[0], cl.calls[count-1]
		nm := NearMiss{Detector: event.DetectorRepeatedFailure, FromCall: from, ToCall: to, Count: count,
			Threshold: min, Family: family, Evidence: append(append([]string{}, cl.lines...), cl.cmds...)}
		progress := 0.0
		for _, p := range run.progress {
			if p.call > from && p.call <= to {
				progress += p.p
			}
		}
		switch {
		case run.polling:
			nm.Closeness = 0.5 * ratio01(count, min)
			nm.Reason = fmt.Sprintf("%d same-error failures of `%s`, but it is a polling command (exempt)", count, family)
		case count < min:
			nm.Closeness = ratio01(count, min)
			nm.Reason = fmt.Sprintf("%d same-error failures of `%s`, threshold is %d (%s)", count, family, min, end)
		case progress >= d.RepeatedFailure.MaxProgress:
			nm.Closeness = 0.95
			nm.Reason = fmt.Sprintf("%d same-error failures of `%s`, but the family made %.1f progress in between (new errors or fewer failing tests; limit %.1f)",
				count, family, progress, d.RepeatedFailure.MaxProgress)
		case to-from+1 > d.Window:
			nm.Closeness = 0.9
			nm.Reason = fmt.Sprintf("%d same-error failures of `%s`, spread over %d calls (window is %d)", count, family, to-from+1, d.Window)
		default:
			nm.Closeness = 0.9
			nm.Reason = fmt.Sprintf("%d same-error failures of `%s`; did not fire (check the policy or detector window)", count, family)
		}
		in.add(nm)
	}
}

// trackER follows repeats of one action since the last successful write
// (D1's window).
func (in *insights) trackER(c *event.ToolCall, n int) {
	switch c.Event.ToolName {
	case event.ToolEdit, event.ToolWrite, event.ToolPatch:
		if !c.Failed() {
			in.flushER()
			in.er = map[uint64]*erGroup{}
		}
		return
	}
	g := in.er[c.ActionFP]
	if g == nil {
		g = &erGroup{results: map[uint64]int{}, tool: c.Event.ToolName, command: clipText(c.Command, 160), file: c.FilePath, polling: c.Polling}
		in.er[c.ActionFP] = g
	}
	g.calls = append(g.calls, n)
	g.results[c.ResultFP]++
	if same := g.results[c.ResultFP]; same > g.maxSame {
		g.maxSame = same
	}
}

func (in *insights) flushER() {
	min := in.th.Detectors.ExactRepeat.MinCount
	for _, g := range in.er {
		total := len(g.calls)
		if total < 2 {
			continue
		}
		subject := g.command
		if subject == "" {
			subject = g.tool
			if g.file != "" {
				subject += " " + relPath(g.file, in.cwd)
			}
		}
		nm := NearMiss{Detector: event.DetectorExactRepeat, FromCall: g.calls[0], ToCall: g.calls[total-1],
			Count: g.maxSame, Threshold: min, Evidence: []string{subject}}
		if g.tool == event.ToolBash {
			nm.Family = strings.TrimSpace(subject)
		}
		switch {
		case g.polling:
			nm.Closeness = 0.5 * ratio01(g.maxSame, min)
			nm.Reason = fmt.Sprintf("same call %d×, %d with identical output, but it is a polling command (exempt)", total, g.maxSame)
		case g.tool == event.ToolRead || g.tool == event.ToolGrep || g.tool == event.ToolGlob:
			// Reads and searches are D4's: a near miss of it, dropped below
			// when D4 fired on these calls.
			need := in.th.Detectors.ReadThrash.MinReads
			if g.tool != event.ToolRead {
				need = max(2, need-1)
			}
			nm.Detector, nm.Threshold = event.DetectorReadThrash, need
			nm.Closeness = 0.6 * ratio01(g.maxSame, need)
			nm.Reason = fmt.Sprintf("same %s %d×, %d with identical output (read thrash needs %d with no write to the file, and little progress in the last 15 calls)",
				g.tool, total, g.maxSame, need)
		case g.maxSame >= min:
			continue // fired (or deduped into D2)
		default:
			nm.Closeness = ratio01(g.maxSame, min)
			nm.Reason = fmt.Sprintf("same call %d× with no write in between, %d with identical output (%d distinct outputs), threshold is %d identical",
				total, g.maxSame, len(g.results), min)
		}
		in.add(nm)
	}
}

// trackFile follows writes to each file in the turn (D3).
func (in *insights) trackFile(c *event.ToolCall, n int) {
	switch c.Event.ToolName {
	case event.ToolEdit, event.ToolWrite, event.ToolPatch:
	default:
		return
	}
	if c.Failed() || c.FilePath == "" {
		return
	}
	f := in.files[c.FilePath]
	if f == nil {
		f = &fileTurn{}
		in.files[c.FilePath] = f
	}
	f.calls = append(f.calls, n)
	for _, e := range c.Edits {
		for _, old := range f.edits {
			if old.Old == e.New && old.New == e.Old {
				f.inverses++
				break
			}
		}
		f.edits = append(f.edits, e)
	}
	if h := c.FileHashAfter; h != 0 {
		k := len(f.hashes)
		if k == 0 || f.hashes[k-1] != h {
			for i := k - 2; i >= 0; i-- {
				if f.hashes[i] == h {
					f.revisits++
					break
				}
			}
		}
		f.hashes = append(f.hashes, h)
	}
}

func (in *insights) flushFiles() {
	min := in.th.Detectors.Oscillation.MinRevisits
	for path, f := range in.files {
		writes := len(f.calls)
		if writes < 3 {
			continue
		}
		best := max(f.revisits, f.inverses)
		if best >= min {
			continue // fired
		}
		nm := NearMiss{Detector: event.DetectorOscillation, FromCall: f.calls[0], ToCall: f.calls[writes-1],
			Count: best, Threshold: min, File: relPath(path, in.cwd)}
		known := ""
		if len(f.hashes) < writes {
			known = fmt.Sprintf("; content unknown after %d edits (replay has no file hashes for Edit)", writes-len(f.hashes))
		}
		if best == 0 {
			nm.Closeness = 0.4 * ratio01(writes, 10)
			nm.Reason = fmt.Sprintf("same file written %d times, no revisit or inverse edit%s", writes, known)
		} else {
			nm.Closeness = ratio01(best, min)
			nm.Reason = fmt.Sprintf("same file written %d times, %d revisits and %d inverse edits, threshold is %d%s",
				writes, f.revisits, f.inverses, min, known)
		}
		nm.Evidence = []string{nm.File}
		in.add(nm)
	}
}

// trackStall follows the longest stretch of calls with no progress (D5),
// where looking at something new (ToolCall.Explore) is progress too.
func (in *insights) trackStall(c *event.ToolCall, n int) {
	if c.Progress > 0 || c.Explore {
		in.stall = stallRun{}
		return
	}
	if in.stall.n == 0 {
		in.stall.from = n
	}
	in.stall.n++
	in.stall.to = n
	if !c.Polling && (c.Failed() || detect.Looks(c)) {
		in.stall.bad++ // a failure or a repeated read, fetch, or search
	}
	if in.stall.n > in.stallB.n {
		in.stallB = in.stall
	}
}

func (in *insights) flushStall() {
	s := in.stallB
	window := in.th.Detectors.Stall.Window
	if s.n < 8 {
		return
	}
	in.add(NearMiss{
		Detector: event.DetectorStall, FromCall: s.from, ToCall: s.to, Count: s.n, Threshold: window,
		Closeness: 0.9 * ratio01(s.n, window),
		Reason: fmt.Sprintf("%d calls in a row with no progress, %d of them failures or repeated reads "+
			"(a stall needs %d calls with ≥8 failures or repeated reads; new reads, fetches, and searches count as progress)",
			s.n, s.bad, window),
		Evidence: []string{},
	})
}

func (in *insights) flushTurn() {
	for fam, run := range in.rf {
		in.flushRF(fam, run, "the turn ended")
	}
	in.flushER()
	in.flushFiles()
	in.flushStall()
}

func (in *insights) add(nm NearMiss) {
	nm.SessionID, nm.AgentID, nm.Repo = in.st.SessionID, in.st.AgentID, in.cwd
	if nm.Evidence == nil {
		nm.Evidence = []string{}
	}
	in.near = append(in.near, nm)
}

// finish flushes the last turn and drops near misses that overlap a loop
// the same detector did report.
func (in *insights) finish(loops []*Loop) {
	in.flushTurn()
	kept := in.near[:0]
	for _, nm := range in.near {
		hit := false
		for _, l := range loops {
			if l.Detector == nm.Detector && overlaps(nm.FromCall, nm.ToCall, l.FirstCall, l.LastCall) {
				hit = true
				break
			}
		}
		if !hit {
			kept = append(kept, nm)
		}
	}
	in.near = kept
}

func ratio01(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	r := float64(n) / float64(d)
	if r > 0.99 {
		r = 0.99
	}
	return r
}

func appendUniq(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// SortNearMisses orders near misses by closeness, then count, then stream
// and call.
func SortNearMisses(ns []NearMiss) {
	sort.SliceStable(ns, func(i, j int) bool {
		a, b := ns[i], ns[j]
		if a.Closeness != b.Closeness {
			return a.Closeness > b.Closeness
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Stream() != b.Stream() {
			return a.Stream() < b.Stream()
		}
		return a.FromCall < b.FromCall
	})
}

// MarkerCounts counts soft failures per marker (a call with two markers
// counts for both).
func MarkerCounts(soft []SoftFailure) map[string]int {
	m := map[string]int{}
	for _, s := range soft {
		for _, k := range s.Markers {
			m[k]++
		}
	}
	return m
}
