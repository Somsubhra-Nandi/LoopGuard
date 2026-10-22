package daemon

import (
	"sync"

	"github.com/Somsubhra-Nandi/loopguard/internal/pathid"
)

// repoNames gives each repo one spelling. Where paths ignore case
// (Windows, macOS), a session in C:\...\desktop\app and one in
// C:\...\Desktop\app are in the same repo: the first spelling seen (or
// stored, loaded at start) is the one recorded, so budgets, pauses, and
// the dashboard see one repo.
type repoNames struct {
	mu  sync.Mutex
	key func(string) string // pathid.Key
	m   map[string]string   // key → spelling
}

func (r *repoNames) load(m map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]string{}
	}
	for k, v := range m {
		if _, ok := r.m[k]; !ok {
			r.m[k] = v
		}
	}
}

// name returns the recorded spelling of repo, recording repo's own if it
// is new.
func (r *repoNames) name(repo string) string {
	if repo == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.key == nil {
		r.key = pathid.Key
	}
	if r.m == nil {
		r.m = map[string]string{}
	}
	k := r.key(repo)
	if s, ok := r.m[k]; ok {
		return s
	}
	r.m[k] = repo
	return repo
}
