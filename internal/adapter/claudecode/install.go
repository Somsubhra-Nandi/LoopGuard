package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Somsubhra-Nandi/loopguard/internal/adapter"
)

// HookBinary is the base name of the program the hooks run. A hook whose
// command has this base name (with or without .exe) is LoopGuard's: that
// is how Install stays idempotent and Uninstall removes only its own.
const HookBinary = "loopguard-hook"

// BackupSuffix is appended to the settings file name for the backup made
// before LoopGuard's first change.
const BackupSuffix = ".loopguard.bak"

// EnvConfigDir is Claude Code's override of its ~/.claude directory.
const EnvConfigDir = "CLAUDE_CONFIG_DIR"

// hookSpec is one installed hook (LLD §3.3).
type hookSpec struct {
	Event   string
	Matcher string // "" for events without a tool matcher
	Timeout int    // seconds
}

// Hooks are the events LoopGuard installs, in install order.
var Hooks = []hookSpec{
	{"SessionStart", "", 5},
	{"UserPromptSubmit", "", 2},
	{"PreToolUse", "*", 2},
	{"PostToolUse", "*", 2},
	{"PostToolUseFailure", "*", 2},
	{"PostToolBatch", "", 2},
	{"Stop", "", 2},
	{"SessionEnd", "", 1},
}

