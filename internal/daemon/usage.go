package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/detect"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// Tailing (LLD §8.1).
const (
	DefaultTailEvery = 2 * time.Second
	// tailIdle: a session's transcripts are polled while it had a hook
	// event this recently (usage is written a little after the events).
	tailIdle = 10 * time.Minute
	// bookKeep: sessions not seen for this long leave the usage book.
	bookKeep = 48 * time.Hour
)

// usageBook keeps the totals the budgets (D6) are checked against, per
// session (main stream and subagents together), for the local day, and
// per repo for the day. Usage comes from the transcript tailers, tool
// calls and wall time from the hook events. A daemon restart reloads them
// from the store: the day's totals at start, a session's own the first time
// it is seen.
type usageBook struct {
	st      *store.Store
	pricing *cost.PricingLoader
	log     *slog.Logger

	mu       sync.Mutex
	day      string // local date of the daily totals
	dayStart time.Time
	daily    totals
	repos    map[string]*totals   // repo → the day's totals
	sessions map[string]*sessBook // session id → totals
	// once holds the budget announcements made (soft nudges, notifications,
	// would-have halts), by session and period, so each is made once.
	once map[string]bool
}

// totals is what one scope used.
type totals struct {
	usage    cost.Usage
	costUSD  float64
	unpriced int64 // tokens of messages without a verified price
	calls    int64
	// wall holds, per session id, the first and latest event of the
	// period (wall minutes add up per session).
	wall map[string][2]time.Time
}

func (t *totals) wallMinutes() float64 {
	var d time.Duration
	for _, w := range t.wall {
		d += w[1].Sub(w[0])
	}
	return d.Minutes()
}

func (t *totals) touch(id string, at time.Time) {
	if t.wall == nil {
		t.wall = map[string][2]time.Time{}
	}
	w, ok := t.wall[id]
	if !ok || at.Before(w[0]) {
		w[0] = at
	}
	if at.After(w[1]) {
		w[1] = at
	}
	t.wall[id] = w
}

func (t *totals) values() map[string]float64 {
	return map[string]float64{
		config.MetricCost: t.costUSD, config.MetricTokens: float64(t.usage.Total()),
		config.MetricWall: t.wallMinutes(), config.MetricToolCalls: float64(t.calls),
	}
}

type sessBook struct {
	totals
	repo    string
	seen    time.Time
	streams map[string]*totals // by stream key
}

func newUsageBook(st *store.Store, pricing *cost.PricingLoader, log *slog.Logger) *usageBook {
	return &usageBook{st: st, pricing: pricing, log: log, repos: map[string]*totals{}, sessions: map[string]*sessBook{}, once: map[string]bool{}}
}

func dayOf(t time.Time) string { return t.Local().Format("2006-01-02") }

