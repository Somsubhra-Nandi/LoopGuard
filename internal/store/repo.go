package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Session is a row of the sessions table.
type Session struct {
	Key           string    `json:"key"` // agent:session_id, or agent:session_id/agent_id for a subagent
	Agent         string    `json:"agent"`
	SessionID     string    `json:"session_id"`
	Repo          string    `json:"repo,omitempty"`
	Cwd           string    `json:"cwd,omitempty"`
	Transcript    string    `json:"transcript,omitempty"`
	Model         string    `json:"model,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	EndedAt       time.Time `json:"ended_at,omitzero"`
	ToolCalls     int64     `json:"tool_calls"`
	Failures      int64     `json:"failures"`
	LoopsDetected int64     `json:"loops_detected"`
	WastedMS      int64     `json:"wasted_ms"`
}

// Call is a row of the calls table: one completed tool call, with only
// fingerprints, redacted error lines, and a redacted, truncated digest of
// the input (never the raw payload).
type Call struct {
	SessionKey  string    `json:"-"`
	Seq         uint64    `json:"seq"`
	TS          time.Time `json:"ts"`
	Tool        string    `json:"tool"`
	Family      string    `json:"family,omitempty"`
	FilePath    string    `json:"file_path,omitempty"`
	ActionFP    uint64    `json:"action_fp"`
	ResultFP    uint64    `json:"result_fp,omitempty"`
	IsError     bool      `json:"is_error"`
	ErrExact    uint64    `json:"err_exact,omitempty"`
	ErrSim      uint64    `json:"err_sim,omitempty"`
	ErrLines    []string  `json:"err_lines,omitempty"`
	InputDigest string    `json:"input_digest,omitempty"`
	Progress    float64   `json:"progress"`
	DurationMS  int64     `json:"duration_ms"`
}

// Signal is a row of the signals table.
type Signal struct {
	ID         int64           `json:"id"`
	SessionKey string          `json:"-"`
	Seq        uint64          `json:"seq"`
	Detector   string          `json:"detector"`
	LoopKey    string          `json:"loop_key"`
	Severity   float64         `json:"severity"`
	Count      int             `json:"count"`
	Evidence   json.RawMessage `json:"evidence,omitempty"`
}

// Intervention is a row of the interventions table. In observe mode it
// records what enforce mode would have done (Mode "observe").
type Intervention struct {
	ID         int64     `json:"id"`
	SessionKey string    `json:"session_key,omitempty"`
	Seq        uint64    `json:"seq"`
	TS         time.Time `json:"ts,omitzero"`
	LoopKey    string    `json:"loop_key"`
	Detector   string    `json:"detector,omitempty"`
	Action     string    `json:"action"`
	Mode       string    `json:"mode"`
	Reason     string    `json:"reason,omitempty"` // why, in one line
	Message    string    `json:"message,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
}

// Intervention modes.
const (
	ModeEnforce = "enforce"
	ModeObserve = "observe"
)

// ErrNotFound is returned when a session doesn't exist.
var ErrNotFound = errors.New("not found")

const sessionCols = `key, agent, session_id, COALESCE(repo, ''), COALESCE(cwd, ''), COALESCE(transcript, ''),
COALESCE(model, ''), started_at, last_seen_at, COALESCE(ended_at, 0), tool_calls, COALESCE(failures, 0),
loops_detected, wasted_ms`

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var started, last, ended int64
	err := sc.Scan(&s.Key, &s.Agent, &s.SessionID, &s.Repo, &s.Cwd, &s.Transcript, &s.Model,
		&started, &last, &ended, &s.ToolCalls, &s.Failures, &s.LoopsDetected, &s.WastedMS)
	s.StartedAt, s.LastSeenAt, s.EndedAt = fromMS(started), fromMS(last), fromMS(ended)
	return s, err
}

// ListOptions filters ListSessions.
type ListOptions struct {
	Since time.Time // sessions last seen at or after this (zero: all)
	Repo  string    // only this repo (exact match; "": all)
	Limit int       // maximum rows (0: 100)
}

