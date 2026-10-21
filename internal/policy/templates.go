package policy

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
	"github.com/Somsubhra-Nandi/loopguard/internal/session"
)

// Message limits. Claude Code caps additionalContext at 10,000
// characters; LoopGuard's messages stay far below (LLD §3.2).
const (
	MaxMessage    = 1200 // whole message, in characters
	maxErrorLines = 3
	maxCommandLen = 120
	maxLineLen    = 160
	maxFiles      = 6
	maxPathLen    = 100
)

// The messages follow LLD §7.3 and TDD D-7: factual statements about what
// happened and options that often help. Never "SYSTEM:", never "you must":
// text framed as an out-of-band instruction can trip Claude's
// prompt-injection defenses.
const optionsHelp = "Options that often help: re-reading the full error and the code it points to, " +
	"checking assumptions (versions, config, env), trying a different approach, or asking the user."

// msg is what a message is built from: the loop and its latest evidence.
type msg struct {
	l       *session.LoopState
	elapsed time.Duration
	session string // short session id, for the halt's pause command
	cwd     string // normalized, so paths can be shown relative to it
}

func newMsg(s *session.SessionState, l *session.LoopState, now time.Time, cwd string) msg {
	if cwd != "" {
		cwd = normalize.NormalizePath(".", cwd)
	}
	return msg{l: l, elapsed: now.Sub(l.StartedAt), session: ShortSessionID(s.Key), cwd: cwd}
}

// rel shows a normalized path relative to the session's working
// directory when it is inside it, redacted and clipped.
func (m msg) rel(p string) string {
	if m.cwd != "" && len(p) > len(m.cwd)+1 && p[len(m.cwd)] == '/' && strings.EqualFold(p[:len(m.cwd)], m.cwd) {
		p = p[len(m.cwd)+1:]
	}
	return clipRunes(redact.String(p), maxPathLen)
}

func (m msg) family() string {
	if m.l.Family != "" {
		return m.l.Family
	}
	return m.l.Evidence.Family
}

func (m msg) path() string {
	p := m.l.Path
	if p == "" {
		p = m.l.Evidence.Path
	}
	return m.rel(p)
}

// nudge is the text injected next to the tool result.
func (m msg) nudge() string {
	var b strings.Builder
	switch m.l.Detector {
	case event.DetectorRepeatedFailure:
		// "in a row" and "first failure … ago", not "within the last
		// minute": the agent read that as a time window.
		fmt.Fprintf(&b, "LoopGuard observation: `%s` has failed %d times in a row with the same error (first failure %s).\n",
			m.family(), m.l.Count, ago(m.elapsed))
		writeExcerpt(&b, m.l.Evidence.ErrorLines)
		if files := m.files(); files != "" {
			fmt.Fprintf(&b, "Between attempts, these files were edited: %s. The error did not change after any of these edits.\n", files)
		} else {
			b.WriteString("No files were edited between attempts.\n")
		}
		b.WriteString("Repeating similar edits is unlikely to change the result. " + optionsHelp)
	case event.DetectorExactRepeat:
		fmt.Fprintf(&b, "LoopGuard observation: the same `%s` call has run %d times with the same result (first run %s), and no file was changed in between.\n",
			m.family(), m.l.Count, ago(m.elapsed))
		if cmds := m.l.Evidence.Commands; len(cmds) > 0 {
			fmt.Fprintf(&b, "  %s\n", clipRunes(redact.String(cmds[len(cmds)-1]), maxLineLen))
		}
		b.WriteString("Running it again is likely to give the same result. Options that often help: changing something first, " +
			"reading the output from a different angle, trying a different approach, or asking the user.")
	case event.DetectorOscillation:
		fmt.Fprintf(&b, "LoopGuard observation: %s has been changed back to a previous version %d times (the same edit was made and then reverted). "+
			"Switching between the same versions is unlikely to change the result. %s", m.path(), m.l.Count, optionsHelp)
	case event.DetectorReadThrash:
		if m.path() != "" {
			fmt.Fprintf(&b, "LoopGuard observation: %s has been read %d times (first read %s) with no change to it in between, and little else has changed. "+
				"Reading it again is unlikely to show anything new.", m.path(), m.l.Count, ago(m.elapsed))
		} else {
			fmt.Fprintf(&b, "LoopGuard observation: the same `%s` search has run %d times with the same result (first run %s). "+
				"Running it again will give the same result.", m.family(), m.l.Count, ago(m.elapsed))
		}
		b.WriteString(" Options that often help: working from what was already found, looking for the specific thing needed, " +
			"trying a different approach, or asking the user.")
	case event.DetectorStall:
		ev := m.l.Evidence
		fmt.Fprintf(&b, "LoopGuard observation: the last %d tool calls made no visible progress: %s failed and %s repeated earlier reads or searches "+
			"(the first of these calls was %s). Options that often help: restating the goal and what is known so far, "+
			"trying a different approach, or asking the user.", m.l.Count, plural(ev.Failures, "call"), plural(ev.Repeats, "call"), ago(m.elapsed))
	default:
		fmt.Fprintf(&b, "LoopGuard observation: %s has signaled %d times (first %s) with no progress in between. %s",
			strings.ReplaceAll(m.l.Detector, "_", " "), m.l.Count, ago(m.elapsed), optionsHelp)
	}
	return clipRunes(b.String(), MaxMessage)
}

