package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/cespare/xxhash/v2"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
	"github.com/Somsubhra-Nandi/loopguard/internal/notify"
	"github.com/Somsubhra-Nandi/loopguard/internal/pipeline"
	"github.com/Somsubhra-Nandi/loopguard/internal/policy"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// Actor limits (LLD §9.2).
const (
	actorQueue      = 256
	DefaultIdle     = 2 * time.Hour
	maxFileHashSize = 2 << 20 // LLD §4.5
	maxDigest       = 300     // calls.input_digest
	maxRecent       = 50      // signals and interventions kept per live session
)

// reply is what the HTTP handler sends back to the shim.
type reply struct {
	stdout []byte
	exit   int
}

type job struct {
	ev    *event.Event
	ad    adapter.Adapter
	reply chan reply // buffered (1): the actor never blocks on a reply
	// newTurn (no event): the parent session got a human prompt, so this
	// subagent stream starts a new turn too.
	newTurn bool
}

// Manager routes events to one actor per session. Each actor processes
// its session's events strictly in order on its own goroutine, so the
// detectors' state needs no locks, and sessions don't wait on each other.
type Manager struct {
	cfg      *config.Loader
	pauses   *pauses
	stops    stops
	book     *usageBook // usage, tool calls, and wall time for the budgets (D6)
	tails    tails
	repos    repoNames // one spelling per repo
	notifier *notify.Dispatcher
	st       *store.Store  // nil: memory-only
	w        *store.Writer // nil: memory-only
	log      *slog.Logger
	met      *metrics
	idle     time.Duration
	clock    func() time.Time

	pipesMu sync.Mutex
	pipes   map[uint64]*pipeline.Pipeline // by config.Effective.Key

	mu     sync.Mutex
	actors map[string]*actor
	// retired holds, per session key, the done channel of the evicted
	// actor, so its successor starts only after the old one's rows are in.
	retired map[string]chan struct{}
	closed  bool
	wg      sync.WaitGroup
}

func newManager(cfg *config.Loader, st *store.Store, w *store.Writer, log *slog.Logger, met *metrics, idle time.Duration) *Manager {
	if idle <= 0 {
		idle = DefaultIdle
	}
	m := &Manager{cfg: cfg, pauses: &pauses{}, notifier: notify.NewDispatcher(nil, log), st: st, w: w, log: log, met: met, idle: idle, clock: time.Now,
		pipes: map[uint64]*pipeline.Pipeline{}, actors: map[string]*actor{}, retired: map[string]chan struct{}{}}
	m.book = newUsageBook(st, cost.NewPricingLoader(filepath.Dir(cfg.GlobalPath())), log)
	m.tails.m = m
	m.stops.st, m.stops.log = st, log
	return m
}

// pipeFor returns the pipeline for a configuration. Pipelines hold no
// session state, so sessions with the same thresholds share one; a config
// change makes a new one.
func (m *Manager) pipeFor(eff config.Effective) *pipeline.Pipeline {
	m.pipesMu.Lock()
	defer m.pipesMu.Unlock()
	p := m.pipes[eff.Key]
	if p == nil {
		if len(m.pipes) >= 64 {
			m.pipes = map[uint64]*pipeline.Pipeline{} // many edits over a long run
		}
		p = pipeline.New(eff.Thresholds)
		m.pipes[eff.Key] = p
	}
	return p
}

// sessionKey is "agent:session_id", or "agent:session_id/agent_id" for a
// subagent: subagent calls interleave with the main agent's, and mixing
// them would confuse the detectors (replay keeps them apart the same way).
func sessionKey(ev *event.Event) string {
	k := string(ev.Agent) + ":" + ev.SessionID
	if ev.AgentID != "" {
		k += "/" + ev.AgentID
	}
	return k
}

// Submit queues ev on its session's actor and returns the channel the
// reply arrives on. It never blocks: if the actor's queue is full or the
// manager is closed it returns nil, and the caller answers "none".
//
// A human prompt to a main session also starts a new turn in its live
// subagent streams, so a loop there doesn't stay blocked into the next
// turn.
func (m *Manager) Submit(ev *event.Event, ad adapter.Adapter) <-chan reply {
	j := job{ev: ev, ad: ad, reply: make(chan reply, 1)}
	key := sessionKey(ev)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	a := m.actors[key]
	if a == nil {
		a = m.newActor(key, ev)
		a.prev = m.retired[key]
		delete(m.retired, key)
		m.actors[key] = a
		m.wg.Add(1)
		go a.run()
	}
	a.touch(m.clock())
	if ev.Phase == event.PhasePrompt && ev.AgentID == "" {
		for k, sub := range m.actors {
			if strings.HasPrefix(k, key+"/") {
				select {
				case sub.in <- job{newTurn: true}:
				default:
				}
			}
		}
	}
	select {
	case a.in <- j:
		return j.reply
	default:
		m.met.error("queue_full")
		return nil
	}
}

