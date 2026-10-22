package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// tails polls the transcripts of active sessions: the main transcript
// (the hook's transcript_path) and the subagent transcripts next to it
// (<session>/subagents/agent-<id>.jsonl), each from its stored offset.
// At start the daemon also backfills the transcripts it knows of that were
// not read to the end (backfill).
type tails struct {
	m *Manager

	mu      sync.Mutex
	groups  map[string]*tailGroup // by session id
	tailing map[string]bool       // paths a live group has a stream for

	// readMu is held while a transcript is read, so the live poll and the
	// backfill never read one file at once.
	readMu sync.Mutex
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
	var groups, dropped []tailJob
	for id, g := range ts.groups {
		j := tailJob{g, g.main, g.repo}
		groups = append(groups, j)
		if g.ended || now.Sub(g.last) > tailIdle {
			delete(ts.groups, id)
			dropped = append(dropped, j)
		}
	}
	ts.mu.Unlock()
	sort.Slice(groups, func(i, j int) bool { return groups[i].g.sessionID < groups[j].g.sessionID })
	for _, j := range groups {
		ts.poll(j, now)
	}
	if len(dropped) > 0 {
		ts.mu.Lock()
		for _, j := range dropped {
			for path := range j.g.streams {
				delete(ts.tailing, path)
			}
		}
		ts.mu.Unlock()
	}
}

// tailJob is one session to poll, with the fields register may change
// copied under the lock. Only the polling goroutine touches g.streams.
type tailJob struct {
	g          *tailGroup
	main, repo string
}

// streamPaths returns a session's transcripts with their stream keys: the
// main one and the subagents' next to it.
func streamPaths(agent, sessionID, main string) map[string]string {
	paths := map[string]string{main: agent + ":" + sessionID}
	dir := strings.TrimSuffix(main, filepath.Ext(main))
	subs, _ := filepath.Glob(filepath.Join(dir, "subagents", "agent-*.jsonl"))
	for _, s := range subs {
		agentID := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(s), "agent-"), ".jsonl")
		paths[s] = agent + ":" + sessionID + "/" + agentID
	}
	return paths
}

func (ts *tails) poll(j tailJob, now time.Time) {
	g := j.g
	for path, key := range streamPaths(g.agent, g.sessionID, j.main) {
		ts.readMu.Lock()
		s := g.streams[path]
		if s == nil {
			s = ts.open(path, key)
			g.streams[path] = s
			ts.mu.Lock()
			if ts.tailing == nil {
				ts.tailing = map[string]bool{}
			}
			ts.tailing[path] = true
			ts.mu.Unlock()
		}
		ts.read(s, g.sessionID, j.repo, now, true)
		ts.readMu.Unlock()
	}
}

// read reads a transcript's new lines into the usage table and the usage
// book (live: as the session's own usage; backfill: see addBackfill), and
// returns how many messages it queued. The caller holds readMu.
func (ts *tails) read(s *tailStream, sessionID, repo string, now time.Time, live bool) (messages int) {
	m := ts.m
	pricing := m.book.pricing.Get()
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
		if live {
			m.book.addUsage(sessionID, s.key, repo, cur.TS, delta, usd, priced, now)
		} else {
			m.book.addBackfill(sessionID, s.key, repo, cur.TS, delta, usd, priced, now)
		}
		row := store.Usage{SessionKey: s.key, SessionID: sessionID, Repo: repo, MessageID: cur.ID, TS: cur.TS, Model: cur.Model,
			Input: cur.Usage.Input, Output: cur.Usage.Output, CacheRead: cur.Usage.CacheRead, CacheWrite: cur.Usage.CacheWrite,
			CacheWrite1h: cur.Usage.CacheWrite1h, Speed: cur.Speed, Geo: cur.Geo}
		if total, ok := pricing.Cost(cur); ok {
			row.CostUSD = &total
		}
		m.w.Enqueue(row)
		messages++
		if !live && messages%backfillFlushRows == 0 {
			m.w.Flush() // the writer's queue is bounded: let it catch up
		}
	})
	if err != nil {
		m.log.Warn("reading transcript", "path", s.t.Path, "err", err)
		return messages
	}
	if s.t.At != before {
		m.w.Enqueue(store.TranscriptOffset{Path: s.t.Path, Offset: s.t.At.Offset, Fingerprint: s.t.At.Fingerprint})
	}
	return messages
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

// backfillFlushRows is how many rows the backfill queues before it waits
// for the writer (whose queue drops rows when full).
const backfillFlushRows = 500

// backfill reads, in the background at start, the transcripts of every
// session LoopGuard knows whose usage is missing or incomplete: no stored
// offset, or one short of the file's size (sessions from before usage
// tracking, or that went on while the daemon was down). A session's
// transcript is its stored transcript_path, else <transcripts>/*/<id>.jsonl;
// when it is gone the session is skipped (the dashboard marks its usage
// incomplete). Newest sessions first; files a live session is tailing are
// left to it.
func (ts *tails) backfill(ctx context.Context, transcripts string) {
	m := ts.m
	if m.st == nil || m.w == nil {
		return
	}
	sessions, err := m.st.ListSessions(ctx, store.ListOptions{Limit: 1_000_000})
	if err != nil {
		m.log.Warn("backfill: listing sessions", "err", err)
		return
	}
	var files, messages int
	for _, s := range sessions {
		if strings.Contains(s.Key, "/") || s.SessionID == doctorSessionID {
			continue
		}
		main := s.Transcript
		if main == "" || !fileExists(main) {
			main = ""
			if transcripts != "" {
				if found, _ := filepath.Glob(filepath.Join(transcripts, "*", s.SessionID+".jsonl")); len(found) > 0 {
					main = found[0]
				}
			}
		}
		if main == "" {
			continue
		}
		for path, key := range streamPaths(s.Agent, s.SessionID, main) {
			if ctx.Err() != nil {
				return
			}
			if n, ok := ts.backfillFile(ctx, path, key, s.SessionID, s.Repo); ok {
				files++
				messages += n
			}
		}
	}
	if files > 0 {
		m.log.Info("backfilled usage from transcripts", "files", files, "messages", messages)
	}
}

// backfillFile reads one transcript from its stored offset if it has
// more, and reports whether it did.
func (ts *tails) backfillFile(ctx context.Context, path, key, sessionID, repo string) (int, bool) {
	m := ts.m
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	ts.readMu.Lock()
	defer ts.readMu.Unlock()
	ts.mu.Lock()
	live := ts.tailing[path]
	ts.mu.Unlock()
	if live {
		return 0, false
	}
	// What earlier polls queued (offsets, rows) is in the store first.
	m.w.Flush()
	if o, ok, err := m.st.GetTranscriptOffset(ctx, path); err != nil || (ok && o.Offset >= fi.Size()) {
		return 0, false
	}
	n := ts.read(ts.open(path, key), sessionID, repo, time.Now(), false)
	m.w.Flush()
	return n, true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
