package normalize

import (
	"regexp"
	"strings"
)

// MaxInput is the size cap applied before normalizing (LLD §4.1).
const MaxInput = 64 << 10

// Placeholders substituted for noisy tokens.
const (
	PhTS  = "<TS>"
	PhDur = "<DUR>"
	PhID  = "<ID>"
	PhTmp = "<TMP>"
	PhN   = "<N>"
)

// tempPathRe matches absolute temp paths on Unix, macOS, and Windows,
// including the 8.3 short form of the user directory and usernames with
// spaces. Group 1 is the delimiter before the path, which is kept.
var tempPathRe = regexp.MustCompile(`(?i)(^|[\s'"` + "`" + `=(\[,:])(?:` +
	`(?:/private)?/var/folders(?:/[^\s'"` + "`" + `:,;)\]]*)?` +
	`|/(?:var/)?tmp\b(?:/[^\s'"` + "`" + `:,;)\]]*)?` +
	`|[a-z]:[\\/]+users[\\/]+[^\\/\r\n]+?[\\/]+appdata[\\/]+local[\\/]+temp\b(?:[\\/][^\s'"` + "`" + `,;)\]]*)?` +
	`|[a-z]:[\\/]+windows[\\/]+temp\b(?:[\\/][^\s'"` + "`" + `,;)\]]*)?` +
	`|%te?mp%(?:[\\/][^\s'"` + "`" + `,;)\]]*)?` +
	`|\$(?:\{tmpdir\}|tmpdir|env:te?mp)(?:[\\/][^\s'"` + "`" + `,;)\]]*)?` +
	`)`)

// rfcTimeRe matches RFC 1123/2822 and ctime-style timestamps. Only run on
// lines that contain a weekday abbreviation.
var rfcTimeRe = regexp.MustCompile(`\b(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun),? +(?:\d{1,2} +[A-Z][a-z]{2} +\d{4}|[A-Z][a-z]{2} +\d{1,2})(?: +\d{1,2}:\d{2}:\d{2}(?:\.\d+)?)?(?: +\d{4})?(?: +(?:[A-Z]{2,5}|[+-]\d{4}))?`)

var weekdays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// Text normalizes a command or tool output (LLD §4.1): it strips ANSI
// escapes and carriage-return progress overwrites, replaces volatile tokens
// with placeholders, collapses whitespace, and drops empty lines. Line
// structure is kept; lines are joined with "\n".
func Text(s string) string {
	if len(s) > MaxInput {
		s = s[:MaxInput]
	}
	s = stripANSI(s)
	var b strings.Builder
	b.Grow(len(s))
	for len(s) > 0 {
		var line string
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			line, s = s[:i], s[i+1:]
		} else {
			line, s = s, ""
		}
		line = strings.TrimRight(line, "\r")
		if i := strings.LastIndexByte(line, '\r'); i >= 0 {
			// Progress bars rewrite the line with \r; keep what was shown last.
			line = line[i+1:]
		}
		if mayHaveTempPath(line) {
			line = tempPathRe.ReplaceAllString(line, "${1}"+PhTmp)
		}
		if hasWeekday(line) {
			line = rfcTimeRe.ReplaceAllString(line, PhTS)
		}
		mark := b.Len()
		if mark > 0 {
			b.WriteByte('\n')
		}
		if !scanLine(&b, line) {
			// Empty after trimming: drop the line (and the separator).
			trimTo(&b, mark)
		}
	}
	return b.String()
}

// trimTo truncates the builder back to n bytes.
func trimTo(b *strings.Builder, n int) {
	if b.Len() == n {
		return
	}
	s := b.String()[:n]
	b.Reset()
	b.WriteString(s)
}

func mayHaveTempPath(line string) bool {
	return strings.Contains(line, "tmp") || strings.Contains(line, "TMP") ||
		strings.Contains(line, "emp") || strings.Contains(line, "EMP") ||
		strings.Contains(line, "var/folders")
}

func hasWeekday(line string) bool {
	if len(line) < 10 {
		return false
	}
	for _, d := range weekdays {
		if strings.Contains(line, d) {
			return true
		}
	}
	return false
}

// stripANSI removes CSI and OSC escape sequences and other ESC-prefixed
// two-byte sequences.
func stripANSI(s string) string {
	if strings.IndexByte(s, 0x1b) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != 0x1b {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			break
		}
		switch s[i+1] {
		case '[': // CSI: parameters and intermediates, then a final byte in @..~
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
		case ']': // OSC: terminated by BEL or ESC \
			j := i + 2
			for j < len(s) && s[j] != 0x07 && s[j] != 0x1b {
				j++
			}
			if j < len(s) && s[j] == 0x1b {
				j++ // the '\' of ST
			}
			i = j
		default:
			i++
		}
	}
	return b.String()
}

