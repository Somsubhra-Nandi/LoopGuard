package normalize

import (
	"math/bits"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
)

// TailSize is how much of a failed call's output the error signature looks
// at (LLD §4.4).
const TailSize = 8 << 10

const (
	maxKeyLines   = 5
	shingleWords  = 4
	maxKeyLineLen = 300
)

// keyWords mark the lines that carry the error (LLD §4.4 step 2), matched
// case-insensitively anywhere in the line. On top of the LLD keywords this
// covers assertion payloads that carry no error word ("Received: 8",
// "actual") and jest's "●" test headers, so that different assertions get
// different signatures.
var keyWords = []string{
	"error", "exception", "fail", "panic", "traceback", "assert", "expected", "undefined",
	"cannot", "not found", "denied", "refused", "timeout", "received", "actual", "●",
}

// keyWholeWords match only as whole words ("got 4, want 3", "left: 4").
var keyWholeWords = []string{"got", "want", "left", "right"}

// IsKeyLine reports whether a normalized line looks like it carries an
// error. It is hand-written rather than a regexp: a case-insensitive
// alternation runs on every output line and dominated the pipeline's cost.
func IsKeyLine(line string) bool {
	if line == "" {
		return false
	}
	lower := strings.ToLower(line)
	for _, w := range keyWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	for _, w := range keyWholeWords {
		for i := 0; ; {
			j := strings.Index(lower[i:], w)
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(w)
			if (start == 0 || !isWordByte(lower[start-1])) && boundaryAt(lower, end) {
				return true
			}
			i = end
		}
	}
	return false
}

// maxSimLines caps the error-bearing lines that feed the simhash.
const maxSimLines = 40

// softFailureMarkers flag a failure in the output of a command that
// exited 0 (LLD §4.4). A marker counts only at the start of a line (after
// optional whitespace), and errorMarker only for test and build runners
// (config.Thresholds.ErrorMarkerFamilies).
var softFailureMarkers = []string{
	"FAILED", errorMarker, "Traceback", "panic:", "npm ERR!", "error[E", "✕", "AssertionError",
}

const errorMarker = "Error:"

// IsSoftFailure reports whether output has a failure marker at the start
// of a line. withError says whether the "Error:" marker applies (the
// command is a test or build runner).
func IsSoftFailure(output string, withError bool) bool {
	for _, m := range softFailureMarkers {
		if (withError || m != errorMarker) && markerIndex(output, m) >= 0 {
			return true
		}
	}
	return false
}

// markerIndex returns the index of the first occurrence of m that starts a
// line (after optional spaces or tabs), or -1.
func markerIndex(s, m string) int {
	off := 0
	for {
		i := strings.Index(s[off:], m)
		if i < 0 {
			return -1
		}
		i += off
		j := i
		for j > 0 && (s[j-1] == ' ' || s[j-1] == '\t') {
			j--
		}
		if j == 0 || s[j-1] == '\n' || s[j-1] == '\r' {
			return i
		}
		off = i + len(m)
	}
}

// SoftFailureMarkers returns the failure markers in output, in marker-list
// order, and the line holding the first one found in the text. Markers
// count as in IsSoftFailure.
func SoftFailureMarkers(output string, withError bool) (markers []string, line string) {
	first := -1
	for _, m := range softFailureMarkers {
		if !withError && m == errorMarker {
			continue
		}
		i := markerIndex(output, m)
		if i < 0 {
			continue
		}
		markers = append(markers, m)
		if first < 0 || i < first {
			first = i
		}
	}
	if first >= 0 {
		start := strings.LastIndexByte(output[:first], '\n') + 1
		end := strings.IndexByte(output[first:], '\n')
		if end < 0 {
			end = len(output) - first
		}
		line = output[start : first+end]
	}
	return markers, line
}

// Signature computes the error signature of a failed call's output.
func Signature(output string) *event.ErrorSignature {
	if len(output) > TailSize {
		output = cutToLine(output[len(output)-TailSize:])
	}
	return signatureOfNormalized(Text(output))
}