// Active returns the number of live actors.
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.actors)
}

// Live returns the summaries of the live sessions.
func (m *Manager) Live() map[string]liveSummary {
	m.mu.Lock()
	actors := make([]*actor, 0, len(m.actors))
	for _, a := range m.actors {
		actors = append(actors, a)
	}
	m.mu.Unlock()
	out := make(map[string]liveSummary, len(actors))
	for _, a := range actors {
		s := a.summary()
		out[s.Key] = s
	}
	return out
}

// evictIdle stops actors idle for longer than the idle timeout. Their
// state is already persisted row by row; the next event for the session
// starts a new actor that continues the numbering and the counters.
func (m *Manager) evictIdle() {
	now := m.clock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, a := range m.actors {
		if now.Sub(a.lastActive()) > m.idle && len(a.in) == 0 {
			close(a.in)
			delete(m.actors, key)
			m.retired[key] = a.done
			m.log.Debug("session evicted", "session", key)
		}
	}
}

// Close stops all actors after they finish their queued events, waiting
// at most until ctx is done.
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		for key, a := range m.actors {
			close(a.in)
			delete(m.actors, key)
		}
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// liveSummary is what the API shows for a live session in memory-only
// mode, and the recent interventions (real and would-have) in any mode.
type liveSummary struct {
	store.Session
	Mode          string               `json:"mode,omitempty"`     // the level at the latest event: off | observe | notify | assist | guard| enforce | off, at the latest event
	ModeWhy       string               `json:"mode_why,omitempty"` // why, in words
	Signals       []store.Signal       `json:"signals,omitempty"`
	Interventions []store.Intervention `json:"interventions,omitempty"`
}

// actor owns one session's state. Only its goroutine touches state and the
// fields below it; the summary is shared with the API under mu.
type actor struct {
	m        *Manager
	key      string
	in       chan job
	done     chan struct{}   // closed when run returns, after the actor's last rows are queued
	prev     <-chan struct{} // the evicted predecessor's done, or nil
	last     atomic.Int64    // unix nanos of the last submitted event
	state    *session.SessionState
	outcomes *policy.Outcomes

	mu  sync.Mutex
	sum liveSummary

	loops map[string]bool // loop keys seen, for loops_detected
	repos map[string]string

	// interactive: the latest event came from a terminal session a human
	// attends (terminal notifications).
	interactive bool
	// announced holds the loops notified as detected (or stalled) in the
	// turn that started at announcedTurn.
	announced     map[string]bool
	announcedTurn uint64
	// budgetTurn holds, per budget period, TurnSeq+1 of the turn whose hard
	// budget halt was recorded.
	budgetTurn map[string]uint64
}

func (m *Manager) newActor(key string, ev *event.Event) *actor {
	a := &actor{
		m:          m,
		key:        key,
		in:         make(chan job, actorQueue),
		done:       make(chan struct{}),
		loops:      map[string]bool{},
		repos:      map[string]string{},
		announced:  map[string]bool{},
		budgetTurn: map[string]uint64{},
	}
	a.sum.Key, a.sum.Agent, a.sum.SessionID = key, string(ev.Agent), ev.SessionID
	return a
}

func (a *actor) touch(t time.Time) { a.last.Store(t.UnixNano()) }
func (a *actor) lastActive() time.Time {
	return time.Unix(0, a.last.Load())
}

func (a *actor) summary() liveSummary {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sum
	s.Signals = append([]store.Signal(nil), s.Signals...)
	s.Interventions = append([]store.Intervention(nil), s.Interventions...)
	return s
}

func (a *actor) run() {
	defer a.m.wg.Done()
	defer close(a.done)
	a.resume()
	for j := range a.in {
		a.handle(j)
	}
	// Evicted or shutting down: the state that would decide the pending
	// outcomes is going away.
	a.mu.Lock()
	a.resolve(a.outcomes.End())
	a.mu.Unlock()
}

