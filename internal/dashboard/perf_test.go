package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// pageBudget is how long a page may take with 500 sessions in the
// database. The race detector slows everything several times, so it gets
// more room there (the budget is checked without -race).
const pageBudget = 200 * time.Millisecond

// TestPagePerformance seeds 500 sessions over 30 days, each the size of a
// real one (the private corpus averages 95 API messages and 127 tool calls
// per session), every 5th with a loop and interventions, and times the
// pages. The 30-day totals are a snapshot (snapshot.go); computing one
// cold is logged, not limited.
func TestPagePerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	f := newFixture(t)
	// A few large transactions (the default writer flushes every 100 rows).
	w := f.st.NewWriter(store.WriterOptions{QueueSize: 100_000, BatchSize: 20_000, FlushEvery: time.Hour})
	for i := 0; i < 500; i++ {
		sid := fmt.Sprintf("perf%04d-0000-4000-8000-000000000000", i)
		key := "claude-code:" + sid
		repo := fmt.Sprintf("/work/repo%02d", i%25)
		start := at.Add(-time.Duration(i) * 85 * time.Minute) // spread over ~30 days
		w.Enqueue(store.Session{Key: key, Agent: "claude-code", SessionID: sid, Repo: repo, Cwd: repo, StartedAt: start,
			LastSeenAt: start.Add(40 * time.Minute), EndedAt: start.Add(40 * time.Minute), ToolCalls: 20, Failures: 2})
		if i%10 == 0 { // a subagent stream
			w.Enqueue(store.Session{Key: key + "/agent-1", Agent: "claude-code", SessionID: sid, Repo: repo, Cwd: repo, StartedAt: start,
				LastSeenAt: start.Add(30 * time.Minute), ToolCalls: 5})
		}
		for c := 1; c <= 60; c++ {
			w.Enqueue(store.Call{SessionKey: key, Seq: uint64(c), TS: start.Add(time.Duration(c) * time.Minute), Tool: "bash", Family: "npm test", IsError: c%10 == 0})
		}
		for m := 0; m < 95; m++ {
			w.Enqueue(store.Usage{SessionKey: key, SessionID: sid, Repo: repo, MessageID: fmt.Sprintf("%s-%d", sid, m), TS: start.Add(time.Duration(m) * time.Minute),
				Model: "claude-opus-5-5", Input: 5, Output: 900, CacheRead: 40000, CacheWrite: 1500, CacheWrite1h: 1500})
		}
		if i%5 == 0 {
			w.Enqueue(store.Signal{SessionKey: key, Seq: 8, Detector: "repeated_failure", LoopKey: "rf:" + sid, Severity: 0.8, Count: 4,
				Evidence: []byte(`{"seqs":[3,4,5,6,7,8]}`)})
			w.Enqueue(store.Intervention{SessionKey: key, Seq: 8, TS: start.Add(8 * time.Minute), LoopKey: "rf:" + sid, Detector: "repeated_failure",
				Action: "nudge", Mode: store.ModeEnforce, Outcome: "recovered"})
		}
	}
	w.Close()
	if _, dropped, failed := w.Stats(); dropped+failed > 0 {
		t.Fatalf("seeding lost %d rows", dropped+failed)
	}
	if n, _ := f.st.ListSessions(context.Background(), store.ListOptions{Limit: 100000}); len(n) < 550 {
		t.Fatalf("seeded %d streams", len(n))
	}

	start := time.Now()
	f.d.snapMu.Lock()
	f.d.snap = f.d.computeSnapshot(context.Background())
	f.d.snapMu.Unlock()
	t.Logf("%-55s %v (cold, once per %v)", "30-day snapshot", time.Since(start).Round(time.Millisecond), snapshotFresh)
	c := f.login()
	for _, p := range []string{"/ui/sessions?from=2026-09-01&to=2026-10-10", "/ui/sessions?from="} {
		start = time.Now()
		if r := c.get(p); r.Code != http.StatusOK {
			t.Fatal(r.Code)
		}
		t.Logf("%-55s %v (other dates than the last 30 days: not cached)", p, time.Since(start).Round(time.Millisecond))
	}
	limit := pageBudget

	if raceEnabled {
		limit *= 10
	}
	for _, p := range []string{"/ui/", "/ui/sessions", "/ui/sessions?sort=cost", "/ui/sessions?sort=loops&project=%2Fwork%2Frepo03",
		"/ui/settings", "/ui/sessions/perf0005-0000-4000-8000-000000000000"} {

		best := time.Hour
		for run := 0; run < 3; run++ {
			start := time.Now()
			r := c.get(p)
			took := time.Since(start)
			if r.Code != http.StatusOK {
				t.Fatalf("%s: %d", p, r.Code)
			}
			best = min(best, took)
		}
		t.Logf("%-55s %v", p, best.Round(time.Millisecond))
		if best > limit {
			t.Errorf("%s took %v with 500 sessions (limit %v)", p, best, limit)
		}
	}
}
