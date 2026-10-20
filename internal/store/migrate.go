package store

import (
	"context"
	"database/sql"
	"time"
)

// migrations are applied in order; migration i brings the schema to
// version i+1. Never edit an applied migration: append a new one.
var migrations = []string{
	// 1: the LLD §11 schema.
	`
CREATE TABLE sessions (
  key            TEXT PRIMARY KEY,
  agent          TEXT NOT NULL,
  session_id     TEXT NOT NULL,
  repo           TEXT,
  cwd            TEXT,
  transcript     TEXT,
  model          TEXT,
  started_at     INTEGER NOT NULL,
  last_seen_at   INTEGER NOT NULL,
  ended_at       INTEGER,
  tool_calls     INTEGER DEFAULT 0,
  input_tokens   INTEGER DEFAULT 0,
  output_tokens  INTEGER DEFAULT 0,
  cache_read     INTEGER DEFAULT 0,
  cache_write    INTEGER DEFAULT 0,
  cost_usd       REAL DEFAULT 0,
  loops_detected INTEGER DEFAULT 0,
  wasted_ms      INTEGER DEFAULT 0
);
CREATE INDEX idx_sessions_last_seen ON sessions(last_seen_at);

CREATE TABLE calls (
  session_key  TEXT NOT NULL,
  seq          INTEGER NOT NULL,
  ts           INTEGER NOT NULL,
  tool         TEXT NOT NULL,
  family       TEXT,
  file_path    TEXT,
  action_fp    INTEGER NOT NULL,
  result_fp    INTEGER,
  is_error     INTEGER NOT NULL,
  err_exact    INTEGER,
  err_sim      INTEGER,
  err_lines    TEXT,
  input_digest TEXT,
  progress     REAL,
  duration_ms  INTEGER,
  PRIMARY KEY (session_key, seq)
);

CREATE TABLE signals (
  id          INTEGER PRIMARY KEY,
  session_key TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  detector    TEXT NOT NULL,
  loop_key    TEXT NOT NULL,
  severity    REAL NOT NULL,
  count       INTEGER,
  evidence    TEXT
);
CREATE INDEX idx_signals_session ON signals(session_key, seq);

CREATE TABLE interventions (
  id          INTEGER PRIMARY KEY,
  session_key TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  loop_key    TEXT NOT NULL,
  action      TEXT NOT NULL,
  mode        TEXT NOT NULL,
  message     TEXT,
  outcome     TEXT
);
CREATE INDEX idx_interventions_session ON interventions(session_key, seq);

CREATE TABLE usage (
  session_key TEXT NOT NULL,
  message_id  TEXT NOT NULL,
  ts          INTEGER,
  model       TEXT,
  input_tokens INTEGER, output_tokens INTEGER, cache_read INTEGER, cache_write INTEGER,
  cost_usd    REAL,
  PRIMARY KEY (session_key, message_id)
);

CREATE TABLE transcript_offsets (path TEXT PRIMARY KEY, offset INTEGER NOT NULL, inode INTEGER);
`,
	// 2: failure counts per session, and when and why for each intervention
	// (`loopguard status`).
	`
ALTER TABLE sessions ADD COLUMN failures INTEGER DEFAULT 0;
ALTER TABLE interventions ADD COLUMN ts INTEGER;
ALTER TABLE interventions ADD COLUMN detector TEXT;
ALTER TABLE interventions ADD COLUMN reason TEXT;
`,
}

// migrate creates schema_migrations if needed and applies the migrations
// not yet recorded there, each in its own transaction.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER)`); err != nil {
		return err
	}
	for i, m := range migrations {
		if err := apply(ctx, db, i+1, m); err != nil {
			return err
		}
	}
	return nil
}

func apply(ctx context.Context, db *sql.DB, version int, ddl string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Checked inside the transaction, so two processes opening a new
	// database at once don't both apply it.
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
		version, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// SchemaLatest is the schema version Open migrates to.
var SchemaLatest = len(migrations)
