package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/cost"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
)

// ExportStats summarizes an anonymized export.
type ExportStats struct {
	Lines     int
	ToolCalls int
	Prompts   int
	Files     int
}

// Anonymized exports keep what the detectors need and nothing else:
//
//   - structure: tool calls, results (with is_error), human prompts, and
//     permission modes, as transcript lines replay can read back;
//   - tool names, and bash commands as normalized, redacted commands
//     with their free text (long quoted strings, heredoc and script
//     bodies) replaced by hash tokens;
//   - edit strings, written content, search patterns, and other tool inputs
//     as hash tokens, so equal inputs stay equal (inverse edits, revisits,
//     exact repeats) without their text;
//   - outputs as the redacted error key lines of a failure plus a hash
//     token of the whole normalized output;
//   - timestamps shifted so the session starts at the Unix epoch;
//   - file paths as stable fake names (src/file_1.ts, keeping the
//     extension); the working directory as /repo; other paths below a home
//     directory as /home/user/p_<hash>; URLs as <url:hash>; the username
//     and repo name learned from those paths as "user" and "repo", wherever
//     they still appear; email addresses as <email>.
//
// Prompt text, assistant text and thinking, and full tool outputs are
// dropped.

const anonCwd = "/repo"

var (
	epoch = time.Unix(0, 0).UTC()
	// homeDirRe matches a home directory in any spelling: C:\Users\<name>,
	// C:/Users/<name>, /c/Users/<name> (Git Bash), /mnt/c/Users/<name>
	// (WSL), /Users/<name>, /home/<name>. Usernames can contain spaces.
	// Group 1 is the rest of the path below the home directory.
	homeDirRe = regexp.MustCompile(`(?i)(?:\b[a-z]:[\\/]+|/(?:mnt/)?[a-z]/|/)(?:users|home)[\\/]+[^\\/"'<>\n]+([\\/][^\s"'<>|;&]*)?`)
	emailRe   = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// projectDirRe matches Claude Code's encoded project directory names
	// (C--Users-name-work-repo), which spell out a full path.
	// Windows (C--Users-name-...) and macOS/Linux (-Users-name-...,
	// -home-name-...) encodings are both matched on every OS; group 1 is
	// the separator before a Unix one, which is kept.
	projectDirRe = regexp.MustCompile(`(?i)\b[a-z]--users-[A-Za-z0-9-]+|(^|[/\\\s"'=])-(?:users|home)-[A-Za-z0-9-]+`)
	urlRe        = regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>|]+`)
	wordSplit    = regexp.MustCompile(`[^A-Za-z0-9]+`)
	// homeSegRe finds the username in a normalized path.
	homeSegRe     = regexp.MustCompile(`(?i)/(?:users|home)/([^/]+)`)
	originalNames = map[string]string{
		event.ToolBash: "Bash", event.ToolEdit: "Edit", event.ToolWrite: "Write",
		event.ToolRead: "Read", event.ToolGrep: "Grep", event.ToolGlob: "Glob", event.ToolPatch: "apply_patch",
	}
)

type exporter struct {
	w       *bufio.Writer
	b       *normalize.Builder
	session string
	t0      time.Time
	paths   map[string]string // normalized original path → fake path
	cwds    map[string]bool   // original working directories seen
	words   map[string]string // names learned from paths (username, repo) → replacement
	stats   ExportStats
	err     error

	// usage is the API message of the line being read, if it is one with
	// a tool call: its pre events carry it. chars holds each tool result's
	// original length, by tool use id.
	usage *cost.Message
	chars map[string]int64
}

// Export reads one stream and writes its anonymized copy to w.
func Export(st Stream, w io.Writer, th config.Thresholds) (ExportStats, error) {
	f, err := os.Open(st.Path)
	if err != nil {
		return ExportStats{}, err
	}
	defer f.Close()
	x := &exporter{
		w:       bufio.NewWriter(w),
		b:       normalize.BuilderFor(th),
		session: "anon-" + strconv.FormatUint(normalize.HashString(st.SessionID), 16)[:8],
		paths:   map[string]string{},
		cwds:    map[string]bool{},
		words:   map[string]string{},
		chars:   map[string]int64{},
	}
	if home, err := os.UserHomeDir(); err == nil {
		x.learn(home)
	}
	if _, err := ReadStreamLines(f, st, x.event, x.line); err != nil {
		return x.stats, err
	}
	if x.err != nil {
		return x.stats, x.err
	}
	x.stats.Files = len(x.paths)
	return x.stats, x.w.Flush()
}

func (x *exporter) event(ev *event.Event, _ uint64) {
	if x.err != nil {
		return
	}
	if x.t0.IsZero() {
		x.t0 = ev.ReceivedAt
	}
	if ev.Cwd != "" && !x.cwds[ev.Cwd] {
		x.cwds[ev.Cwd] = true
		x.learn(ev.Cwd)
	}
	ts := epoch.Add(ev.ReceivedAt.Sub(x.t0)).Format("2006-01-02T15:04:05.000Z")
	base := map[string]any{
		"sessionId": x.session, "timestamp": ts, "cwd": anonCwd, "isSidechain": false,
	}
	if ev.PermMode != "" {
		base["permissionMode"] = ev.PermMode
	}
	switch ev.Phase {
	case event.PhasePrompt:
		x.stats.Prompts++
		base["type"] = "user"
		base["origin"] = map[string]string{"kind": "human"}
		base["message"] = map[string]any{"role": "user", "content": "<prompt>"}
	case event.PhasePre:
		x.stats.ToolCalls++
		base["type"] = "assistant"
		msg := map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": anonID(ev.ToolUseID), "name": originalToolName(ev.ToolName), "input": x.input(ev),
		}}}
		if x.usage != nil {
			addUsage(msg, *x.usage)
		}
		base["message"] = msg
	case event.PhasePost, event.PhasePostFailure:
		c := x.b.Build(ev)
		content := "out_" + strconv.FormatUint(c.ResultFP, 16)
		if c.Failed() {
			lines := make([]string, 0, len(c.ErrorSig.Lines)+1)
			for _, l := range c.ErrorSig.Lines {
				lines = append(lines, x.text(l))
			}
			content = strings.Join(append(lines, content), "\n")
		}
		base["type"] = "user"
		result := map[string]any{
			"type": "tool_result", "tool_use_id": anonID(ev.ToolUseID), "content": content, "is_error": c.Failed(),
		}
		if n, ok := x.chars[ev.ToolUseID]; ok {
			result["chars"] = n // the original length, for cost estimates
			delete(x.chars, ev.ToolUseID)
		}
		base["message"] = map[string]any{"role": "user", "content": []any{result}}
	default:
		return
	}
	b, err := json.Marshal(base)
	if err != nil {
		x.err = err
		return
	}
	x.stats.Lines++
	_, x.err = x.w.Write(append(b, '\n'))
}

// input returns the anonymized tool input.
func (x *exporter) input(ev *event.Event) map[string]any {
	var in map[string]any
	_ = json.Unmarshal(ev.Input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	out := map[string]any{}
	switch ev.ToolName {
	case event.ToolBash:
		c := x.b.Build(ev)
		out["command"] = x.text(anonCommand(c.Command))
	case event.ToolEdit:
		out["file_path"] = x.fakePath(firstNonEmpty(str("file_path"), str("notebook_path")), ev.Cwd)
		if edits, ok := in["edits"].([]any); ok {
			var es []any
			for _, e := range edits {
				em, _ := e.(map[string]any)
				o, _ := em["old_string"].(string)
				n, _ := em["new_string"].(string)
				es = append(es, map[string]string{"old_string": token(o), "new_string": token(n)})
			}
			out["edits"] = es
		} else {
			out["old_string"] = token(str("old_string"))
			out["new_string"] = token(firstNonEmpty(str("new_string"), str("new_source")))
		}
	case event.ToolWrite:
		out["file_path"] = x.fakePath(str("file_path"), ev.Cwd)
		out["content"] = token(str("content"))
	case event.ToolRead:
		out["file_path"] = x.fakePath(str("file_path"), ev.Cwd)
		for _, k := range []string{"offset", "limit"} {
			if v, ok := in[k]; ok {
				out[k] = v
			}
		}
	case event.ToolGrep, event.ToolGlob:
		out["pattern"] = token(str("pattern"))
		if p := str("path"); p != "" {
			out["path"] = token(normalize.NormalizePath(p, ev.Cwd))
		}
	default:
		out["input_fp"] = token(normalize.CanonicalJSON(ev.Input))
	}
	return out
}

// fakePath maps a file path to a stable fake one under /repo.
func (x *exporter) fakePath(p, cwd string) string {
	if p == "" {
		return ""
	}
	n := normalize.NormalizePath(p, cwd)
	fake, ok := x.paths[n]
	if !ok {
		ext := path.Ext(n)
		if len(ext) > 8 || strings.ContainsAny(ext, " <>") {
			ext = ""
		}
		fake = fmt.Sprintf("src/file_%d%s", len(x.paths)+1, ext)
		x.paths[n] = fake
	}
	return anonCwd + "/" + fake
}

// text anonymizes free text (a command or an error line): known file
// paths, working directories, home directories, and email addresses.
func (x *exporter) text(s string) string {
	if s == "" {
		return s
	}
	s = redact.String(s)
	type pair struct{ from, to string }
	var pairs []pair
	for orig, fake := range x.paths {
		for _, v := range pathVariants(orig) {
			pairs = append(pairs, pair{v, anonCwd + "/" + fake})
		}
	}
	for cwd := range x.cwds {
		for _, v := range pathVariants(normalize.NormalizePath(cwd, "")) {
			pairs = append(pairs, pair{v, anonCwd})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return len(pairs[i].from) > len(pairs[j].from) })
	for _, p := range pairs {
		s = replaceFold(s, p.from, p.to)
	}
	// URLs can name private hosts: keep that there was a URL, and which.
	s = urlRe.ReplaceAllStringFunc(s, func(u string) string { return "<url:" + token(u)[2:10] + ">" })
	s = projectDirRe.ReplaceAllString(s, "${1}<project-dir>")
	// Paths below a home directory become a stable hashed name.
	s = homeDirRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := homeDirRe.FindStringSubmatch(m)
		if len(sub) < 2 || len(sub[1]) <= 1 {
			return "/home/user"
		}
		return "/home/user/p_" + token(strings.ToLower(strings.ReplaceAll(sub[1], `\`, "/")))[2:10]
	})
	s = emailRe.ReplaceAllString(s, "<email>")
	// Finally, the names learned from paths, wherever they still appear
	// (a bare repo name in `cd repo`, a username in a git config).
	words := make([]string, 0, len(x.words))
	for w := range x.words {
		words = append(words, w)
	}
	sort.Slice(words, func(i, j int) bool { return len(words[i]) > len(words[j]) })
	for _, w := range words {
		s = replaceFold(s, w, x.words[w])
	}
	return s
}

// longQuotedRe matches quoted strings with more than 20 characters: commit
// messages, inline scripts, JSON payloads.
var longQuotedRe = regexp.MustCompile(`'[^'\n]{21,}'|"(?:[^"\\\n]|\\.){21,}"`)

