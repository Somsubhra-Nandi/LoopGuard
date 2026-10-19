package event

import (
	"encoding/json"
	"time"
)

// AgentKind identifies the coding agent that produced an event.
type AgentKind string

// Supported agents.
const (
	AgentClaudeCode AgentKind = "claude-code"
	AgentCodex      AgentKind = "codex"
)

// Phase is the lifecycle point an event was captured at.
type Phase string

// Event phases (LLD §2, §3.1).
const (
	PhasePre          Phase = "pre"
	PhasePost         Phase = "post"
	PhasePostFailure  Phase = "post_failure"
	PhaseBatchEnd     Phase = "batch_end"
	PhaseSessionStart Phase = "session_start"
	PhaseSessionEnd   Phase = "session_end"
	PhaseStop         Phase = "stop"
	PhasePrompt       Phase = "prompt"
)

// Normalized tool names. MCP tools are "mcp:<server>:<tool>" and anything
// else is "other:<name>".
const (
	ToolBash  = "bash"
	ToolEdit  = "edit"
	ToolWrite = "write"
	ToolRead  = "read"
	ToolGrep  = "grep"
	ToolGlob  = "glob"
	ToolPatch = "patch"
)

// Event is one hook invocation (or one reconstructed transcript entry),
// already mapped to agent-neutral fields by an adapter.
type Event struct {
	ID         string // ulid
	Agent      AgentKind
	SessionID  string // agent's session_id
	AgentID    string // subagent id if any ("" = main agent)
	Phase      Phase
	ToolName   string // normalized, see the Tool* constants
	ToolUseID  string
	Input      json.RawMessage // raw tool_input
	Output     json.RawMessage // raw tool_response (post only)
	IsError    bool            // post_failure, or detected error in output
	Cwd        string
	Transcript string // transcript_path
	PermMode   string // permission_mode ("auto", "bypassPermissions", ...)
	Prompt     string // UserPromptSubmit only (stored hashed + truncated)
	ReceivedAt time.Time
	Seq        uint64          // assigned by the session
	Raw        json.RawMessage // full payload, kept in memory only
}

// EditPair holds the hashes of an edit's old and new strings, so inverse
// edits (A→B then B→A) can be spotted without keeping the text.
type EditPair struct {
	Old uint64
	New uint64
}

// ToolCall is a completed tool call: the post event plus everything the
// normalizer derived from it.
type ToolCall struct {
	Event         *Event          // the post event
	ActionFP      uint64          // see LLD §4.2
	Family        string          // e.g. "npm test", "pytest", "go test"; the tool name for non-bash tools
	Command       string          // bash only: normalized command line, for evidence
	Polling       bool            // bash only: the primary command is a polling command (exempt from D1/D2)
	ResultFP      uint64          // hash of normalized output
	ErrorSig      *ErrorSignature // nil if success
	FilePath      string          // for edit/write/read/patch: normalized path ("/"-separated)
	Edits         []EditPair      // for edit: one pair per (old, new) replacement (MultiEdit has several)
	FileHashAfter uint64          // content hash after write/edit (0 = unknown); filled in by the caller
	FailingTests  int             // failing-test count parsed from the output, -1 if none found
	TodosDone     int             // completed todo items in a todo/task update, -1 if not a todo call
	Duration      time.Duration
	Progress      float64 // progress score assigned by the session (LLD §5.1)
	// FamilyProgress is the part of Progress that is about this call's own
	// family: it recovered, its error changed to a never-seen one, or fewer
	// of its tests fail. Only this resets or suppresses repeated failures.
	FamilyProgress float64
}

// Seq returns the session sequence number of the call.
func (c *ToolCall) Seq() uint64 { return c.Event.Seq }

// Failed reports whether the call failed, outright or softly.
func (c *ToolCall) Failed() bool { return c.ErrorSig != nil }

// ErrorSignature identifies an error so that re-runs with the same failure
// can be recognized despite noise (LLD §4.4).
type ErrorSignature struct {
	Exact uint64   // xxhash of normalized key lines (source line/col masked, file names kept)
	Sim   uint64   // 64-bit simhash of the error-bearing lines
	Loc   uint64   // xxhash of the sorted source files the error lines point at (0 if none)
	Lines []string // up to 5 key lines, for evidence and nudge text
}

// Detector names, shared by detectors (which emit them) and the policy
// (which maps them to capabilities).
const (
	DetectorExactRepeat     = "exact_repeat"
	DetectorRepeatedFailure = "repeated_failure"
	DetectorOscillation     = "oscillation"
	DetectorReadThrash      = "read_thrash"
	DetectorStall           = "stall"
	DetectorBudget          = "budget"
)

// Signal is a detector's claim that a loop is happening.
type Signal struct {
	Detector string  // "repeated_failure" ...
	LoopKey  string  // stable key for this loop instance, e.g. "rf:npm test:<errsig>"
	Severity float64 // 0..1
	Count    int
	Since    uint64   // first seq in the loop
	Evidence Evidence // human-readable facts: commands tried, error lines, files
}

// Evidence is the set of facts behind a signal, used for messages, reports,
// and for matching later actions against the loop.
type Evidence struct {
	Family     string        // command family (D1/D2)
	ActionFP   uint64        // the repeated action (D1)
	Path       string        // the oscillating file (D3)
	Commands   []string      // distinct commands tried, oldest first
	ErrorLines []string      // key error lines
	Files      []string      // files edited between attempts (D2)
	Seqs       []uint64      // the calls that make up the loop, oldest first
	Elapsed    time.Duration // from the first call of the loop to the current one
	// Anchor is the error signature that identifies a repeated-failure loop
	// (D2): the policy keeps it with the loop, and later failures that match
	// it stay in the same loop, whatever the detector window holds.
	Anchor *ErrorSignature
}

// Action is what LoopGuard does about a loop.
type Action string

// Actions, in escalation order.
const (
	ActionNone  Action = "none"
	ActionNudge Action = "nudge"
	ActionDeny  Action = "deny"
	ActionHalt  Action = "halt"
)

// Decision is the policy's output for one event.
//
// In observe mode Action is always ActionNone and WouldHave records what
// enforce mode would have done, with Reason explaining why.
type Decision struct {
	Action    Action
	LoopKey   string
	Message   string // text for the agent (nudge/deny) or the human (halt stopReason)
	Notify    bool
	WouldHave Action  // the action enforce mode would have taken
	Reason    string  // why, in one line
	Detector  string  // detector behind the loop
	Severity  float64 // severity of the signal that triggered the decision
	Seq       uint64  // seq of the event the decision was made on
}
