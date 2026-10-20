# LoopGuard: Low-Level Design (LLD)

| | |
|---|---|
| Status | Draft v0.1 |
| Date | 2026-10-19 |
| Depends on | [HLD.md](./HLD.md) |

This document specifies modules, data structures, algorithms, schemas, and interfaces precisely enough to implement from. Default thresholds are starting values and get tuned with the replay harness (see [TDD.md](./TDD.md) §6).

> **Hook API note:** The Claude Code hook fields below follow the official hooks reference (code.claude.com/docs/en/hooks) as of Oct 2026. Codex hooks are newer and still changing, so verify field names against developers.openai.com/codex/hooks for the installed Codex version. All agent-specific details live inside the adapters.

---

## 1. Repository layout

```
loopguard/
├── cmd/loopguard/main.go           # cobra root: hook, daemon, init, uninstall, doctor, status, capture, report, replay, pause, tail
├── cmd/loopguard-hook/main.go      # the binary the hooks run: only the shim, no cobra, minimal imports (fast start)
├── internal/
│   ├── event/                      # core types: Event, ToolCall, Decision, Signal
│   ├── adapter/
│   │   ├── adapter.go              # Adapter interface + registry
│   │   ├── claudecode/             # parse CC hook JSON, render CC decisions, install into settings.json
│   │   └── codex/                  # parse Codex hook JSON, render decisions, install into hooks.json/config.toml
│   ├── shim/                       # loopguard-hook and `loopguard hook`: stdin → daemon → stdout/exit, auto-start daemon
│   ├── daemon/                     # http server, auth, session manager, actors, lifecycle
│   ├── capture/                    # capture mode (flag file), payload capture, anonymized export for golden tests
│   ├── anon/                       # OS-independent path handling for anonymization (usernames, home spellings)
│   ├── home/                       # LoopGuard home: daemon.json, token; home/homedir is the dependency-free part the shim uses
│   ├── proc/                       # detached spawn and pid liveness (package syscall only)
│   ├── textdiff/                   # unified line diff for `init --dry-run`
│   ├── normalize/                  # text normalizer, command parser, error-signature extractor, simhash
│   ├── session/                    # SessionState, ring buffers, per-file history, progress tracker
│   ├── detect/                     # Detector interface + exact_repeat, repeated_failure, oscillation, read_thrash, stall, budget
│   ├── policy/                     # escalation state machine, cooldowns, message templates
│   ├── pipeline/                   # normalize → session → detect → policy, shared by replay and the daemon
│   ├── cost/                       # transcript tailer, usage dedupe, pricing, budgets
│   ├── notify/                     # desktop, ntfy, webhook (slack/discord), terminal-sequence
│   ├── store/                      # sqlite (modernc.org/sqlite), migrations, repositories
│   ├── replay/                     # offline transcript → event stream → detectors → report
│   ├── dashboard/                  # go:embed templates + htmx, REST handlers
│   ├── config/                     # config.yaml + .loopguard.yaml merge, defaults, validation
│   └── redact/                     # secret patterns
├── pricing.yaml                    # default model price table (user-overridable)
├── testdata/
│   ├── transcripts/                # anonymized real sessions (labeled)
│   └── hooks/                      # golden hook payloads per agent/version
├── scripts/                        # install.sh, release helpers
└── docs/
```

Dependencies (kept small): `spf13/cobra`, `modernc.org/sqlite`, `gopkg.in/yaml.v3`, `pelletier/go-toml/v2`, `gen2brain/beeep` (desktop notifications), `mvdan.cc/sh/v3/syntax` (bash parsing), and the standard library for HTTP.

---

## 2. Core types (`internal/event`)

```go
type AgentKind string // "claude-code" | "codex"

type Phase string // "pre" | "post" | "post_failure" | "batch_end" | "session_start" | "session_end" | "stop" | "prompt"

type Event struct {
    ID          string            // ulid
    Agent       AgentKind
    SessionID   string            // agent's session_id
    AgentID     string            // subagent id if any ("" = main agent)
    Phase       Phase
    ToolName    string            // normalized: "bash", "edit", "write", "read", "grep", "glob", "patch", "mcp:<server>:<tool>", "other:<name>"
    ToolUseID   string
    Input       json.RawMessage   // raw tool_input
    Output      json.RawMessage   // raw tool_response (post only)
    IsError     bool              // post_failure, or detected error in output
    Cwd         string
    Transcript  string            // transcript_path
    PermMode    string            // permission_mode ("auto", "bypassPermissions", ...)
    Prompt      string            // UserPromptSubmit only (stored hashed + truncated)
    ReceivedAt  time.Time
    Seq         uint64            // assigned by session actor
    Raw         json.RawMessage   // full payload, kept in memory only
}

type ToolCall struct {                // derived from a post event (which carries the tool input)
    Event         *Event              // the post event
    ActionFP      uint64              // see §4.2
    Family        string              // e.g. "npm test", "pytest", "go test"; the tool name for non-bash tools
    Command       string              // bash: normalized, redacted command (evidence)
    Polling       bool                // bash: primary command is a polling command (§4.3)
    ResultFP      uint64              // hash of normalized, redacted output
    ErrorSig      *ErrorSignature     // nil if success (failed or soft failure otherwise)
    FilePath      string              // for edit/write/read/patch: normalized "/"-separated path
    Edits         []EditPair          // for edit: one (old, new) hash pair per replacement (MultiEdit has several)
    FileHashAfter uint64              // content hash after write/edit (0 = unknown); supplied by the caller
    FailingTests  int                 // failing-test count parsed from the output, -1 if none
    TodosDone     int                 // completed todo items (TodoWrite / task updates), -1 if n/a
    Duration      time.Duration
    Progress      float64             // progress score (§5.1)
    FamilyProgress float64            // the part of Progress about this call's own family (§5.1)
}

type ErrorSignature struct {
    Exact   uint64   // xxhash of masked key lines + the source files referenced (§4.4)
    Sim     uint64   // 64-bit simhash of the error-bearing lines
    Loc     uint64   // xxhash of the sorted source files referenced (0 if none)
    Lines   []string // up to 5 key lines (normalized, redacted, line numbers kept), for evidence/nudge text
}

type Signal struct {
    Detector  string   // "repeated_failure" ...
    LoopKey   string   // stable key for this loop instance, e.g. "rf:npm test:<errsig>"
    Severity  float64  // 0..1
    Count     int
    Since     uint64   // first seq in the loop
    Evidence  Evidence // facts: family, ActionFP, path, commands, error lines, files, seqs, elapsed, anchor signature
}

type Action string // "none" | "nudge" | "deny" | "halt"

type Decision struct {
    Action    Action
    LoopKey   string
    Message   string   // text for the agent (nudge/deny) or the human (halt stopReason)
    Notify    bool
    WouldHave Action   // observe mode: what enforce mode would have done (Action stays "none")
    Reason    string   // one line: why
    Detector  string
    Severity  float64
    Seq       uint64   // event the decision was made on
}
```

---

## 3. Adapters (`internal/adapter`)

```go
type Adapter interface {
    Kind() AgentKind
    Parse(raw []byte) (*event.Event, error)
    Render(ev *event.Event, d event.Decision) (stdout []byte, exitCode int)
    Install(opts InstallOpts) (InstallReport, error) // merge hooks, backup, idempotent
    Uninstall() error
    Doctor() []Check
}
```

### 3.1 Claude Code: event mapping

| Hook event | Phase | Notes |
|---|---|---|
| `SessionStart` | session_start | Records `source` (startup/resume/compact/fork), `model` if present, transcript path |
| `UserPromptSubmit` | prompt | Marks a human turn: **resets stall counters**. Also fires on scheduled tasks and `/loop`; treat `prompt_id` changes as turn boundaries |
| `PreToolUse` | pre | Enforcement point (deny) |
| `PostToolUse` | post | Main detection point |
| `PostToolUseFailure` | post_failure | `IsError = true` |
| `PostToolBatch` | batch_end | Batch boundary for parallel calls; can halt before the next model call |
| `Stop` | stop | End of a turn; session summary update |
| `SessionEnd` | session_end | Flush and finalize. Keep work tiny: the SessionEnd hook budget is short |

Tool name normalization: `Bash`/`PowerShell` → `bash`, `Edit`/`MultiEdit` → `edit`, `Write` → `write`, `Read` → `read`, `NotebookEdit` → `edit`, `Grep` → `grep`, `Glob` → `glob`, `mcp__srv__tool` → `mcp:srv:tool`, everything else → `other:<name>`.

### 3.2 Claude Code: rendering decisions

| Decision | Phase | Output (stdout JSON, exit 0) |
|---|---|---|
| none | any | no output, exit 0 |
| nudge | post / post_failure | `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"<msg>"}}` (use `PostToolUseFailure` as the event name for failures) |
| deny | pre | `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"<msg>"}}` |
| halt | pre / post / batch_end | `{"continue":false,"stopReason":"<msg>","systemMessage":"LoopGuard halted this session: <short>"}` |
| notify (side effect) | any | may add `"terminalSequence":"\u001b]777;notify;LoopGuard;<msg>\u0007"` for a terminal notification |

