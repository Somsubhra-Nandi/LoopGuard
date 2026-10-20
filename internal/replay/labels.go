package replay

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/home"
)

// Label verdicts.
const (
	VerdictReal          = "real"
	VerdictFalsePositive = "false_positive"
	VerdictMissed        = "missed"
)

// Label is one line of labels.jsonl: a verdict on a detected loop, or a
// loop the detectors missed. Calls are numbered from 1 per stream, as in
// the replay report and `replay --session <id> --calls`.
type Label struct {
	Kind      string    `json:"kind"` // "detected" | "missed"
	SessionID string    `json:"session_id"`
	AgentID   string    `json:"agent_id,omitempty"`
	LoopKey   string    `json:"loop_key,omitempty"`
	Detector  string    `json:"detector,omitempty"`
	FromCall  int       `json:"from_call"`
	ToCall    int       `json:"to_call"`
	Verdict   string    `json:"verdict"` // real | false_positive | missed
	Note      string    `json:"note,omitempty"`
	LabeledAt time.Time `json:"labeled_at"`
}

// DetectedLabel returns a label for a detected loop.
func DetectedLabel(l *Loop, verdict string, at time.Time) Label {
	return Label{
		Kind: "detected", SessionID: l.SessionID, AgentID: l.AgentID, LoopKey: l.LoopKey, Detector: l.Detector,
		FromCall: l.FirstCall, ToCall: l.LastCall, Verdict: verdict, LabeledAt: at.UTC(),
	}
}

// Home returns LoopGuard's data directory: $LOOPGUARD_HOME, or ~/.loopguard.
func Home() (string, error) { return home.Dir() }

// DefaultLabelsPath returns <Home>/labels.jsonl.
func DefaultLabelsPath() (string, error) {
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "labels.jsonl"), nil
}

// ReadLabels reads a labels file. A missing file has no labels; malformed
// lines are skipped.
func ReadLabels(path string) ([]Label, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Label
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var l Label
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.SessionID != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// AppendLabel appends one label, creating the file and its directory.
func AppendLabel(path string, l Label) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	b, err := json.Marshal(l)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ParseStream splits "session" or "session/agent".
func ParseStream(s string) (session, agent string) {
	session, agent, _ = strings.Cut(s, "/")
	return session, agent
}

// sameStream compares by session id prefix (labels may use a short id).
func sameStream(lSession, lAgent string, loop *Loop) bool {
	return lAgent == loop.AgentID && lSession != "" && strings.HasPrefix(loop.SessionID, lSession)
}

func overlaps(a1, a2, b1, b2 int) bool { return a1 <= b2 && b1 <= a2 }

// LabelFor returns the latest verdict label on a detected loop: same
// stream, same loop key, overlapping calls.
func LabelFor(labels []Label, loop *Loop) (Label, bool) {
	var found Label
	ok := false
	for _, l := range labels {
		if l.Kind == "detected" && l.LoopKey == loop.LoopKey && sameStream(l.SessionID, l.AgentID, loop) &&
			overlaps(l.FromCall, l.ToCall, loop.FirstCall, loop.LastCall) {
			found, ok = l, true
		}
	}
	return found, ok
}
