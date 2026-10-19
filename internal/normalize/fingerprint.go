package normalize

import (
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
)

// HashString is the content hash used everywhere: xxhash64.
func HashString(s string) uint64 { return xxhash.Sum64String(s) }

// NormalizePath makes a file path comparable across tools and platforms:
// backslashes become slashes, a relative path is resolved against cwd,
// "." and ".." are cleaned, and a Windows drive letter is lower-cased.
// Case is otherwise kept, since most file systems outside Windows are case
// sensitive.
func NormalizePath(p, cwd string) string {
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if !isAbsPath(p) && cwd != "" {
		p = strings.ReplaceAll(cwd, `\`, "/") + "/" + p
	}
	vol := ""
	if hasDrive(p) {
		vol, p = strings.ToLower(p[:2]), p[2:]
	}
	return vol + path.Clean(p)
}

func hasDrive(p string) bool {
	return len(p) >= 2 && p[1] == ':' && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

func isAbsPath(p string) bool {
	return strings.HasPrefix(p, "/") || hasDrive(p) && len(p) >= 3 && p[2] == '/'
}

// toolInput holds the input fields any supported tool may use.
type toolInput struct {
	Command      string          `json:"command"`
	FilePath     string          `json:"file_path"`
	NotebookPath string          `json:"notebook_path"`
	Path         string          `json:"path"`
	OldString    string          `json:"old_string"`
	NewString    string          `json:"new_string"`
	NewSource    string          `json:"new_source"`
	Content      string          `json:"content"`
	Edits        []editInput     `json:"edits"`
	Offset       json.Number     `json:"offset"`
	Limit        json.Number     `json:"limit"`
	Pattern      string          `json:"pattern"`
	Patch        string          `json:"patch"`
	Input        string          `json:"input"`
	Todos        []todoInput     `json:"todos"`
	Status       string          `json:"status"`
	Raw          json.RawMessage `json:"-"`
}

type editInput struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

type todoInput struct {
	Status string `json:"status"`
}

func parseInput(raw json.RawMessage) toolInput {
	var in toolInput
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &in)
	}
	in.Raw = raw
	return in
}

// fileOf returns the file a tool input targets.
func (in toolInput) fileOf() string {
	switch {
	case in.FilePath != "":
		return in.FilePath
	case in.NotebookPath != "":
		return in.NotebookPath
	}
	return ""
}

// editPairs returns the (old, new) hash pairs of an edit input.
func (in toolInput) editPairs() []event.EditPair {
	if len(in.Edits) > 0 {
		out := make([]event.EditPair, 0, len(in.Edits))
		for _, e := range in.Edits {
			out = append(out, event.EditPair{Old: HashString(e.OldString), New: HashString(e.NewString)})
		}
		return out
	}
	newStr := in.NewString
	if in.NewSource != "" {
		newStr = in.NewSource
	}
	return []event.EditPair{{Old: HashString(in.OldString), New: HashString(newStr)}}
}

// canonicalInput returns the canonical form of a tool input (LLD §4.2) and,
// for bash, the parsed command.
func canonicalInput(tool string, in toolInput, cwd string) (string, *Command) {
	switch {
	case tool == event.ToolBash:
		c := ParseCommand(redact.String(in.Command))
		return c.Canonical, &c
	case tool == event.ToolEdit:
		var b strings.Builder
		b.WriteString(NormalizePath(in.fileOf(), cwd))
		for _, p := range in.editPairs() {
			b.WriteString("\x00" + strconv.FormatUint(p.Old, 16) + ":" + strconv.FormatUint(p.New, 16))
		}
		return b.String(), nil
	case tool == event.ToolWrite:
		return NormalizePath(in.fileOf(), cwd) + "\x00" + strconv.FormatUint(HashString(in.Content), 16), nil
	case tool == event.ToolRead:
		return NormalizePath(in.fileOf(), cwd) + "\x00" + in.Offset.String() + "\x00" + in.Limit.String(), nil
	case tool == event.ToolGrep || tool == event.ToolGlob:
		return in.Pattern + "\x00" + NormalizePath(in.Path, cwd), nil
	case tool == event.ToolPatch:
		return canonicalPatch(patchText(in), cwd), nil
	default:
		return CanonicalJSON(in.Raw), nil
	}
}

func patchText(in toolInput) string {
	switch {
	case in.Patch != "":
		return in.Patch
	case in.Input != "":
		return in.Input
	}
	return in.Command
}

// canonicalPatch reduces an apply_patch body to a sorted list of
// (path, hunk hash) entries.
func canonicalPatch(body, cwd string) string {
	var entries []string
	var file string
	var hunk strings.Builder
	flush := func() {
		if file != "" {
			entries = append(entries, file+"\x00"+strconv.FormatUint(HashString(hunk.String()), 16))
		}
		hunk.Reset()
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if p, ok := PatchFileHeader(line); ok {
			flush()
			file = NormalizePath(p, cwd)
			continue
		}
		if strings.HasPrefix(line, "@@") {
			flush()
			continue
		}
		if strings.HasPrefix(line, "*** ") {
			continue
		}
		hunk.WriteString(line)
		hunk.WriteByte('\n')
	}
	flush()
	sort.Strings(entries)
	return strings.Join(entries, "\n")
}

// PatchFileHeader parses an apply_patch file header ("*** Update File:
// path", "*** Add File:", "*** Delete File:").
func PatchFileHeader(line string) (string, bool) {
	for _, p := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
		if strings.HasPrefix(line, p) {
			return strings.TrimSpace(line[len(p):]), true
		}
	}
	return "", false
}

// CanonicalJSON re-encodes JSON with sorted object keys and no
// insignificant whitespace. Invalid JSON is returned as is.
func CanonicalJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	// encoding/json sorts map keys when marshaling.
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// ActionFP fingerprints a tool call's input (LLD §4.2):
// xxhash64(tool + "\x00" + canonical(input)).
func ActionFP(tool string, input json.RawMessage, cwd string) uint64 {
	canon, _ := canonicalInput(tool, parseInput(input), cwd)
	return actionFP(tool, canon)
}

func actionFP(tool, canon string) uint64 {
	d := xxhash.New()
	_, _ = d.WriteString(tool)
	_, _ = d.WriteString("\x00")
	_, _ = d.WriteString(canon)
	return d.Sum64()
}

// OutputText extracts the text of a tool response: a JSON string as is,
// stdout and stderr of a bash result, or the common text fields of other
// results; anything else as canonical JSON.
func OutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if raw[0] == '{' && json.Unmarshal(raw, &obj) == nil {
		var parts []string
		for _, k := range []string{"stdout", "stderr", "output", "error", "content", "result"} {
			v, ok := obj[k]
			if !ok {
				continue
			}
			var t string
			if json.Unmarshal(v, &t) == nil {
				if t != "" {
					parts = append(parts, t)
				}
				continue
			}
			parts = append(parts, CanonicalJSON(v))
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return CanonicalJSON(raw)
}
