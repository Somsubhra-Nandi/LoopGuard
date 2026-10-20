// Package capture saves real hook payloads for the adapters' golden tests:
// redacted, with the user's home directory replaced, at most 20 per event
// type. Capture is on while the flag file <home>/capture.on exists
// (`loopguard capture on`) or when the daemon runs with
// LOOPGUARD_CAPTURE=1; Export copies the captures into testdata with a
// final anonymization pass.
package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/Somsubhra-Nandi/loopguard/internal/anon"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
)

// EnvCapture turns capture on for the daemon's whole run when set to 1.
const EnvCapture = "LOOPGUARD_CAPTURE"

// MaxPerEvent caps the payloads saved per agent and event type.
const MaxPerEvent = 20

// HomePlaceholder replaces the user's home directory in captured payloads.
const HomePlaceholder = "<HOME>"

// Capturer saves raw hook payloads as <dir>/<agent>/<event>-<n>.json,
// after redacting secrets and replacing the user's home directory in every
// string (keys too). Payloads that aren't JSON are skipped. It is safe for
// concurrent use.
type Capturer struct {
	dir  string
	home *anon.HomeReplacer // the user's home directory, in every spelling
	mu   sync.Mutex
	n    map[string]int // agent/event → payloads saved so far (including earlier runs)
}

// New returns a capturer writing below dir.
func New(dir, userHome string) *Capturer {
	return &Capturer{dir: dir, home: anon.NewHomeReplacer(userHome), n: map[string]int{}}
}

var nonWord = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// Save captures one payload. It never fails the event: errors are
// returned for logging only.
func (c *Capturer) Save(agent string, raw []byte) error {
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
	if n >= MaxPerEvent {
		c.n[key] = n
		c.mu.Unlock()
		return nil
	}
	n++
	c.n[key] = n
	c.mu.Unlock()

	b, err := encode(walkStrings(v, c.cleanString))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d.json", name, n)), b, 0o600)
}

// encode writes v as indented JSON, keeping <REDACTED:…> and <HOME>
// readable.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return b.Bytes(), err
}

// walkStrings applies f to every string in v, keys included.
func walkStrings(v any, f func(string) string) any {
	switch t := v.(type) {
	case string:
		return f(t)
	case []any:
		for i := range t {
			t[i] = walkStrings(t[i], f)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[f(k)] = walkStrings(val, f)
		}
		return out
	}
	return v
}

func (c *Capturer) cleanString(s string) string {
	s = redact.String(s)
	return c.home.Replace(s, HomePlaceholder)
}
