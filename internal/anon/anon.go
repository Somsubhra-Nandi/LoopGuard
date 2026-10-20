// Package anon is the OS-independent path handling behind anonymization
// (capture export, payload capture): finding usernames in home directory
// paths, and replacing a home directory in every spelling it can have in a
// payload or transcript.
//
// Those paths come from the data, not from this machine: a capture made on
// Windows is exported on Linux in CI, and a macOS transcript may be read on
// Windows. So nothing here uses path/filepath or runtime.GOOS: both
// separators, drive letters, and both case conventions are handled the
// same way on every OS.
package anon

import (
	"regexp"
	"sort"
	"strings"
)

// Base returns the last element of a path written with either separator.
func Base(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

var (
	// winHomeRe finds Windows home directories: C:\Users\<name>,
	// C:/Users/<name>, /c/Users/<name> (Git Bash), /mnt/c/Users/<name>
	// (WSL), and a bare \Users\<name>. Names can contain spaces.
	winHomeRe = regexp.MustCompile(`(?i)(?:\b[a-z]:[\\/]+|/mnt/[a-z]/+|/[a-z]/+|\\)users[\\/]+([^\\/"'<>\r\n|*?:]+)`)
	// unixHomeRe finds /home/<name> and /Users/<name> (macOS). These
	// names have no spaces.
	// (Not after a drive or a letter: C:/Users/x and /c/Users/x are
	// Windows paths.)
	unixHomeRe = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.~:-])/(?:users|home)/+([^\s\\/"'<>|*?:]+)`)
)

// HomeUsers returns the usernames of the home directories that appear in
// s, in any spelling.
func HomeUsers(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{winHomeRe, unixHomeRe} {
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			if name := strings.TrimSpace(m[1]); name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// ShortName returns the stem of the Windows 8.3 short name of a long
// directory name ("Subhash Nandi" → "SUBHAS", spelled SUBHAS~1 in
// paths), or "" if the name needs no short name. Temp paths often use it:
// C:\Users\SUBHAS~1\AppData\Local\Temp.
func ShortName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r > 127:
			return "" // Windows' rule for these is codepage-dependent
		case r == ' ' || r == '.':
		case strings.ContainsRune(`"*+,/:;<=>?[\]|`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	stem := b.String()
	if len(stem) > 6 {
		stem = stem[:6]
	}
	if len(name) <= 8 && !strings.ContainsAny(name, ` .+,;=[]`) {
		return ""
	}
	return stem
}

// HomeReplacer replaces one home directory in text, in every spelling:
// either separator, Git Bash (/c/...) and WSL (/mnt/c/...) forms of a
// Windows path, Claude Code's project directory encoding (C--Users-name),
// and the 8.3 short name of its last element (C:\Users\SUBHAS~1). Matching
// ignores case, and a match must not continue into a longer name.
type HomeReplacer struct {
	re *regexp.Regexp // nil: nothing to replace
}

// NewHomeReplacer returns a replacer for the home directory h, written
// either way (C:\Users\name or /home/name).
func NewHomeReplacer(h string) *HomeReplacer {
	n := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(h), `\`, "/"), "/")
	if len(n) < 2 {
		return &HomeReplacer{}
	}
	var literal []string // spellings of the whole path, forward slashes
	switch {
	case len(n) >= 3 && n[1] == ':' && n[2] == '/' && isLetter(n[0]):
		drive, rest := strings.ToLower(n[:1]), n[2:] // rest starts with "/"
		literal = []string{n, "/" + drive + rest, "/mnt/" + drive + rest}
	default:
		literal = []string{n}
	}
	var alts []string
	add := func(form string) {
		// Either separator wherever the form has one.
		parts := strings.Split(form, "/")
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		alts = append(alts, strings.Join(parts, `[\\/]+`))
	}
	for _, f := range literal {
		add(f)
	}
	if stem := ShortName(Base(n)); stem != "" {
		parent := n[:len(n)-len(Base(n))]
		parts := strings.Split(strings.TrimRight(parent, "/"), "/")
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		alts = append(alts, strings.Join(parts, `[\\/]+`)+`[\\/]+`+regexp.QuoteMeta(stem)+`~[0-9]+`)
	}
	// Claude Code's project directory names: every non-alphanumeric
	// character of the path becomes '-'.
	if enc := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(n, "-"); strings.Trim(enc, "-") != "" {
		alts = append(alts, regexp.QuoteMeta(enc))
	}
	sort.SliceStable(alts, func(i, j int) bool { return len(alts[i]) > len(alts[j]) })
	return &HomeReplacer{re: regexp.MustCompile(`(?i)(?:` + strings.Join(alts, "|") + `)`)}
}

// Replace replaces the home directory in s with placeholder.
func (h *HomeReplacer) Replace(s, placeholder string) string {
	if h == nil || h.re == nil {
		return s
	}
	ms := h.re.FindAllStringIndex(s, -1)
	if ms == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range ms {
		if m[1] < len(s) && isAlnum(s[m[1]]) {
			continue // C:\Users\Jane is not C:\Users\Janet
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(placeholder)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// ReplaceWord replaces case-insensitive occurrences of w in s that are not
// part of a longer word (letters or digits on either side).
func ReplaceWord(s, w, repl string) string {
	lower, lw := strings.ToLower(s), strings.ToLower(w)
	if w == "" || len(lower) != len(s) || len(lw) != len(w) || !strings.Contains(lower, lw) {
		return s
	}
	var b strings.Builder
	for i := 0; ; {
		j := strings.Index(lower[i:], lw)
		if j < 0 {
			b.WriteString(s[i:])
			return b.String()
		}
		j += i
		end := j + len(w)
		if (j == 0 || !isAlnum(s[j-1])) && (end == len(s) || !isAlnum(s[end])) {
			b.WriteString(s[i:j])
			b.WriteString(repl)
		} else {
			b.WriteString(s[i:end])
		}
		i = end
	}
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isAlnum(c byte) bool { return isLetter(c) || c >= '0' && c <= '9' }
