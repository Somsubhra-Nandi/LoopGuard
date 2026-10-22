package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/home"
	"github.com/Somsubhra-Nandi/loopguard/internal/store"
)

// TestBackfillOnStart: a known session's transcript that grew while the
// daemon was down (a subagent's too) is read when the next daemon starts,
// without any hook event; one already read to the end is not read again.
func TestBackfillOnStart(t *testing.T) {
	dir := homeWithConfig(t, "mode: observe\n")
	d := startDaemon(t, dir, fastTail)
	s := newScenario(d, "bf")
	s.prompt("go")
	spend(t, s.transcript(), "msg_1", 0.4)
	d.waitCost(t, "bf", 0.4)
	done := newScenario(d, "done")
	done.prompt("go")
	spend(t, done.transcript(), "msg_d", 0.1)
	d.waitCost(t, "done", 0.1)
	if err := d.stop(); err != nil {
		t.Fatal(err)
	}

	// While the daemon is down: more usage, and a subagent.
	spend(t, s.transcript(), "msg_2", 0.2)
	sub := filepath.Join(strings.TrimSuffix(s.transcript(), ".jsonl"), "subagents", "agent-x1.jsonl")
	spend(t, sub, "msg_sub", 0.3)

	d2 := startDaemon(t, dir, func(o *Options) { o.TailEvery = time.Hour }) // no live polls
	st, err := store.Open(filepath.Join(dir, home.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	var rows []store.Usage
	for {
		rows, _ = st.UsageRows(ctx, store.UsageFilter{SessionID: "bf"})
		if len(rows) == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.MessageID] = r.SessionKey
	}
	want := map[string]string{"msg_1": "claude-code:bf", "msg_2": "claude-code:bf", "msg_sub": "claude-code:bf/x1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("usage rows after backfill = %v, want %v", got, want)
	}
	if today := d2.usage(t).Today; fmt.Sprintf("%.2f", today.CostUSD) != "1.00" {
		t.Errorf("today = $%.2f, want $1.00 (each message once)", today.CostUSD)
	}
	for _, p := range []string{s.transcript(), sub} {
		if o, ok, _ := st.GetTranscriptOffset(ctx, p); !ok || o.Offset == 0 {
			t.Errorf("no offset stored for %s", p)
		}
	}
}