// UserSettingsPath returns Claude Code's user settings file:
// $CLAUDE_CONFIG_DIR/settings.json, else ~/.claude/settings.json.
func UserSettingsPath() (string, error) {
	if d := os.Getenv(EnvConfigDir); d != "" {
		return filepath.Join(d, "settings.json"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".claude", "settings.json"), nil
}

// ProjectSettingsPath returns the project's local (not committed) settings
// file: <root>/.claude/settings.local.json.
func ProjectSettingsPath(root string) string {
	return filepath.Join(root, ".claude", "settings.local.json")
}

// IsLoopGuardHook reports whether a hook command runs loopguard-hook,
// whatever the directory and path separator.
func IsLoopGuardHook(command string) bool {
	command = strings.Trim(strings.TrimSpace(command), `"'`)
	if i := strings.LastIndexAny(command, `/\`); i >= 0 {
		command = command[i+1:]
	}
	command = strings.ToLower(command)
	return command == HookBinary || command == HookBinary+".exe"
}

// hookEntry is the JSON of one installed hook, in exec form: the command
// is the absolute path, so neither PATH nor shell quoting is involved, and
// a path with spaces works.
type hookJSON struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

type groupJSON struct {
	Matcher string     `json:"matcher,omitempty"`
	Hooks   []hookJSON `json:"hooks"`
}

func hookEntry(hookPath string, s hookSpec) hookJSON {
	return hookJSON{Type: "command", Command: hookPath, Args: []string{"--agent", "claude-code"}, Timeout: s.Timeout}
}

// Install merges LoopGuard's hooks into the settings file (LLD §3.3).
func (Adapter) Install(o adapter.InstallOpts) (adapter.InstallReport, error) {
	if !filepath.IsAbs(o.Hook) {
		return adapter.InstallReport{}, fmt.Errorf("hook path %q is not absolute", o.Hook)
	}
	return updateSettings(o, func(doc *settings, r *adapter.InstallReport) error { return doc.install(o.Hook, r) })
}

// Uninstall removes LoopGuard's hooks from the settings file, and nothing
// else. The backup is left in place.
func (Adapter) Uninstall(o adapter.InstallOpts) (adapter.InstallReport, error) {
	return updateSettings(o, func(doc *settings, r *adapter.InstallReport) error { return doc.uninstall(r) })
}

// updateSettings reads the settings file, applies change, and writes the result
// (unless it is unchanged or o.DryRun), backing the file up first.
func updateSettings(o adapter.InstallOpts, change func(*settings, *adapter.InstallReport) error) (adapter.InstallReport, error) {
	r := adapter.InstallReport{SettingsPath: o.Settings}
	if o.Settings == "" {
		return r, errors.New("no settings file")
	}
	before, err := os.ReadFile(o.Settings)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return r, err
	}
	if exists {
		r.Before = before
	}
	doc, err := parseSettings(before)
	if err != nil {
		return r, fmt.Errorf("%s is not valid JSON, so LoopGuard won't touch it (%v); fix the file and run this again", o.Settings, err)
	}
	if err := change(doc, &r); err != nil {
		return r, fmt.Errorf("%s: %w", o.Settings, err)
	}
	if doc.disableAllHooks() {
		r.Warnings = append(r.Warnings, `"disableAllHooks": true is set in this file: Claude Code runs no hooks until it is removed`)
	}
	r.BackupPath = o.Settings + BackupSuffix
	if _, err := os.Stat(r.BackupPath); err != nil {
		r.BackupPath = ""
	}

	if !doc.changed {
		r.After = before
		return r, nil
	}
	after, err := doc.marshal(before)
	if err != nil {
		return r, err
	}
	r.After, r.Changed = after, true
	if o.DryRun {
		return r, nil
	}
	if exists && r.BackupPath == "" {
		r.BackupPath = o.Settings + BackupSuffix
		if err := writeNew(r.BackupPath, before); err != nil {
			return r, fmt.Errorf("backing up %s: %w", o.Settings, err)
		}
		r.BackupCreated = true
	}
	if err := os.MkdirAll(filepath.Dir(o.Settings), 0o700); err != nil {
		return r, err
	}
	return r, writeAtomic(o.Settings, after)
}

// settings is a parsed settings file. Its members keep their order and
// their original text, so a change shows up in a diff as just that change.
type settings struct {
	top     jsonObject
	hooks   jsonObject // the "hooks" member
	changed bool
}

func parseSettings(b []byte) (*settings, error) {
	s := &settings{}
	if len(bytes.TrimSpace(b)) == 0 {
		return s, nil // a missing or empty file is {}
	}
	var err error
	if s.top, err = parseObject(b); err != nil {
		return nil, err
	}
	if raw, ok := s.top.get("hooks"); ok && !isNull(raw) {
		if s.hooks, err = parseObject(raw); err != nil {
			return nil, errors.New(`"hooks" is not an object`)
		}
	}
	return s, nil
}

func (s *settings) disableAllHooks() bool {
	raw, ok := s.top.get("disableAllHooks")
	var v bool
	return ok && json.Unmarshal(raw, &v) == nil && v
}

// install puts exactly one LoopGuard hook, as wanted, on each event in
// Hooks, and removes LoopGuard's hooks from any other event. Another
// tool's hooks are never touched. A LoopGuard hook that differs from the
// wanted one (an older path, timeout, ...) is replaced in place.
func (s *settings) install(hookPath string, r *adapter.InstallReport) error {
	want := map[string]hookSpec{}
	for _, h := range Hooks {
		want[h.Event] = h
	}
	// Other events first: an older version may have hooked more.
	for _, m := range s.hooks {
		if _, ok := want[m.key]; ok {
			continue
		}
		groups, err := parseGroups(m.key, m.val)
		if err != nil {
			return err
		}
		if kept, n := groups.withoutOurs(); n > 0 {
			s.setEvent(m.key, kept)
			r.Removed = append(r.Removed, m.key)
		}
	}
	for _, h := range Hooks {
		raw, _ := s.hooks.get(h.Event)
		groups, err := parseGroups(h.Event, raw)
		if err != nil {
			return err
		}
		wantGroup := ourGroup(hookPath, h)
		if i, ok := groups.onlyOurs(); ok && sameJSON(groups[i].raw, wantGroup) {
			r.Unchanged = append(r.Unchanged, h.Event)
			continue
		}
		at := groups.firstOurs()
		kept, n := groups.withoutOurs()
		if at < 0 || at > len(kept) {
			at = len(kept)
		}
		g, _ := marshalJSON(wantGroup)
		kept = append(kept[:at], append(groupList{{raw: g}}, kept[at:]...)...)
		s.setEvent(h.Event, kept)
		if n > 0 {
			r.Updated = append(r.Updated, h.Event)
		} else {
			r.Added = append(r.Added, h.Event)
		}
	}
	return nil
}

func (s *settings) uninstall(r *adapter.InstallReport) error {
	for _, m := range append(jsonObject(nil), s.hooks...) {
		groups, err := parseGroups(m.key, m.val)
		if err != nil {
			return err
		}
		if kept, n := groups.withoutOurs(); n > 0 {
			s.setEvent(m.key, kept)
			r.Removed = append(r.Removed, m.key)
		}
	}
	return nil
}

// setEvent replaces an event's matcher groups; an event left with none is
// removed, and so is "hooks" when it ends up empty.
func (s *settings) setEvent(event string, groups groupList) {
	s.changed = true
	if len(groups) == 0 {
		s.hooks.del(event)
	} else {
		s.hooks.set(event, groups.marshal())
	}
}

// marshal renders the settings with the file's indentation (two spaces, Claude
// Code's own style, for a new file), its line endings, and a final newline.
func (s *settings) marshal(before []byte) ([]byte, error) {
	top := append(jsonObject(nil), s.top...)
	if len(s.hooks) == 0 {
		top.del("hooks")
	} else {
		top.set("hooks", s.hooks.marshal())
	}
	var out bytes.Buffer
	if err := json.Indent(&out, top.marshal(), "", indentOf(before)); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	b := out.Bytes()
	if bytes.Contains(before, []byte("\r\n")) {
		b = bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	}
	return b, nil
}

func ourGroup(hookPath string, h hookSpec) groupJSON {
	return groupJSON{Matcher: h.Matcher, Hooks: []hookJSON{hookEntry(hookPath, h)}}
}

// A matcher group: {"matcher": "...", "hooks": [{...}, ...]}.
type group struct {
	raw   json.RawMessage
	obj   jsonObject        // nil if raw is not an object
	hooks []json.RawMessage // its "hooks" array
	ours  []bool            // which hooks are LoopGuard's
}

type groupList []group

func parseGroups(event string, raw json.RawMessage) (groupList, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("hooks.%s is not an array", event)
	}
	gs := make(groupList, len(items))
	for i, it := range items {
		gs[i].raw = it
		obj, err := parseObject(it)
		if err != nil {
			continue // not ours to judge: kept as is
		}
		gs[i].obj = obj
		hraw, ok := obj.get("hooks")
		if !ok || json.Unmarshal(hraw, &gs[i].hooks) != nil {
			gs[i].hooks = nil
			continue
		}
		gs[i].ours = make([]bool, len(gs[i].hooks))
		for j, h := range gs[i].hooks {
			var cmd struct {
				Command string `json:"command"`
			}
			gs[i].ours[j] = json.Unmarshal(h, &cmd) == nil && IsLoopGuardHook(cmd.Command)
		}
	}
	return gs, nil
}

func (g group) countOurs() int {
	n := 0
	for _, o := range g.ours {
		if o {
			n++
		}
	}
	return n
}

// onlyOurs returns the index of the only group with LoopGuard hooks, if
// exactly one group has any and it holds just one hook.
func (gs groupList) onlyOurs() (int, bool) {
	at, total := -1, 0
	for i, g := range gs {
		if n := g.countOurs(); n > 0 {
			at, total = i, total+n
		}
	}
	return at, total == 1 && len(gs[at].hooks) == 1
}

func (gs groupList) firstOurs() int {
	for i, g := range gs {
		if g.countOurs() > 0 {
			return i
		}
	}
	return -1
}

// withoutOurs removes LoopGuard's hooks, dropping groups left without
// hooks, and returns the result and how many hooks were removed.
func (gs groupList) withoutOurs() (groupList, int) {
	var out groupList
	removed := 0
	for _, g := range gs {
		n := g.countOurs()
		if n == 0 {
			out = append(out, g)
			continue
		}
		removed += n
		var rest []json.RawMessage
		for j, h := range g.hooks {
			if !g.ours[j] {
				rest = append(rest, h)
			}
		}
		if len(rest) == 0 {
			continue
		}
		obj := append(jsonObject(nil), g.obj...)
		b, _ := json.Marshal(rest)
		obj.set("hooks", b)
		out = append(out, group{raw: obj.marshal(), obj: obj, hooks: rest, ours: make([]bool, len(rest))})
	}
	return out, removed
}

func (gs groupList) marshal() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, g := range gs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(g.raw)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// InstalledHook is a LoopGuard hook found in a settings file.
type InstalledHook struct {
	Event   string
	Command string
	Args    []string
	Timeout float64
}

// FindHooks returns the LoopGuard hooks in a settings file's content and
// whether the file sets disableAllHooks.
func FindHooks(content []byte) (hooks []InstalledHook, disableAll bool, err error) {
	s, err := parseSettings(content)
	if err != nil {
		return nil, false, err
	}
	for _, m := range s.hooks {
		groups, err := parseGroups(m.key, m.val)
		if err != nil {
			return nil, false, err
		}
		for _, g := range groups {
			for j, h := range g.hooks {
				if !g.ours[j] {
					continue
				}
				var ih InstalledHook
				_ = json.Unmarshal(h, &ih)
				ih.Event = m.key
				hooks = append(hooks, ih)
			}
		}
	}
	return hooks, s.disableAllHooks(), nil
}

// A jsonObject is a JSON object whose members keep their order and text.
type member struct {
	key string
	val json.RawMessage
}

type jsonObject []member

func parseObject(b []byte) (jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	o := jsonObject{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		o = append(o, member{key, val})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON object")
	}
	return o, nil
}

// get returns the last member with this key (the one JSON decoders use).
func (o jsonObject) get(key string) (json.RawMessage, bool) {
	for i := len(o) - 1; i >= 0; i-- {
		if o[i].key == key {
			return o[i].val, true
		}
	}
	return nil, false
}

func (o *jsonObject) set(key string, val json.RawMessage) {
	for i := len(*o) - 1; i >= 0; i-- {
		if (*o)[i].key == key {
			(*o)[i].val = val
			return
		}
	}
	*o = append(*o, member{key, val})
}

func (o *jsonObject) del(key string) {
	out := (*o)[:0]
	for _, m := range *o {
		if m.key != key {
			out = append(out, m)
		}
	}
	*o = out
}

func (o jsonObject) marshal() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := marshalJSON(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// marshalJSON is json.Marshal without HTML escaping (a command like
// `x 2>&1` stays readable).
func marshalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// sameJSON reports whether raw decodes to the same value as v.
func sameJSON(raw json.RawMessage, v any) bool {
	var a any
	b, err := marshalJSON(v)
	if err != nil || json.Unmarshal(raw, &a) != nil {
		return false
	}
	var w any
	_ = json.Unmarshal(b, &w)
	return reflect.DeepEqual(a, w)
}

// writeNew writes a file that must not exist yet.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	return errors.Join(werr, f.Close())
}

// writeAtomic replaces path through a temporary file in the same
// directory, keeping the file's permissions, so Claude Code never reads a
// half-written settings file.
func writeAtomic(path string, b []byte) error {
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), mode)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// indentOf returns the indentation of the first indented line of a JSON
// document, or two spaces.
func indentOf(b []byte) string {
	for _, line := range bytes.Split(b, []byte("\n")) {
		n := 0
		for n < len(line) && (line[n] == ' ' || line[n] == '\t') {
			n++
		}
		if n > 0 && n < len(line) && line[n] != '\r' {
			return string(line[:n])
		}
	}
	return "  "
}
