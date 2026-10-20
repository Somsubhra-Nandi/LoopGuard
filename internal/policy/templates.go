package policy

import (
	"fmt"
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
	maxErrorLines = 2
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
		fmt.Fprintf(&b, "LoopGuard observation: `%s` has failed %d times %s with the same error", m.family(), m.l.Count, inTheLast(m.elapsed))
		m.errorLines(&b)
		if files := m.files(); files != "" {
			fmt.Fprintf(&b, "Between attempts, these files were edited: %s. The error did not change after any of these edits.\n", files)
		} else {
			b.WriteString("No files were edited between attempts.\n")
		}
		b.WriteString("Repeating similar edits is unlikely to change the result. " + optionsHelp)
	case event.DetectorExactRepeat:
		fmt.Fprintf(&b, "LoopGuard observation: the same `%s` call has run %d times %s with the same result, and no file was changed in between.\n",
			m.family(), m.l.Count, inTheLast(m.elapsed))
		if cmds := m.l.Evidence.Commands; len(cmds) > 0 {
			fmt.Fprintf(&b, "  %s\n", clipRunes(redact.String(cmds[len(cmds)-1]), maxLineLen))
		}
		b.WriteString("Running it again is likely to give the same result. Options that often help: changing something first, " +
			"reading the output from a different angle, trying a different approach, or asking the user.")
	case event.DetectorOscillation:
		fmt.Fprintf(&b, "LoopGuard observation: %s has been changed back to a previous version %d times (the same edit was made and then reverted). "+
			"Switching between the same versions is unlikely to change the result. %s", m.path(), m.l.Count, optionsHelp)
	default:
		fmt.Fprintf(&b, "LoopGuard observation: %s has signaled %d times %s with no progress in between. %s",
			strings.ReplaceAll(m.l.Detector, "_", " "), m.l.Count, inTheLast(m.elapsed), optionsHelp)
	}
	return clipRunes(b.String(), MaxMessage)
}

// deny is the permissionDecisionReason of a blocked action.
func (m msg) deny() string {
	var s string
	switch m.l.Detector {
	case event.DetectorRepeatedFailure:
		s = fmt.Sprintf("LoopGuard blocked this command: it is the same `%s` run that has failed %d times with the same error, "+
			"with no change in outcome. A different approach, or a question to the user, is needed before running it again.", m.family(), m.l.Count)
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

// halt is the stopReason, shown to the human. It always ends with the
// command that lets the agent continue.
func (m msg) halt() string {
	var what string
	switch m.l.Detector {
	case event.DetectorRepeatedFailure:
		what = fmt.Sprintf("stuck on `%s` (%d failures with the same error %s)", m.family(), m.l.Count, over(m.elapsed))
	case event.DetectorExactRepeat:
		what = fmt.Sprintf("stuck repeating the same `%s` call (%d runs with the same result %s)", m.family(), m.l.Count, over(m.elapsed))
	case event.DetectorOscillation:
		what = fmt.Sprintf("stuck changing %s back and forth (%d reverts %s)", m.path(), m.l.Count, over(m.elapsed))
	default:
		what = fmt.Sprintf("stuck in a %s loop %s", strings.ReplaceAll(m.l.Detector, "_", " "), over(m.elapsed))
	}
	if m.l.Denies > 0 {
		what += fmt.Sprintf(", retried after %s", plural(m.l.Denies, "block"))
	}
	tail := " Details: loopguard status. To let it continue, run: " + PauseCommand(m.session)
	return clipRunes("LoopGuard halted the agent: "+what+".", MaxMessage-utf8.RuneCountInString(tail)) + tail
}

// haltReason is the halt's one-line reason (status, and the systemMessage
// Claude Code shows).
func (m msg) haltReason(byTime bool, minutes, limit float64) string {
	why := fmt.Sprintf("the loop continued after %s", plural(m.l.Denies, "block"))
	if byTime {
		why = fmt.Sprintf("the loop has lasted %.0f min (limit %.0f)", minutes, limit)
	}
	switch m.l.Detector {
	case event.DetectorRepeatedFailure:
		return fmt.Sprintf("`%s` failed %d times with the same error; %s", m.family(), m.l.Count, why)
	case event.DetectorExactRepeat:
		return fmt.Sprintf("the same `%s` call ran %d times with the same result; %s", m.family(), m.l.Count, why)
	case event.DetectorOscillation:
		return fmt.Sprintf("%s was reverted %d times; %s", m.path(), m.l.Count, why)
	}
	return why
}

// errorLines writes ":" and up to two redacted error lines, or ".".
func (m msg) errorLines(b *strings.Builder) {
	lines := redact.Lines(m.l.Evidence.ErrorLines)
	n := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if n == 0 {
			b.WriteString(":\n")
		}
		fmt.Fprintf(b, "  %s\n", clipRunes(line, maxLineLen))
		if n++; n == maxErrorLines {
			break
		}
	}
	if n == 0 {
		b.WriteString(".\n")
	}
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

func inTheLast(d time.Duration) string {
	if d < time.Minute {
		return "within the last minute"
	}
	return fmt.Sprintf("in the last %d min", int(d.Minutes()+0.5))
}

func over(d time.Duration) string {
	if d < time.Minute {
		return "within a minute"
	}
	return fmt.Sprintf("over %d min", int(d.Minutes()+0.5))
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