// newState starts empty session state (and outcome tracking) for the
// actor's session.
func (a *actor) newState() {
	eff := a.m.cfg.For("")
	a.state = a.m.pipeFor(eff).NewSession(a.key, event.AgentKind(a.sum.Agent))
	a.outcomes = policy.NewOutcomes(eff.Thresholds.Detectors.SimHashMaxDistance)
}

// resume starts the session state, continuing a session the store already
// knows (an evicted actor, or an earlier daemon run): the seq numbering
// continues, and so do the counters. Detector state starts empty.
func (a *actor) resume() {
	a.newState()
	if a.m.st == nil {
		return
	}
	// The numbering continues after the stored rows, so an evicted
	// predecessor must have queued its last rows, and they must be written,
	// before MaxSeq is read; otherwise new calls would reuse its seqs and be
	// dropped as duplicates. After hours idle it has long finished; the wait
	// matters when an event arrives right after the eviction.
	if a.prev != nil {
		select {
		case <-a.prev:
		case <-time.After(2 * time.Second):
			a.m.log.Warn("resuming session: the evicted actor is still running", "session", a.key)
		}
		a.m.w.Flush()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if seq, err := a.m.st.MaxSeq(ctx, a.key); err == nil {
		a.state.ResumeAt(seq)
	} else {
		a.m.log.Warn("resuming session: reading seq", "session", a.key, "err", err)
	}
	if prev, err := a.m.st.GetSession(ctx, a.key); err == nil && prev.Key == a.key {
		a.mu.Lock()
		a.sum.Session = prev
		a.mu.Unlock()
	}
}

// handle processes one event and replies. A panic anywhere in it is
// recovered: the reply is "none", and the session state starts over (LLD
// §9.2), keeping the numbering.
func (a *actor) handle(j job) {
	if j.newTurn {
		a.state.NewTurn()
		a.mu.Lock()
		a.resolve(a.outcomes.End())
		a.mu.Unlock()
		return
	}
	replied := false
	defer func() {
		if r := recover(); r != nil {
			a.m.met.error("panic")
			a.m.log.Error("actor panic; session state reset", "session", a.key, "panic", r, "stack", string(debug.Stack()))
			if !replied {
				j.reply <- reply{}
			}
			seq := a.state.Seq()
			pending := a.outcomes.End()
			a.newState()
			a.state.ResumeAt(seq)
			a.mu.Lock()
			a.resolve(pending)
			a.mu.Unlock()
		}
	}()

	ev := j.ev
	repo := a.repoOf(ev.Cwd)
	eff := a.m.cfg.For(repo)
	mode := a.m.sessionMode(eff, ev, repo, ev.ReceivedAt)
	if mode.Mode == config.ModeOff {
		// The kill switch (enabled: false): nothing is processed or stored.
		j.reply <- reply{}
		replied = true
		a.mu.Lock()
		a.sum.Mode, a.sum.ModeWhy = mode.Mode, mode.Why
		a.mu.Unlock()
		return
	}

	a.state.Allow = allowFor(mode.Mode)
	pipe := a.m.pipeFor(eff)
	res := pipe.Handle(a.state, ev, a.fileHash(ev))
	a.m.book.event(ev.SessionID, a.key, repo, ev.ReceivedAt, res.Call != nil)
	a.m.tails.register(ev.SessionID, string(ev.Agent), ev.Transcript, repo, ev.ReceivedAt, ev.Phase == event.PhaseSessionEnd && ev.AgentID == "")
	if d, ok := a.stop(pipe, ev, mode); ok {
		res.Decisions = append(res.Decisions, d)
	}
	budgetDs, budgetNotes := a.budget(pipe, ev, eff, mode, repo)
	res.Decisions = append(res.Decisions, budgetDs...)
	d := capAction(pick(res.Decisions), a.state.Allow) // the policy already caps; belt and braces
	a.interactive = ev.Interactive
	notes := a.notifications(ev, res, mode, eff.Thresholds.Policy.NudgeThreshold)
	notes = append(notes, budgetNotes...)
	for _, n := range notes {
		if d.Terminal = a.m.notifier.Terminal(eff.Notify, n, a.interactive); d.Terminal != "" {
			break
		}
	}
	out, code := j.ad.Render(ev, d)
	j.reply <- reply{stdout: out, exit: code}
	replied = true
	for _, n := range notes {
		a.m.notifier.Dispatch(eff.Notify, n)
	}

	if len(res.Decisions) == 0 {
		a.m.met.decision(string(event.ActionNone))
	}
	for _, d := range res.Decisions {
		a.m.met.decision(string(d.WouldHave))
	}
	resolved := a.outcomes.Observe(a.state, res.Call, res.Signals, res.Decisions)
	if ev.Phase == event.PhasePrompt || ev.Phase == event.PhaseSessionEnd {
		resolved = append(resolved, a.outcomes.End()...)
	}
	resolved = append(resolved, a.outcomes.Add(a.state, res.Decisions)...)
	a.record(ev, res, mode, eff.Thresholds.Policy.NudgeThreshold, resolved)
}

var actionRank = map[event.Action]int{event.ActionNone: 0, event.ActionNudge: 1, event.ActionDeny: 2, event.ActionHalt: 3}

// pick returns the decision to render: the one with the strongest action,
// else the first. Nudges for several loops on one call are joined into one
// message (within the template cap).
func pick(ds []event.Decision) event.Decision {
	best := event.Decision{Action: event.ActionNone}
	for _, d := range ds {
		if actionRank[d.Action] > actionRank[best.Action] {
			best = d
		}
	}
	if best.Action == event.ActionNudge {
		for _, d := range ds {
			if d.Action == event.ActionNudge && d.LoopKey != best.LoopKey && d.Message != "" &&
				utf8.RuneCountInString(best.Message)+utf8.RuneCountInString(d.Message)+2 <= policy.MaxMessage {
				best.Message += "\n\n" + d.Message
			}
		}
	}
	return best
}

// capAction enforces the level's invariant: no action stronger than the
// level allows reaches the agent, whatever the policy says (observe and
// notify: none; assist: nudges). The user's stop and the notes that an
// earlier stop or block is over are exempt (policy.Exempt).
func capAction(d event.Decision, allow event.Action) event.Decision {
	if d.Action.Rank() > allow.Rank() && !policy.Exempt(d) {
		d.Action = event.ActionNone
	}
	return d
}

// fileHash reads a file after a successful edit or write and returns the
// xxhash64 of its content (LLD §4.5), or 0: for other events, missing or
// large files, binaries, and files outside the session's cwd.
func (a *actor) fileHash(ev *event.Event) uint64 {
	if ev.Phase != event.PhasePost || (ev.ToolName != event.ToolEdit && ev.ToolName != event.ToolWrite) || ev.Cwd == "" {
		return 0
	}
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if json.Unmarshal(ev.Input, &in) != nil {
		return 0
	}
	p := in.FilePath
	if p == "" {
		p = in.NotebookPath
	}
	if p == "" {
		return 0
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(ev.Cwd, p)
	}
	if !within(filepath.Clean(p), filepath.Clean(ev.Cwd)) {
		return 0
	}
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxFileHashSize {
		return 0
	}
	b := make([]byte, fi.Size())
	if _, err := f.ReadAt(b, 0); err != nil && fi.Size() > 0 {
		return 0
	}
	head := b
	if len(head) > 8000 {
		head = head[:8000]
	}
	if strings.IndexByte(string(head), 0) >= 0 {
		return 0 // binary
	}
	return xxhash.Sum64(b)
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// record updates the live summary and queues the event's rows for the
// store. It runs after the reply, off the hook's critical path.
func (a *actor) record(ev *event.Event, res pipeline.Result, mode config.ModeInfo, nudgeAt float64, resolved []policy.Resolved) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &a.sum
	s.Mode, s.ModeWhy = mode.Mode, mode.Why
	if s.StartedAt.IsZero() || ev.ReceivedAt.Before(s.StartedAt) {
		s.StartedAt = ev.ReceivedAt
	}
	if ev.ReceivedAt.After(s.LastSeenAt) {
		s.LastSeenAt = ev.ReceivedAt
	}
	if ev.Cwd != "" && ev.Cwd != s.Cwd {
		s.Cwd = ev.Cwd
		s.Repo = a.repoOf(ev.Cwd)
	}
	if ev.Transcript != "" {
		s.Transcript = ev.Transcript
	}
	if ev.Model != "" {
		s.Model = ev.Model
	}
	if ev.Phase == event.PhaseSessionEnd {
		s.EndedAt = ev.ReceivedAt
	} else {
		s.EndedAt = time.Time{}
	}

	if c := res.Call; c != nil {
		s.ToolCalls++
		if c.Failed() {
			s.Failures++
		}
		a.m.w.Enqueue(callRow(a.key, c))
	}
	for _, sig := range res.Signals {
		if !a.loops[sig.LoopKey] && sig.Severity >= nudgeAt {
			a.loops[sig.LoopKey] = true
			s.LoopsDetected++
		}
		row := store.Signal{SessionKey: a.key, Seq: ev.Seq, Detector: sig.Detector, LoopKey: sig.LoopKey,
			Severity: sig.Severity, Count: sig.Count, Evidence: evidenceJSON(sig.Evidence)}
		s.Signals = appendRecent(s.Signals, row)
		a.m.w.Enqueue(row)
	}
	for _, d := range res.Decisions {
		if d.WouldHave == "" || d.WouldHave == event.ActionNone {
			continue
		}
		msg := d.Message
		if msg == "" {
			msg = d.Reason
		}
		// enforce: the action was taken (it reached the agent unless the
		// reply was late); observe: what enforce mode would have done.
		m := store.ModeObserve
		if d.Action != event.ActionNone {
			m = store.ModeEnforce
		}
		row := store.Intervention{SessionKey: a.key, Seq: d.Seq, TS: ev.ReceivedAt, LoopKey: d.LoopKey, Detector: d.Detector,
			Action: string(d.WouldHave), Mode: m, Reason: redact.String(d.Reason), Message: redact.String(msg)}
		s.Interventions = appendRecent(s.Interventions, row)
		a.m.w.Enqueue(row)
	}
	a.resolve(resolved)
	a.m.w.Enqueue(s.Session)
}

// resolve records the outcomes of earlier interventions, in the store and
// the live summary. The caller holds a.mu.
func (a *actor) resolve(rs []policy.Resolved) {
	for _, r := range rs {
		d := r.Decision
		a.m.w.Enqueue(store.OutcomeUpdate{SessionKey: a.key, Seq: d.Seq, LoopKey: d.LoopKey, Action: string(d.WouldHave), Outcome: r.Outcome})
		for i := range a.sum.Interventions {
			iv := &a.sum.Interventions[i]
			if iv.Seq == d.Seq && iv.LoopKey == d.LoopKey && iv.Action == string(d.WouldHave) && iv.Outcome == "" {
				iv.Outcome = r.Outcome
			}
		}
	}
}

func appendRecent[T any](xs []T, x T) []T {
	xs = append(xs, x)
	if len(xs) > maxRecent {
		xs = xs[len(xs)-maxRecent:]
	}
	return xs
}

// repoOf returns the git root above cwd ("" if none), cached per cwd.
func (a *actor) repoOf(cwd string) string {
	if r, ok := a.repos[cwd]; ok {
		return r
	}
	r := ""
	for dir, i := filepath.Clean(cwd), 0; i < 64; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			r = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	r = a.m.repos.name(r)
	a.repos[cwd] = r
	return r
}

// callRow is the persisted form of a call: fingerprints, the redacted
// error lines, and a redacted, truncated digest of the input.
func callRow(key string, c *event.ToolCall) store.Call {
	row := store.Call{
		SessionKey: key, Seq: c.Seq(), TS: c.Event.ReceivedAt, Tool: c.Event.ToolName, Family: c.Family,
		FilePath: c.FilePath, ActionFP: c.ActionFP, ResultFP: c.ResultFP, IsError: c.Failed(),
		Progress: c.Progress, DurationMS: c.Duration.Milliseconds(),
	}
	if sig := c.ErrorSig; sig != nil {
		row.ErrExact, row.ErrSim, row.ErrLines = sig.Exact, sig.Sim, sig.Lines
	}
	switch {
	case c.Command != "":
		row.InputDigest = c.Command // already redacted and normalized
	case c.FilePath != "":
		row.InputDigest = c.FilePath
	default:
		row.InputDigest = redact.String(normalize.CanonicalJSON(c.Event.Input))
	}
	row.InputDigest = clipDigest(row.InputDigest)
	return row
}

func clipDigest(s string) string {
	if len(s) <= maxDigest {
		return s
	}
	n := maxDigest
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// evidenceJSON is the persisted form of a signal's evidence.
func evidenceJSON(e event.Evidence) json.RawMessage {
	b, err := json.Marshal(struct {
		Family     string   `json:"family,omitempty"`
		Path       string   `json:"path,omitempty"`
		Commands   []string `json:"commands,omitempty"`
		ErrorLines []string `json:"error_lines,omitempty"`
		Files      []string `json:"files,omitempty"`
		Seqs       []uint64 `json:"seqs,omitempty"`
		ElapsedMS  int64    `json:"elapsed_ms"`
	}{e.Family, e.Path, e.Commands, redact.Lines(e.ErrorLines), e.Files, e.Seqs, e.Elapsed.Milliseconds()})
	if err != nil {
		return nil
	}
	return b
}

// stop returns the halt for a pending `loopguard stop` of the event's
// session (LLD §7.1). A halt can only be given on a pre, post, or batch
// end; the main stream's turn ending (Stop, a human prompt, SessionEnd)
// clears the request, as does an observe level (a stop is the user's own
// request, so it applies at every other level).
func (a *actor) stop(pipe *pipeline.Pipeline, ev *event.Event, mode config.ModeInfo) (event.Decision, bool) {
	stops := &a.m.stops
	if !stops.pending(ev.SessionID) {
		return event.Decision{}, false
	}
	switch ev.Phase {
	case event.PhasePre, event.PhasePost, event.PhasePostFailure, event.PhaseBatchEnd:
	case event.PhasePrompt, event.PhaseStop, event.PhaseSessionEnd:
		if ev.AgentID == "" {
			stops.clear(ev.SessionID)
		}
		return event.Decision{}, false
	default:
		return event.Decision{}, false
	}
	if config.LevelRank(mode.Mode) <= config.LevelRank(config.ModeObserve) {
		stops.clear(ev.SessionID)
		a.m.log.Info("stop request dropped: the session observes", "session", a.key, "why", mode.Why)
		return event.Decision{}, false
	}
	pending, first := stops.take(ev.SessionID, ev.ReceivedAt)
	if !pending {
		return event.Decision{}, false
	}
	return pipe.Stop(a.state, first), true
}

// notifications returns what to tell the human about the event (LLD §15),
// at the notify level and above: a loop of D1-D3 reaching the nudge
// threshold (loop_detected) or a stall (D5), once per loop per turn, and
// each nudge, deny, and halt that reached the agent. The user's own stop
// isn't notified. Rate limits and channel choice are the dispatcher's.
func (a *actor) notifications(ev *event.Event, res pipeline.Result, mode config.ModeInfo, nudgeAt float64) []notify.Notification {
	if !config.Notifies(mode.Mode) {
		return nil
	}
	if a.announcedTurn != a.state.TurnSeq {
		a.announced, a.announcedTurn = map[string]bool{}, a.state.TurnSeq
	}
	project := baseName(a.repoOf(ev.Cwd))
	if project == "" {
		project = baseName(ev.Cwd)
	}
	note := func(kind, key, detector, family string, count int, d time.Duration) notify.Notification {
		return notify.New(kind, project, a.key, key, detector, family, count, d, ev.ReceivedAt)
	}
	var out []notify.Notification
	for _, sig := range res.Signals {
		kind := config.EventLoopDetected
		switch sig.Detector {
		case event.DetectorStall:
			kind = config.EventStall
		case event.DetectorExactRepeat, event.DetectorRepeatedFailure, event.DetectorOscillation:
		default:
			continue
		}
		if sig.Severity < nudgeAt || a.announced[sig.LoopKey] {
			continue
		}
		a.announced[sig.LoopKey] = true
		out = append(out, note(kind, sig.LoopKey, sig.Detector, sig.Evidence.Family, sig.Count, sig.Evidence.Elapsed))
	}
	for _, d := range res.Decisions {
		kind := map[event.Action]string{event.ActionNudge: config.EventNudge, event.ActionDeny: config.EventDeny,
			event.ActionHalt: config.EventHalt}[d.Action]
		l := a.state.Loops[d.LoopKey]
		if kind == "" || policy.Exempt(d) || l == nil {
			continue
		}
		if kind == config.EventHalt {
			// A halted loop keeps halting the agent's further attempts;
			// the human hears about it once.
			if a.announced["halt\x00"+d.LoopKey] {
				continue
			}
			a.announced["halt\x00"+d.LoopKey] = true
		}
		family := l.Family
		if family == "" {
			family = l.Evidence.Family
		}
		out = append(out, note(kind, d.LoopKey, d.Detector, family, l.Count, ev.ReceivedAt.Sub(l.StartedAt)))
	}
	return out
}

// baseName is the last element of a path with either separator.
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