// deny is the permissionDecisionReason of a blocked action c. A repeated
// failure names the command itself: only that command is blocked, not its
// whole family.
func (m msg) deny(c *event.ToolCall) string {
	var s string
	switch m.l.Detector {
	case event.DetectorRepeatedFailure:
		cmd := m.family()
		if c != nil && c.Command != "" {
			cmd = clipRunes(redact.String(c.Command), maxCommandLen)
		}
		s = fmt.Sprintf("LoopGuard blocked this command: it is the same `%s` run that has failed %d times with the same error, "+
			"with no change in outcome. A different approach, or a question to the user, is needed before running it again.", cmd, m.l.Count)
	case event.DetectorExactRepeat:
		s = fmt.Sprintf("LoopGuard blocked this call: it is the same `%s` call that has run %d times with the same result, "+
			"with nothing changed in between. A different approach, or a question to the user, is needed before running it again.", m.family(), m.l.Count)
	case event.DetectorOscillation:
		s = fmt.Sprintf("LoopGuard blocked this edit: %s has been changed back to a previous version %d times. "+
			"A different approach, or a question to the user, is needed before editing it again.", m.path(), m.l.Count)
	default:
		s = fmt.Sprintf("LoopGuard blocked this action: it repeats a loop (%s) that continued after an observation.", m.l.Detector)
	}
	return clipRunes(s, MaxMessage)
}

// HaltPrefix starts every halt text. A halt ends one turn; the session
// goes on with the next human message, and the wording must say so: an
// earlier "halted this session" made Claude refuse to run anything in the
// following turns.
const HaltPrefix = "LoopGuard stopped this turn: "

// haltMessage is the stopReason, shown to the human: the reason, that the
// next message continues normally, and how to turn LoopGuard off for the
// session if the human wants that.
func haltMessage(reason, shortID string) string {
	tail := ". Your next message continues normally. (To turn LoopGuard off for this session: " + PauseCommand(shortID) + ")"
	return clipRunes(HaltPrefix+reason, MaxMessage-utf8.RuneCountInString(tail)) + tail
}

// haltReason is the halt's one-line reason, e.g. "`pytest` failed 7 times
// with the same error and the loop continued after 2 blocks" (status,
// stopReason, and the systemMessage Claude Code shows).
func (m msg) haltReason(byTime bool, minutes, limit float64) string {
	why := fmt.Sprintf("the loop continued after %s", plural(m.l.Denies, "block"))
	if byTime {
		why = fmt.Sprintf("the loop has lasted %.0f min (limit %.0f) after %s", minutes, limit, plural(m.l.Denies, "block"))
	}
	switch m.l.Detector {
	case event.DetectorRepeatedFailure:
		return fmt.Sprintf("`%s` failed %d times with the same error and %s", m.family(), m.l.Count, why)
	case event.DetectorExactRepeat:
		return fmt.Sprintf("the same `%s` call ran %d times with the same result and %s", m.family(), m.l.Count, why)
	case event.DetectorOscillation:
		return fmt.Sprintf("%s was reverted %d times and %s", m.path(), m.l.Count, why)
	}
	return fmt.Sprintf("a %s loop went on and %s", strings.ReplaceAll(m.l.Detector, "_", " "), why)
}

