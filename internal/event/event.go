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
	// PhaseAutoPrompt is a prompt submission that isn't a human turn (an
	// empty prompt, or injected content such as a task notification). It
	// doesn't reset loop state.
	PhaseAutoPrompt Phase = "auto_prompt"
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
	Model      string // SessionStart only, if the agent reports it
	Source     string // SessionStart only: startup | resume | clear | compact | ...
	ReceivedAt time.Time
	// RunTime is the tool's own run time when the agent reports it
	// (PostToolUse duration_ms); 0 if unknown. Unlike the time from the pre
	// event to the post, it excludes waiting for a permission decision.
	RunTime time.Duration
	// Interactive is set by the daemon from the hook's environment: the
	// agent runs in a terminal a human is attending (LLD §15).
	Interactive bool
	Seq         uint64          // assigned by the session
	Raw         json.RawMessage // full payload, kept in memory only
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
	Event    *Event // the post event
	ActionFP uint64 // see LLD §4.2
	Family   string // e.g. "npm test", "pytest", "go test"; the tool name for non-bash tools
	Command  string // bash only: normalized command line, for evidence
	// Identity (bash only) is the primary command's family plus its
	// meaningful arguments (normalize.Identity): what a block matches.
	Identity      string
	Polling       bool            // bash only: the primary command is a polling command (exempt from D1/D2)
	ResultFP      uint64          // hash of normalized output
	ErrorSig      *ErrorSignature // nil if success
	FilePath      string          // for edit/write/read/patch: normalized path ("/"-separated)
	Edits         []EditPair      // for edit: one pair per (old, new) replacement (MultiEdit has several)
	FileHashAfter uint64          // content hash after write/edit (0 = unknown); filled in by the caller
	FailingTests  int             // failing-test count parsed from the output, -1 if none found
	TodosDone     int             // completed todo items in a todo/task update, -1 if not a todo call
	Duration      time.Duration
	// ReadOnly (bash only): the primary command only reads or searches
	// (read_only_commands).
	ReadOnly bool
	// Explore is set by the session: the call looked at something new in
	// the session (a file read, a URL fetched, a search or read-only command
	// not run before). The stall detector counts it as progress (LLD §6 D5).
	Explore  bool
	Progress float64 // progress score assigned by the session (LLD §5.1)
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
	// Best holds up to 3 lines that say what went wrong (assertion
	// payloads and error messages before test names; never FAIL or
	// summary lines), in output order. Nudges show these.
	Best []string
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
	// DetectorSelfProtection is not a loop detector: it marks the denies of
	// agent actions that would turn LoopGuard off (internal/guard).
	DetectorSelfProtection = "self_protection"
	// DetectorUserStop is not a loop detector either: it marks the halt
	// the user asked for with `loopguard stop`.
	DetectorUserStop = "user_stop"
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
	// Failures and Repeats count, for a stall (D5), the failed calls and
	// the repeated reads and searches among its calls.
	Failures, Repeats int
	// Anchor is the error signature that identifies a repeated-failure loop
	// (D2): the policy keeps it with the loop, and later failures that match
	// it stay in the same loop, whatever the detector window holds.
	Anchor *ErrorSignature
	// Budget is set on a budget signal (D6).
	Budget *BudgetEvidence
}

// BudgetEvidence is a budget limit that was passed (D6).
type BudgetEvidence struct {
	Scope  string  // session | daily | repo
	Metric string  // cost_usd | tokens | wall_minutes | tool_calls
	Level  string  // soft | hard
	Value  float64 // the current value
	Limit  float64 // the limit passed
	Key    string  // the limit's config key, e.g. budgets.session.hard_usd
	// Period identifies what the value is counted over: the session id, the
	// local date, or the date and repo. A soft limit is announced once per
	// period.
	Period string
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

// Rank orders actions by escalation: none (and "") 0, nudge 1, deny 2,
// halt 3.
func (a Action) Rank() int {
	switch a {
	case ActionNudge:
		return 1
	case ActionDeny:
		return 2
	case ActionHalt:
		return 3
	}
	return 0
}

// Decision is the policy's output for one event.
//
// WouldHave records what the guard level does, with Reason explaining
// why; Action is that action when the session's level allows it (assist:
// nudges only; observe and notify: nothing), else ActionNone.
type Decision struct {
	Action  Action
	LoopKey string
	Message string // text for the agent (nudge/deny) or the human (halt stopReason)
	// Terminal, if set, is a notification for the human's terminal, sent
	// with the hook reply (interactive sessions only; LLD §15).
	Terminal  string
	WouldHave Action  // the action the guard level takes
	Reason    string  // why, in one line
	Detector  string  // detector behind the loop
	Severity  float64 // severity of the signal that triggered the decision
	Seq       uint64  // seq of the event the decision was made on
}