Rules:
- Print **only** the JSON object on stdout; never print anything else from the shim.
- Never exit 2 from the shim. JSON decisions are explicit, and exit codes have per-event side effects.
- Nudge text must be **factual statements**, not imperative "system" instructions. The hooks docs warn that text framed as out-of-band system commands can trigger Claude's prompt-injection defenses (see §7.3 templates).
- Keep `additionalContext` well under the 10,000-character cap (target < 1,200 chars).

### 3.3 Claude Code: installed config

`loopguard init` merges this into the user settings (`~/.claude/settings.json`, or `$CLAUDE_CONFIG_DIR/settings.json`); `--project` writes the repo's `.claude/settings.local.json` instead, and `--settings <file>` any file. `HOOK` is the **absolute path** of `loopguard-hook` (by default the one next to the `loopguard` executable; `--hook` overrides):

```json
{
  "hooks": {
    "SessionStart":       [{ "hooks": [{ "type": "command", "command": "HOOK", "args": ["--agent", "claude-code"], "timeout": 5 }] }],
    "UserPromptSubmit":   [{ "hooks": [{ "type": "command", "command": "HOOK", "args": ["--agent", "claude-code"], "timeout": 2 }] }],
    "PreToolUse":         [{ "matcher": "*", "hooks": [{ "type": "command", "command": "HOOK", "args": ["--agent", "claude-code"], "timeout": 2 }] }],
    "PostToolUse":        [{ "matcher": "*", "hooks": [{ "type": "command", "command": "HOOK", "args": ["--agent", "claude-code"], "timeout": 2 }] }],
    "PostToolUseFailure": [{ "matcher": "*", "hooks": [{ "type": "command", "command": "HOOK", "args": ["--agent", "claude-code"], "timeout": 2 }] }],
    "PostToolBatch":      [{ "hooks": [{ "type": "command", "command": "HOOK", "args": ["--agent", "claude-code"], "timeout": 2 }] }],
    "Stop":               [{ "hooks": [{ "type": "command", "command": "HOOK", "args": ["--agent", "claude-code"], "timeout": 2 }] }],
    "SessionEnd":         [{ "hooks": [{ "type": "command", "command": "HOOK", "args": ["--agent", "claude-code"], "timeout": 1 }] }]
  }
}
```

- Exec form (`command` + `args`) avoids shell quoting issues: a path with spaces works, and on Windows it runs the real `.exe`. The path is always absolute, so the PATH Claude Code inherits doesn't matter.
- **LoopGuard's hooks are recognized by the command's base name** (`loopguard-hook` or `loopguard-hook.exe`, any directory, either slash). That makes `init` idempotent and `uninstall` remove only ours.
- **Merge, never overwrite:** every other setting and every other tool's hooks (including ones in the same matcher group) are kept; the file's key order, indentation, and line endings are kept too. `init` puts exactly one hook, as above, on each event: a LoopGuard hook that differs (old path, timeout) is replaced where it is; LoopGuard hooks on events not in the table are removed. Unchanged → nothing is written.
- **Backup** before the first change: `<file>.loopguard.bak`, never overwritten. A file that isn't valid JSON (or whose `hooks` isn't an object of arrays) is refused, untouched. `--dry-run` prints a unified diff. The file is replaced atomically (temp file + rename).
- `uninstall` removes LoopGuard's hooks from every event (and an event or `hooks` left empty), nothing else, and keeps the backup.
- `onFailure` is never set to `"block"`, so LoopGuard stays fail-open.
- **Alternative (v0.3+):** `type: "http"` hooks pointing straight at the daemon save the process spawn. The trade-off: when the daemon is down, Claude Code shows a non-blocking hook-error notice. Offer it as an opt-in `--transport http`.
- Plugin packaging: the same structure in a plugin's `hooks/hooks.json`, with `${CLAUDE_PLUGIN_ROOT}/bin/loopguard-hook`.

### 3.4 Codex: mapping (verify per version)

