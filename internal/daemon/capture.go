package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
)

// EnvCapture turns on payload capture when set to 1 at daemon start.
const EnvCapture = "LOOPGUARD_CAPTURE"

// maxCapturePerEvent caps the payloads saved per agent and event type.
const maxCapturePerEvent = 20

// homePlaceholder replaces the user's home directory in captured payloads.
const homePlaceholder = "<HOME>"

// capturer saves raw hook payloads for golden tests, as
// <home>/capture/<agent>/<event>-<n>.json, after redacting secrets and
// replacing the user's home directory in every string value. Payloads that
// aren't JSON are skipped. It is safe for concurrent use.
type capturer struct {
	dir   string
	homes []string // forms of the user's home directory to replace, longest first
	mu    sync.Mutex
	n     map[string]int // agent/event → payloads saved so far (including earlier runs)
}

func newCapturer(dir, userHome string) *capturer {
	return &capturer{dir: dir, homes: homeForms(userHome), n: map[string]int{}}
}

var nonWord = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// homeForms returns the spellings of the home directory that appear in
// payloads: as is, with the other slash, and as Claude Code encodes it in
// project directory names (every non-alphanumeric character → '-').
func homeForms(h string) []string {
	if h == "" {
		return nil
	}
	forms := []string{h, strings.ReplaceAll(h, `\`, "/"), strings.ReplaceAll(h, "/", `\`),
		regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(h, "-")}
	seen := map[string]bool{}
	out := forms[:0]
	for _, f := range forms {
		if len(f) > 1 && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// save captures one payload. It never fails the event: errors are
// returned for logging only.
func (c *capturer) save(agent string, raw []byte) error {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil // not JSON: nothing worth a golden file
	}
	name := "unknown"
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["hook_event_name"].(string); ok && nonWord.ReplaceAllString(s, "") != "" {
			name = nonWord.ReplaceAllString(s, "")
		}
	}
	agent = nonWord.ReplaceAllString(agent, "")
	dir := filepath.Join(c.dir, agent)

	c.mu.Lock()
	key := agent + "/" + name
	n, ok := c.n[key]
	if !ok {
		// Count files from earlier runs, so restarts don't exceed the cap.
		old, _ := filepath.Glob(filepath.Join(dir, name+"-*.json"))
		n = len(old)
	}
	if n >= maxCapturePerEvent {
		c.n[key] = n
		c.mu.Unlock()
		return nil
	}
	n++
	c.n[key] = n
	c.mu.Unlock()

	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep <REDACTED:…> and <HOME> readable
	enc.SetIndent("", "  ")
	if err := enc.Encode(c.clean(v)); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d.json", name, n)), b.Bytes(), 0o600)
}

// clean redacts and de-homes every string in v, keys included.
func (c *capturer) clean(v any) any {
	switch t := v.(type) {
	case string:
		return c.cleanString(t)
	case []any:
		for i := range t {
			t[i] = c.clean(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[c.cleanString(k)] = c.clean(val)
		}
		return out
	}
	return v
}

func (c *capturer) cleanString(s string) string {
	s = redact.String(s)
	for _, h := range c.homes {
		s = replaceFold(s, h, homePlaceholder)
	}
	return s
}

// replaceFold replaces old in s, ignoring case on Windows, where paths
// are case-insensitive.
func replaceFold(s, old, repl string) string {
	if runtime.GOOS != "windows" {
		return strings.ReplaceAll(s, old, repl)
	}
	lower, lowerOld := strings.ToLower(s), strings.ToLower(old)
	if len(lower) != len(s) || len(lowerOld) != len(old) {
		// Lower-casing changed byte lengths (rare non-ASCII); be exact.
		return strings.ReplaceAll(s, old, repl)
	}
	var b strings.Builder
	for {
		i := strings.Index(lower, lowerOld)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(repl)
		s, lower = s[i+len(old):], lower[i+len(old):]
	}
}