// anonCommand drops the free text of a normalized command, keeping its
// shape: a multi-line command (heredoc, script) keeps its first line plus a
// token of the rest, and long quoted strings become tokens. Equal commands
// stay equal, and the family (the first words) is untouched.
func anonCommand(cmd string) string {
	if first, rest, ok := strings.Cut(cmd, "\n"); ok {
		cmd = first + " <body:" + token(rest)[2:10] + ">"
	}
	return longQuotedRe.ReplaceAllStringFunc(cmd, func(q string) string {
		return q[:1] + "q_" + token(q)[2:10] + q[:1]
	})
}

// learn records the sensitive names in a directory path: the username of
// a home directory (and its words), and the last element (the repo).
func (x *exporter) learn(dir string) {
	n := normalize.NormalizePath(dir, "")
	if m := homeSegRe.FindStringSubmatch(n); m != nil {
		x.words[m[1]] = "user"
		for _, part := range wordSplit.Split(m[1], -1) {
			if len(part) >= 3 {
				x.words[part] = "user"
			}
		}
	}
	if base := path.Base(n); len(base) >= 3 && base != "." && base != "/" && !strings.EqualFold(base, "users") {
		if m := homeSegRe.FindStringSubmatch(n); m == nil || !strings.EqualFold(m[1], base) {
			x.words[base] = "repo"
		}
	}
}