// signatureOfNormalized computes the signature from text that has already
// been through Text, keeping only its last TailSize bytes.
//
// Source locations (file:line:col, file(line,col), "line N", code-frame
// gutters) are masked before hashing, so an edit that shifts the failing
// line does not change the signature. Lines keeps the unmasked text for
// evidence.
func signatureOfNormalized(norm string) *event.ErrorSignature {
	if len(norm) > TailSize {
		norm = cutToLine(norm[len(norm)-TailSize:])
	}
	lines := strings.Split(norm, "\n")
	masked := make([]string, len(lines))
	isKey := make([]bool, len(lines))
	for i, l := range lines {
		masked[i] = MaskLocations(l)
		isKey[i] = IsKeyLine(masked[i])
	}
	idx := keyLines(masked, isKey)
	key := make([]string, len(idx))
	evidence := make([]string, len(idx))
	for i, j := range idx {
		key[i] = masked[j]
		evidence[i] = clip(lines[j])
	}
	refs := fileRefs(masked)
	return &event.ErrorSignature{
		Exact: xxhash.Sum64String(strings.Join(key, "\n") + "\x00" + refs),
		Sim:   SimHash(simWords(masked, isKey), shingleWords),
		Loc:   locHash(refs),
		Lines: evidence,
	}
}

// fileRefRe finds source file references in masked lines: "path.ext:<LN>",
// "path.ext(<LN>)", and Python's `File "path", line <LN>`.
var fileRefRe = regexp.MustCompile(`([\w@~./\\-]*\.[A-Za-z0-9]{1,8})(?::|\()` + regexp.QuoteMeta(PhLoc) + `|File "([^"]+)", line ` + regexp.QuoteMeta(PhLoc))