func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\v' || c == '\f' }

// scanLine writes the normalized line to b and reports whether it wrote
// anything. Whitespace runs collapse to one space and the line is trimmed.
func scanLine(b *strings.Builder, s string) bool {
	wrote := false
	pendingSpace := false
	emit := func(t string) {
		if pendingSpace && wrote {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteString(t)
		wrote = true
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if isSpace(c) {
			pendingSpace = true
			i++
			continue
		}
		if !isWordByte(c) {
			// Copy non-word runs (punctuation, UTF-8) in one go.
			j := i + 1
			for j < len(s) && !isWordByte(s[j]) && !isSpace(s[j]) {
				j++
			}
			emit(s[i:j])
			i = j
			continue
		}
		// At the start of a word: i is a word boundary by construction.
		if ph, n := matchToken(s, i); n > 0 {
			emit(ph)
			i += n
			continue
		}
		j := i + 1
		for j < len(s) && isWordByte(s[j]) {
			j++
		}
		emit(s[i:j])
		i = j
	}
	return wrote
}

// boundaryAt reports whether position j ends a word (RE2 \b semantics).
func boundaryAt(s string, j int) bool { return j >= len(s) || !isWordByte(s[j]) }

// matchToken tries every placeholder rule at a word start and returns the
// placeholder and the number of bytes it replaces (0 if none apply).
func matchToken(s string, i int) (string, int) {
	if n := matchHexAddr(s, i); n > 0 {
		return PhID, n
	}
	if n := matchUUID(s, i); n > 0 {
		return PhID, n
	}
	if isDigit(s[i]) {
		if n := matchISOTime(s, i); n > 0 {
			return PhTS, n
		}
		if n := matchClock(s, i); n > 0 {
			return PhTS, n
		}
	}
	if n := matchULID(s, i); n > 0 {
		return PhID, n
	}
	if n := matchSHA(s, i); n > 0 {
		return PhID, n
	}
	if !isDigit(s[i]) {
		return "", 0
	}
	if n := matchDuration(s, i); n > 0 {
		return PhDur, n
	}
	j := i
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	if !boundaryAt(s, j) {
		return "", 0
	}
	if j-i >= 4 || afterPIDWord(s, i) {
		return PhN, j - i
	}
	return "", 0
}

// matchHexAddr: 0x followed by hex digits.
func matchHexAddr(s string, i int) int {
	if i+2 >= len(s) || s[i] != '0' || (s[i+1] != 'x' && s[i+1] != 'X') || !isHex(s[i+2]) {
		return 0
	}
	j := i + 2
	for j < len(s) && isHex(s[j]) {
		j++
	}
	return j - i
}

// matchUUID: 8-4-4-4-12 hex digits.
func matchUUID(s string, i int) int {
	if i+36 > len(s) {
		return 0
	}
	for k := 0; k < 36; k++ {
		c := s[i+k]
		switch k {
		case 8, 13, 18, 23:
			if c != '-' {
				return 0
			}
		default:
			if !isHex(c) {
				return 0
			}
		}
	}
	if !boundaryAt(s, i+36) {
		return 0
	}
	return 36
}

// digitsAt returns how many digits start at i, up to max.
func digitsAt(s string, i, max int) int {
	n := 0
	for i+n < len(s) && n < max && isDigit(s[i+n]) {
		n++
	}
	return n
}

// matchISOTime: YYYY-MM-DD, optionally followed by [T ]HH:MM[:SS[.frac]]
// and a zone (Z or ±HH[:]MM).
func matchISOTime(s string, i int) int {
	if digitsAt(s, i, 4) != 4 || i+10 > len(s) || s[i+4] != '-' || digitsAt(s, i+5, 2) != 2 ||
		s[i+7] != '-' || digitsAt(s, i+8, 2) != 2 {
		return 0
	}
	j := i + 10
	if j+5 < len(s) && (s[j] == 'T' || s[j] == ' ') && digitsAt(s, j+1, 2) == 2 && s[j+3] == ':' && digitsAt(s, j+4, 2) == 2 {
		j += 6
		if j+2 < len(s) && s[j] == ':' && digitsAt(s, j+1, 2) == 2 {
			j += 3
			if j+1 < len(s) && (s[j] == '.' || s[j] == ',') && isDigit(s[j+1]) {
				j++
				for j < len(s) && isDigit(s[j]) {
					j++
				}
			}
		}
		switch {
		case j < len(s) && s[j] == 'Z':
			j++
		case j+2 < len(s) && (s[j] == '+' || s[j] == '-') && digitsAt(s, j+1, 2) == 2:
			k := j + 3
			if k < len(s) && s[k] == ':' {
				k++
			}
			if digitsAt(s, k, 2) == 2 {
				j = k + 2
			}
		}
	}
	if !boundaryAt(s, j) {
		return 0
	}
	return j - i
}

// matchClock: H:MM:SS or HH:MM:SS with an optional fraction.
func matchClock(s string, i int) int {
	h := digitsAt(s, i, 2)
	if h == 0 {
		return 0
	}
	j := i + h
	if j+6 > len(s) || s[j] != ':' || digitsAt(s, j+1, 2) != 2 || s[j+3] != ':' || digitsAt(s, j+4, 2) != 2 {
		return 0
	}
	if s[j+1] > '5' || s[j+4] > '5' {
		return 0
	}
	j += 6
	if j+1 < len(s) && (s[j] == '.' || s[j] == ',') && isDigit(s[j+1]) {
		j++
		for j < len(s) && isDigit(s[j]) {
			j++
		}
	}
	if !boundaryAt(s, j) {
		return 0
	}
	return j - i
}

// matchULID: 26 Crockford base32 characters, at least one digit and one
// letter, first character 0-7.
func matchULID(s string, i int) int {
	if i+26 > len(s) || s[i] < '0' || s[i] > '7' || !boundaryAt(s, i+26) {
		return 0
	}
	letters := false
	for k := 0; k < 26; k++ {
		c := s[i+k]
		switch {
		case isDigit(c):
		case c >= 'A' && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U':
			letters = true
		default:
			return 0
		}
	}
	if !letters {
		return 0
	}
	return 26
}

// matchSHA: a whole word of at least 7 hex characters mixing digits and
// letters (git SHAs, other hex IDs). Requiring both avoids English words
// like "defaced" and plain numbers.
func matchSHA(s string, i int) int {
	j := i
	digits, letters := false, false
	for j < len(s) && isHex(s[j]) {
		if isDigit(s[j]) {
			digits = true
		} else {
			letters = true
		}
		j++
	}
	if j-i < 7 || !digits || !letters || !boundaryAt(s, j) {
		return 0
	}
	return j - i
}

// durUnits are the units allowed directly after a number ("450ms",
// "3m2s"), longest first.
var durUnits = []string{"ns", "us", "µs", "ms", "s", "m", "h"}

// durWords are the units allowed after a number and one space ("1.2 s").
var durWords = []string{"seconds", "second", "secs", "sec", "minutes", "minute", "mins", "min", "hours", "hour", "hrs", "hr", "ms", "s"}

// matchDuration: number[.frac] followed by a unit, possibly compound
// ("1h2m3.5s"), or number[.frac] + space + unit word.
func matchDuration(s string, i int) int {
	j := i
	units := 0
	for {
		n := digitsAt(s, j, 32)
		if n == 0 {
			break
		}
		k := j + n
		if k+1 < len(s) && s[k] == '.' && isDigit(s[k+1]) {
			k++
			for k < len(s) && isDigit(s[k]) {
				k++
			}
		}
		u := unitAt(s, k, durUnits)
		if u == 0 {
			if units == 0 && k < len(s) && s[k] == ' ' {
				if w := unitAt(s, k+1, durWords); w > 0 && boundaryAt(s, k+1+w) {
					return k + 1 + w - i
				}
			}
			break
		}
		j = k + u
		units++
	}
	if units == 0 || !boundaryAt(s, j) {
		return 0
	}
	return j - i
}

func unitAt(s string, i int, units []string) int {
	for _, u := range units {
		if strings.HasPrefix(s[i:], u) {
			return len(u)
		}
	}
	return 0
}

// afterPIDWord reports whether the number at i follows "pid", "process", or
// "port" plus a separator, as in "pid 123" or "port=80".
func afterPIDWord(s string, i int) bool {
	j := i - 1
	for j >= 0 && (s[j] == ' ' || s[j] == ':' || s[j] == '=' || s[j] == '#') {
		j--
	}
	if j == i-1 {
		return false
	}
	end := j + 1
	for j >= 0 && isWordByte(s[j]) {
		j--
	}
	w := strings.ToLower(s[j+1 : end])
	return w == "pid" || w == "process" || w == "port"
}
