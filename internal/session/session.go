package session

import (
	"strings"
	"time"

	"github.com/Somsubhra-Nandi/loopguard/internal/config"
	"github.com/Somsubhra-Nandi/loopguard/internal/event"
	"github.com/Somsubhra-Nandi/loopguard/internal/normalize"
)

// Buffer sizes. These bound memory, not detection: detector windows are
// configured in config.Thresholds and must fit inside them.
const (
	CallHistory   = 200 // completed calls kept per session
	FileHistoryN  = 30  // writes and edits kept per file
	FamilySigsMax = 50  // distinct error signatures remembered per family
	maxPending    = 1024
)

// SessionState is everything LoopGuard knows about one agent session. It is
// not safe for concurrent use; the daemon gives each session one actor.
type SessionState struct {
	Key       string // agent + ":" + session_id
	Agent     event.AgentKind
	StartedAt time.Time
	LastSeen  time.Time
	Repo      string                  // git root of first cwd (set by the daemon)
	Calls     *Ring[*event.ToolCall]  // last CallHistory completed calls
	Pending   map[string]*event.Event // pre events by ToolUseID awaiting post
	Files     map[string]*FileHistory // path → history
	Families  map[string]*FamilyStats // family → stats
	Errors    map[uint64]*ErrorStats  // ErrorSig.Exact → stats
	Progress  *ProgressTracker
	Loops     map[string]*LoopState // LoopKey → escalation state (owned by the policy)
	Paused    bool
	TurnSeq   uint64 // seq at last human prompt
	PermMode  string // latest permission_mode seen

	// NudgesThisTurn counts nudges (or would-have nudges) since the last
	// human prompt, for the policy's per-turn cap.
	NudgesThisTurn int
	// CallCount counts completed calls; cooldowns and quiet periods are
	// measured in calls, not seqs (which also count pre and prompt events).
	CallCount uint64

	seq       uint64
	todosDone int
	maxDist   int
}

// FileHistory is the write/edit/read history of one file.
type FileHistory struct {
	Writes    []HashRecord // content hash after each write or edit (last FileHistoryN)
	Edits     []EditRecord // (oldHash, newHash) for each edit (last FileHistoryN)
	Reads     int          // reads since the last write to this file
	LastWrite uint64       // seq
	// TouchedTurn is the TurnSeq of the turn the file was last modified in.
	TouchedTurn uint64
	touched     bool
}

// HashRecord is a file content hash observed after a write.
type HashRecord struct {
	Seq  uint64
	Hash uint64
}

// EditRecord is one (old, new) replacement of an edit call.
type EditRecord struct {
	Seq  uint64
	Pair event.EditPair
}

// FamilyStats tracks the runs of one command family.
type FamilyStats struct {
	Runs, Fails      int
	ConsecFails      int
	LastErr          *event.ErrorSignature
	LastSuccessSeq   uint64
	LastFailingTests int                     // -1 if unknown
	Sigs             []*event.ErrorSignature // distinct signatures seen (last FamilySigsMax)
}

// ErrorStats tracks one error signature across the session.
type ErrorStats struct {
	Sig      *event.ErrorSignature
	Count    int
	FirstSeq uint64
	LastSeq  uint64
}

// Stage is a loop's position in the escalation state machine (LLD §7.1).
type Stage string

// Escalation stages.
const (
	StageObserving Stage = "observing"
	StageNudged    Stage = "nudged"
	StageDenying   Stage = "denying"
	StageHalted    Stage = "halted"
)

// LoopState is the policy's escalation state for one LoopKey.
type LoopState struct {
	Key      string
	Detector string
	Stage    Stage
	// What later actions belong to this loop: the repeated action (D1), the
	// failing family (D2), or the oscillating file (D3).
	ActionFP uint64
	Family   string
	Path     string
	// Anchor is the error signature of a repeated-failure loop. Failures of
	// Family that match it keep this loop's key for as long as the loop
	// state lives (until the next human prompt).
	Anchor *event.ErrorSignature

	StartedAt      time.Time // when the loop's first call happened
	FirstSeq       uint64
	StageSeq       uint64 // seq the current stage was entered at
	LastSignalSeq  uint64
	LastSignalCall uint64 // CallCount at the last signal
	LastNudgeCall  uint64 // CallCount at the last nudge
	Nudges         int
	// Occurrences counts signals since the current stage was entered.
	Occurrences int
	// Denies counts (would-have) denies since the loop entered Denying.
	Denies       int
	LastSeverity float64
}

