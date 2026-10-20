package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Lööp Guard home")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "loopguard.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOpenMigratesOnceWithSpacesInPath(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	v, err := s.SchemaVersion(ctx)
	if err != nil || v != len(migrations) {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, len(migrations))
	}
	// Reopening applies nothing twice.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil || n != len(migrations) {
		t.Errorf("schema_migrations rows = %d, %v", n, err)
	}
	for _, table := range []string{"sessions", "calls", "signals", "interventions", "usage", "transcript_offsets"} {
		if _, err := s2.db.Exec("SELECT COUNT(*) FROM " + table); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestOpenFailsOnGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.db")
	if err := os.WriteFile(path, []byte("this is not a database, just some text that is long enough"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open on a non-database file succeeded")
	}
}

func TestWriterRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	w := s.NewWriter(WriterOptions{})
	defer w.Close()
	ctx := context.Background()
	t0 := time.UnixMilli(1_700_000_000_000)

	key := "claude-code:abc"
	w.Enqueue(Session{Key: key, Agent: "claude-code", SessionID: "abc", Cwd: `C:\repo`, StartedAt: t0, LastSeenAt: t0, ToolCalls: 1})
	w.Enqueue(Session{Key: key, Agent: "claude-code", SessionID: "abc", Model: "opus", StartedAt: t0, LastSeenAt: t0.Add(time.Minute), ToolCalls: 2, Failures: 1, LoopsDetected: 1})
	big := uint64(1<<63 + 12345) // high bit set: stored as a negative int64
	call := Call{SessionKey: key, Seq: 3, TS: t0, Tool: "bash", Family: "go test", ActionFP: big, ResultFP: 7,
		IsError: true, ErrExact: big - 1, ErrSim: 9, ErrLines: []string{"--- FAIL: TestX"}, InputDigest: "go test ./...",
		Progress: 0.5, DurationMS: 1200}
	w.Enqueue(call)
	w.Enqueue(Signal{SessionKey: key, Seq: 3, Detector: "repeated_failure", LoopKey: "rf:go test:1", Severity: 0.6, Count: 4,
		Evidence: json.RawMessage(`{"family":"go test"}`)})
	iv := Intervention{SessionKey: key, Seq: 3, TS: t0, LoopKey: "rf:go test:1", Detector: "repeated_failure", Action: "nudge",
		Mode: ModeObserve, Reason: "go test failed 4 times with the same error", Message: "msg"}
	w.Enqueue(iv)
	w.Flush()

	sess, err := s.GetSession(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	// The later row wins, but a missing model or cwd doesn't erase one.
	if sess.Key != key || sess.ToolCalls != 2 || sess.Failures != 1 || sess.LoopsDetected != 1 || sess.Model != "opus" || sess.Cwd != `C:\repo` ||
		!sess.StartedAt.Equal(t0) || !sess.LastSeenAt.Equal(t0.Add(time.Minute)) || !sess.EndedAt.IsZero() {
		t.Errorf("session = %+v", sess)
	}
	if byKey, err := s.GetSession(ctx, key); err != nil || byKey.Key != key {
		t.Errorf("GetSession(key) = %+v, %v", byKey, err)
	}
	if _, err := s.GetSession(ctx, "nope"); err != ErrNotFound {
		t.Errorf("GetSession(missing) err = %v, want ErrNotFound", err)
	}

	calls, err := s.Calls(ctx, key, 10)
	if err != nil || len(calls) != 1 {
		t.Fatalf("Calls = %+v, %v", calls, err)
	}
	if got := calls[0]; !reflect.DeepEqual(got, call) {
		t.Errorf("call round trip:\n got %+v\nwant %+v", got, call)
	}
	sigs, err := s.Signals(ctx, key, 10)
	if err != nil || len(sigs) != 1 || sigs[0].LoopKey != "rf:go test:1" || string(sigs[0].Evidence) != `{"family":"go test"}` {
		t.Errorf("Signals = %+v, %v", sigs, err)
	}
	ivs, err := s.Interventions(ctx, key, 10)
	iv.ID = 1
	if err != nil || len(ivs) != 1 || !reflect.DeepEqual(ivs[0], iv) {
		t.Errorf("Interventions = %+v, %v; want %+v", ivs, err, iv)
	}
	if recent, err := s.RecentInterventions(ctx, 5); err != nil || len(recent) != 1 || !reflect.DeepEqual(recent[0], iv) {
		t.Errorf("RecentInterventions = %+v, %v", recent, err)
	}
	if seq, err := s.MaxSeq(ctx, key); err != nil || seq != 3 {
		t.Errorf("MaxSeq = %d, %v; want 3", seq, err)
	}
	if seq, err := s.MaxSeq(ctx, "claude-code:other"); err != nil || seq != 0 {
		t.Errorf("MaxSeq(unknown) = %d, %v; want 0", seq, err)
	}

	// A duplicate call (same session and seq) is skipped, not an error
	// that fails the batch.
	w.Enqueue(call)
	w.Enqueue(Session{Key: key, Agent: "claude-code", SessionID: "abc", StartedAt: t0, LastSeenAt: t0.Add(2 * time.Minute), EndedAt: t0.Add(2 * time.Minute), ToolCalls: 3})
	w.Flush()
	if written, dropped, failed := w.Stats(); dropped != 0 || failed != 0 || written != 7 {
		t.Errorf("Stats = %d written, %d dropped, %d failed", written, dropped, failed)
	}
	sess, _ = s.GetSession(ctx, key)
	if sess.ToolCalls != 3 || sess.EndedAt.IsZero() {
		t.Errorf("session after end = %+v", sess)
	}

	list, err := s.ListSessions(ctx, ListOptions{Since: t0.Add(time.Minute)})
	if err != nil || len(list) != 1 {
		t.Errorf("ListSessions = %+v, %v", list, err)
	}
	if list, _ := s.ListSessions(ctx, ListOptions{Since: t0.Add(time.Hour)}); len(list) != 0 {
		t.Errorf("ListSessions(since later) = %+v", list)
	}
	if list, _ := s.ListSessions(ctx, ListOptions{Repo: "/elsewhere"}); len(list) != 0 {
		t.Errorf("ListSessions(other repo) = %+v", list)
	}
}

func TestWriterFlushesOnSizeAndTime(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	count := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM calls").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitFor := func(want int, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for count() != want {
			if time.Now().After(deadline) {
				t.Fatalf("calls = %d after %v, want %d", count(), within, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Size: a full batch is written long before the (huge) flush interval.
	w := s.NewWriter(WriterOptions{FlushEvery: time.Hour, BatchSize: 10})
	for i := 1; i <= 10; i++ {
		w.Enqueue(Call{SessionKey: "k", Seq: uint64(i), Tool: "bash"})
	}
	waitFor(10, 5*time.Second)
	w.Enqueue(Call{SessionKey: "k", Seq: 11, Tool: "bash"})
	time.Sleep(50 * time.Millisecond)
	if count() != 10 {
		t.Error("a partial batch was written before the flush interval")
	}
	w.Close() // writes the rest
	if count() != 11 {
		t.Errorf("Close left rows unwritten: %d", count())
	}
	if w.Enqueue(Call{SessionKey: "k", Seq: 12, Tool: "bash"}) {
		t.Error("Enqueue after Close accepted a row")
	}

	// Time: a partial batch is written after FlushEvery.
	w = s.NewWriter(WriterOptions{FlushEvery: 20 * time.Millisecond, BatchSize: 1000})
	defer w.Close()
	w.Enqueue(Call{SessionKey: "k", Seq: 13, Tool: "bash"})
	waitFor(12, 5*time.Second)
}

func TestWriterNeverBlocks(t *testing.T) {
	s, _ := openTemp(t)
	w := s.NewWriter(WriterOptions{QueueSize: 4, BatchSize: 1000, FlushEvery: time.Hour})
	defer w.Close()
	start := time.Now()
	accepted := 0
	for i := 0; i < 1000; i++ {
		if w.Enqueue(Call{SessionKey: "k", Seq: uint64(i), Tool: "bash"}) {
			accepted++
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("1000 Enqueue calls took %v", d)
	}
	if _, dropped, _ := w.Stats(); dropped == 0 || accepted+int(dropped) != 1000 {
		t.Errorf("accepted %d, dropped %d: want drops once the queue is full", accepted, dropped)
	}
}

func TestNilWriter(t *testing.T) {
	var w *Writer
	if w.Enqueue(Call{}) {
		t.Error("nil writer accepted a row")
	}
	w.Flush()
	w.Close()
	if a, b, c := w.Stats(); a+b+c != 0 {
		t.Error("nil writer has stats")
	}
}

// TestMigrateFromV1 opens a database created by the first schema version
// (an M2-A install) and checks that its rows survive the upgrade.
func TestMigrateFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1 db", "loopguard.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER)`,
		migrations[0],
		`INSERT INTO schema_migrations VALUES (1, 0)`,
		`INSERT INTO sessions (key, agent, session_id, started_at, last_seen_at, tool_calls) VALUES ('claude-code:old', 'claude-code', 'old', 1, 2, 5)`,
		`INSERT INTO interventions (session_key, seq, loop_key, action, mode) VALUES ('claude-code:old', 4, 'rf:x', 'nudge', 'observe')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.SchemaVersion(ctx); v != len(migrations) {
		t.Errorf("version %d after upgrade", v)
	}
	sess, err := s.GetSession(ctx, "old")
	if err != nil || sess.ToolCalls != 5 || sess.Failures != 0 {
		t.Errorf("old session = %+v, %v", sess, err)
	}
	ivs, err := s.RecentInterventions(ctx, 10)
	if err != nil || len(ivs) != 1 || ivs[0].Action != "nudge" || !ivs[0].TS.IsZero() || ivs[0].Detector != "" {
		t.Errorf("old interventions = %+v, %v", ivs, err)
	}
}
