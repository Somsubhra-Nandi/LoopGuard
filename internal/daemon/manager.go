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
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
	"github.com/Somsubhra-Nandi/loopguard/internal/pipeline"
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
	maxRecent       = 50      // signals and would-have decisions kept per live session
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
}

// Manager routes events to one actor per session. Each actor processes
// its session's events strictly in order on its own goroutine, so the
// detectors' state needs no locks, and sessions don't wait on each other.
type Manager struct {
	pipe  *pipeline.Pipeline
	st    *store.Store  // nil: memory-only
	w     *store.Writer // nil: memory-only
	log   *slog.Logger
	met   *metrics
	idle  time.Duration
	clock func() time.Time
	// nudgeAt is the severity from which a signal counts as a loop in
	// loops_detected, as in replay (weaker ones are "below threshold").
	nudgeAt float64

	mu     sync.Mutex
	actors map[string]*actor
	closed bool
	wg     sync.WaitGroup
}

func newManager(pipe *pipeline.Pipeline, nudgeAt float64, st *store.Store, w *store.Writer, log *slog.Logger, met *metrics, idle time.Duration) *Manager {
	if idle <= 0 {
		idle = DefaultIdle
	}
	return &Manager{pipe: pipe, nudgeAt: nudgeAt, st: st, w: w, log: log, met: met, idle: idle, clock: time.Now, actors: map[string]*actor{}}
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
		m.actors[key] = a
		m.wg.Add(1)
		go a.run()
	}
	a.touch(m.clock())
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
// mode, and the recent would-have decisions in any mode.
type liveSummary struct {
	store.Session
	Signals   []store.Signal       `json:"signals,omitempty"`
	WouldHave []store.Intervention `json:"would_have,omitempty"`
}

// actor owns one session's state. Only its goroutine touches state and the
// fields below it; the summary is shared with the API under mu.
type actor struct {
	m     *Manager
	key   string
	in    chan job
	last  atomic.Int64 // unix nanos of the last submitted event
	state *session.SessionState

	mu  sync.Mutex
	sum liveSummary

	loops map[string]bool // loop keys seen, for loops_detected
	repos map[string]string
}

func (m *Manager) newActor(key string, ev *event.Event) *actor {
	a := &actor{
		m:     m,
		key:   key,
		in:    make(chan job, actorQueue),
		loops: map[string]bool{},
		repos: map[string]string{},
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
	s.WouldHave = append([]store.Intervention(nil), s.WouldHave...)
	return s
}

func (a *actor) run() {
	defer a.m.wg.Done()
	a.resume()
	for j := range a.in {
		a.handle(j)
	}
}

// resume starts the session state, continuing a session the store already
// knows (an evicted actor, or an earlier daemon run): the seq numbering
// continues, and so do the counters. Detector state starts empty.
func (a *actor) resume() {
	a.state = a.m.pipe.NewSession(a.key, event.AgentKind(a.sum.Agent))
	if a.m.st == nil {
		return
	}
	// A previous actor for this session was evicted after hours idle, so
	// none of its rows are still queued in the writer.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if seq, err := a.m.st.MaxSeq(ctx, a.key); err == nil {
		a.state.ResumeAt(seq)
	} else {
		a.m.log.Warn("resuming session: reading seq", "session", a.key, "err", err)
	}
	if prev, err := a.m.st.GetSession(ctx, a.key); err == nil && prev.Key == a.key {
		a.mu.Lock()
		sig, wh := a.sum.Signals, a.sum.WouldHave
		a.sum.Session = prev
		a.sum.Signals, a.sum.WouldHave = sig, wh
		a.mu.Unlock()
	}
}

// handle processes one event and replies. A panic anywhere in it is
// recovered: the reply is "none", and the session state starts over (LLD
// §9.2), keeping the numbering.
func (a *actor) handle(j job) {
	replied := false
	defer func() {
		if r := recover(); r != nil {
			a.m.met.error("panic")
			a.m.log.Error("actor panic; session state reset", "session", a.key, "panic", r, "stack", string(debug.Stack()))
			if !replied {
				j.reply <- reply{}
			}
			seq := a.state.Seq()
			a.state = a.m.pipe.NewSession(a.key, event.AgentKind(a.sum.Agent))
			a.state.ResumeAt(seq)
		}
	}()

	ev := j.ev
	res := a.m.pipe.Handle(a.state, ev, a.fileHash(ev))
	d := observeOnly(pick(res.Decisions))
	out, code := j.ad.Render(ev, d)
	j.reply <- reply{stdout: out, exit: code}
	replied = true

	if len(res.Decisions) == 0 {
		a.m.met.decision(string(event.ActionNone))
	}
	for _, d := range res.Decisions {
		a.m.met.decision(string(d.WouldHave))
	}
	a.record(ev, res)
}

// pick returns the decision to render: the one with the strongest action
// (enforce mode), else the first.
func pick(ds []event.Decision) event.Decision {
	rank := map[event.Action]int{event.ActionNone: 0, event.ActionNudge: 1, event.ActionDeny: 2, event.ActionHalt: 3}
	best := event.Decision{Action: event.ActionNone}
	for _, d := range ds {
		if rank[d.Action] > rank[best.Action] {
			best = d
		}
	}
	return best
}

// observeOnly enforces M2's invariant: the daemon never returns a decision
// other than "none", whatever the policy says. TODO(M3): enforce mode.
func observeOnly(d event.Decision) event.Decision {
	d.Action = event.ActionNone
	d.Notify = false
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
func (a *actor) record(ev *event.Event, res pipeline.Result) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &a.sum
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
		a.m.w.Enqueue(callRow(a.key, c))
	}
	for _, sig := range res.Signals {
		if !a.loops[sig.LoopKey] && sig.Severity >= a.m.nudgeAt {
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
		row := store.Intervention{SessionKey: a.key, Seq: d.Seq, LoopKey: d.LoopKey, Action: string(d.WouldHave),
			Mode: store.ModeObserve, Message: redact.String(msg)}
		s.WouldHave = appendRecent(s.WouldHave, row)
		a.m.w.Enqueue(row)
	}
	a.m.w.Enqueue(s.Session)
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