// New returns an empty session. Only th.Detectors.SimHashMaxDistance is
// used, to tell new error signatures from known ones.
func New(key string, agent event.AgentKind, th config.Thresholds) *SessionState {
	return &SessionState{
		Key:       key,
		Agent:     agent,
		Calls:     NewRing[*event.ToolCall](CallHistory),
		Pending:   make(map[string]*event.Event),
		Files:     make(map[string]*FileHistory),
		Families:  make(map[string]*FamilyStats),
		Errors:    make(map[uint64]*ErrorStats),
		Progress:  NewProgressTracker(CallHistory),
		Loops:     make(map[string]*LoopState),
		todosDone: -1,
		maxDist:   th.Detectors.SimHashMaxDistance,
	}
}

// Seq returns the last assigned sequence number.
func (s *SessionState) Seq() uint64 { return s.seq }

// ResumeAt makes numbering continue after seq, for a session whose earlier
// events were numbered by a previous state (an evicted actor or an earlier
// daemon run). It never moves numbering backwards.
func (s *SessionState) ResumeAt(seq uint64) {
	if seq > s.seq {
		s.seq = seq
	}
}

// Assign gives ev the next sequence number and updates LastSeen.
func (s *SessionState) Assign(ev *event.Event) {
	s.seq++
	ev.Seq = s.seq
	if s.StartedAt.IsZero() {
		s.StartedAt = ev.ReceivedAt
	}
	if ev.ReceivedAt.After(s.LastSeen) {
		s.LastSeen = ev.ReceivedAt
	}
	if ev.PermMode != "" {
		s.PermMode = ev.PermMode
	}
}

// HumanPrompt marks a turn boundary: detector windows restart after it, and
// loop escalation state and the per-turn nudge count are cleared.
func (s *SessionState) HumanPrompt(ev *event.Event) {
	s.Assign(ev)
	s.TurnSeq = ev.Seq
	s.Loops = make(map[string]*LoopState)
	s.NudgesThisTurn = 0
}

// AddPending stores a pre event until its post arrives.
func (s *SessionState) AddPending(ev *event.Event) {
	if ev.ToolUseID == "" {
		return
	}
	if len(s.Pending) >= maxPending {
		// Posts that never arrived (interrupted calls); forget them.
		s.Pending = make(map[string]*event.Event)
	}
	s.Pending[ev.ToolUseID] = ev
}

// TakePending removes and returns the pre event for a tool use, if any.
func (s *SessionState) TakePending(toolUseID string) *event.Event {
	ev, ok := s.Pending[toolUseID]
	if ok {
		delete(s.Pending, toolUseID)
	}
	return ev
}

// Window returns the calls detectors may look at: at most n of the newest
// calls, none from before the last human prompt. Oldest first.
func (s *SessionState) Window(n int) []*event.ToolCall {
	calls := s.Calls.Tail(n)
	i := 0
	for i < len(calls) && calls[i].Seq() <= s.TurnSeq {
		i++
	}
	return calls[i:]
}

// WindowStart returns the lowest seq inside Window(n), or the next seq if
// the window is empty.
func (s *SessionState) WindowStart(n int) uint64 {
	w := s.Window(n)
	if len(w) == 0 {
		return s.seq + 1
	}
	return w[0].Seq()
}

// File returns the history of path, creating it if needed.
func (s *SessionState) File(path string) *FileHistory {
	fh := s.Files[path]
	if fh == nil {
		fh = &FileHistory{}
		s.Files[path] = fh
	}
	return fh
}

// Family returns the stats of a family, creating them if needed.
func (s *SessionState) Family(name string) *FamilyStats {
	fs := s.Families[name]
	if fs == nil {
		fs = &FamilyStats{LastFailingTests: -1}
		s.Families[name] = fs
	}
	return fs
}

