package store

import (
	"context"
	"sort"

	"github.com/Somsubhra-Nandi/loopguard/internal/pathid"
)

// RepoSpellings returns, for each repo's identity (pathid.Key), the
// spelling stored for it: the one of its most recently seen session.
func (s *Store) RepoSpellings(ctx context.Context) (map[string]string, error) {
	return s.repoSpellings(ctx, pathid.Key)
}

func (s *Store) repoSpellings(ctx context.Context, key func(string) string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo, MAX(last_seen_at) FROM sessions WHERE COALESCE(repo, '') != '' GROUP BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type spelling struct {
		repo string
		seen int64
	}
	var all []spelling
	for rows.Next() {
		var sp spelling
		if err := rows.Scan(&sp.repo, &sp.seen); err != nil {
			return nil, err
		}
		all = append(all, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Most recent first; ties by spelling, so the choice is stable.
	sort.Slice(all, func(i, j int) bool {
		if all[i].seen != all[j].seen {
			return all[i].seen > all[j].seen
		}
		return all[i].repo < all[j].repo
	})
	out := map[string]string{}
	for _, sp := range all {
		if k := key(sp.repo); out[k] == "" {
			out[k] = sp.repo
		}
	}
	return out, nil
}

// mergeRepoCase rewrites the repos that are one repo under key but were
// stored with different spellings (C:\...\desktop\app and C:\...\Desktop\app
// on Windows) to one spelling, the most recently seen: in sessions, usage,
// and repo pauses. It returns the rows changed.
func (s *Store) mergeRepoCase(ctx context.Context, key func(string) string) (int64, error) {
	canon, err := s.repoSpellings(ctx, key)
	if err != nil {
		return 0, err
	}
	var spellings []string
	for _, q := range []string{
		`SELECT DISTINCT repo FROM sessions WHERE COALESCE(repo, '') != ''`,
		`SELECT DISTINCT repo FROM usage WHERE COALESCE(repo, '') != ''`,
		`SELECT DISTINCT target FROM pauses WHERE scope = 'repo'`,
	} {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				rows.Close()
				return 0, err
			}
			spellings = append(spellings, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
	}
	renames := map[string]string{}
	for _, r := range spellings {
		if c, ok := canon[key(r)]; ok && c != r {
			renames[r] = c
		}
	}
	if len(renames) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int64
	for from, to := range renames {
		for _, q := range []string{
			`UPDATE sessions SET repo = ?1 WHERE repo = ?2`,
			`UPDATE usage SET repo = ?1 WHERE repo = ?2`,
			// A pause of the other spelling yields to one already there.
			`UPDATE OR IGNORE pauses SET target = ?1 WHERE scope = 'repo' AND target = ?2`,
			`DELETE FROM pauses WHERE scope = 'repo' AND target = ?2`,
		} {
			res, err := tx.ExecContext(ctx, q, to, from)
			if err != nil {
				return 0, err
			}
			c, _ := res.RowsAffected()
			n += c
		}
	}
	return n, tx.Commit()
}
