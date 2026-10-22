package store

import (
	"context"
	"testing"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/pathid"
)

// TestMergeRepoCase: one repo stored in several spellings (desktop and
// Desktop, C: and c:) becomes one, the most recently seen spelling, in
// sessions, usage, and repo pauses; other repos are untouched.
func TestMergeRepoCase(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	spellings := []string{
		`C:\Users\me\desktop\loopguard`,
		`C:\Users\me\Desktop\loopguard`, // the latest: kept
		`c:\Users\me\desktop\loopguard`,
		`c:/Users/me/Desktop/loopguard/`,
	}
	w := s.NewWriter(WriterOptions{})
	for i, r := range spellings {
		key := "claude-code:s" + string(rune('a'+i))
		at := t0.Add(time.Duration(i) * time.Hour)
		if i == 1 {
			at = t0.Add(10 * time.Hour)
		}
		w.Enqueue(Session{Key: key, Agent: "claude-code", SessionID: "s" + string(rune('a'+i)), Repo: r, Cwd: r, StartedAt: at, LastSeenAt: at})
		w.Enqueue(Usage{SessionKey: key, SessionID: "s" + string(rune('a'+i)), Repo: r, MessageID: "m", TS: at, Model: "m", Output: 1})
	}
	w.Enqueue(Session{Key: "claude-code:other", Agent: "claude-code", SessionID: "other", Repo: `C:\Users\me\desktop\other`, StartedAt: t0, LastSeenAt: t0})
	w.Close()
	for _, target := range []string{spellings[0], spellings[1], spellings[2]} {
		if err := s.SetPause(ctx, Pause{Scope: PauseRepo, Target: target, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.mergeRepoCase(ctx, pathid.FoldKey); err != nil {
		t.Fatal(err)
	}
	repos := map[string]int{}
	rows, _ := s.ListSessions(ctx, ListOptions{Limit: 100})
	for _, r := range rows {
		repos[r.Repo]++
	}
	want := map[string]int{spellings[1]: 4, `C:\Users\me\desktop\other`: 1}
	if len(repos) != len(want) || repos[spellings[1]] != 4 || repos[`C:\Users\me\desktop\other`] != 1 {
		t.Errorf("sessions' repos = %v, want %v", repos, want)
	}
	us, _ := s.UsageRows(ctx, UsageFilter{})
	for _, u := range us {
		if u.Repo != spellings[1] {
			t.Errorf("usage row %s: repo %q", u.SessionKey, u.Repo)
		}
	}
	ps, _ := s.Pauses(ctx, t0)
	if len(ps) != 1 || ps[0].Target != spellings[1] {
		t.Errorf("pauses = %+v, want one on %q", ps, spellings[1])
	}
	sp, _ := s.repoSpellings(ctx, pathid.FoldKey)
	if len(sp) != 2 || sp[pathid.FoldKey(`c:\users\ME\desktop\LOOPGUARD`)] != spellings[1] {
		t.Errorf("spellings = %v", sp)
	}
	// Idempotent.
	if n, err := s.mergeRepoCase(ctx, pathid.FoldKey); n != 0 || err != nil {
		t.Errorf("second merge changed %d rows, %v", n, err)
	}
}

// TestMergeRepoCaseSensitive: where paths are case-sensitive (Linux),
// the spellings are different repos.
func TestMergeRepoCaseSensitive(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	w := s.NewWriter(WriterOptions{})
	w.Enqueue(Session{Key: "a:1", Agent: "a", SessionID: "1", Repo: "/home/me/App", StartedAt: t0, LastSeenAt: t0})
	w.Enqueue(Session{Key: "a:2", Agent: "a", SessionID: "2", Repo: "/home/me/app", StartedAt: t0, LastSeenAt: t0})
	w.Close()
	if n, err := s.mergeRepoCase(ctx, func(p string) string { return p }); n != 0 || err != nil {
		t.Errorf("merge changed %d rows, %v", n, err)
	}
}