// ListSessions returns sessions, most recently seen first.
func (s *Store) ListSessions(ctx context.Context, o ListOptions) ([]Session, error) {
	if o.Limit <= 0 {
		o.Limit = 100
	}
	q := "SELECT " + sessionCols + " FROM sessions WHERE last_seen_at >= ?"
	args := []any{ms(o.Since)}
	if o.Repo != "" {
		q += " AND repo = ?"
		args = append(args, o.Repo)
	}
	q += " ORDER BY last_seen_at DESC LIMIT ?"
	args = append(args, o.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// GetSession returns the session with this key, or else the most recently
// seen session with this agent session id.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+sessionCols+
		" FROM sessions WHERE key = ? OR session_id = ? ORDER BY key = ? DESC, last_seen_at DESC LIMIT 1", id, id, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	return sess, err
}

// MaxSeq returns the highest call, signal, or intervention seq stored for a
// session (0 if none), so a new state for the session can continue the
// numbering.
func (s *Store) MaxSeq(ctx context.Context, key string) (uint64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(
  COALESCE((SELECT MAX(seq) FROM calls WHERE session_key = ?1), 0),
  COALESCE((SELECT MAX(seq) FROM signals WHERE session_key = ?1), 0),
  COALESCE((SELECT MAX(seq) FROM interventions WHERE session_key = ?1), 0))`, key).Scan(&v)
	return uint64(v), err
}

// Calls returns the newest limit calls of a session, oldest first.
func (s *Store) Calls(ctx context.Context, key string, limit int) ([]Call, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (
SELECT seq, ts, tool, COALESCE(family, ''), COALESCE(file_path, ''), action_fp, COALESCE(result_fp, 0), is_error,
  COALESCE(err_exact, 0), COALESCE(err_sim, 0), COALESCE(err_lines, ''), COALESCE(input_digest, ''),
  COALESCE(progress, 0), COALESCE(duration_ms, 0)
FROM calls WHERE session_key = ? ORDER BY seq DESC LIMIT ?) ORDER BY seq`, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		c := Call{SessionKey: key}
		var seq, ts, afp, rfp, exact, sim int64
		var lines string
		if err := rows.Scan(&seq, &ts, &c.Tool, &c.Family, &c.FilePath, &afp, &rfp, &c.IsError,
			&exact, &sim, &lines, &c.InputDigest, &c.Progress, &c.DurationMS); err != nil {
			return nil, err
		}
		c.Seq, c.TS, c.ActionFP, c.ResultFP, c.ErrExact, c.ErrSim = uint64(seq), fromMS(ts), u64(afp), u64(rfp), u64(exact), u64(sim)
		if lines != "" {
			_ = json.Unmarshal([]byte(lines), &c.ErrLines)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Signals returns the newest limit signals of a session, oldest first.
func (s *Store) Signals(ctx context.Context, key string, limit int) ([]Signal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT * FROM (
SELECT id, seq, detector, loop_key, severity, COALESCE(count, 0), COALESCE(evidence, '')
FROM signals WHERE session_key = ? ORDER BY id DESC LIMIT ?) ORDER BY id`, key, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Signal{}
	for rows.Next() {
		g := Signal{SessionKey: key}
		var seq int64
		var ev string
		if err := rows.Scan(&g.ID, &seq, &g.Detector, &g.LoopKey, &g.Severity, &g.Count, &ev); err != nil {
			return nil, err
		}
		g.Seq = uint64(seq)
		if ev != "" {
			g.Evidence = json.RawMessage(ev)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

const interventionCols = `id, session_key, seq, COALESCE(ts, 0), loop_key, COALESCE(detector, ''), action, mode,
COALESCE(reason, ''), COALESCE(message, ''), COALESCE(outcome, '')`

// Interventions returns the newest limit interventions of a session,
// oldest first.
func (s *Store) Interventions(ctx context.Context, key string, limit int) ([]Intervention, error) {
	return s.interventions(ctx, `SELECT * FROM (SELECT `+interventionCols+`
FROM interventions WHERE session_key = ? ORDER BY id DESC LIMIT ?) ORDER BY id`, key, limit)
}

// RecentInterventions returns the newest limit interventions of all
// sessions, newest first.
func (s *Store) RecentInterventions(ctx context.Context, limit int) ([]Intervention, error) {
	return s.interventions(ctx, `SELECT `+interventionCols+` FROM interventions ORDER BY id DESC LIMIT ?`, limit)
}

func (s *Store) interventions(ctx context.Context, q string, args ...any) ([]Intervention, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Intervention{}
	for rows.Next() {
		var iv Intervention
		var seq, ts int64
		if err := rows.Scan(&iv.ID, &iv.SessionKey, &seq, &ts, &iv.LoopKey, &iv.Detector, &iv.Action, &iv.Mode,
			&iv.Reason, &iv.Message, &iv.Outcome); err != nil {
			return nil, err
		}
		iv.Seq, iv.TS = uint64(seq), fromMS(ts)
		out = append(out, iv)
	}
	return out, rows.Err()
}

// The write side: each row type knows how to write itself in a batch.

const (
	upsertSession = `INSERT INTO sessions (key, agent, session_id, repo, cwd, transcript, model,
  started_at, last_seen_at, ended_at, tool_calls, failures, loops_detected, wasted_ms)
VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, NULLIF(?, 0), ?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
  repo = COALESCE(excluded.repo, repo),
  cwd = COALESCE(excluded.cwd, cwd),
  transcript = COALESCE(excluded.transcript, transcript),
  model = COALESCE(excluded.model, model),
  started_at = MIN(started_at, excluded.started_at),
  last_seen_at = MAX(last_seen_at, excluded.last_seen_at),
  ended_at = excluded.ended_at,
  tool_calls = excluded.tool_calls,
  failures = excluded.failures,
  loops_detected = excluded.loops_detected,
  wasted_ms = excluded.wasted_ms`
	insertCall = `INSERT INTO calls (session_key, seq, ts, tool, family, file_path, action_fp, result_fp, is_error,
  err_exact, err_sim, err_lines, input_digest, progress, duration_ms)
VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)
ON CONFLICT DO NOTHING`
	insertSignal = `INSERT INTO signals (session_key, seq, detector, loop_key, severity, count, evidence)
VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''))`
	insertIntervention = `INSERT INTO interventions (session_key, seq, ts, loop_key, detector, action, mode, reason, message, outcome)
VALUES (?, ?, NULLIF(?, 0), ?, NULLIF(?, ''), ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''))`
)

func (r Session) write(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, upsertSession, r.Key, r.Agent, r.SessionID, r.Repo, r.Cwd, r.Transcript, r.Model,
		ms(r.StartedAt), ms(r.LastSeenAt), ms(r.EndedAt), r.ToolCalls, r.Failures, r.LoopsDetected, r.WastedMS)
	return err
}

func (r Call) write(ctx context.Context, tx *sql.Tx) error {
	var lines string
	if len(r.ErrLines) > 0 {
		b, _ := json.Marshal(r.ErrLines)
		lines = string(b)
	}
	_, err := tx.ExecContext(ctx, insertCall, r.SessionKey, i64(r.Seq), ms(r.TS), r.Tool, r.Family, r.FilePath,
		i64(r.ActionFP), i64(r.ResultFP), r.IsError, i64(r.ErrExact), i64(r.ErrSim), lines, r.InputDigest,
		r.Progress, r.DurationMS)
	return err
}

func (r Signal) write(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, insertSignal, r.SessionKey, i64(r.Seq), r.Detector, r.LoopKey, r.Severity, r.Count,
		string(r.Evidence))
	return err
}

func (r Intervention) write(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, insertIntervention, r.SessionKey, i64(r.Seq), ms(r.TS), r.LoopKey, r.Detector, r.Action,
		r.Mode, r.Reason, r.Message, r.Outcome)
	return err
}
