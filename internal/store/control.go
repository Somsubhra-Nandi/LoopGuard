package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Pause scopes (`loopguard pause`).
const (
	PauseSession = "session" // Target: the agent's session id (subagents included)
	PauseRepo    = "repo"    // Target: the repo's root directory
	PauseAll     = "all"     // Target: ""
)

// Pause is a row of the pauses table: enforcement is off (observe mode)
// for its scope until Until (zero: until resumed).
type Pause struct {
	Scope     string    `json:"scope"`
	Target    string    `json:"target,omitempty"`
	Until     time.Time `json:"until,omitzero"`
	CreatedAt time.Time `json:"created_at"`
}

// Active reports whether p is in effect at now.
func (p Pause) Active(now time.Time) bool { return p.Until.IsZero() || now.Before(p.Until) }

// SetPause adds a pause, replacing one with the same scope and target.
func (s *Store) SetPause(ctx context.Context, p Pause) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pauses (scope, target, until, created_at) VALUES (?, ?, NULLIF(?, 0), ?)
ON CONFLICT(scope, target) DO UPDATE SET until = excluded.until, created_at = excluded.created_at`,
		p.Scope, p.Target, ms(p.Until), ms(p.CreatedAt))
	return err
}

// DeletePauses removes the pauses with this scope and target; an empty
// scope removes every pause. It returns how many were removed.
func (s *Store) DeletePauses(ctx context.Context, scope, target string) (int64, error) {
	var res sql.Result
	var err error
	if scope == "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM pauses`)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM pauses WHERE scope = ? AND target = ?`, scope, target)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Pauses returns the pauses in effect at now, after deleting expired ones.
func (s *Store) Pauses(ctx context.Context, now time.Time) ([]Pause, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pauses WHERE until IS NOT NULL AND until <= ?`, ms(now)); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT scope, target, COALESCE(until, 0), created_at FROM pauses ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pause{}
	for rows.Next() {
		var p Pause
		var until, created int64
		if err := rows.Scan(&p.Scope, &p.Target, &until, &created); err != nil {
			return nil, err
		}
		p.Until, p.CreatedAt = fromMS(until), fromMS(created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SessionIDs returns the distinct agent session ids that start with
// prefix, most recently seen first (at most limit).
func (s *Store) SessionIDs(ctx context.Context, prefix string, limit int) ([]string, error) {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	rows, err := s.db.QueryContext(ctx, `SELECT session_id FROM sessions WHERE session_id LIKE ? ESCAPE '\'
GROUP BY session_id ORDER BY MAX(last_seen_at) DESC LIMIT ?`, escaped+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OutcomeUpdate sets the outcome of an intervention (LLD §11) once it is
// known. It goes through the Writer after the intervention's own row, so
// the row it updates is already there.
type OutcomeUpdate struct {
	SessionKey string
	Seq        uint64
	LoopKey    string
	Action     string
	Outcome    string
}

func (r OutcomeUpdate) write(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE interventions SET outcome = ?
WHERE session_key = ? AND seq = ? AND loop_key = ? AND action = ? AND outcome IS NULL`,
		r.Outcome, r.SessionKey, i64(r.Seq), r.LoopKey, r.Action)
	return err
}

// CloseOpenOutcomes marks every intervention without an outcome as
// "unknown". A daemon calls it at start: the session state that would
// have decided them is gone.
func (s *Store) CloseOpenOutcomes(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE interventions SET outcome = 'unknown' WHERE outcome IS NULL`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// InterventionCount is one group of InterventionStats.
type InterventionCount struct {
	Action   string `json:"action"`
	Mode     string `json:"mode"`
	Detector string `json:"detector"`
	Outcome  string `json:"outcome"` // "" while pending
	Count    int64  `json:"count"`
}

// InterventionStats counts interventions since a time (zero: all) by
// action, mode, detector, and outcome.
func (s *Store) InterventionStats(ctx context.Context, since time.Time) ([]InterventionCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT action, mode, COALESCE(detector, ''), COALESCE(outcome, ''), COUNT(*)
FROM interventions WHERE COALESCE(ts, 0) >= ? GROUP BY 1, 2, 3, 4 ORDER BY 1, 2, 3, 4`, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InterventionCount{}
	for rows.Next() {
		var c InterventionCount
		if err := rows.Scan(&c.Action, &c.Mode, &c.Detector, &c.Outcome, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