func midnight(t time.Time) time.Time {
	y, m, d := t.Local().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// rollover starts a new day's totals when now is on another day, loading
// the day's totals from the store (a restart, or the first event of the
// day). The caller holds mu.
func (b *usageBook) rollover(now time.Time) {
	day := dayOf(now)
	if day == b.day {
		return
	}
	b.day, b.dayStart = day, midnight(now)
	b.daily, b.repos = totals{}, map[string]*totals{}
	if b.st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := b.st.UsageRows(ctx, store.UsageFilter{Since: b.dayStart})
	if err != nil {
		b.log.Warn("loading today's usage", "err", err)
	}
	p := b.pricing.Get()
	for _, r := range rows {
		usd, ok := p.Cost(messageOf(r))
		b.addDay(r.Repo, usageOf(r), usd, ok)
	}
	sessions, err := b.st.ListSessions(ctx, store.ListOptions{Since: b.dayStart, Limit: 10000})
	if err != nil {
		b.log.Warn("loading today's sessions", "err", err)
	}
	calls, err := b.st.CallCounts(ctx, b.dayStart)
	if err != nil {
		b.log.Warn("loading today's tool calls", "err", err)
	}
	for _, s := range sessions {
		if s.SessionID == doctorSessionID {
			continue
		}
		start := s.StartedAt
		if start.Before(b.dayStart) {
			start = b.dayStart
		}
		rt := b.repoTotals(s.Repo)
		for _, t := range []*totals{&b.daily, rt} {
			if t == nil {
				continue
			}
			t.calls += calls[s.Key]
			t.touch(s.SessionID, start)
			t.touch(s.SessionID, s.LastSeenAt)
		}
	}
}

func (b *usageBook) repoTotals(repo string) *totals {
	if repo == "" {
		return nil
	}
	t := b.repos[repo]
	if t == nil {
		t = &totals{}
		b.repos[repo] = t
	}
	return t
}

func (b *usageBook) addDay(repo string, u cost.Usage, usd float64, priced bool) {
	for _, t := range []*totals{&b.daily, b.repoTotals(repo)} {
		if t == nil {
			continue
		}
		t.usage = t.usage.Add(u)
		if priced {
			t.costUSD += usd
		} else {
			t.unpriced += u.Total()
		}
	}
}

// session returns a session's totals, loading them from the store the
// first time. The caller holds mu.
func (b *usageBook) session(id, repo string, now time.Time) *sessBook {
	s := b.sessions[id]
	if s != nil {
		if repo != "" {
			s.repo = repo
		}
		return s
	}
	s = &sessBook{repo: repo, seen: now, streams: map[string]*totals{}}
	b.sessions[id] = s
	if b.st == nil {
		return s
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := b.st.UsageRows(ctx, store.UsageFilter{SessionID: id})
	if err != nil {
		b.log.Warn("loading a session's usage", "session", id, "err", err)
	}
	p := b.pricing.Get()
	for _, r := range rows {
		usd, ok := p.Cost(messageOf(r))
		s.add(r.SessionKey, usageOf(r), usd, ok)
	}
	streams, err := b.st.SessionStreams(ctx, id)
	if err != nil {
		b.log.Warn("loading a session's streams", "session", id, "err", err)
	}
	for _, st := range streams {
		s.calls += st.ToolCalls
		s.stream(st.Key).calls += st.ToolCalls
		s.touch(id, st.StartedAt)
		s.touch(id, st.LastSeenAt)
	}
	return s
}

func (s *sessBook) stream(key string) *totals {
	t := s.streams[key]
	if t == nil {
		t = &totals{}
		s.streams[key] = t
	}
	return t
}

func (s *sessBook) add(key string, u cost.Usage, usd float64, priced bool) {
	for _, t := range []*totals{&s.totals, s.stream(key)} {
		t.usage = t.usage.Add(u)
		if priced {
			t.costUSD += usd
		} else {
			t.unpriced += u.Total()
		}
	}
}

// addUsage records the growth of one message's usage (delta tokens and
// cost, from the tailer). Messages from another day count for the session
// only.
func (b *usageBook) addUsage(sessionID, key, repo string, ts time.Time, delta cost.Usage, usd float64, priced bool, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	b.session(sessionID, repo, now).add(key, delta, usd, priced)
	if ts.IsZero() || !ts.Before(b.dayStart) {
		b.addDay(repo, delta, usd, priced)
	}
}

// event records a hook event of a session at at: wall time, and a
// completed tool call if call.
func (b *usageBook) event(sessionID, key, repo string, at time.Time, call bool) {
	if sessionID == doctorSessionID {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(at)
	s := b.session(sessionID, repo, at)
	s.seen = at
	ts := []*totals{&s.totals, &b.daily, b.repoTotals(s.repo)}
	for _, t := range ts {
		if t == nil {
			continue
		}
		t.touch(sessionID, at)
		if call {
			t.calls++
		}
	}
	if call {
		s.stream(key).calls++
	}
}

// budgetTotals returns the totals of the scopes a session's event is
// checked against: the session, the day, and the session's repo's day.
func (b *usageBook) budgetTotals(sessionID, repo string, now time.Time) []detect.BudgetTotals {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	s := b.session(sessionID, repo, now)
	out := []detect.BudgetTotals{
		{Scope: config.ScopeSession, Period: sessionID, Values: s.values()},
		{Scope: config.ScopeDaily, Period: b.day, Values: b.daily.values()},
	}
	if r := b.repos[s.repo]; r != nil {
		out = append(out, detect.BudgetTotals{Scope: config.ScopeRepo, Period: b.day + " " + s.repo, Values: r.values()})
	}
	return out
}

// marked reports whether key was recorded.
func (b *usageBook) marked(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.once[key]
}

// markOnce reports whether key is new, and records it.
func (b *usageBook) markOnce(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.once[key] {
		return false
	}
	b.once[key] = true
	return true
}

// UsageView is a scope's usage in API responses (`loopguard status`).
type UsageView struct {
	Tokens         int64      `json:"tokens"`
	Usage          cost.Usage `json:"usage"`
	CostUSD        float64    `json:"cost_usd"` // API-equivalent, priced messages only
	UnpricedTokens int64      `json:"unpriced_tokens,omitempty"`
	ToolCalls      int64      `json:"tool_calls"`
	WallMinutes    float64    `json:"wall_minutes"`
}

func (t *totals) view() UsageView {
	return UsageView{Tokens: t.usage.Total(), Usage: t.usage, CostUSD: t.costUSD, UnpricedTokens: t.unpriced,
		ToolCalls: t.calls, WallMinutes: t.wallMinutes()}
}

// UsageReply is GET /v1/usage: today's totals, each live session's, and
// the budgets in effect (config.yaml's).
type UsageReply struct {
	Day      string               `json:"day"`
	Today    UsageView            `json:"today"`
	Sessions map[string]UsageView `json:"sessions"` // by session id (subagents included)
	Streams  map[string]UsageView `json:"streams"`  // by session key
	Budgets  config.Budgets       `json:"budgets"`
}

func (b *usageBook) reply(ids []string, now time.Time) UsageReply {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	r := UsageReply{Day: b.day, Today: b.daily.view(), Sessions: map[string]UsageView{}, Streams: map[string]UsageView{}}
	for _, id := range ids {
		s := b.sessions[id]
		if s == nil {
			continue
		}
		r.Sessions[id] = s.view()
		for k, t := range s.streams {
			r.Streams[k] = t.view()
		}
	}
	return r
}

// prune forgets sessions not seen for a while, and announcements of past
// periods.
func (b *usageBook) prune(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, s := range b.sessions {
		if now.Sub(s.seen) > bookKeep {
			delete(b.sessions, id)
		}
	}
	if len(b.once) > 10000 {
		b.once = map[string]bool{}
	}
}

// loadAnnounced marks the budget announcements already recorded today, so
// a restarted daemon doesn't nudge or notify again: soft nudges and
// would-have halts are interventions of detector budget.
func (b *usageBook) loadAnnounced(ctx context.Context, now time.Time) {
	if b.st == nil {
		return
	}
	keys, err := b.st.InterventionKeys(ctx, "budget", midnight(now))
	if err != nil {
		b.log.Warn("loading today's budget interventions", "err", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range keys {
		b.once["restart|"+k[0]+"|"+k[1]] = true
	}
}

func messageOf(r store.Usage) cost.Message {
	return cost.Message{ID: r.MessageID, Model: r.Model, TS: r.TS, Usage: usageOf(r), Speed: r.Speed, Geo: r.Geo}
}

func usageOf(r store.Usage) cost.Usage {
	return cost.Usage{Input: r.Input, Output: r.Output, CacheWrite: r.CacheWrite, CacheRead: r.CacheRead, CacheWrite1h: r.CacheWrite1h}
}

// tails polls the transcripts of active sessions: the main transcript
// (the hook's transcript_path) and the subagent transcripts next to it
// (<session>/subagents/agent-<id>.jsonl), each from its stored offset.
type tails struct {
	m *Manager

	mu     sync.Mutex
	groups map[string]*tailGroup // by session id
}

type tailGroup struct {
	sessionID string
	agent     string
	main      string
	repo      string
	last      time.Time // latest hook event
	ended     bool
	streams   map[string]*tailStream // by path
}

type tailStream struct {
	key    string
	t      cost.Tailer
	ledger *cost.Ledger
}

// register starts (or keeps) tailing a session's transcripts.
func (ts *tails) register(sessionID, agent, transcript, repo string, at time.Time, ended bool) {
	if transcript == "" || sessionID == doctorSessionID {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.groups == nil {
		ts.groups = map[string]*tailGroup{}
	}
	g := ts.groups[sessionID]
	if g == nil {
		g = &tailGroup{sessionID: sessionID, agent: agent, streams: map[string]*tailStream{}}
		ts.groups[sessionID] = g
	}
	g.main = transcript
	if repo != "" {
		g.repo = repo
	}
	if at.After(g.last) {
		g.last = at
	}
	g.ended = ended
}

// pollAll polls every active session's transcripts once, and drops
// sessions that ended or went idle (after a last poll).
func (ts *tails) pollAll(now time.Time) {
	ts.mu.Lock()
	var groups []tailJob
	for id, g := range ts.groups {
		groups = append(groups, tailJob{g, g.main, g.repo})
		if g.ended || now.Sub(g.last) > tailIdle {
			delete(ts.groups, id)
		}
	}
	ts.mu.Unlock()
	sort.Slice(groups, func(i, j int) bool { return groups[i].g.sessionID < groups[j].g.sessionID })
	for _, j := range groups {
		ts.poll(j, now)
	}
}

// tailJob is one session to poll, with the fields register may change
// copied under the lock. Only the polling goroutine touches g.streams.
type tailJob struct {
	g          *tailGroup
	main, repo string
}

func (ts *tails) poll(j tailJob, now time.Time) {
	m, g := ts.m, j.g
	paths := map[string]string{j.main: g.agent + ":" + g.sessionID}
	dir := strings.TrimSuffix(j.main, filepath.Ext(j.main))
	subs, _ := filepath.Glob(filepath.Join(dir, "subagents", "agent-*.jsonl"))
	for _, s := range subs {
		agentID := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(s), "agent-"), ".jsonl")
		paths[s] = g.agent + ":" + g.sessionID + "/" + agentID
	}
	pricing := m.book.pricing.Get()
	for path, key := range paths {
		s := g.streams[path]
		if s == nil {
			s = ts.open(path, key)
			g.streams[path] = s
		}
		before := s.t.At
		_, err := s.t.Poll(func(line []byte) {
			msg, ok := cost.ParseLine(line)
			if !ok {
				return
			}
			old := s.ledger.Messages()
			var prev cost.Message
			if i := s.ledger.Index(msg.ID); i >= 0 {
				prev = old[i]
			}
			cur, delta := s.ledger.Add(msg)
			if delta.IsZero() && prev.ID != "" {
				return // a repeated line
			}
			usd, priced := pricing.Cost(cur)
			if prev.ID != "" {
				was, _ := pricing.Cost(prev)
				usd -= was
			}
			m.book.addUsage(g.sessionID, key, j.repo, cur.TS, delta, usd, priced, now)
			row := store.Usage{SessionKey: key, SessionID: g.sessionID, Repo: j.repo, MessageID: cur.ID, TS: cur.TS, Model: cur.Model,
				Input: cur.Usage.Input, Output: cur.Usage.Output, CacheRead: cur.Usage.CacheRead, CacheWrite: cur.Usage.CacheWrite,
				CacheWrite1h: cur.Usage.CacheWrite1h, Speed: cur.Speed, Geo: cur.Geo}
			if total, ok := pricing.Cost(cur); ok {
				row.CostUSD = &total
			}
			m.w.Enqueue(row)
		})
		if err != nil {
			m.log.Warn("reading transcript", "path", path, "err", err)
			continue
		}
		if s.t.At != before {
			m.w.Enqueue(store.TranscriptOffset{Path: path, Offset: s.t.At.Offset, Fingerprint: s.t.At.Fingerprint})
		}
	}
}

// open starts tailing one transcript from its stored offset, with the
// messages already stored for the stream in the ledger, so a message whose
// lines straddle a restart is counted once.
func (ts *tails) open(path, key string) *tailStream {
	m := ts.m
	s := &tailStream{key: key, t: cost.Tailer{Path: path}, ledger: cost.NewLedger()}
	if m.st == nil {
		return s
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if o, ok, err := m.st.GetTranscriptOffset(ctx, path); err != nil {
		m.log.Warn("reading transcript offset", "path", path, "err", err)
	} else if ok {
		s.t.At = cost.Offset{Offset: o.Offset, Fingerprint: o.Fingerprint}
	}
	rows, err := m.st.UsageRows(ctx, store.UsageFilter{SessionKey: key})
	if err != nil {
		m.log.Warn("reading stored usage", "stream", key, "err", err)
	}
	for _, r := range rows {
		s.ledger.Add(messageOf(r))
	}
	return s
}
