// Package store persists sessions, calls, signals, and interventions in
// SQLite (modernc.org/sqlite: pure Go, no cgo), in WAL mode, with schema
// migrations (LLD §11).
//
// Writes from the daemon's event path go through a Writer, which batches
// them on its own goroutine and never blocks the caller. Reads (the REST
// API, actor start-up) query the database directly; WAL lets them run
// alongside the writer.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store is an open LoopGuard database.
type Store struct {
	db *sql.DB
}

// busyTimeout is how long a connection waits for a lock before failing.
// Only the Writer writes, so waits come from checkpoints and other
// LoopGuard processes (a CLI command reading while the daemon writes).
const busyTimeout = 5 * time.Second

// Open opens (creating if needed) the database at path and applies
// pending migrations. The path may contain spaces.
func Open(path string) (*Store, error) {
	// The driver splits the DSN at the first '?', so a path containing one
	// would be cut; LoopGuard's own paths never do.
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	// Transactions take the write lock up front, so two writers wait on
	// busy_timeout instead of failing on a read-to-write upgrade.
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.check(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return s, nil
}

// check verifies that the database is usable and in WAL mode.
func (s *Store) check(ctx context.Context) error {
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("journal_mode is %q, want wal", mode)
	}
	return nil
}

// Close closes the database. Close the Writer first.
func (s *Store) Close() error { return s.db.Close() }

// SchemaVersion returns the highest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v)
	return int(v.Int64), err
}

// u64 and i64 convert between Go's uint64 fingerprints and SQLite's signed
// 64-bit integers, bit for bit.
func u64(v int64) uint64 { return uint64(v) }
func i64(v uint64) int64 { return int64(v) }

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}