- Hooks are opt-in: the installer enables the hooks feature flag in `~/.codex/config.toml` (the key name has changed between versions, so detect by `codex --version`) and writes `~/.codex/hooks.json` with the same `{"hooks": {...}}` shape.
- Events used: `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop` (plus `PostToolUseFailure`/`SessionEnd` where available).
- Tool names seen: `Bash`, `apply_patch`, `mcp__*`. Map `apply_patch` → `patch`, and parse the patch body to extract touched files and hunks so oscillation detection works for Codex too.
- Decisions: `PreToolUse` supports **deny only** (allow/ask are rejected, and it doesn't accept `additionalContext`). Deliver nudges via `PostToolUse` feedback/context, and halt via the stop/`continue:false` path that Codex supports on `PostToolUse`/`Stop`.
- Project-scope hooks load only for trusted projects, so the installer defaults to user scope.
- `Doctor()` must confirm the feature flag is on and that a test event reaches the daemon.

---

## 4. Normalization and fingerprints (`internal/normalize`)

### 4.1 Text normalizer

Applied to command strings and tool outputs before hashing, **after redaction** (§13: secrets are replaced on the raw text first, so placeholders cannot split a token and leave part of it behind):

1. Strip ANSI escape codes and `\r`-overwritten progress lines.
2. Replace with placeholders:
   - ISO/RFC timestamps, `HH:MM:SS(.mmm)` → `<TS>`
   - durations (`1.23s`, `450ms`, `in 3m2s`) → `<DUR>`
   - UUIDs, ULIDs, git SHAs (≥7 hex chars), hex addresses `0x…` → `<ID>`
   - absolute temp paths (`/tmp/...`, `/var/folders/...`, `%TEMP%`) → `<TMP>`
   - PIDs and ports in common phrases (`pid 1234`, `:54321`) → `<N>`
   - numbers with ≥4 digits → `<N>` (keep small numbers, since they're often meaningful: line numbers, counts)
3. Collapse whitespace; drop empty lines; lower-case only for the *family* key (not the error signature).
4. Truncate inputs to 64 KB before processing. Tool **outputs keep their last 64 KB** (cut at a line start), since errors are at the end; commands keep their first 64 KB.

Implemented as a single-pass scanner (temp-path and RFC-date regexes run only behind cheap substring prefilters); a regexp per placeholder was too slow for the 10k-calls/s budget.

### 4.2 Action fingerprint (`ActionFP`)

`xxhash64(tool + "\x00" + canonical(input))`, where canonical is:

| Tool | Canonical input |
|---|---|
| bash | redacted, normalized command (parsed with `mvdan/sh` and re-printed, so `npm  test` == `npm test`); env-var prefixes kept |
| edit | `path + hash(old_string) + hash(new_string)` (one pair per MultiEdit replacement) |
| write | `path + hash(content)` |
| read | `path + offset + limit` |
| grep/glob | pattern + path |
| patch | sorted list of `(path, hunk_hash)` |
| mcp/other | canonical JSON (sorted keys) of input |

### 4.3 Command family

For bash: parse the AST and take the first "real" command of each pipeline (the producer: `npm test 2>&1 | tail` → `npm test`) and every command of a list (`&&`, `||`, `;`), recursing into subshells, blocks, `if`/`for`/`while` bodies, and `bash|sh|zsh -c '…'`.

- **Skipped** (setup, not the result): `cd`, `pushd`, `popd`, `export`, `source`, `.`, `set`, `unset`, `alias`, `true`, `:`, `local`, `declare`, `typeset`, `readonly`, `shopt`, `trap`; **leading `NAME=value` assignments** (`FOO=bar npm test` → `npm test`, also in the whitespace fallback); and **PowerShell variable assignments** (`$env:FOO='x'`, `$x = 1`), so `$env:FOO='x'; npm test` → `npm test`.
- **Wrappers stripped**, with their own flags and arguments: `time`, `env` (and its `X=Y`), `command`, `exec`, `nohup`, `sudo` (and `-u user`), `nice` (and its level), `timeout` (and its duration), `xargs`.
- The command name is lower-cased, with its directory and a Windows extension (`.exe`, `.cmd`, `.bat`, `.ps1`) removed.
- **Family** = command name + the first argument **only if it is the first argument and looks like a subcommand** (`^[a-z][a-z0-9:_-]*$`). So: `npm test`, `npx jest`, `go test`, `cargo build`, but `pytest tests/x.py` → `pytest`, `pytest -k foo` → `pytest`, `git -C dir status` → `git`, `sleep 30` → `sleep`. Flag arity can't be known in general, so only the first argument counts.
- **Runners:** `python|python3|py -m X …` and `uv|poetry|pipenv|hatch|pdm run X …` take X's family (`python -m pytest` → `pytest`). `npm|yarn|pnpm|bun run <script>` keep the script (`npm run build` ≠ `npm run test`).
- Multi-command lines produce a family set; the **primary** family is the last command (usually the one whose result matters). Non-bash tools use the tool name as their family.
- Commands that don't parse as bash (PowerShell, truncated heredocs) fall back to whitespace splitting; quotes around the command word are dropped (`"C:\py\python.exe" -I` → `python`).

**Polling commands** (exempt from D1/D2) are matched on the **primary command only**: its non-flag words must start with a polling prefix (`sleep`, `watch`, `gh run watch`, `gh pr checks`, `git status`, `docker ps`, `ls`), or its text must match a polling pattern (`^kubectl\b.*\s(-w|--watch)\b`, `^curl\b.*/health`). `sleep 5 && npm test` is not polling; `cd x && sleep 10` is. Both lists are configurable (`polling_families`, `polling_patterns`).

**Read-only commands** (`grep`, `rg`, `ag`, `cat`, `head`, `tail`, `less`, `more`, `find`, `fd`, `ls`, `tree`, `wc`, `git log|diff|show|status|blame|grep`, `Select-String`, `Get-Content`, `findstr`, `type`, `docker logs`, `kubectl logs`, `aws logs`, `journalctl`, `curl`, `wget`, `echo`, `printf`; configurable as `read_only_commands`) are matched the same way. Their output quotes other text, so it is never a soft failure (§4.4); a non-zero exit is still a failure.

### 4.4 Error signature

Input: the tail (last 8 KB of the normalized output) of a failed call, or of a **soft failure**: a **bash** call that exited 0 but whose output has a failure marker (`FAILED`, `Error:`, `Traceback`, `panic:`, `npm ERR!`, `error[E`, `✕`, `AssertionError`) **at the start of a line** (after optional whitespace). `Error:` counts only for test and build runner families (`pytest`, `jest`, `vitest`, `go test`, `cargo test|build`, `tsc`, `npm|yarn|pnpm|bun test`, `mvn`, `gradle`, `make`, `dotnet test`, and their `npx`/`run`/wrapper variants; configurable as `error_marker_families`): other programs print `Error:` lines as ordinary output, and `KeyError: 'x'` is not a failure marker at all. Soft failures apply only to bash, and never to read-only commands (§4.3): `grep -rn "Error:" src` or reading a log is not a failure.

1. Normalize (§4.1).
2. **Mask source locations**, per line: `file.ext:12:5` → `file.ext:<LN>`, `file.ext(12,5)` → `file.ext(<LN>)`, `line 12` → `line <LN>`, and code-frame gutters `> 12 |` → `> <LN> |`. An edit that shifts the failing line must not change the error. The **file names stay**.
3. Select **key lines** (on the masked lines): lines containing (case-insensitively) `error`, `exception`, `fail`, `panic`, `traceback`, `assert`, `expected`, `undefined`, `cannot`, `not found`, `denied`, `refused`, `timeout`, `received`, `actual`, or `●` (jest test headers), or the whole words `got`, `want`, `left`, `right`. The extra words catch assertion payloads with no error word (`got 4, want 3`, `Received: 8`, `left: 4`), so different assertions get different signatures. Keep the first 5 unique ones; if none match, use the last 5 non-empty lines. (Matched by hand, not with a regexp: a case-insensitive alternation over every output line dominated the pipeline cost.)
4. **File references:** the sorted, de-duplicated source files referenced anywhere in the tail (`path.ext:<LN>`, `path.ext(<LN>)`, Python `File "path", line <LN>`), stack frames included.
5. `Exact = xxhash64(join(maskedKeyLines) + "\x00" + fileRefs)`; `Loc = xxhash64(fileRefs)` (0 if none).
6. `Sim = simhash64(shingles(words of the error-bearing lines, k=4))`. These are all masked key lines, up to 40, or the whole tail when none match. Simhash over the whole tail let runner chatter (passing tests, summaries) drown a changed assertion: a different cargo assertion was 3 bits away.
7. Two signatures are the **same error** if `Exact` is equal, OR (`Loc` is equal AND `hamming(Sim) ≤ 4`). The same message in `auth.ts` and in `db.ts` is two errors; the same message moved within one file is one.
8. `Lines` keeps the unmasked key lines (redacted) for evidence.

On the real and hand-written fixtures in `testdata/outputs/` (go test, tsc, jest real; pytest, cargo hand-written), every same-error pair has `Exact` equal (hamming 0), and every different-error pair is ≥ 14 bits apart.

### 4.5 File content hash

On post events for `edit`/`write`/`patch`, the daemon reads the file from disk (path resolved against `cwd`, size cap 2 MB) and stores `xxhash64(content)` in the file's history. Skipped for binaries and for files outside `cwd` and the project roots. The detection packages never read the disk: the hash comes in with the event (`pipeline.Handle(…, fileHashAfter)`), so detectors stay pure. **Replay** can't read past disk state: it uses the hash of a `Write`'s `content` and no hash for `Edit` (§14).

---

## 5. Session state (`internal/session`)

```go
type SessionState struct {
    Key        string                    // agent + ":" + session_id
    Agent      AgentKind
    StartedAt  time.Time
    LastSeen   time.Time
    Repo       string                    // git root of first cwd
    Calls      *Ring[*ToolCall]          // last 200 completed calls
    Pending    map[string]*event.Event   // pre events by ToolUseID awaiting post (cap 1024)
    Files      map[string]*FileHistory   // path → history
    Families   map[string]*FamilyStats   // family → stats
    Errors     map[uint64]*ErrorStats    // ErrorSig.Exact → stats
    Progress   *ProgressTracker
    Loops      map[string]*LoopState     // LoopKey → escalation state (owned by the policy)
    Usage      UsageTotals               // tokens, cost, from cost accountant (M4)
    Paused     bool
    TurnSeq    uint64                    // seq at last human prompt
    PermMode   string                    // latest permission_mode seen
    NudgesThisTurn int                   // for max_nudges_per_turn
    CallCount  uint64                    // completed calls; cooldowns count calls, not seqs
}

type FileHistory struct {
    Writes     []HashRecord // (seq, content hash) after each write (last 30)
    Edits      []EditRecord // (seq, oldHash, newHash) for each edit replacement (last 30)
    Reads      int          // reads since last write to this file
    LastWrite  uint64       // seq
    TouchedTurn uint64      // TurnSeq of the last write (for "file not touched this turn")
}

type FamilyStats struct {
    Runs, Fails      int
    ConsecFails      int
    LastErr          *ErrorSignature
    LastSuccessSeq   uint64
    LastFailingTests int               // -1 if unknown
    Sigs             []*ErrorSignature // distinct signatures seen (last 50), for "never-seen"
}

type LoopState struct {           // policy state per LoopKey
    Key, Detector string
    Stage         Stage           // observing | nudged | denying | halted
    ActionFP      uint64          // D1: the repeated action (pre-event matching)
    Family        string          // D2: the failing family
    Path          string          // D3: the oscillating file
    Anchor        *ErrorSignature // D2: the loop's error identity (§6 D2)
    StartedAt     time.Time
    FirstSeq, StageSeq, LastSignalSeq uint64
    LastSignalCall, LastNudgeCall    uint64 // CallCount values
    Nudges, Occurrences, Denies      int
    LastSeverity  float64
}
```

Sequence numbers (`Seq`) count every event (pre, post, prompt). Anything measured "in calls" (cooldowns, quiet periods) uses `CallCount`, which only counts completed calls.

### 5.1 Progress tracker

Each completed call gets a **progress score** in `[0,1]`, which detectors use to reset or suppress:

| Signal | Score |
|---|---|
| A family that was failing now succeeds | 1.0 |
| Error signature for a family changed to a *new, never-seen* signature | 0.6 |
| Number of failing tests in output decreased (parse `N failed` / `N passed` patterns) | 0.6 |
| Write/edit to a file not touched before in this turn | 0.4 |
| Successful command of a family never run before | 0.3 |
| Task/todo marked completed (`TaskCompleted` / TodoWrite status change) | 0.8 |
| Anything else | 0.0 |

- When a call matches several rows, it gets the **highest** score (not the sum).
- The "family" rows (failing → success, new never-seen signature, fewer failing tests) apply only to non-polling families. "New family succeeded" applies only to bash.
- Each score also records its **family part**: the share that came from the first three rows, which describe the call's own family. **Only family progress resets or suppresses a repeated-failure (D2) loop.** Touching new files or succeeding with another family is exactly the "changing things but nothing changes" D2 exists for. Those rows still count toward the general score that the stall detector (D5) and other loops use.

`Progress.Recent(n)` = sum of the scores over the last n calls. `Progress.Since(seq)` = sum over calls after `seq` (exclusive). `Progress.FamilySince(family, seq)` = sum of `family`'s family progress after `seq`.

---

## 6. Detectors (`internal/detect`)

```go
type Detector interface {
    Name() string
    Observe(s *session.SessionState, c *event.ToolCall) []event.Signal  // called after each completed call
}
```

All detectors look only at calls since the later of: the last human prompt (`TurnSeq`), or a window of the last 50 calls. **Default thresholds are configurable.**

### D1. Exact repeat
- **Fires when:** the same `ActionFP` and the same `ResultFP` occur ≥ **3** times in the window **since the last successful file write**, and the call isn't a polling command.
  - A write in between makes a repeat a re-check, not a no-op ("build after every edit" is normal). Repeated *failures* across edits are D2's job.
  - Reads and searches (`read`, `grep`, `glob`) and writes themselves are excluded. D4 owns reads and never escalates past a nudge, while D1 can deny and halt.
- **LoopKey:** `er:<ActionFP>`
- **Severity:** `min(1, 0.5 + 0.15*(count-3))`

### D2. Repeated failure (headline detector)
- **Fires when:** the same family has ≥ **4** failures whose error signatures are the *same error* as the current one (§4.4), with **no success of that family** in between (failures with other errors neither count nor stop the walk back), and the family's own progress since the first counted failure, `Progress.FamilySince(family, first) < 1.0` (§5.1), stays below the threshold. Polling commands are exempt.
- Edits between attempts don't reset it, and neither do edits to new files. That's the point: "changing things but nothing changes".
- **LoopKey:** `rf:<family>:<ErrorSig.Exact of first occurrence>`. **Stable identity:** once the policy tracks a D2 loop, its LoopKey and **anchor signature** live in the loop state. Later failures of the family whose error matches the anchor (`SameError(anchor, sig)`) keep that key, even after the first failure leaves the 50-call window. Otherwise a long loop would get a new key, and its escalation would restart. The state lives until the next human prompt.
- **Severity:** `min(1, 0.55 + 0.1*(count-4))`, +0.1 if a file was edited in at least two of the gaps between attempts.
- **Evidence:** the family, count, key error lines, the files edited between attempts, elapsed time, and the anchor signature.

### D3. Edit oscillation
- **Fires when**, for one file, either:
  - (a) **Inverse edits:** an edit with `(old=A, new=B)` followed later by `(old=B, new=A)`, at least **2** times; or
  - (b) **State revisit:** a content hash after a write equals one seen ≥2 writes earlier (A→B→A), at least **2** revisits within 20 writes. Writing the same content twice in a row is not a revisit.
- It runs only on successful writes, and fires only when the **current** write is itself an inverse edit or a revisit, so an unrelated later edit of the file doesn't re-fire it. When both variants match, the more severe signal is kept.
- **LoopKey:** `os:<path>`
- **Severity:** 0.7 for (a), 0.6 for (b), +0.1 per extra revisit.

### D4. Read thrash
- **Fires when:** the same file is read ≥ **5** times with no write to it in between, AND `Progress.Recent(last 15) < 0.5`. Also covers repeated identical `grep`/`glob` searches (same ActionFP ≥ 4).
- **LoopKey:** `rt:<path or ActionFP>`
- **Severity:** 0.4 to 0.6. **Nudge only; this detector never escalates past nudge.**

### D5. No-progress stall
- **Fires when:** ≥ **25** consecutive calls have `Progress.Recent(25) < 0.5` and at least **8** of them are failures or re-reads.
- **LoopKey:** `st:<TurnSeq>`
- **Severity:** 0.5. It's lower-confidence, so it nudges once and notifies the human if `notify_on_stall` is set; no deny.
- **Exploration is progress** (TODO(M3), from the M1-C review): reads of files not seen before in the session, fetches of URLs not fetched before, and searches with a new pattern must count as progress for the stall detector. Otherwise research sessions look stalled: on real data, research sessions produced 45 false "no progress" stretches in the replay near-miss report. Repeats of the same read, fetch, or search still don't count.

### D6. Budget
- Inputs: `Usage` (from the cost accountant), wall time, and tool-call count.
- **Soft limit:** signal severity 0.5 → notify only.
- **Hard limit:** severity 1.0 → halt.
- Limits per session, per day (global), and per repo: `cost_usd`, `tokens`, `wall_minutes`, `tool_calls`.
- **LoopKey:** `bg:<scope>:<limit>`

### 6.1 Deduplication across detectors

- Signals with the same LoopKey collapse into the most severe one.
- When D1 and D2 fire on overlapping calls (their evidence seqs intersect), **only D2 is kept**, with the higher of the two severities. D2 has the better message. Keeping one detector's LoopKey also stops the loop's escalation state from flipping between `er:` and `rf:` keys as the two severities grow at different rates. "Higher severity wins" would split the escalation.
- Signals are passed to the policy engine sorted by severity (descending; ties by LoopKey).

---

## 7. Policy engine (`internal/policy`)

### 7.1 Escalation state machine (per `LoopKey`)

```mermaid
stateDiagram-v2
    [*] --> Observing
    Observing --> Nudged: signal sev ≥ nudge_threshold (0.55)\n→ NUDGE
    Nudged --> Observing: progress ≥ 1.0 OR no signal for 15 calls (reset)
    Nudged --> Denying: signal persists ≥ 3 more occurrences after nudge\n AND sev ≥ 0.7 AND detector allows deny
    Denying --> Observing: progress ≥ 1.0 (reset)
    Denying --> Halted: ≥ 2 more matching attempts after first deny\n OR time in loop ≥ halt_after_minutes (20)
    Halted --> [*]: HALT + notify human
```

- **Observe-only mode** (`mode: observe`, the default for the first 3 days after install, configurable): the state machine runs and records "would have nudged/denied/halted", but every decision rendered is `none`. This builds trust and produces data. **M1 implements observe mode only:** `Decision.Action` is always `none`, and `Decision.WouldHave` and `Decision.Reason` record the rest. Enforcement lands in M3.
- **Resets:** "progress" is measured since the loop entered its current stage. For **repeated-failure loops it is the family's progress only** (§5.1); other loops use the general score. A reset loop keeps its nudge history, so the cooldown still applies.
- **Deny** applies only to the **looping action**: a `PreToolUse` that matches the loop. A D1 loop matches the same `ActionFP`, a D2 loop the same family, and a D3 loop an edit or write to the same file. Other actions pass.
- **Halt** ends the turn (`continue:false`). The session itself stays usable when the human returns; `Loops` state is reset on the next `UserPromptSubmit`.
- **Cooldown:** max 1 nudge per LoopKey per 10 **completed calls**, and max 3 nudges per turn overall. Suppressed nudges are not recorded.
- **Permission mode awareness:** in `default` (manual approval) mode, the deny and halt occurrence thresholds are raised by 1 (`manual_mode_extra_occurrences`), since a human is presumably watching. In `auto`/`bypassPermissions`/`dontAsk`, the normal thresholds apply.
- Detector capabilities: D1/D2/D3 can deny + halt; D4 nudge only; D5 nudge + notify; D6 notify + halt.

### 7.2 Decision timing

- Signals are produced on **post** events. If a nudge is due, it goes in the post response (`additionalContext`) and is recorded at that post.
- Reaching the deny conditions on a post signal **arms** deny (state `Denying`). No decision is recorded then.
- Deny decisions are evaluated on **pre** events against loops in `Denying`: each matching pre is a (would-have) deny. The first is "the first deny". The `halt_after_deny_occurrences`-th matching attempt after it (default: the 3rd attempt in total) is a halt instead. A matching pre after `halt_after_minutes` in the loop is also a halt. In observe mode the action still runs, so its post produces signals as usual; only the pre attempts count toward halt.
- Halt decisions are rendered on the next event of any phase (pre, post, or batch_end), whichever comes first. Observe mode records the halt once, at the pre where it was decided.

### 7.3 Message templates (factual tone)

Nudge, repeated failure:
```
LoopGuard observation: `{family}` has failed {count} times in the last {minutes} min with the same error:
  {error_line_1}
  {error_line_2}
Between attempts, these files were edited: {files}. The error did not change after any of these edits.
Repeating similar edits is unlikely to change the result. Options that often help: re-reading the full error and the code it points to, checking assumptions (versions, config, env), trying a different approach, or asking the user.
```

Deny:
```
LoopGuard blocked this command: it is the same `{family}` run that has failed {count} times with the same error, with no change in outcome. A different approach, or a question to the user, is needed before running it again.
```

Halt (stopReason, shown to the human):
```
LoopGuard halted the agent: stuck on `{family}` ({count} identical failures, {minutes} min, ~{cost}). See `loopguard report {short_session}`.
```

Oscillation:
```
LoopGuard observation: {path} has been changed back to a previous version {n} times (the same edit was made and then reverted). The two versions alternate between: {short_diff_summary}.
```

---

## 8. Cost accounting (`internal/cost`)

### 8.1 Transcript tailer
- When a `SessionStart` or first event arrives with `transcript_path`, register a tailer: poll every 2 s (fsnotify where available) and read from the stored byte offset.
- Parse JSONL lines; for assistant messages, extract `message.id`, `message.model`, and `message.usage.{input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens}`.
- **Deduplicate by `message.id`** (a streamed message can appear on multiple lines; keep the max of each counter per id).
- Persist `(session, message_id, model, counters)`. Unknown lines are ignored (forward compatible).
- Codex: tail its session rollout JSONL (location and token-count event shape to be verified per version) through a `codex` tailer implementation behind the same interface.

### 8.2 Pricing
`~/.loopguard/pricing.yaml` (shipped default, user-editable, refreshable with `loopguard pricing update` from the repo):

```yaml
currency: USD
models:
  - match: "claude-opus-*"
    input_per_mtok: 0      # fill from the provider's current pricing page
    output_per_mtok: 0
    cache_write_per_mtok: 0
    cache_read_per_mtok: 0
  - match: "claude-sonnet-*"
    # ...
fx:
  INR: 0   # optional display conversion, user-set
```

Cost = Σ tokens × rate. For subscription users, the dashboard labels this **"API-equivalent cost"** and also shows token counts and time.

### 8.3 Budgets
Checked on every usage update and on every pre event, as D6 input. Because transcript usage lags by a few seconds, the hard limit is enforced **at the next tool call after crossing**. The docs must say so honestly.

---

## 9. Daemon (`internal/daemon`)

### 9.1 Process and transport
- Listens on `127.0.0.1:<port>`: default 47821, or the first free port, written to `~/.loopguard/daemon.json` along with the PID. Unix socket on macOS/Linux is optional later.
- Auth: header `Authorization: Bearer <token>` with the token from `~/.loopguard/token` (random 32 bytes, mode 0600). Dashboard routes accept a cookie set via a one-time link printed by `loopguard dashboard`.
- Single instance enforced by a PID file plus a port bind check.

### 9.2 Session manager and actors
```go
type Manager struct {
    mu       sync.Mutex
    actors   map[string]*Actor   // session key → actor
}
type Actor struct {
    in    chan job              // buffered (256)
    state *session.SessionState
}
type job struct { ev *event.Event; reply chan event.Decision }
```
- The ingest handler parses via the adapter, routes to the actor by session key, and waits on `reply` with the remaining time budget (the shim timeout minus 50 ms).
- The actor loop: assign `Seq` → update state → join pre/post → build `ToolCall` → run detectors → policy → write the decision to `reply` → enqueue async persistence (batched writer, flush every 200 ms or 100 rows).
- Actors are evicted after 2 h idle (state snapshot persisted) and rehydrated from SQLite on the next event.
- Each actor runs with `recover()`: on panic, log it, reply `none`, and rebuild state from the DB.

### 9.3 REST API (v1)

| Method & path | Purpose |
|---|---|
| `POST /v1/events?agent=claude-code` | Ingest one hook payload; returns `{"stdout": "...", "exit": 0}`, already rendered by the adapter |
| `GET /v1/sessions?since=&repo=` | List sessions with totals |
| `GET /v1/sessions/{id}` | Session detail: calls, signals, interventions, usage |
| `POST /v1/sessions/{id}/pause` / `resume` | Pause enforcement for a session |
| `GET /v1/stats?range=7d` | Loops caught, time and cost saved estimates |
| `GET /v1/decisions?limit=10` | Newest decisions of all sessions (in observe mode: what it would have done), with detector, reason, and time (`loopguard status`) |
| `GET /healthz`, `GET /metrics` | Health (version, pid, home id, store, capture on/off, start time) and Prometheus metrics |
| `POST /v1/shutdown` | Stop the daemon (`loopguard daemon stop`) |

The adapter renders on the daemon side so the shim stays dumb. Only the shim has to be backward-compatible across versions.

---

## 10. Hook shim (`internal/shim`)

```
loopguard-hook --agent claude-code      # what the installed hooks run
loopguard hook --agent claude-code      # the same code path, kept for compatibility
```
`loopguard-hook` (`cmd/loopguard-hook`) is a separate, minimal binary because it starts once per hook event: no cobra, no `fmt`, `encoding/json`, `net/http`, or crypto (a test enforces the import list). The request is a hand-written HTTP/1.0 POST over `net.Dial` (HTTP/1.0: the reply is never chunked and the connection closes after it); the reply `{"stdout","exit"}` is read by a small decoder fuzz-tested against `encoding/json`; `daemon.json`'s port is read without a JSON decoder (`home/homedir`). It starts the daemon from the `loopguard` executable next to it. `loopguard-hook --version` prints its version (for `doctor`).

1. If `LOOPGUARD_DISABLE=1` → exit 0.
2. Read all of stdin (cap 4 MB).
3. Read `~/.loopguard/daemon.json`; if it's missing or the connection is refused → spawn `loopguard daemon --detach` (rate-limited via a lock file, once per 10 s) and **exit 0 immediately**. This event is lost, which is acceptable.
4. `POST /v1/events` with a timeout of `min(800ms, hook timeout - 200ms)`.
5. On success: write `stdout` exactly and exit with `exit`. On any error or timeout: exit 0 with no output.
6. Never write to stderr except under `LOOPGUARD_DEBUG=1`.

**Performance target** (end to end: process start to exit, against a running daemon): **p99 < 50 ms on macOS and Linux; on Windows, within ~15 ms of the measured process-start floor** (an empty Go program on the same machine), because Windows process creation alone costs 15–45 ms depending on load and antivirus. Measured by `TestHookLatency` (`$env:LOOPGUARD_BENCH_CALLS=500; go test ./cmd/loopguard-hook -run HookLatency -v`); numbers in TDD §1.2.

---

## 11. Storage schema (`internal/store`, SQLite, WAL)

```sql
CREATE TABLE sessions (
  key            TEXT PRIMARY KEY,         -- agent:session_id
  agent          TEXT NOT NULL,
  session_id     TEXT NOT NULL,
  repo           TEXT,
  cwd            TEXT,
  transcript     TEXT,
  model          TEXT,
  started_at     INTEGER NOT NULL,         -- unix ms
  last_seen_at   INTEGER NOT NULL,
  ended_at       INTEGER,
  tool_calls     INTEGER DEFAULT 0,
  failures       INTEGER DEFAULT 0,        -- failed tool calls (migration 2)
  input_tokens   INTEGER DEFAULT 0,
  output_tokens  INTEGER DEFAULT 0,
  cache_read     INTEGER DEFAULT 0,
  cache_write    INTEGER DEFAULT 0,
  cost_usd       REAL DEFAULT 0,
  loops_detected INTEGER DEFAULT 0,
  wasted_ms      INTEGER DEFAULT 0
);
CREATE INDEX idx_sessions_last_seen ON sessions(last_seen_at);

CREATE TABLE calls (
  session_key  TEXT NOT NULL,
  seq          INTEGER NOT NULL,
  ts           INTEGER NOT NULL,
  tool         TEXT NOT NULL,
  family       TEXT,
  file_path    TEXT,
  action_fp    INTEGER NOT NULL,
  result_fp    INTEGER,
  is_error     INTEGER NOT NULL,
  err_exact    INTEGER,
  err_sim      INTEGER,
  err_lines    TEXT,               -- redacted, ≤ 5 lines
  input_digest TEXT,               -- redacted, truncated (≤ 300 chars)
  progress     REAL,
  duration_ms  INTEGER,
  PRIMARY KEY (session_key, seq)
);

CREATE TABLE signals (
  id          INTEGER PRIMARY KEY,
  session_key TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  detector    TEXT NOT NULL,
  loop_key    TEXT NOT NULL,
  severity    REAL NOT NULL,
  count       INTEGER,
  evidence    TEXT                -- JSON
);

CREATE TABLE interventions (
  id          INTEGER PRIMARY KEY,
  session_key TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  ts          INTEGER,            -- unix ms (migration 2)
  loop_key    TEXT NOT NULL,
  detector    TEXT,               -- (migration 2)
  action      TEXT NOT NULL,      -- nudge|deny|halt
  mode        TEXT NOT NULL,      -- enforce|observe (observe = would-have)
  reason      TEXT,               -- one line, redacted (migration 2)
  message     TEXT,
  outcome     TEXT                -- filled later: recovered|continued_loop|halted|unknown
);

CREATE TABLE usage (
  session_key TEXT NOT NULL,
  message_id  TEXT NOT NULL,
  ts          INTEGER,
  model       TEXT,
  input_tokens INTEGER, output_tokens INTEGER, cache_read INTEGER, cache_write INTEGER,
  cost_usd    REAL,
  PRIMARY KEY (session_key, message_id)
);

CREATE TABLE transcript_offsets (path TEXT PRIMARY KEY, offset INTEGER NOT NULL, inode INTEGER);
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER);
```

- **Intervention outcome** is computed 10 calls after each intervention: `recovered` if progress ≥ 1.0 or the loop signal stopped; `continued_loop` otherwise. This is the core product metric ("nudge success rate").
- **Wasted time** for a loop = time from the first call in the loop (`Since`) to recovery or halt.
- **Retention:** a nightly job deletes `calls`/`signals` older than `retention_days` (30); `sessions` and `interventions` aggregates are kept.

---

## 12. Configuration (`internal/config`)

`~/.loopguard/config.yaml` (global) merged with `<repo>/.loopguard.yaml` (per repo; the repo file can only *tighten* budgets and *disable* detectors, never loosen global hard limits):

```yaml
mode: enforce            # observe | enforce
observe_days_after_install: 3
detectors:
  window: 50                     # calls a detector looks back (never past the last human prompt)
  simhash_max_distance: 4
  exact_repeat:     { enabled: true, min_count: 3 }
  repeated_failure: { enabled: true, min_count: 4, max_progress: 1.0 }
  oscillation:      { enabled: true, min_revisits: 2, revisit_window: 20 }
  read_thrash:      { enabled: true, min_reads: 5 }
  stall:            { enabled: true, window: 25 }
policy:
  nudge_threshold: 0.55
  deny_after_nudges_occurrences: 3
  deny_min_severity: 0.7
  halt_after_deny_occurrences: 2
  halt_after_minutes: 20
  max_nudges_per_turn: 3
  nudge_cooldown_calls: 10
  reset_quiet_calls: 15
  reset_progress: 1.0
  manual_mode_extra_occurrences: 1
polling_families: ["sleep", "watch", "gh run watch", "gh pr checks", "git status", "docker ps", "ls"]
polling_patterns: ['^kubectl\b.*\s(-w|--watch)\b', '^curl\b.*/health']
read_only_commands: ["grep", "rg", "ag", "cat", "head", "tail", "less", "more", "find", "fd", "ls", "tree", "wc",
                     "git log", "git diff", "git show", "git status", "git blame", "git grep",
                     "select-string", "get-content", "findstr", "type",
                     "docker logs", "kubectl logs", "aws logs", "journalctl", "curl", "wget", "echo", "printf"]
error_marker_families: ["pytest", "jest", "vitest", "go test", "cargo test", "cargo build", "tsc",
                        "npm test", "yarn test", "pnpm test", "bun test", "npm run test", "yarn run test",
                        "pnpm run test", "bun run test", "npx jest", "npx vitest", "npx tsc",
                        "mvn", "mvnw", "gradle", "gradlew", "make", "dotnet test"]
budgets:
  session: { soft_usd: 5, hard_usd: 15, wall_minutes: 180 }
  daily:   { soft_usd: 20, hard_usd: 50 }
notify:
  desktop: true
  terminal: true
  ntfy:    { topic: "" , server: "https://ntfy.sh" }
  webhook: { url: "", format: "slack" }   # slack | discord | generic
  on: [halt, budget_soft, budget_hard, stall]
privacy:
  retention_days: 30
  store_outputs: tail   # none | tail
```

---

## 13. Redaction (`internal/redact`)

Applied before anything is persisted or put in a notification. Types:

- `private_key`: blocks, including truncated ones.
- `anthropic_key` (`sk-ant-…`) and `openai_key` (`sk-…`, `sk-proj-…`).
- `aws_access_key` (`AKIA`/`ASIA`).
- `github_token` (`ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`, `github_pat_`).
- `slack_token` (`xox?-`) and `google_api_key` (`AIza…`).
- `jwt`.
- `bearer`: the token after `Bearer`.
- `url_credentials`: the password in `scheme://user:pass@host`.
- `env_secret`: `.env`-style `KEY=value` lines where KEY contains SECRET|TOKEN|KEY|PASS.
- `secret`: `password=`/`token:`/`"client_secret": "…"`-style pairs.

Replacement: `<REDACTED:type>`. The two key=value rules keep the key and skip values that refer to a secret rather than contain one: `process.env.API_KEY`, `$TOKEN`, `${X}`, `{{ … }}`, placeholders like `<…>` or `****`.

Redaction runs on the **raw** command and output, **before** normalization (§4.1), so fingerprints, signatures, and evidence lines are all computed on redacted text, and secrets never reach the DB, even hashed. Each rule runs its regexp only behind a cheap prefilter (substrings, or a hand check such as "an `=` after an upper-case identifier naming a secret"). Redaction must not cost more than the rest of the pipeline.

---

## 14. Replay (`internal/replay`)

```
loopguard replay [--since 30d] [--path ~/.claude/projects] [--repo DIR] [--session ID] [--json] [--top N]
                 [--label] [--labels FILE] [--calls] [--export-anonymized ID --out FILE] [--workers N]
loopguard label missed <session[/agent]> <from-call> <to-call> [note]
loopguard eval --path DIR [--labels FILE]          # make eval: needs LOOPGUARD_EVAL_DIR
```

The transcript format is described in [transcript-format.md](./transcript-format.md), from what was observed.

**Reading**
- Each JSONL line is read with a `bufio.Reader` and no line-length limit (results can be MBs). Malformed lines are counted and skipped, and so are line types replay doesn't use.
- Events: `tool_use` → pre; the matching `tool_result` (joined on the tool-use id within the file) → post, or post_failure when `is_error`. The post event carries the tool input, as `PostToolUse` does; the pipeline builds the ToolCall from it. The output is the text the model saw (`tool_result.content`).
- **Turn boundaries** (pipeline `prompt` events) are real human prompts: `origin.kind == "human"`, or plain text without `isMeta`; slash commands; interrupts and rejected tool calls. Task notifications, injected context (`isMeta`, `<local-command-stdout>`, `<system-reminder>`, …), and compaction summaries are not turn boundaries.
- **Time** comes only from tool and prompt lines, clamped per stream to never go backwards. Metadata lines carry their own clocks.
- **Streams:** each main session file and each `subagents/agent-*.jsonl` is its own stream, with its own session state (TDD §3.4).
- **File hashes:** replay can't read past disk state. `Write` uses the hash of its `content` as `FileHashAfter`; `Edit` has none, so D3 state revisit only sees writes. Inverse edits (D3a) still work from the `old_string`/`new_string` pairs.
- **Filters:** `--since` keeps files modified within the window (whole sessions, not single events). `--repo` matches Claude Code's encoded project directory (`C:\a\b` → `C--a-b`) for the directory and its subdirectories.
- Streams are replayed in parallel (one goroutine per stream, `--workers`), and a session is streamed, never loaded whole.

**Report**
- A *loop* is one LoopKey within one human turn of one stream, whose highest severity reached the nudge threshold. Sub-threshold signals are only counted.
- **Calls** are numbered from 1 per stream in completion order; the report, labels, and `--calls` all use these numbers.
- **Wasted time** of a loop runs from its first call to its last (min/max of the calls in its signals' evidence). **Wasted tool calls** are `last - first + 1`. Totals are the union over a stream's loops, so overlaps count once.
- Each top loop shows the repo, detector, family or file, count, duration, 2 redacted error lines, and what LoopGuard would have done: nudge, deny, or halt, at which call. `--top 0` lists no loops; `--top -1` lists all.
- `--json` emits the same data with a versioned schema ([replay-json.md](./replay-json.md)). Cost is M4.

**Tuning visibility** (detection-free; changes no thresholds)
- Every report shows the **top 10 command families by failures** (runs, failures, soft failures) and the **longest same-family failure streak per stream**. A streak is any errors, with no success of the family in between.
- `--near-misses[=N]` (default 10) lists the stretches that came closest to firing without firing, closest first. A side tracker follows the detectors' own conditions per turn:
  - D2: same-error clusters per family, cut by a success of the family or the end of the turn.
  - D1: repeats of one action since the last write.
  - D3: writes per file, with revisits and inverse edits.
  - Stall: the longest no-progress stretch.

  Each entry shows count vs threshold, a closeness in 0..1, and why it didn't fire: below the threshold, family progress, outside the window, polling, reads left to D4, or "stall arrives in M3". It also shows redacted evidence. Stretches that overlap a loop the same detector reported are dropped.
- `--soft-failures` lists the calls counted as soft failures (bash, exit 0, failure marker): family, command, the markers found, and the redacted line with the first one. It also counts calls per marker, so noisy markers stand out. Markers count only at the start of a line, and `Error:` only for test and build runners (§4.4).

**Labels and eval**
- `--label` asks `[y] real loop / [n] false positive / [s] skip / [q] quit` for each detected loop without a label, and appends to `~/.loopguard/labels.jsonl` (`$LOOPGUARD_HOME` overrides the directory).
- A label records: `kind` (detected|missed), `session_id`, `agent_id`, `loop_key`, `detector`, `from_call`, `to_call`, `verdict` (real|false_positive|missed), `note`, and `labeled_at`. A detected loop counts as labeled when a label has the same stream and loop key and an overlapping call range, so re-runs skip it even if the range moved a little.
- `loopguard label missed` records a loop the detectors missed. `replay --session ID --calls` lists a session's calls with their numbers.
- `eval` replays the whole corpus and prints, per detector:
  - **precision** = TP / (TP + FP) over the labeled detections;
  - **recall** = the real loops it caught / all real loops;
  - **mean detection delay** = calls from a real loop's first call to the detector's first signal on it.
- *Real loops* are the real-labeled detections, with overlapping ones in a stream merged, plus missed loops. A missed loop that a detection now overlaps counts as caught.

**Anonymized export** (`--export-anonymized`) writes transcript-shaped JSONL that replay reads back with the same results. It keeps:
- structure: tool calls, `is_error`, human prompts, permission modes;
- tool names, and normalized, redacted commands, with long quoted strings and heredoc/script bodies as hash tokens;
- edit strings, written content, search patterns, and other tool inputs as hash tokens, so equality is preserved;
- for failures, the redacted error key lines; every output as a hash token;
- timestamps shifted to start at the Unix epoch;
- file paths as `src/file_N.<ext>`; the cwd as `/repo`; other paths below a home directory as `/home/user/p_<hash>`; URLs as `<url:hash>`; the username and repo name learned from paths as `user`/`repo`; emails as `<email>`.

Prompt text, assistant text and thinking, and full outputs are dropped.

---

## 15. Notifications (`internal/notify`)

```go
type Notifier interface { Send(ctx context.Context, n Notification) error }
type Notification struct { Kind, Title, Body, SessionKey, Repo string; Severity float64 }
```
- **desktop:** beeep (macOS Notification Center, libnotify, Windows toast).
- **terminal:** the adapter attaches `terminalSequence` (OSC 777/9) to the next hook response. This works only in interactive sessions.
- **ntfy:** `POST {server}/{topic}` with Title/Priority headers. Free phone push with no account required.
- **webhook:** Slack `{"text":...}` / Discord `{"content":...}` / generic JSON.
- Sends happen async with retry (3×, exponential backoff); failures are logged and never block decisions.

---

## 16. Testing strategy

| Layer | Tests |
|---|---|
| normalize | Table tests per placeholder rule; same/different-error pairs on `testdata/outputs/` (go test, tsc, jest real; pytest, cargo hand-written) |
| redact | One test per secret type; normal text (code references, "PASS", "KeyError") left alone |
| adapters | Golden files: every payload under `testdata/hooks/<agent>/**` (hand-written ones in `synthetic/`, real exports in `<version>/`) → expected Event in the `.golden` next to it (`-update` rewrites them); an installed event must map to a phase; Decision → expected stdout/exit. Install/uninstall: merge, idempotence, in-place update, backups, refusal of invalid JSON, formatting kept |
| detectors | Scripted sessions (YAML DSL in `testdata/sessions/`, runner in `internal/pipeline`) → expected signals and would-have decisions; a 60-call loop keeps one LoopKey; property tests: polling commands never fire |
| policy | State machine transition tests, including cooldowns, family-only resets, and the observe-mode invariant (never an action other than `none`) |
| replay | Synthetic transcripts (`internal/replay/replaytest`) covering every observed line shape → events, loops, report, labels, eval, export round trip; golden test on anonymized real sessions (`testdata/transcripts/`) |
| shim | Daemon down, slow (timeout), and garbage-response cases all → exit 0, empty stdout |
| e2e | Spin up the daemon, run real `claude -p` in a sandbox repo with a deliberately unfixable failing test (e.g. a test asserting a wrong constant from a read-only file), and assert a nudge appears in the transcript and the halt happens |
| eval | `make eval` / `loopguard eval`: replay the labeled private corpus → precision, recall, detection delay per detector. Skipped without `LOOPGUARD_EVAL_DIR` (CI has no private data) |
| perf | Benchmark: 10k calls through the full pipeline in < 1 s (`BenchmarkPipeline10kCalls`); replay throughput; hook round trip (`TestHookLatency`, opt-in): p99 < 50 ms on macOS/Linux, within ~15 ms of the process-start floor on Windows (§10) |

`make test` runs `go test -race` on Linux and macOS. On Windows it runs without `-race` (no C toolchain is assumed); `RACE=1` forces it.

---

## 17. Design decisions from M1-A

Decisions made while building the detection core that the first draft of this document didn't specify, and the fixes from the M1-A review. The sections above are updated to match; this is the list in one place.

**Normalization and signatures**
1. **Location masking** (§4.4): line/column numbers next to a file reference, `line N`, and code-frame gutters are masked before hashing, so an edit that shifts the failing line doesn't change the error. Evidence lines keep the real numbers.
2. **File names stay in the signature** (review fix): the files an error references, stack frames included, are part of `Exact` and form `Loc`. The simhash path of `SameError` requires equal `Loc`. The same message in `auth.ts` and `db.ts` is two errors.
3. **Extra key-line keywords** (§4.4): `got`, `want`, `left`, `right` (whole words), `received`, `actual`, and `●`, so assertion payloads count. They are matched by hand rather than with a regexp, for speed.
4. **Simhash over the error-bearing lines** (up to 40), not the whole tail (§4.4).
5. **Outputs keep their tail** when over 64 KB (§4.1). The signature uses the last 8 KB of the normalized output.
6. **Soft failures are bash-only** (§4.4), and **never for read/search commands** (review fix; `read_only_commands`, §4.3).
7. **Redaction first** (§13): on the raw command and output, before normalization and hashing.
8. Single-pass normalizer instead of one regexp per placeholder (§4.1).

**Commands**
9. **Family rules** (§4.3): the subcommand counts only as the first argument and only if word-like; `python -m`, `uv|poetry|pipenv|hatch|pdm run` take the inner family; `npm|yarn|pnpm|bun run <script>` keep the script.
10. **Wrapper stripping** (§4.3): `time`, `env X=Y`, `command`, `exec`, `nohup`, `sudo -u x`, `nice N`, `timeout N`, `xargs`; `bash -c '…'` is parsed recursively. Commands are lower-cased, with their directory and Windows extension removed.
11. **Polling is judged on the last (primary) command** (§4.3), and arguments-dependent pollers are regexps (`polling_patterns`).

**Types**
12. **`Edits []EditPair`** replaces `EditPair *EditPair` (MultiEdit has several pairs).
13. **New fields** (§2): `ToolCall.Command`, `.Polling`, `.FailingTests`, `.TodosDone`, `.Progress`, `.FamilyProgress`; `ErrorSignature.Loc`; `Evidence.ActionFP`, `.Path`, `.Seqs`, `.Elapsed`, `.Anchor`; `Decision.WouldHave`, `.Reason`, `.Detector`, `.Severity`, `.Seq`; session `CallCount`, `PermMode`, `NudgesThisTurn`; `LoopState` (§5).

**Detectors**
14. **D1 scope** (§6): only repeats since the last successful write; reads, searches, and writes excluded.
15. **D2 progress is family-only** (review fix, §5.1, §6): only the family recovering, its error changing to a never-seen one, or fewer failing tests suppress or reset D2. New files and other families succeeding don't.
16. **Stable D2 loop identity** (review fix, §6): the LoopKey and anchor signature live in the loop state while failures match the anchor, independent of the window.
17. **D2 always wins the D1/D2 dedup** on overlapping calls, with the max severity (§6.1).
18. **D3 trigger rule** (§6): fires only when the current write is itself an inverse edit or a revisit; the more severe variant wins.

**Policy**
19. **Timing** (§7.2): nudge recorded at the post; deny armed at a post and recorded at each matching pre; halt at the matching attempt after the configured number of denies (or after `halt_after_minutes`).
20. **Counting completed calls** (§5, §7.1): cooldowns and quiet periods use `CallCount`, not seqs.
21. **Pre-event matching** (§7.1): D1 by ActionFP, D2 by family, D3 by edits or writes to the file.
22. **Permission-mode adjustment** (§7.1): `default` mode adds `manual_mode_extra_occurrences` (1) to the deny and halt thresholds.

**Structure**
23. **`internal/pipeline`** (§1): one `Handle(session, event, fileHashAfter)` path for replay and the daemon, so offline and live detection behave the same. Detectors are pure (no I/O, no clock); the file content hash comes in with the event.
24. **All thresholds in `config.Thresholds`** (§12), including the ones the first draft only gave in prose.

## 18. Design decisions from M2-A

Decisions made while building the store, the Claude Code adapter, the daemon, and the shim that the sections above didn't specify.

**Soft failures (M1-C review)**
1. **Markers count only at a line start** (after optional whitespace), and **`Error:` only for test/build runner families** (`error_marker_families`, §4.4, §12). On the private corpus this took soft failures from 48 to 22 calls (`Error:` 34 → 0).

**Store**
2. **Transactions are `BEGIN IMMEDIATE`** (`_txlock=immediate`), so two processes opening a new database don't fail on a read-to-write lock upgrade while migrating.
3. **Fingerprints are stored bit-for-bit** as signed 64-bit integers (`int64(uint64)`); high-bit values read back unchanged.
4. **Duplicate calls are skipped** (`ON CONFLICT DO NOTHING`), and a failing row is counted and skipped without failing its batch. The writer drops rows when its queue (4,096) is full, never blocking; drops show in `/metrics`.
5. **Session rows carry absolute counters**, upserted on every event; `COALESCE` keeps fields an event doesn't carry (model, cwd).
6. Retention (§11) is not implemented yet.

**Adapter**
7. **Lenient parsing**: a field of the wrong type is treated as missing, not as a parse error. Only a non-object payload, or one without `hook_event_name` or `session_id`, fails. Unknown events parse with an empty phase and are ignored (counted as `event="unknown"`).
8. **`PostToolUseFailure` output** is the `error` field (falling back to `tool_response`). **`SessionStart.model`** may be a string or an object (`id`, else `display_name`). New `Event` fields: `Model`, `Source`.
9. **Render ignores actions that don't fit the phase** (deny on a post, nudge on a pre, halt on stop/prompt → no output), so a policy bug can't produce output Claude Code would misread. `additionalContext` and reasons are clipped at 9,000 bytes as a backstop.
10. **Golden payloads** under `testdata/hooks/claude-code/synthetic/` are hand-written from the hooks reference until real ones are captured (`LOOPGUARD_CAPTURE=1`).

**Daemon**
11. **Session key**: `agent:session_id`, or `agent:session_id/agent_id` for a subagent, like replay's streams: interleaved subagent calls would confuse the detectors.
12. **Single instance** is decided by `/healthz`: a daemon counts as running only if it answers with the same `home_id` (a hash of the home path). A pid in `daemon.json` alone isn't trusted (pids get reused); a port taken by something else, or by a daemon for another home, moves on to the next port.
13. **Time budget**: the shim sends its timeout in `X-LoopGuard-Timeout-Ms`; the daemon answers within it minus 50 ms. A late event is still processed, in order; only its decision is dropped.
14. **Every `/v1/events` failure is a 200 with an empty decision**, so the shim never has to distinguish errors.
15. **Observe-only is enforced twice**: the policy only returns `none`, and the daemon also forces `Action = none` and `Notify = false` before rendering (TODO(M3)).
16. **Eviction and resume**: actors are evicted after 2 h idle. A new actor for a known session continues the seq numbering (`MAX(seq)` from the store) and the counters, but **detector state starts empty**: there is no state snapshot. A panicking actor resets its state the same way.
17. **`loops_detected`** counts distinct loop keys that reached the nudge threshold, as replay does (weaker ones are "below threshold").
18. **File hash after edit/write** (§4.5) is read from disk only for files inside the session's `cwd`, ≤ 2 MB, without a NUL byte in the first 8 KB.
19. **Extra routes**: `POST /v1/shutdown` (used by `loopguard daemon stop`). `/metrics` needs the token like everything but `/healthz`. Metric names are prefixed `loopguard_`, plus `loopguard_errors_total{kind}` and `loopguard_store_rows_total{outcome}`.
20. **Logs**: JSON lines in `<home>/logs/daemon.log`, rotated at 5 MB, two backups. The foreground daemon also logs to stderr. `LOOPGUARD_DEBUG=1` enables debug logs.
21. **Capture mode** (`LOOPGUARD_CAPTURE=1`): every string in the payload (keys too) is redacted, and the user's home directory is replaced with `<HOME>` in its native, slash-flipped, and Claude-project-dir (`C--Users-name`) spellings. Payloads that aren't JSON are skipped. Files are named `<event>-<n>.json`, at most 20 per event type across restarts.
22. **Token file**: mode 0600 on Unix; on Windows a protected DACL with one entry for the current user, set before the token is written.

**Shim**
23. **The shim starts `loopguard daemon` directly** with the detach flags (Windows: `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`, plus `CREATE_BREAKAWAY_FROM_JOB` when the agent's job object allows it, so a job that kills hook children on exit doesn't take the daemon with it; Unix: `setsid`), with its working directory set to the LoopGuard home so it doesn't hold the agent's project directory open. `loopguard daemon --detach` uses the same spawn and waits for `/healthz`.
24. **Autostart rate limit**: the mtime of `<home>/autostart.lock` records the last attempt; two hooks racing past it may both spawn, and the daemon's single-instance check makes the extra one exit. `LOOPGUARD_NO_AUTOSTART=1` disables autostart (for tests, or a daemon run as a service).
25. **Stdin over 4 MB** is read to the end (so the agent's write doesn't fail) and dropped. A reply with an exit code outside 0–255 is ignored. The shim never uses `HTTP(S)_PROXY`.
26. **Latency** (Windows 11, 500 calls, see the M2-A report): p50 60 ms, p99 82 ms end to end, against a 50 ms target. The daemon's own part is ~0.5 ms; process start dominates, and about half of it is binary size (a hook-only build ran at p99 57 ms). Options: a separate small shim binary, or `type: "http"` hooks (§3.3). M2-B did the former (decision 27).

### M2-B

Decisions made while building the hook binary, `init`/`uninstall`, `doctor`, `status`, and capture mode, plus the fixes from the M2-A review.

**Hook binary**
27. **`cmd/loopguard-hook`** is the shim alone (§10): 77 packages instead of 264, 2.4 MB instead of 13.4 MB. Measured on the Windows 11 development machine, 300 runs each: `net.Dial` + a hand-written request cost ~3 ms over an empty program, `net/http` ~5 ms, `encoding/json` ~1 ms, `crypto/rand` ~1.5 ms; so the shim avoids all three, and `TestMinimalDependencies` keeps it that way. Of what is left, `net`'s package init (Winsock start-up) is ~3 ms on Windows; a named-pipe transport would remove it, but isn't worth the complexity yet.
28. **HTTP/1.0** for the shim's request: the daemon (net/http) answers without chunking and closes the connection, so the shim's reply parser is a few lines. Replies over 1 MB, chunked replies, and anything that isn't `HTTP/1.x 200` fail open.
29. **`home/homedir`** holds the shim's view of the home (dir, file names, token, port) with only os/strings/strconv; `home` re-exports it. `proc` uses `syscall` instead of `golang.org/x/sys/windows` for the same reason.
30. **Daemon executable**: `loopguard-hook` starts `loopguard daemon` from the `loopguard` executable in its own directory; `loopguard hook` starts itself. Both binaries must therefore live in the same directory.
31. **Performance target** (§10): p99 < 50 ms on macOS/Linux; on Windows within ~15 ms of the process-start floor, which this machine measures at 25–45 ms p99 depending on load.

**Commands (M2-A review)**
32. **Read-only commands** also include `docker logs`, `kubectl logs`, `aws logs`, `journalctl`, `curl`, `wget`, `echo`, `printf` (§4.3).
33. **Assignments are skipped in families** (§4.3): leading `NAME=value` words (the whitespace fallback kept them: `S="/tmp/x"; "py" -I - <<'EOF'` was the family `s="<tmp>";`) and PowerShell `$env:X=…`/`$x = …` statements. Quotes around a fallback command word are dropped. The replay golden changed accordingly (two bogus families gone).

**Install** (§3.3)
34. **Ownership by base name** (`loopguard-hook[.exe]`), not by a marker field: Claude Code may drop unknown fields when it rewrites settings, and a path survives.
35. **Order-preserving merge**: the settings are edited as ordered JSON members with their original text, then re-indented with the file's own indentation and line endings, so a diff shows only LoopGuard's lines (plus re-wrapping of compact one-line objects, since the whole file is re-indented). Our hook objects are written as `type, command, args, timeout`.
36. **One LoopGuard hook per event**: duplicates (e.g. after a copy-paste) are collapsed; a stale one is replaced at its position; ours are removed from events LoopGuard no longer hooks. Equality is semantic (decoded JSON), so key order alone never counts as a change.
37. **`$CLAUDE_CONFIG_DIR`** is honored for the user settings path. `--project` uses the git root above the current directory (else the current directory). `--settings <file>` targets any file (used by tests, and for unusual setups).
38. **`init` requires the hook binary to exist** at the path it installs, and prints where it wrote, the backup, and the exact undo command. Open Claude Code sessions need a restart to pick up the hooks.

**Doctor and status**
39. **Doctor starts the daemon** if it is down (the next hook would anyway) and sends a synthetic `Stop` event for session `loopguard-doctor` through the installed `loopguard-hook` with `LOOPGUARD_DEBUG=1` and `LOOPGUARD_NO_AUTOSTART=1`: success needs exit 0, empty stdout, **empty stderr** (so a silent fail-open can't pass), the daemon having recorded the event, and < 1 s. Doctor also warns about `disableAllHooks` in the user, project, and local settings files, and fails (exit 1) on any failed check.
40. **Status** lists only active sessions (live actors), leaves out the doctor's session, and shows the last 10 would-have decisions from `GET /v1/decisions`; when the daemon is down it reads them from the database. Reasons are cut at the first line and 100 characters. Migration 2 adds `sessions.failures` and `interventions.ts/detector/reason` (§11); rows from M2-A read back with empty values.

**Capture** (§18.21)
41. **Flag file** `<home>/capture.on` (`loopguard capture on|off`), stat'ed at most every 2 s by the daemon (`capture.Switch`), so no restart; `LOOPGUARD_CAPTURE=1` still forces it on. `/healthz` reports the daemon's view; `status` and `doctor` show the flag file or the daemon's view.
42. **Export** (`capture export`) redacts and de-homes again, then replaces names learned from the payloads: the username of any home directory path (and its words of ≥ 3 letters, e.g. first and last name) → `user`, the last element of each `cwd` (unless generic: `src`, `desktop`, ...) → `project`, matched as whole words, case-insensitively; emails → `<email>`. It writes the `.golden` for each payload (`claudecode.Golden`, the same mapping the test uses), and prints the replaced names so they can be checked. Real payloads hold the user's prompts and tool output and are never committed: default output `testdata/private/hooks/claude-code/<claude --version>/` (git-ignored), and export refuses a `--out` under `testdata/hooks/` without `--structure-only`. **`--structure-only`** (default output `testdata/hooks/claude-code/<version>/`, committed) keeps every key, the nesting and value types, numbers and booleans, but replaces each string with `"<str:N>"` (N = length in characters, after anonymization), except the strings of a keep-list outside content fields: enum-like fields (`hook_event_name`, `tool_name`, `permission_mode`, `source`, `reason`, `model`, `level`, ...) and the anonymized ids and paths (`session_id`, `tool_use_id`, `prompt_id`, `cwd`, `transcript_path`, ...). Everything below a content field (`prompt`, `tool_input`, `tool_response`, `error`, `last_assistant_message`, `message`, `content`) is replaced whatever its key, and so is any field not on the keep-list, so fields Claude Code adds later can't leak.
43. **Golden test** walks `testdata/hooks/claude-code/**` (committed: synthetic and structure-only) and `testdata/private/hooks/claude-code/**` when it exists (local: real payloads), so exported captures are tested with no code change.
44. **Anonymization is OS-independent** (`internal/anon`, CI fix): paths in payloads and transcripts come from whatever OS recorded them, so they are parsed as strings, never with `path/filepath` or `runtime.GOOS`. Both separators; `C:\Users\x`, `C:/Users/x`, `/c/Users/x`, `/mnt/c/Users/x`, `/Users/x`, `/home/x`; Claude Code's project-dir encodings (`C--Users-x-…`, `-home-x-…`, `-Users-x-…`); and the 8.3 short name of a long username (`C:\Users\JANEQ-~1`). Home matching always ignores case. The anonymization tests run Windows-, macOS-, and Linux-style inputs on every OS. `filepath` stays only for files on this machine (settings, home, logs, transcript discovery, hashing an edited file).
45. **Committed-payload check** (`make check-testdata`, a CI step; `TestCommittedHookPayloadsHaveNoContent`): fails if any `.json` or `.golden` under `testdata/hooks/` has a string longer than 40 characters below a content field (`capture.ContentKeys`, which includes the `.golden` names `input` and `output`). It is a safety net against committing a real export; hand-written synthetic payloads must stay within it too.
