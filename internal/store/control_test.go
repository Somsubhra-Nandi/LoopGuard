package store

import (
	"context"
	"testing"
	"time"
)

func TestPauses(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetPause(ctx, Pause{Scope: PauseSession, Target: "abc", CreatedAt: now}))
	must(s.SetPause(ctx, Pause{Scope: PauseRepo, Target: `C:\r`, Until: now.Add(time.Hour), CreatedAt: now}))
	must(s.SetPause(ctx, Pause{Scope: PauseAll, Until: now.Add(-time.Second), CreatedAt: now})) // already expired
	// Replacing keeps one row per scope and target.
	must(s.SetPause(ctx, Pause{Scope: PauseSession, Target: "abc", Until: now.Add(time.Minute), CreatedAt: now}))

	ps, err := s.Pauses(ctx, now)
	must(err)
	if len(ps) != 2 || ps[0].Scope != PauseSession || ps[0].Until.IsZero() || ps[1].Target != `C:\r` {
		t.Fatalf("pauses = %+v", ps)
	}
	// They survive a reopen (a daemon restart).
	s2, err := Open(path)
	must(err)
	defer s2.Close()
	if ps, _ := s2.Pauses(ctx, now); len(ps) != 2 {
		t.Errorf("after reopen: %+v", ps)
	}
	if n, err := s.DeletePauses(ctx, PauseSession, "abc"); err != nil || n != 1 {
		t.Errorf("DeletePauses = %d, %v", n, err)
	}
	if n, _ := s.DeletePauses(ctx, "", ""); n != 1 {
		t.Errorf("delete all removed %d", n)
	}
}

func TestOutcomesAndStats(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	w := s.NewWriter(WriterOptions{})
	now := time.Now()
	w.Enqueue(Session{Key: "cc:1a2b", Agent: "claude-code", SessionID: "1a2b", StartedAt: now, LastSeenAt: now})
	w.Enqueue(Session{Key: "cc:1a2b/agent-1", Agent: "claude-code", SessionID: "1a2b", StartedAt: now, LastSeenAt: now})
	for i, iv := range []Intervention{
		{Seq: 5, LoopKey: "rf:x", Detector: "repeated_failure", Action: "nudge", Mode: ModeEnforce},
		{Seq: 9, LoopKey: "rf:x", Detector: "repeated_failure", Action: "deny", Mode: ModeEnforce},
		{Seq: 3, LoopKey: "er:y", Detector: "exact_repeat", Action: "nudge", Mode: ModeObserve},
	} {
		iv.SessionKey, iv.TS = "cc:1a2b", now.Add(time.Duration(i)*time.Second)
		w.Enqueue(iv)
	}
	w.Enqueue(OutcomeUpdate{SessionKey: "cc:1a2b", Seq: 5, LoopKey: "rf:x", Action: "nudge", Outcome: "recovered"})
	w.Enqueue(OutcomeUpdate{SessionKey: "cc:1a2b", Seq: 5, LoopKey: "rf:x", Action: "nudge", Outcome: "continued_loop"}) // already set: ignored
	w.Close()

	ivs, err := s.Interventions(ctx, "cc:1a2b", 10)
	if err != nil || len(ivs) != 3 || ivs[0].Outcome != "recovered" || ivs[1].Outcome != "" {
		t.Fatalf("interventions = %+v, %v", ivs, err)
	}
	if n, err := s.CloseOpenOutcomes(ctx); err != nil || n != 2 {
		t.Errorf("CloseOpenOutcomes = %d, %v", n, err)
	}
	stats, err := s.InterventionStats(ctx, time.Time{})
	if err != nil || len(stats) != 3 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if c := stats[0]; c.Action != "deny" || c.Mode != ModeEnforce || c.Outcome != "unknown" || c.Count != 1 {
		t.Errorf("first group = %+v", c)
	}
	if stats, _ := s.InterventionStats(ctx, now.Add(90*time.Minute)); len(stats) != 0 {
		t.Errorf("since filter: %+v", stats)
	}

	ids, err := s.SessionIDs(ctx, "1a", 5)
	if err != nil || len(ids) != 1 || ids[0] != "1a2b" {
		t.Errorf("SessionIDs = %v, %v", ids, err)
	}
	if ids, _ := s.SessionIDs(ctx, "%", 5); len(ids) != 0 {
		t.Errorf("LIKE wildcard not escaped: %v", ids)
	}
}
