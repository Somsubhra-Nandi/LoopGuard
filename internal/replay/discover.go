package replay

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultPath returns the directory Claude Code keeps transcripts in:
// $CLAUDE_CONFIG_DIR/projects if set, else ~/.claude/projects.
func DefaultPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// EncodeProjectDir encodes a directory the way Claude Code names project
// directories: every character that is not an ASCII letter or digit
// becomes "-".
func EncodeProjectDir(dir string) string {
	b := []byte(dir)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// Filter selects transcripts.
type Filter struct {
	// Since keeps files modified at or after this time (zero: all).
	Since time.Time
	// Repo keeps projects whose directory is Repo or inside it.
	Repo string
	// Session keeps one session (and its subagents) by id or id prefix.
	Session string
}

// Discover finds transcripts under root: <project>/<session>.jsonl main
// sessions and <project>/<session>/subagents/agent-<id>.jsonl subagents,
// sorted by path. Unreadable directories are skipped.
func Discover(root string, f Filter) ([]Stream, error) {
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	repo := ""
	if f.Repo != "" {
		abs := f.Repo
		if !hasDrive(abs) {
			// A Windows path (C:\...) is absolute wherever replay runs, since
			// transcripts can be copied between machines.
			var err error
			if abs, err = filepath.Abs(abs); err != nil {
				return nil, err
			}
		}
		repo = strings.ToLower(strings.TrimRight(EncodeProjectDir(abs), "-"))
	}
	var out []Stream
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		if repo != "" && !matchesRepo(strings.ToLower(p.Name()), repo) {
			continue
		}
		dir := filepath.Join(root, p.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			switch {
			case !e.IsDir() && strings.HasSuffix(name, ".jsonl"):
				id := strings.TrimSuffix(name, ".jsonl")
				if keep(filepath.Join(dir, name), id, f) {
					out = append(out, Stream{Path: filepath.Join(dir, name), SessionID: id, Project: p.Name()})
				}
			case e.IsDir():
				subs, _ := filepath.Glob(filepath.Join(dir, name, "subagents", "agent-*.jsonl"))
				for _, s := range subs {
					agent := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(s), "agent-"), ".jsonl")
					if keep(s, name, f) {
						out = append(out, Stream{Path: s, SessionID: name, AgentID: agent, Project: p.Name()})
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func hasDrive(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

// matchesRepo: the project is the repo itself or a directory inside it.
func matchesRepo(project, repo string) bool {
	return project == repo || strings.HasPrefix(project, repo+"-")
}

func keep(path, sessionID string, f Filter) bool {
	if f.Session != "" && !strings.HasPrefix(sessionID, f.Session) {
		return false
	}
	if f.Since.IsZero() {
		return true
	}
	info, err := os.Stat(path)
	return err == nil && !info.ModTime().Before(f.Since)
}
