package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Usage is a row of the usage table: one API message of one stream (main
// session or subagent), deduplicated by message id (LLD §8.1). Cost is
// not stored as the truth: readers price the counters with the current
// pricing.yaml, so a price added later applies to old rows too. CostUSD is
// the cost when the row was written (nil: the model had no price).
type Usage struct {
	SessionKey   string    `json:"session_key"`
	SessionID    string    `json:"session_id"`
	Repo         string    `json:"repo,omitempty"`
	MessageID    string    `json:"message_id"`
	TS           time.Time `json:"ts"`
	Model        string    `json:"model"`
	Input        int64     `json:"input_tokens"`
	Output       int64     `json:"output_tokens"`
	CacheRead    int64     `json:"cache_read"`
	CacheWrite   int64     `json:"cache_write"`
	CacheWrite1h int64     `json:"cache_write_1h"`
	Speed        string    `json:"speed,omitempty"`
	Geo          string    `json:"geo,omitempty"`
	CostUSD      *float64  `json:"cost_usd,omitempty"`
}

// A later reading of a message never lowers a counter: the row keeps the
// maximum of each (a message spans several transcript lines).
const upsertUsage = `INSERT INTO usage (session_key, message_id, ts, model, input_tokens, output_tokens, cache_read, cache_write,
  cost_usd, session_id, repo, cache_write_1h, speed, geo)
VALUES (?, ?, NULLIF(?, 0), NULLIF(?, ''), ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, ''), NULLIF(?, ''))
ON CONFLICT(session_key, message_id) DO UPDATE SET
  ts = COALESCE(ts, excluded.ts),
  model = COALESCE(excluded.model, model),
  input_tokens = MAX(input_tokens, excluded.input_tokens),
  output_tokens = MAX(output_tokens, excluded.output_tokens),
  cache_read = MAX(cache_read, excluded.cache_read),
  cache_write = MAX(cache_write, excluded.cache_write),
  cache_write_1h = MAX(COALESCE(cache_write_1h, 0), excluded.cache_write_1h),
  cost_usd = COALESCE(excluded.cost_usd, cost_usd),
  session_id = COALESCE(excluded.session_id, session_id),
  repo = COALESCE(excluded.repo, repo),
  speed = COALESCE(excluded.speed, speed),
  geo = COALESCE(excluded.geo, geo)`

func (r Usage) write(ctx context.Context, tx *sql.Tx) error {
	var cost any
	if r.CostUSD != nil {
		cost = *r.CostUSD
	}
	_, err := tx.ExecContext(ctx, upsertUsage, r.SessionKey, r.MessageID, ms(r.TS), r.Model, r.Input, r.Output, r.CacheRead,
		r.CacheWrite, cost, r.SessionID, r.Repo, r.CacheWrite1h, r.Speed, r.Geo)
	return err
}

// UsageFilter selects usage rows; zero fields don't filter.
type UsageFilter struct {
	SessionID  string    // the agent's session id (all its streams)
	SessionIDs []string  // any of these session ids (nil: no filter)
	SessionKey string    // one stream
	Since      time.Time // ts at or after
	Until      time.Time // ts before
}

// UsageRows returns usage rows, oldest first.
func (s *Store) UsageRows(ctx context.Context, f UsageFilter) ([]Usage, error) {
	q := `SELECT session_key, COALESCE(session_id, ''), COALESCE(repo, ''), message_id, COALESCE(ts, 0), COALESCE(model, ''),
  COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(cache_read, 0), COALESCE(cache_write, 0),
  COALESCE(cache_write_1h, 0), COALESCE(speed, ''), COALESCE(geo, ''), cost_usd FROM usage WHERE 1 = 1`
	var args []any
	if f.SessionID != "" {
		q += " AND session_id = ?"
		args = append(args, f.SessionID)
	}
	switch {
	case f.SessionIDs == nil:
	case len(f.SessionIDs) == 0:
		q += " AND 0"
	default:
		q += " AND session_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(f.SessionIDs)), ",") + ")"
		for _, id := range f.SessionIDs {
			args = append(args, id)
		}
	}
	if f.SessionKey != "" {
		q += " AND session_key = ?"
		args = append(args, f.SessionKey)
	}
	if !f.Since.IsZero() {
		q += " AND ts >= ?"
		args = append(args, ms(f.Since))
	}
	if !f.Until.IsZero() {
		q += " AND ts < ?"
		args = append(args, ms(f.Until))
	}
	q += " ORDER BY ts, rowid"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Usage{}
	for rows.Next() {
		var u Usage
		var ts int64
		var cost sql.NullFloat64
		if err := rows.Scan(&u.SessionKey, &u.SessionID, &u.Repo, &u.MessageID, &ts, &u.Model, &u.Input, &u.Output,
			&u.CacheRead, &u.CacheWrite, &u.CacheWrite1h, &u.Speed, &u.Geo, &cost); err != nil {
			return nil, err
		}
		u.TS = fromMS(ts)
		if cost.Valid {
			v := cost.Float64
			u.CostUSD = &v
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// TranscriptOffset is a row of transcript_offsets: how far the tailer has
// read a transcript, and the fingerprint of the file's first bytes
// (stored in the inode column).
type TranscriptOffset struct {
	Path        string
	Offset      int64
	Fingerprint int64
}

func (r TranscriptOffset) write(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO transcript_offsets (path, offset, inode) VALUES (?, ?, ?)
ON CONFLICT(path) DO UPDATE SET offset = excluded.offset, inode = excluded.inode`, r.Path, r.Offset, r.Fingerprint)
	return err
}

// GetTranscriptOffset returns the stored offset of a transcript.
func (s *Store) GetTranscriptOffset(ctx context.Context, path string) (TranscriptOffset, bool, error) {
	o := TranscriptOffset{Path: path}
	err := s.db.QueryRowContext(ctx, `SELECT offset, COALESCE(inode, 0) FROM transcript_offsets WHERE path = ?`, path).
		Scan(&o.Offset, &o.Fingerprint)
	if err == sql.ErrNoRows {
		return o, false, nil
	}
	return o, err == nil, err
}

// CallCounts returns the completed calls per session key at or after
// since.
func (s *Store) CallCounts(ctx context.Context, since time.Time) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_key, COUNT(*) FROM calls WHERE ts >= ? GROUP BY session_key`, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// SessionStreams returns the session rows (main stream and subagents) of
// an agent session id.
func (s *Store) SessionStreams(ctx context.Context, sessionID string) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+sessionCols+" FROM sessions WHERE session_id = ? ORDER BY key", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// LoopSpan is one loop LoopGuard saw live: a loop key of one stream whose
// signals reached a severity, with the time from its first call to its
// last (the calls in its signals' evidence).
type LoopSpan struct {
	SessionKey string    `json:"session_key"`
	LoopKey    string    `json:"loop_key"`
	Detector   string    `json:"detector"`
	FirstSeq   uint64    `json:"first_seq"`
	LastSeq    uint64    `json:"last_seq"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	// LastSeen is when the stream was last seen (its session row).
	LastSeen time.Time `json:"last_seen"`
}

// LoopSpans returns the loops of the streams last seen at or after since
// whose signals reached minSeverity: one per (stream, loop key), spanning
// all the calls its signals named.
func (s *Store) LoopSpans(ctx context.Context, since time.Time, minSeverity float64) ([]LoopSpan, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.session_key, g.loop_key, g.detector, MAX(g.severity), GROUP_CONCAT(COALESCE(g.evidence, ''), char(30)), MAX(s.last_seen_at)
FROM signals g JOIN sessions s ON s.key = g.session_key
WHERE s.last_seen_at >= ? GROUP BY g.session_key, g.loop_key, g.detector ORDER BY g.session_key, MIN(g.id)`, ms(since))
	if err != nil {
		return nil, err
	}
	var spans []LoopSpan
	for rows.Next() {
		var sp LoopSpan
		var sev float64
		var evs string
		var seen int64
		if err := rows.Scan(&sp.SessionKey, &sp.LoopKey, &sp.Detector, &sev, &evs, &seen); err != nil {
			rows.Close()
			return nil, err
		}
		sp.LastSeen = fromMS(seen)
		if sev < minSeverity {
			continue
		}
		for _, ev := range strings.Split(evs, "\x1e") {
			var e struct {
				Seqs []uint64 `json:"seqs"`
			}
			if json.Unmarshal([]byte(ev), &e) != nil {
				continue
			}
			for _, q := range e.Seqs {
				if sp.FirstSeq == 0 || q < sp.FirstSeq {
					sp.FirstSeq = q
				}
				sp.LastSeq = max(sp.LastSeq, q)
			}
		}
		if sp.FirstSeq > 0 {
			spans = append(spans, sp)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range spans {
		sp := &spans[i]
		var first, last sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT
  (SELECT ts FROM calls WHERE session_key = ?1 AND seq >= ?2 ORDER BY seq LIMIT 1),
  (SELECT ts FROM calls WHERE session_key = ?1 AND seq <= ?3 ORDER BY seq DESC LIMIT 1)`,
			sp.SessionKey, i64(sp.FirstSeq), i64(sp.LastSeq)).Scan(&first, &last); err != nil {
			return nil, err
		}
		sp.Start, sp.End = fromMS(first.Int64), fromMS(last.Int64)
	}
	return spans, nil
}

// StopRequest is a pending `loopguard stop` (table stop_requests), kept so
// a daemon restart doesn't lose it.
type StopRequest struct {
	SessionID   string
	RequestedAt time.Time
	Recorded    bool
}

// SetStopRequest stores a stop request, replacing one for the session.
func (s *Store) SetStopRequest(ctx context.Context, r StopRequest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO stop_requests (session_id, requested_at, recorded) VALUES (?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET requested_at = excluded.requested_at, recorded = excluded.recorded`,
		r.SessionID, ms(r.RequestedAt), r.Recorded)
	return err
}

// DeleteStopRequest removes a session's stop request.
func (s *Store) DeleteStopRequest(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM stop_requests WHERE session_id = ?`, sessionID)
	return err
}

// StopRequests returns the stored stop requests requested at or after
// since, after deleting older ones.
func (s *Store) StopRequests(ctx context.Context, since time.Time) ([]StopRequest, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM stop_requests WHERE requested_at < ?`, ms(since)); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, requested_at, recorded FROM stop_requests ORDER BY requested_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StopRequest
	for rows.Next() {
		var r StopRequest
		var at int64
		if err := rows.Scan(&r.SessionID, &at, &r.Recorded); err != nil {
			return nil, err
		}
		r.RequestedAt = fromMS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// InterventionKeys returns the (session key, loop key) pairs of the
// interventions of a detector recorded at or after since.
func (s *Store) InterventionKeys(ctx context.Context, detector string, since time.Time) ([][2]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT session_key, loop_key FROM interventions WHERE detector = ? AND COALESCE(ts, 0) >= ?`,
		detector, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var k [2]string
		if err := rows.Scan(&k[0], &k[1]); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// CallTimes returns the times of the calls of the given streams, by
// session key, oldest first.
func (s *Store) CallTimes(ctx context.Context, keys []string) (map[string][]time.Time, error) {
	out := map[string][]time.Time{}
	for len(keys) > 0 {
		n := min(len(keys), 500)
		batch := keys[:n]
		keys = keys[n:]
		args := make([]any, len(batch))
		for i, k := range batch {
			args[i] = k
		}
		rows, err := s.db.QueryContext(ctx, `SELECT session_key, ts FROM calls WHERE session_key IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`) ORDER BY ts`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			var ts int64
			if err := rows.Scan(&k, &ts); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = append(out[k], fromMS(ts))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// HasTranscriptOffset reports whether path has a stored offset (LoopGuard
// read it, live or by backfill).
func (s *Store) HasTranscriptOffset(ctx context.Context, path string) (bool, error) {
	_, ok, err := s.GetTranscriptOffset(ctx, path)
	return ok, err
}
