package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/anon"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
)

// ExportOptions configures Export.
type ExportOptions struct {
	UserHome string // the user's home directory ("" → os.UserHomeDir)
	// Usernames are extra names to replace with "user" (e.g. $USERNAME).
	Usernames []string
	// Golden, if set, maps an exported payload to its expected golden
	// file, written next to it as <name>.golden.
	Golden func(payload []byte) ([]byte, error)
}

// ExportStats says what Export wrote.
type ExportStats struct {
	Files    []string          // payloads written (base names)
	Goldens  int               // golden files written
	Replaced map[string]string // learned name → its replacement
	Skipped  []string          // files that weren't JSON
}

// Placeholders for the names Export learns.
const (
	userPlaceholder    = "user"
	projectPlaceholder = "project"
)

// Export copies the captured payloads in src (one agent's capture
// directory) to out, for committing as golden test data. On top of what
// capture already did, it redacts every string again, replaces the home
// directory again, and replaces the names it learns from the payloads'
// paths: usernames (from home directories) become "user", and project
// names (the last element of each working directory) become "project",
// wherever they appear as a word. Email addresses become <email>.
func Export(src, out string, o ExportOptions) (ExportStats, error) {
	st := ExportStats{Replaced: map[string]string{}}
	files, err := filepath.Glob(filepath.Join(src, "*.json"))
	if err != nil {
		return st, err
	}
	if len(files) == 0 {
		return st, errors.New("no captured payloads in " + src)
	}
	sort.Strings(files)
	if o.UserHome == "" {
		o.UserHome, _ = os.UserHomeDir()
	}

	type payload struct {
		name string
		v    any
	}
	var payloads []payload
	x := &anonymizer{home: anon.NewHomeReplacer(o.UserHome), words: map[string]string{}}
	x.learnUser(anon.Base(o.UserHome))
	for _, u := range o.Usernames {
		x.learnUser(u)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return st, err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			st.Skipped = append(st.Skipped, filepath.Base(f))
			continue
		}
		x.learnPaths(v)
		payloads = append(payloads, payload{filepath.Base(f), v})
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return st, err
	}
	for _, p := range payloads {
		b, err := encode(walkStrings(p.v, x.clean))
		if err != nil {
			return st, err
		}
		dst := filepath.Join(out, p.name)
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return st, err
		}
		st.Files = append(st.Files, p.name)
		if o.Golden != nil {
			g, err := o.Golden(b)
			if err != nil {
				return st, errors.New(p.name + ": " + err.Error())
			}
			if err := os.WriteFile(strings.TrimSuffix(dst, ".json")+".golden", g, 0o644); err != nil {
				return st, err
			}
			st.Goldens++
		}
	}
	for w, r := range x.words {
		st.Replaced[w] = r
	}
	return st, nil
}

type anonymizer struct {
	home  *anon.HomeReplacer
	words map[string]string // name → placeholder
	short []*regexp.Regexp  // 8.3 short names of the learned usernames (JANEDO~1)
}

var (
	nameSplit = regexp.MustCompile(`[\s._-]+`)
	emailRe   = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// genericNames are directory names too common to be a project's name.
var genericNames = map[string]bool{
	"src": true, "app": true, "desktop": true, "documents": true, "downloads": true, "projects": true,
	"repos": true, "code": true, "dev": true, "work": true, "tmp": true, "temp": true, "home": true,
	"users": true, "user": true, "project": true,
}

func (x *anonymizer) learnUser(name string) {
	name = strings.TrimSpace(name)
	if len(name) < 3 || strings.Contains(name, "<") {
		return
	}
	x.words[name] = userPlaceholder
	if stem := anon.ShortName(name); stem != "" {
		x.short = append(x.short, regexp.MustCompile(`(?i)`+regexp.QuoteMeta(stem)+`~[0-9]+`))
	}
	for _, part := range nameSplit.Split(name, -1) {
		if len(part) >= 3 {
			x.words[part] = userPlaceholder
		}
	}
}

// learnPaths learns names from the payload's directories: the username
// in any home directory path, and the last element of cwd (the project).
func (x *anonymizer) learnPaths(v any) {
	walkStrings(v, func(s string) string {
		for _, u := range anon.HomeUsers(s) {
			x.learnUser(u)
		}
		return s
	})
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if cwd, ok := m["cwd"].(string); ok {
		base := anon.Base(cwd)
		if len(base) >= 3 && !genericNames[strings.ToLower(base)] && !strings.ContainsAny(base, "<>:") {
			if _, isUser := x.words[base]; !isUser {
				x.words[base] = projectPlaceholder
			}
		}
	}
}

func (x *anonymizer) clean(s string) string {
	s = redact.String(s)
	s = x.home.Replace(s, HomePlaceholder)
	for _, re := range x.short {
		s = re.ReplaceAllString(s, userPlaceholder)
	}
	s = emailRe.ReplaceAllString(s, "<email>")
	words := make([]string, 0, len(x.words))
	for w := range x.words {
		words = append(words, w)
	}
	sort.Slice(words, func(i, j int) bool {
		if len(words[i]) != len(words[j]) {
			return len(words[i]) > len(words[j])
		}
		return words[i] < words[j]
	})
	for _, w := range words {
		s = anon.ReplaceWord(s, w, x.words[w])
	}
	return s
}