// pathVariants returns the spellings a normalized path can have in text:
// forward slashes, backslashes (single and JSON-escaped), and the Git Bash
// (/c/...) and WSL (/mnt/c/...) forms of a Windows path. Matching is case
// insensitive, so drive-letter case does not matter.
func pathVariants(p string) []string {
	vs := []string{p, strings.ReplaceAll(p, "/", `\`), strings.ReplaceAll(p, "/", `\\`)}
	if len(p) >= 3 && p[1] == ':' && p[2] == '/' {
		rest := p[2:]
		vs = append(vs, "/"+p[:1]+rest, "/mnt/"+p[:1]+rest)
	}
	return vs
}

// replaceFold replaces every case-insensitive occurrence of old.
func replaceFold(s, old, new string) string {
	if old == "" {
		return s
	}
	lower, lold := strings.ToLower(s), strings.ToLower(old)
	if len(lower) != len(s) || !strings.Contains(lower, lold) {
		return strings.ReplaceAll(s, old, new)
	}
	var b strings.Builder
	for {
		i := strings.Index(lower, lold)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(new)
		s, lower = s[i+len(old):], lower[i+len(old):]
	}
}

func token(s string) string { return "t_" + strconv.FormatUint(normalize.HashString(s), 16) }

func anonID(id string) string { return "toolu_" + strconv.FormatUint(normalize.HashString(id), 16) }

func originalToolName(n string) string {
	if o, ok := originalNames[n]; ok {
		return o
	}
	if rest, ok := strings.CutPrefix(n, "mcp:"); ok {
		if srv, tool, ok := strings.Cut(rest, ":"); ok {
			return "mcp__" + srv + "__" + tool
		}
	}
	return strings.TrimPrefix(n, "other:")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// line sees each raw line before its events: it keeps an API message's
// usage for the line's tool calls, writes the usage of messages without
// one (usage counts are not private), notes each tool result's original
// length, and keeps compaction boundaries, so cost accounting (M4) works
// on exports. Message ids become hash tokens.
func (x *exporter) line(b []byte) {
	x.usage = nil
	if x.err != nil {
		return
	}
	if m, ok := cost.ParseLine(b); ok {
		if bytes.Contains(b, []byte(`"tool_use"`)) {
			x.usage = &m
			return
		}
		msg := map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "<text>"}}}
		addUsage(msg, m)
		x.write(map[string]any{"type": "assistant", "message": msg}, m.TS)
		return
	}
	var l struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		TS      string `json:"timestamp"`
		Message *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	switch {
	case bytes.Contains(b, []byte(`"tool_result"`)):
		if json.Unmarshal(b, &l) != nil || l.Message == nil {
			return
		}
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return
		}
		for _, bl := range blocks {
			if bl.Type == "tool_result" {
				x.chars[bl.ToolUseID] = cost.ResultChars(bl.Content)
			}
		}
	case bytes.Contains(b, []byte(`compact_boundary`)):
		if json.Unmarshal(b, &l) == nil && l.Type == "system" && l.Subtype == "compact_boundary" {
			ts, _ := time.Parse(time.RFC3339Nano, l.TS)
			x.write(map[string]any{"type": "system", "subtype": "compact_boundary"}, ts)
		}
	}
}

// write writes a line of the export's own (usage, compaction), at the
// shifted time of ts.
func (x *exporter) write(line map[string]any, ts time.Time) {
	if x.t0.IsZero() {
		x.t0 = ts
	}
	if ts.Before(x.t0) {
		ts = x.t0
	}
	line["sessionId"] = x.session
	line["timestamp"] = epoch.Add(ts.Sub(x.t0)).Format("2006-01-02T15:04:05.000Z")
	line["cwd"] = anonCwd
	line["isSidechain"] = false
	b, err := json.Marshal(line)
	if err != nil {
		x.err = err
		return
	}
	x.stats.Lines++
	_, x.err = x.w.Write(append(b, '\n'))
}

// addUsage puts a message's id (as a hash token), model, and usage on an
// exported message.
func addUsage(msg map[string]any, m cost.Message) {
	msg["id"] = "msg_" + strconv.FormatUint(normalize.HashString(m.ID), 16)
	msg["model"] = m.Model
	u := map[string]any{
		"input_tokens": m.Usage.Input, "output_tokens": m.Usage.Output,
		"cache_creation_input_tokens": m.Usage.CacheWrite, "cache_read_input_tokens": m.Usage.CacheRead,
		"cache_creation": map[string]int64{"ephemeral_1h_input_tokens": m.Usage.CacheWrite1h,
			"ephemeral_5m_input_tokens": m.Usage.CacheWrite - m.Usage.CacheWrite1h},
	}
	if m.Speed != "" {
		u["speed"] = m.Speed
	}
	if m.Geo != "" {
		u["inference_geo"] = m.Geo
	}
	msg["usage"] = u
}