// writeExcerpt writes up to three lines of tool output as a quoted block:
// "Output excerpt:" and the lines, indented. Tool output can carry text
// from anywhere (a test fixture, a web page, a log line), so it is shown
// only as a quote, never inside LoopGuard's own sentences: secrets are
// redacted, markup-like tags and control characters removed, and each
// line clipped (Excerpt). Nothing is written if no line is left.
func writeExcerpt(b *strings.Builder, lines []string) {
	ex := Excerpt(lines)
	if len(ex) == 0 {
		return
	}
	b.WriteString("Output excerpt:\n")
	for _, line := range ex {
		b.WriteString(excerptIndent + line + "\n")
	}
}

const excerptIndent = "    "

// tagRE matches markup-like tags: <name ...>, </name>, <name/>, comments,
// and <!...>/<?...> declarations. `Map<K, V>` is not a tag.
var tagRE = regexp.MustCompile(`<!--[\s\S]*?-->|<[!?][^<>]*>|</?[A-Za-z][\w:.-]*(?:\s[^<>]*)?/?>`)

// placeholderRE matches LoopGuard's own placeholders, which look like
// tags but are kept: the normalizer's (<N>, <TS>, …) and redaction's
// (<REDACTED:type>). Exact names only, so an <IMPORTANT> from the output
// is still removed.
var placeholderRE = regexp.MustCompile(`^<(?:DUR|ID|LN|N|TMP|TS|HOME|REDACTED:[a-z_]+)>$`)

// Excerpt cleans tool output lines for quoting (writeExcerpt): redacted,
// tags and control characters removed, whitespace trimmed, empty lines
// dropped, each clipped at 160 characters, at most 3.
func Excerpt(lines []string) []string {
	var out []string
	for _, line := range redact.Lines(lines) {
		line = tagRE.ReplaceAllStringFunc(line, func(tag string) string {
			if placeholderRE.MatchString(tag) {
				return tag
			}
			return ""
		})
		line = strings.Map(func(r rune) rune {
			switch {
			case r == '\t':
				return ' '
			case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == ' ' || r == ' ':
				return -1
			}
			return r
		}, line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, clipRunes(line, maxLineLen))
		if len(out) == maxErrorLines {
			break
		}
	}
	return out
}

// files lists the files edited between attempts.
func (m msg) files() string {
	fs := m.l.Evidence.Files
	if len(fs) == 0 {
		return ""
	}
	var out []string
	for i, f := range fs {
		if i == maxFiles {
			out = append(out, fmt.Sprintf("and %d more", len(fs)-maxFiles))
			break
		}
		out = append(out, m.rel(f))
	}
	return strings.Join(out, ", ")
}

// ago says when a loop's first call was: "less than a minute ago", "3 min
// ago". A point in time, not a window: "failed 4 times within the last
// minute" read as a time window to the agent.
func ago(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute ago"
	}
	return fmt.Sprintf("%d min ago", int(d.Minutes()+0.5))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// ShortSessionID is the first 8 characters of a session key's agent
// session id ("claude-code:1a2b3c4d-…/agent-1" → "1a2b3c4d"), as used by
// `loopguard pause --session`.
func ShortSessionID(key string) string {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		key = key[i+1:]
	}
	if i := strings.IndexByte(key, '/'); i >= 0 {
		key = key[:i]
	}
	if len(key) > 8 {
		key = key[:8]
	}
	return key
}

// PauseCommand is the command that pauses LoopGuard for a session.
func PauseCommand(shortID string) string { return "loopguard pause --session " + shortID }

// clipRunes cuts s to at most n characters, ending with "…" if cut.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