// Record adds a completed call to the session: it scores the call's
// progress (LLD §5.1) against the state before the call, then updates the
// call history, family, error, and file statistics. c.Event.Seq must
// already be assigned.
func (s *SessionState) Record(c *event.ToolCall) {
	c.Progress, c.FamilyProgress = s.score(c)
	s.Progress.Record(c.Seq(), c.Progress, c.Family, c.FamilyProgress)
	s.Calls.Push(c)
	s.CallCount++

	seq := c.Seq()
	if c.Family != "" {
		fs := s.Family(c.Family)
		fs.Runs++
		if c.Failed() {
			fs.Fails++
			fs.ConsecFails++
			fs.LastErr = c.ErrorSig
			fs.LastFailingTests = c.FailingTests
			if !s.knownSig(fs, c.ErrorSig) {
				fs.Sigs = append(fs.Sigs, c.ErrorSig)
				if len(fs.Sigs) > FamilySigsMax {
					fs.Sigs = fs.Sigs[1:]
				}
			}
		} else {
			fs.ConsecFails = 0
			fs.LastSuccessSeq = seq
			fs.LastFailingTests = -1
		}
	}
	if c.ErrorSig != nil {
		es := s.Errors[c.ErrorSig.Exact]
		if es == nil {
			es = &ErrorStats{Sig: c.ErrorSig, FirstSeq: seq}
			s.Errors[c.ErrorSig.Exact] = es
		}
		es.Count++
		es.LastSeq = seq
	}
	if c.FilePath != "" {
		fh := s.File(c.FilePath)
		switch c.Event.ToolName {
		case event.ToolRead:
			fh.Reads++
		case event.ToolEdit, event.ToolWrite, event.ToolPatch:
			if c.Failed() {
				break
			}
			fh.Reads = 0
			fh.LastWrite = seq
			fh.TouchedTurn = s.TurnSeq
			fh.touched = true
			for _, p := range c.Edits {
				fh.Edits = appendCapped(fh.Edits, EditRecord{Seq: seq, Pair: p})
			}
			if c.FileHashAfter != 0 {
				fh.Writes = appendCapped(fh.Writes, HashRecord{Seq: seq, Hash: c.FileHashAfter})
			}
		}
	}
	if c.TodosDone >= 0 && isTodoList(c) {
		s.todosDone = c.TodosDone
	}
}

func appendCapped[T any](xs []T, x T) []T {
	xs = append(xs, x)
	if len(xs) > FileHistoryN {
		xs = xs[len(xs)-FileHistoryN:]
	}
	return xs
}

func (s *SessionState) knownSig(fs *FamilyStats, sig *event.ErrorSignature) bool {
	for _, k := range fs.Sigs {
		if normalize.SameError(k, sig, s.maxDist) {
			return true
		}
	}
	return false
}

func isTodoList(c *event.ToolCall) bool {
	return strings.HasSuffix(c.Event.ToolName, "TodoWrite")
}

// Progress scores (LLD §5.1).
const (
	scoreFamilyRecovered = 1.0
	scoreTodoCompleted   = 0.8
	scoreNewErrorSig     = 0.6
	scoreFewerFailures   = 0.6
	scoreNewFileTouched  = 0.4
	scoreNewFamilyOK     = 0.3
)

// score computes the progress score of c from the state before c, and the
// part of it that is about c's own family (the family recovered, its error
// changed to a never-seen one, or fewer of its tests fail). Only family
// progress may reset or suppress a repeated-failure loop; the overall score
// also counts general activity (new files, new families, todos). A call
// that matches several rows gets the highest score.
func (s *SessionState) score(c *event.ToolCall) (overall, family float64) {
	best := 0.0
	up := func(v float64) {
		if v > best {
			best = v
		}
	}
	upFamily := func(v float64) {
		up(v)
		if v > family {
			family = v
		}
	}
	isBash := c.Event.ToolName == event.ToolBash
	if c.Family != "" && !c.Polling {
		fs := s.Families[c.Family]
		switch {
		case c.Failed() && fs != nil:
			if fs.LastErr != nil && !normalize.SameError(fs.LastErr, c.ErrorSig, s.maxDist) && !s.knownSig(fs, c.ErrorSig) {
				upFamily(scoreNewErrorSig)
			}
			if c.FailingTests >= 0 && fs.LastFailingTests >= 0 && c.FailingTests < fs.LastFailingTests {
				upFamily(scoreFewerFailures)
			}
		case !c.Failed() && fs != nil && fs.ConsecFails > 0:
			upFamily(scoreFamilyRecovered)
		case !c.Failed() && fs == nil && isBash:
			up(scoreNewFamilyOK)
		}
	}
	switch c.Event.ToolName {
	case event.ToolEdit, event.ToolWrite, event.ToolPatch:
		if !c.Failed() && c.FilePath != "" {
			fh := s.Files[c.FilePath]
			if fh == nil || !fh.touched || fh.TouchedTurn != s.TurnSeq {
				up(scoreNewFileTouched)
			}
		}
	}
	if c.TodosDone >= 0 {
		if isTodoList(c) {
			if c.TodosDone > s.todosDone && s.todosDone >= 0 {
				up(scoreTodoCompleted)
			}
		} else if c.TodosDone > 0 {
			up(scoreTodoCompleted)
		}
	}
	return best, family
}