// fileRefs returns the sorted, de-duplicated source files referenced
// anywhere in the tail (error lines and stack frames), joined by newlines.
// Line and column numbers are masked, but which files an error points at
// still tells errors apart: the same message in auth.ts and in db.ts is two
// different errors.
func fileRefs(masked []string) string {
	var refs []string
	for _, l := range masked {
		if !strings.Contains(l, PhLoc) {
			continue
		}
		for _, m := range fileRefRe.FindAllStringSubmatch(l, -1) {
			refs = append(refs, strings.ReplaceAll(m[1]+m[2], `\`, "/"))
		}
	}
	sort.Strings(refs)
	return strings.Join(slices.Compact(refs), "\n")
}

// locHash hashes the file references, or returns 0 when there are none.
func locHash(refs string) uint64 {
	if refs == "" {
		return 0
	}
	return xxhash.Sum64String(refs)
}

// simWords returns the words the simhash is computed over: every
// error-bearing line (up to maxSimLines), or the whole tail when no line
// looks like an error. Hashing the whole tail instead lets runner chatter
// (progress, passing tests, summaries) drown out a changed assertion.
func simWords(masked []string, isKey []bool) []string {
	var words []string
	n := 0
	for i, l := range masked {
		if isKey[i] {
			words = append(words, strings.Fields(l)...)
			if n++; n == maxSimLines {
				break
			}
		}
	}
	if n > 0 {
		return words
	}
	return strings.Fields(strings.Join(masked, "\n"))
}

// PhLoc replaces source line and column numbers in error signatures.
const PhLoc = "<LN>"

var (
	locColonRe  = regexp.MustCompile(`(\.[A-Za-z0-9]{1,8}):\d+(?::\d+)?\b`)
	locParenRe  = regexp.MustCompile(`(\.[A-Za-z0-9]{1,8})\(\d+,\d+\)`)
	locLineRe   = regexp.MustCompile(`\b([Ll]ine) \d+\b`)
	locGutterRe = regexp.MustCompile(`^(> )?\d+ \|`)
)

// MaskLocations replaces source line/column numbers in one normalized line
// with <LN>.
func MaskLocations(line string) string {
	if !strings.ContainsAny(line, "0123456789") {
		return line
	}
	if strings.IndexByte(line, ':') >= 0 {
		line = locColonRe.ReplaceAllString(line, "${1}:"+PhLoc)
	}
	if strings.IndexByte(line, '(') >= 0 {
		line = locParenRe.ReplaceAllString(line, "${1}("+PhLoc+")")
	}
	if strings.Contains(line, "ine ") {
		line = locLineRe.ReplaceAllString(line, "${1} "+PhLoc)
	}
	if strings.IndexByte(line, '|') >= 0 {
		line = locGutterRe.ReplaceAllString(line, "${1}"+PhLoc+" |")
	}
	return line
}

// cutToLine drops a partial first line left over from cutting a tail.
func cutToLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 && i+1 < len(s) {
		return s[i+1:]
	}
	return s
}

// keyLines returns the indexes of the first five unique lines that look
// like errors, or of the last five non-empty lines when none do.
func keyLines(lines []string, isKey []bool) []int {
	var out []int
	seen := make(map[string]bool)
	for i, l := range lines {
		if !isKey[i] || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, i)
		if len(out) == maxKeyLines {
			return out
		}
	}
	if len(out) > 0 {
		return out
	}
	for i := len(lines) - 1; i >= 0 && len(out) < maxKeyLines; i-- {
		if lines[i] != "" {
			out = append(out, i)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func clip(s string) string {
	if len(s) > maxKeyLineLen {
		return s[:maxKeyLineLen]
	}
	return s
}

// SimHash computes a 64-bit simhash over k-word shingles. Fewer than k words
// form a single shingle.
func SimHash(words []string, k int) uint64 {
	if len(words) == 0 {
		return 0
	}
	var acc [64]int32
	add := func(h uint64) {
		for b := 0; b < 64; b++ {
			if h&(1<<b) != 0 {
				acc[b]++
			} else {
				acc[b]--
			}
		}
	}
	if len(words) < k {
		add(xxhash.Sum64String(strings.Join(words, " ")))
	} else {
		d := xxhash.New()
		for i := 0; i+k <= len(words); i++ {
			d.Reset()
			for j, w := range words[i : i+k] {
				if j > 0 {
					_, _ = d.WriteString(" ")
				}
				_, _ = d.WriteString(w)
			}
			add(d.Sum64())
		}
	}
	var h uint64
	for b := 0; b < 64; b++ {
		if acc[b] > 0 {
			h |= 1 << b
		}
	}
	return h
}

// Hamming returns the number of differing bits.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// SameError reports whether two signatures describe the same error: equal
// key lines, or simhashes within maxDistance bits that point at the same
// source files (LLD §4.4 step 5).
func SameError(a, b *event.ErrorSignature, maxDistance int) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Exact == b.Exact || a.Loc == b.Loc && Hamming(a.Sim, b.Sim) <= maxDistance
}

// FailingTests parses the number of failing tests (or compile errors) from
// normalized output, or returns -1 if no count is found. Counts come from
// pytest/jest/mocha/cargo summaries ("3 failed"), tsc ("Found 3 errors"),
// and go test ("--- FAIL:" lines). The last count in the output wins.
func FailingTests(norm string) int {
	lower := strings.ToLower(norm)
	best, bestPos := -1, -1
	for _, w := range []string{" failed", " failing", " failures", " failure"} {
		if n, pos := lastCountBefore(lower, w); pos > bestPos {
			best, bestPos = n, pos
		}
	}
	if best >= 0 {
		return best
	}
	if n, pos := lastCountAfter(norm, "Found ", " error"); pos >= 0 {
		return n
	}
	if n := strings.Count(norm, "--- FAIL:"); n > 0 {
		return n
	}
	return -1
}

// lastCountBefore finds the last "<digits><suffix>" where the digits start
// a word and the suffix ends one, and returns the number and its position.
func lastCountBefore(s, suffix string) (int, int) {
	for end := len(s); end > 0; {
		i := strings.LastIndex(s[:end], suffix)
		if i < 0 {
			break
		}
		end = i
		j := i
		for j > 0 && isDigit(s[j-1]) {
			j--
		}
		if j == i || (j > 0 && isWordByte(s[j-1])) || !boundaryAt(s, i+len(suffix)) {
			continue
		}
		if n, err := strconv.Atoi(s[j:i]); err == nil {
			return n, j
		}
	}
	return -1, -1
}

// lastCountAfter finds the last "<prefix><digits><suffix>" and returns the
// number and its position.
func lastCountAfter(s, prefix, suffix string) (int, int) {
	for end := len(s); end > 0; {
		i := strings.LastIndex(s[:end], prefix)
		if i < 0 {
			break
		}
		end = i
		j := i + len(prefix)
		k := j
		for k < len(s) && isDigit(s[k]) {
			k++
		}
		if k == j || !strings.HasPrefix(s[k:], suffix) {
			continue
		}
		if n, err := strconv.Atoi(s[j:k]); err == nil {
			return n, i
		}
	}
	return -1, -1
}
