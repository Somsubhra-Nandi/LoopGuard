package notify

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/redact"
)

// EventTest is the event of `loopguard notify test`: not in any on: list,
// sent to every enabled channel, never rate limited.
const EventTest = "test"

// Notification is what LoopGuard tells the human about one loop (LLD §15).
// It holds only what may leave the machine (ntfy, webhooks): the project
// folder's name, the detector, the command family, a count, a duration,
// and the short session id for the stop command. Never file paths, error
// lines, or code.
type Notification struct {
	Event    string // config.Event* or EventTest
	Project  string // the project folder's name
	Session  string // short session id (first 8 characters)
	Detector string
	Family   string // command family (D1/D2), or the tool (D4)
	Count    int
	Duration time.Duration
	// Budget describes a budget passed (budget events only), e.g. "session
	// cost $15.40 (API-equivalent), hard limit $15.00": numbers only.
	Budget string
	// SessionKey and LoopKey identify the loop for rate limiting; they are
	// never sent.
	SessionKey, LoopKey string
	At                  time.Time
}

// New returns a notification with every field cleaned for sending.
func New(ev, project, sessionKey, loopKey, detector, family string, count int, d time.Duration, at time.Time) Notification {
	return Notification{
		Event: ev, Project: cleanName(project, 60), Session: shortID(sessionKey), Detector: detector,
		Family: cleanFamily(family), Count: count, Duration: d, SessionKey: sessionKey, LoopKey: loopKey, At: at,
	}
}

// Title is "LoopGuard: <project>".
func (n Notification) Title() string {
	if n.Project == "" {
		return "LoopGuard"
	}
	return "LoopGuard: " + n.Project
}

// StopCommand is the command that stops the session's turn.
func (n Notification) StopCommand() string {
	if n.Session == "" {
		return ""
	}
	return "loopguard stop " + n.Session
}

// Body is the notification text: what happened and, unless the turn has
// already stopped, how to stop it.
func (n Notification) Body() string {
	if n.Event == EventTest {
		return "Test notification from LoopGuard. If you can read this, this channel works."
	}
	what := n.what()
	var s string
	switch n.Event {
	case config.EventLoopDetected:
		s = "Loop detected: " + what
	case config.EventStall:
		s = "No progress: " + what
	case config.EventNudge:
		s = "Sent the agent a note: " + what
	case config.EventDeny:
		s = "Blocked a repeated action: " + what
	case config.EventHalt:
		return "Stopped the turn: " + what + ". Your next message continues normally."
	case config.EventBudgetSoft:
		s = "Budget passed: " + cleanBudget(n.Budget)
	case config.EventBudgetHard:
		return "Hard budget passed: " + cleanBudget(n.Budget) + ". At the guard level, tool calls stop until the limit is raised."
	default:
		s = what
	}
	if cmd := n.StopCommand(); cmd != "" {
		s += ". To stop it: " + cmd
	}
	return s
}

// Text is the title and body on one line (terminal notifications).
func (n Notification) Text() string { return n.Title() + ": " + n.Body() }

// what describes the loop in a clause, from the allowed fields only.
func (n Notification) what() string {
	var s string
	fam := n.Family
	if fam == "" {
		fam = "a command"
	} else {
		fam = `"` + fam + `"`
	}
	switch n.Detector {
	case event.DetectorRepeatedFailure:
		s = fmt.Sprintf("%s failed %d times in a row with the same error", fam, n.Count)
	case event.DetectorExactRepeat:
		s = fmt.Sprintf("the same %s call ran %d times with the same result", fam, n.Count)
	case event.DetectorOscillation:
		s = fmt.Sprintf("a file was changed back to a previous version %d times", n.Count)
	case event.DetectorReadThrash:
		s = fmt.Sprintf("the same file or search was read %d times with no change", n.Count)
	case event.DetectorStall:
		s = fmt.Sprintf("%d tool calls with no visible progress", n.Count)
	default:
		s = fmt.Sprintf("%s (%d)", strings.ReplaceAll(n.Detector, "_", " "), n.Count)
	}
	switch {
	case n.Duration <= 0:
	case n.Duration < time.Minute:
		s += ", within a minute"
	default:
		s += fmt.Sprintf(", over %d min", int(n.Duration.Minutes()+0.5))
	}
	return s
}

// cleanFamily keeps a command family short and free of paths: a family
// is a command name and maybe a subcommand ("npm test"), but a script run
// by path would carry the path ("./tools/x/run.sh" → "run.sh").
func cleanFamily(f string) string {
	words := strings.Fields(redact.String(f))
	for i, w := range words {
		if j := strings.LastIndexAny(w, `/\`); j >= 0 {
			words[i] = w[j+1:]
		}
	}
	return cleanName(strings.Join(words, " "), 40)
}

// cleanName keeps letters, digits, spaces, and . _ - + : and clips.
func cleanName(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return r
		case strings.ContainsRune(" ._-+:", r):
			return r
		}
		return -1
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}

// shortID is the first 8 characters of a session key's session id
// ("claude-code:1a2b3c4d-…/agent" → "1a2b3c4d"), as `loopguard stop` takes
// it.
func shortID(key string) string {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		key = key[i+1:]
	}
	if i := strings.IndexByte(key, '/'); i >= 0 {
		key = key[:i]
	}
	if len(key) > 8 {
		key = key[:8]
	}
	return cleanName(key, 8)
}

// cleanBudget keeps a budget summary's letters, digits, spaces, and
// . _ - + : $ ( ) , ' and clips it at 120 characters.
func cleanBudget(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" ._-+:$(),'", r) {
			return r
		}
		return -1
	}, s)
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	return s
}
