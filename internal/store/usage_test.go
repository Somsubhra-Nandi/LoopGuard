package store

import (
	"context"
	"testing"
	"time"
)

func TestUsageKeepsTheMaximum(t *testing.T) {
	s, _ := openTemp(t)
	w := s.NewWriter(WriterOptions{})
	ts := time.UnixMilli(1_800_000_000_000)
	c1 := 0.5
	w.Enqueue(Usage{SessionKey: "claude-code:s", SessionID: "s", MessageID: "m1", TS: ts, Model: "claude-opus-5-5",
		Input: 3, Output: 10, CacheWrite: 100, CacheWrite1h: 100, CacheRead: 1000, CostUSD: &c1})
	// A later line of the same message: output grew; a stale one never
	// lowers a counter.
	w.Enqueue(Usage{SessionKey: "claude-code:s", SessionID: "s", MessageID: "m1", TS: ts, Model: "claude-opus-5-5", Output: 50})
	w.Enqueue(Usage{SessionKey: "claude-code:s/a1", SessionID: "s", Repo: "/r", MessageID: "m2", TS: ts.Add(time.Hour), Model: "x", Output: 7})
	w.Enqueue(TranscriptOffset{Path: "/t.jsonl", Offset: 1234, Fingerprint: -5})
	w.Close()
	ctx := context.Background()
	rows, err := s.UsageRows(ctx, UsageFilter{SessionID: "s"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %+v", err, rows)
	}
	m1 := rows[0]
	if m1.Input != 3 || m1.Output != 50 || m1.CacheWrite != 100 || m1.CacheWrite1h != 100 || m1.CacheRead != 1000 || m1.CostUSD == nil || *m1.CostUSD != 0.5 {
		t.Errorf("m1 %+v", m1)
	}
	if rows[1].CostUSD != nil || rows[1].Repo != "/r" {
		t.Errorf("m2 %+v", rows[1])
	}
	if r, _ := s.UsageRows(ctx, UsageFilter{SessionKey: "claude-code:s/a1"}); len(r) != 1 {
		t.Errorf("by stream: %+v", r)
	}
	if r, _ := s.UsageRows(ctx, UsageFilter{Since: ts.Add(time.Minute)}); len(r) != 1 || r[0].MessageID != "m2" {
		t.Errorf("since: %+v", r)
	}
	o, ok, err := s.GetTranscriptOffset(ctx, "/t.jsonl")
	if !ok || err != nil || o.Offset != 1234 || o.Fingerprint != -5 {
		t.Errorf("offset %+v %v %v", o, ok, err)
	}
	if _, ok, _ := s.GetTranscriptOffset(ctx, "/none"); ok {
		t.Error("offset for an unknown path")
	}
}

func TestStopRequestsTable(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	s.SetStopRequest(ctx, StopRequest{SessionID: "old", RequestedAt: now.Add(-2 * time.Hour)})
	s.SetStopRequest(ctx, StopRequest{SessionID: "new", RequestedAt: now})
	s.SetStopRequest(ctx, StopRequest{SessionID: "new", RequestedAt: now, Recorded: true})
	got, err := s.StopRequests(ctx, now.Add(-time.Hour))
	if err != nil || len(got) != 1 || got[0].SessionID != "new" || !got[0].Recorded {
		t.Fatalf("%v %+v", err, got)
	}
	s.DeleteStopRequest(ctx, "new")
	if got, _ := s.StopRequests(ctx, time.Time{}); len(got) != 0 {
		t.Errorf("after delete: %+v", got)
	}
}

func TestLoopSpans(t *testing.T) {
	s, _ := openTemp(t)
	w := s.NewWriter(WriterOptions{})
	t0 := time.UnixMilli(1_800_000_000_000)
	w.Enqueue(Session{Key: "k", Agent: "claude-code", SessionID: "s", StartedAt: t0, LastSeenAt: t0.Add(time.Hour)})
	for i := 1; i <= 6; i++ {
		w.Enqueue(Call{SessionKey: "k", Seq: uint64(i), TS: t0.Add(time.Duration(i) * time.Minute), Tool: "bash"})
	}
	w.Enqueue(Signal{SessionKey: "k", Seq: 4, Detector: "repeated_failure", LoopKey: "rf:x", Severity: 0.4, Evidence: []byte(`{"seqs":[2,3]}`)})
	w.Enqueue(Signal{SessionKey: "k", Seq: 5, Detector: "repeated_failure", LoopKey: "rf:x", Severity: 0.6, Evidence: []byte(`{"seqs":[2,3,5]}`)})
	w.Enqueue(Signal{SessionKey: "k", Seq: 6, Detector: "read_thrash", LoopKey: "rt:y", Severity: 0.5, Evidence: []byte(`{"seqs":[6]}`)})
	w.Close()
	spans, err := s.LoopSpans(context.Background(), t0, 0.55)
	if err != nil || len(spans) != 1 {
		t.Fatalf("%v %+v", err, spans)
	}
	sp := spans[0]
	if sp.LoopKey != "rf:x" || sp.FirstSeq != 2 || sp.LastSeq != 5 || !sp.Start.Equal(t0.Add(2*time.Minute)) || !sp.End.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("span %+v", sp)
	}
}
