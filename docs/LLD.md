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
│   ├── cost/                       # transcript tailer, usage dedupe, pricing (+ the shipped pricing.yaml), per-call estimates
│   ├── notify/                     # desktop, ntfy, webhook (slack/discord), terminal-sequence
│   ├── store/                      # sqlite (modernc.org/sqlite), migrations, repositories
│   ├── replay/                     # offline transcript → event stream → detectors → report
│   ├── dashboard/                  # go:embed templates + htmx, REST handlers
│   ├── config/                     # config.yaml + .loopguard.yaml merge, defaults, validation
│   └── redact/                     # secret patterns
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
- **Equivalences** (one table, `internal/normalize/identity.go`, an example test per rule): `py`/`python3`/`python3.12` → `python`; `python [flags] -m X` → `X`; `npx`/`bunx`/`pnpm exec|dlx`/`yarn exec|dlx`/`npm exec`/`bun x X` → `X`; `npm|yarn|pnpm|bun run test|start|stop|restart` → `… test` (and `npm t`). They apply to families too, so `npx jest` is the family `jest`.
- **Command identity** (what a deny matches, §7.1): the canonical command name plus every remaining argument except harmless flags (verbosity, color, report format, fail-fast: `-q`, `-v`, `--color=…`, `--tb=…`, `-x`, `--ci`, `-count=1`, …), path arguments cleaned (slashes, no `./`, relative to cwd), sorted, so flag order doesn't matter.
- **Lines that aren't valid bash** (PowerShell) are split quote-aware at `;`, `&&`, `||`, and `|` (the pipeline's producer only), dropping redirections and the `&`/`.` call operators. `Set-Location`/`Push-Location`/`Pop-Location` are skipped like `cd`.
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
- **Fires when**, on a successful read, the same file is read ≥ **5** times (`read_thrash.min_reads`) in the window with no successful write to it in between, AND `Progress.Recent(last 15) < 0.5`. The count is the reads minus the distinct ranges (`offset`/`limit`) among them, plus one: paging through a long file in chunks is not thrashing, reading the same part again is.
- Also covers repeated `grep`/`glob` searches: the same ActionFP (pattern and path) with the same result ≥ **4** times (`min_reads − 1`, at least 2), with the same progress condition.
- **LoopKey:** `rt:<path>` for files, `rt:<ActionFP>` for searches.
- **Severity:** `min(0.6, 0.55 + 0.05*(count − threshold))`: it reaches the nudge threshold when it fires. **Nudge only; this detector never escalates past nudge** (one nudge per loop per cooldown).

### D5. No-progress stall
- **Fires when** the last **25** calls of the turn (`stall.window`; it needs that many) made less than **0.5** progress, counting **exploration** as progress (below), and at least **8** of them failed or repeated an earlier read, fetch, or search (polling commands count as neither).
- **Exploration is progress** (`ToolCall.Explore`, set by the session): a successful read of a file not read before in the session, a fetch of a URL not fetched before (`WebFetch`, by URL), a web search with a new query, a `grep`/`glob` with a new pattern and path, and a read-only bash command (`read_only_commands`) not run before. Each adds **0.2** to the stall's progress, so two new things in a stuck stretch don't end it, three do. Repeats don't count. On the private corpus, research sessions produced 45 false "no progress" near misses (longest 77 calls) without this, and 4 false D5 loops in 4 sessions with the detector on but exploration not counted; with it, 0 (§18 M3-C).
- **Quiet while a loop is handled:** no signal while a D1-D3 loop of the turn is nudged or further along; that loop's message is more specific.
- **LoopKey:** `st:<TurnSeq>`. A stall that goes on signals again only after another 25 calls.
- **Severity:** 0.55 (at the nudge threshold). It's lower-confidence: it **nudges once per turn** and can notify the human (event `stall`, §15); it never denies or halts.

### D6. Budget
- **Inputs:** totals kept by the daemon across sessions (`usageBook`, §8.3), not one session's calls: API-equivalent cost and tokens from the transcript tailer (§8.1), wall minutes and completed tool calls from the hook events. `detect.Budget(limits, totals)` is pure, like the other detectors; it runs on every tool event (pre, post, post-failure, batch end), not in the per-call detector loop.
- **Scopes:** `session` (one agent session with its subagents, its whole life), `daily` (all sessions, the local calendar day), `repo` (the sessions of one git repo, the local day). **Metrics:** `cost_usd` (API-equivalent, priced messages only), `tokens` (input + output + cache writes + cache reads, Claude Code's own convention), `wall_minutes` (first to latest event per session, summed over sessions for daily/repo), `tool_calls` (completed calls). Each has a soft and a hard limit; 0 is off. **Defaults: everything off except `daily.soft_usd: 20`.**
- **Fires when** a value is **over** a limit (at the limit is not over): one signal per scope and metric, the hard one if the hard limit is passed, else the soft one.
- **Soft** (severity 0.5): one factual nudge per session and period, given with a tool result (post), reaching the agent at assist and guard (recorded as a would-have below); one `budget_soft` notification per period.
- **Hard** (severity 1.0): at **guard**, a halt on **every tool event of the session while over the limit**, subagent streams included (unlike loop halts, decision 57: the budget is the session's, so usage from a subagent may stop the parent's turn); recorded once per turn. The next human prompt starts a turn whose first tool call halts again, so there is no "commands are not blocked now" note. Below guard: recorded once per period as a would-have; at notify and above one `budget_hard` notification per period.
- **Lag:** usage is read from the transcript every 2 s and the transcript is written a little after the call, so a hard cost or token limit stops the turn at the first tool event **after** the tailer has seen the crossing: the total can run a little past the limit. The halt message says so.
- **LoopKey:** `bg:<scope>:<metric>` (e.g. `bg:session:cost_usd`). The period (session id, local date, or date + repo) is in `Evidence.Budget`.
- **Halt message** (stopReason): `LoopGuard stopped this turn: this session's API-equivalent cost is $15.40, over the hard budget of $15.00. Usage is read from the transcript a few seconds late, so the total can pass the limit before the stop. Your next message continues normally, but tool calls stop again while the budget is exceeded. To raise it, set budgets.session.hard_usd in config.yaml (loopguard home), or turn LoopGuard off for this session: loopguard pause --session 1a2b3c4d` (the lag sentence only for cost and tokens). **Nudge:** `LoopGuard observation: this session's API-equivalent cost is $5.20, past the soft budget of $5.00 (budgets.session.soft_usd). Nothing is blocked; this is a heads-up. Options that often help: finishing the current step and checking with the user before starting more work.`

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
    Denying --> Halted: ≥ 2 more matching attempts after first deny\n OR (≥ 1 deny AND time in loop ≥ max(halt_after_minutes, 3× median run time))
    Halted --> [*]: HALT + notify human
```

- **Levels** (`mode:`, §12): the state machine runs the same at every level and records what the **guard** level does; the level decides what reaches the agent and the human.

  | Level | Agent | Human |
  |---|---|---|
  | `observe` (the default, and the first 3 days after install) | nothing | nothing (records only) |
  | `notify` | nothing | notifications (§15) |
  | `assist` | nudges only | notifications |
  | `guard` (`enforce` is its old name) | nudge → deny → halt | notifications |

  `Decision.WouldHave`, `.Reason`, and `.Message` always record what guard does; `Decision.Action` is that action when the session's level allows it (`SessionState.Allow`: `halt` for guard, `nudge` for assist, `none` otherwise, also for pauses and replay), and `none` otherwise. The daemon caps the rendered action again (`capAction`).
- **User stop** (`loopguard stop`, §18 M3-C): a halt the human asks for, given on the session's next tool event (pre, post, or batch end) with stopReason `Stopped by the user via loopguard stop.`, whether or not a loop was seen. It applies at every level but observe, and it is exempt from the level cap (as are the notes after a stop or block, below). The request stays (in memory and, since M4, in the `stop_requests` table, so a daemon restart keeps it) until the main stream's turn ends (Stop, a human prompt, SessionEnd) or an hour passes; every halt-capable event of the session (subagents included) is halted until then, and the first is recorded (`detector = user_stop`, outcome `n/a`).
- **Resets:** "progress" is measured since the loop entered its current stage. For **repeated-failure loops it is the family's progress only** (§5.1); other loops use the general score. A reset loop keeps its nudge history, so the cooldown still applies.
- **Deny** applies only to the **looping action**: a `PreToolUse` that matches the loop. A D1 loop matches the same `ActionFP`, a D2 loop one of the **command identities** of its failing runs (§4.3, `normalize.Identity`: the canonical command and its meaningful arguments, so `go test ./pkg/a` doesn't block `go test ./pkg/b`, while `python3 -m pytest tests -q` matches `pytest tests`; tools without identities match by family), and a D3 loop an edit or write to the same file. Other actions pass.
- **One more try after a real change**: once a D2 loop has denied at least once, a matching action runs once more if, since the loop's last failing run, a file not edited during the loop was edited or written. If that run fails with the loop's error, the loop is back to denying and the try counts toward the halt like a denied attempt; if it succeeds or fails differently, the loop resets. One try per failing run. (A changed command identity isn't blocked in the first place; if it fails the same way, it joins the loop's identities.)
- **Resets of D2 loops** with identities come only from those commands succeeding or their error changing; another command of the family passing doesn't unblock the looping one.
- **Time-based halt**: only from Denying with at least one deny delivered (a slow loop is always blocked before it is stopped). The limit is `max(halt_after_minutes, 3 × the median run time of that command identity this session)`. Families in `slow_families` never halt on time, only on counts. The time in the loop leaves out `PermissionWait`: in permission mode `default`, per call, the time from `PreToolUse` to the post minus the tool's own `duration_ms` (what can be measured: mostly the human deciding on the permission prompt, plus hook overhead).
- **Halt** ends the turn (`continue:false`). The session itself stays usable when the human returns; `Loops` state is reset on the next human `UserPromptSubmit`, and that prompt's reply carries a short note (§7.3) that the stop is over. After a turn whose denies reached the agent but that didn't halt, the first human prompt gets the same kind of note about the blocks.
- **Cooldown:** max 1 nudge per LoopKey per 10 **completed calls**, and max 3 nudges per turn overall. Suppressed nudges are not recorded. D5's loop (the turn) is nudged at most once.
- **Permission mode awareness:** in `default` (manual approval) mode, the deny and halt occurrence thresholds are raised by 1 (`manual_mode_extra_occurrences`), since a human is presumably watching. In `auto`/`bypassPermissions`/`dontAsk`, the normal thresholds apply.
- Detector capabilities: D1/D2/D3 can deny + halt; D4 nudge only; D5 one nudge per turn + notify; D6 one soft nudge per period + notify, and a hard halt at guard on every tool event of the session (§6 D6; outcome `n/a`).

### 7.2 Decision timing

- Signals are produced on **post** events. If a nudge is due, it goes in the post response (`additionalContext`) and is recorded at that post.
- Reaching the deny conditions on a post signal **arms** deny (state `Denying`). No decision is recorded then.
- Deny decisions are evaluated on **pre** events against loops in `Denying`: each matching pre is a (would-have) deny. The first is "the first deny". The `halt_after_deny_occurrences`-th matching attempt after it (default: the 3rd attempt in total) is a halt instead. A matching pre after `halt_after_minutes` in the loop is also a halt. Below the guard level the action still runs, so its post produces signals as usual; only the pre attempts count toward halt.
- Halt decisions are rendered on the next event of any phase (pre, post, or batch_end), whichever comes first. Below the guard level the halt is recorded once, at the pre where it was decided.

### 7.3 Message templates (factual tone)

Nudge, repeated failure. "In a row" and "first failure … ago", never "within the last minute": in the live test Claude read that as a time window.
```
LoopGuard observation: `{family}` has failed {count} times in a row with the same error (first failure {N} min ago).
Output excerpt:
    {error_line_1}
    {error_line_2}
Between attempts, these files were edited: {files}. The error did not change after any of these edits.
Repeating similar edits is unlikely to change the result. Options that often help: re-reading the full error and the code it points to, checking assumptions (versions, config, env), trying a different approach, or asking the user.
```
(`first failure less than a minute ago` under a minute.) D1 reads "the same `{family}` call has run {count} times with the same result (first run {N} min ago), and no file was changed in between."

**Quoted evidence.** Error lines come from tool output, which can carry text from anywhere (a test fixture, a web page, a log). They appear only in the block introduced by `Output excerpt:`, each line indented by 4 spaces, never inside LoopGuard's own sentences: redacted (§13), markup-like tags removed (`<name …>`, `</name>`, `<!-- … -->`, `<!…>`/`<?…>`; LoopGuard's own placeholders `<N>`, `<TS>`, `<REDACTED:type>`, … are kept), control characters removed, each line clipped at 160 characters, at most 3 lines (`policy.Excerpt`). With no line left, the block is left out.

Read thrash (D4):
```
LoopGuard observation: {path} has been read {count} times (first read {N} min ago) with no change to it in between, and little else has changed. Reading it again is unlikely to show anything new. Options that often help: working from what was already found, looking for the specific thing needed, trying a different approach, or asking the user.
```
A search reads "the same `{grep}` search has run {count} times with the same result (first run …). Running it again will give the same result."

Stall (D5):
```
LoopGuard observation: the last {n} tool calls made no visible progress: {f} calls failed and {r} calls repeated earlier reads or searches (the first of these calls was {N} min ago). Options that often help: restating the goal and what is known so far, trying a different approach, or asking the user.
```

Deny:
```
LoopGuard blocked this command: it is the same `{command}` run that has failed {count} times with the same error, with no change in outcome. A different approach, or a question to the user, is needed before running it again.
```

Halt (stopReason, shown to the human). It says **turn**, never "session": after an earlier wording ("halted this session", "To let it continue, run: loopguard pause"), Claude refused to run anything in the following turns.
```
LoopGuard stopped this turn: `{family}` failed {count} times with the same error and the loop continued after {denies} blocks. Your next message continues normally. (To turn LoopGuard off for this session: loopguard pause --session {short_session})
```
A time-based halt reads "… and the loop has lasted {minutes} min (limit {limit}) after {denies} blocks". The `systemMessage` (and the deny reason of a halt on `PreToolUse`) is `LoopGuard stopped this turn: {reason}`.

After a halt (first human prompt, `UserPromptSubmit` `additionalContext`):
```
LoopGuard's earlier stop applied only to the previous turn; commands are not blocked now.
```
After a turn with denies but no halt (same place, once):
```
LoopGuard's earlier blocks applied only to the previous turn; commands are not blocked now.
```
Both are sent only after the halt or deny reached the agent (guard level, or a user stop).

User stop (`loopguard stop`): stopReason `Stopped by the user via loopguard stop.`; systemMessage `LoopGuard stopped this turn: stopped by the user (loopguard stop)`.

The nudge shows up to 3 error lines, preferring assertion payloads and error messages (`Expected:`/`Received:`, pytest `E` lines, `left:`/`right:`, `error TS…`, `error[E…]`, `x_test.go:N: … want`, `SomeError: …`) over test names, and never FAIL or summary lines (`ErrorSignature.Best`).

Oscillation:
```
LoopGuard observation: {path} has been changed back to a previous version {n} times (the same edit was made and then reverted). The two versions alternate between: {short_diff_summary}.
```

---

## 8. Cost accounting (`internal/cost`)

### 8.1 Transcript tailer
- **Registration:** every hook event with a `transcript_path` registers its session (by session id) with the daemon's tailer. While the session had an event in the last **10 minutes** and hasn't ended, its transcripts are polled every **2 s** (`Options.TailEvery`): the main transcript and every `<session>/subagents/agent-<id>.jsonl` next to it (subagent hook events carry the parent's path), each as its own stream (`claude-code:<sid>` and `claude-code:<sid>/<agent>`). After `SessionEnd` or 10 idle minutes it is polled once more and dropped; the next event registers it again. No fsnotify: polling is simpler and enough at 2 s.
- **Incremental reads** (`cost.Tailer`): from the stored byte offset (`transcript_offsets`), line by line with a buffered reader (never the whole file; a 45 MB transcript is fine, and lines of any length). Only complete lines are consumed: a line still being written (no newline yet) is read on a later poll.
- **Rotation and truncation:** the offset row also stores a fingerprint of the file's first `min(1 KB, offset)` bytes (in the `inode` column: inode numbers aren't portable to Windows, and a fingerprint also catches a file rewritten in place). If the file is shorter than the offset, or its fingerprint changed, reading starts over at 0. Re-read lines are harmless (deduplication below).
- **Parsing** (`cost.ParseLine`): assistant lines with `message.id` and `message.usage`: `input_tokens`, `output_tokens` (includes thinking), `cache_creation_input_tokens` (with `cache_creation.ephemeral_1h_input_tokens`, the part written to the one-hour cache), `cache_read_input_tokens`, `speed` (`fast` is fast mode), `inference_geo` (`us` is US-only inference), and `message.model`. Lines without `"assistant"` skip the JSON decode. Unknown lines and fields are ignored.
- **Deduplicate by `message.id`** (`cost.Ledger`): one API message spans several lines, each repeating its usage, and the counters can grow between lines; the ledger keeps the **max of each counter** per id. A restarted daemon pre-loads the stream's stored rows into the ledger, so a message whose lines straddle the restart counts once.
- **Persisted** as `usage` rows (§11) through the batched writer: one row per (stream, message) with the maxima (`ON CONFLICT … MAX(…)`), then the new offset. Growth (delta tokens and cost) goes into the usage book (§8.3).
- Codex (M5): a `codex` tailer behind the same interface, once its session file's token events are verified.

### 8.2 Pricing
`<home>/pricing.yaml`, written by the daemon at start from the table shipped in the binary (`internal/cost/pricing.yaml`, go:embed) when there is none, and never overwritten: the user edits it. Re-read when it changes (checked every 2 s). An invalid file is logged (daemon), shown by `doctor`, and the shipped table is used. Keys are checked strictly (a typo in a price key would price at 0).

```yaml
currency: USD                 # prices are always USD
currency_display: ""          # optional, e.g. INR: shown next to USD
fx_rate: 0                    # display currency per USD, set by the user; never fetched
models:                       # first match wins
  - match: claude-opus-5-5    # the model id, ignoring a -YYYYMMDD date or [suffix]; globs (*, ?) work
    input_per_mtok: 4
    output_per_mtok: 20
    cache_write_per_mtok: 5          # 5-minute cache
    cache_write_1h_per_mtok: 8       # one-hour cache (Claude Code mostly writes this one)
    cache_read_per_mtok: 0.20
    fast_multiplier: 2               # usage.speed "fast"
    us_multiplier: 1.1               # usage.inference_geo "us"
    long_prompt: { above_tokens: 100000, input_per_mtok: …, … }   # Haiku 5.5: prompts over 100k tokens
    source: https://platform.claude.com/docs/en/about-claude/pricing
    checked: "2026-10-21"            # or "unverified"
```

- **Cost of a message** = (input × input + output × output + 5-minute writes × cache_write + one-hour writes × cache_write_1h + cache reads × cache_read) / 1e6, × the fast and US multipliers when they apply; the long-prompt rates when input + cache writes + cache reads is over `above_tokens`.
- **Shipped prices** (all `checked: 2026-10-21`, from the official pricing page above): Fable 5.1, Mythos 5.1, Fable 5, Mythos 5, Opus 5.5, Opus 5, 4.8, 4.7, 4.6, 4.5, 4.1, 4, Sonnet 5.5, 5, 4.6, 4.5, 4, Haiku 5.5 (with its long-prompt tier), Haiku 4.5, Haiku 3.5. Checked against Claude Code's own `cost-state` lines (§18 M4-A).
- **Unknown or unverified models:** an entry with `checked: unverified` (or none) prices nothing: its tokens are counted and its cost is **unknown**, never 0 (`unpriced_tokens`); `doctor` warns about unverified entries and lists the models seen in the last 30 days without a price. `<synthetic>` messages (written by the client) cost 0.
- **"API-equivalent" everywhere** (`replay`, `report`, `status`, budgets, notifications): subscription users don't pay per token; the number says what the same usage would cost through the API.
- Costs are recomputed from the stored counters with the current table when read (`report`, `status` with the daemon down, the daemon's start), so a price added later applies to old usage too. `usage.cost_usd` is only the cost when written.

### 8.3 Budgets
The daemon's **usage book** (`daemon.usageBook`) keeps, per session (subagents included, and per stream for display), per local day, and per repo for the day: tokens, API-equivalent cost, unpriced tokens, tool calls, and each session's first and latest event (wall time). Usage growth comes from the tailer, calls and wall time from the hook events. At start, and when the day changes, the day's totals are loaded from the store (usage rows since midnight, the day's calls, the sessions seen today); a session's own totals are loaded the first time it is seen. Sessions not seen for 48 h are forgotten.

D6 (§6) is checked on every tool event against the limits in effect for the event's repo (§12). Because transcript usage lags by a few seconds, a hard cost or token limit is enforced **at the next tool call after the tailer saw the crossing**; tool calls and wall time are counted at once. The docs and the halt message say so.

### 8.4 Cost estimates per tool call
`loopguard report` (and `replay --json` via the library) estimates what each tool call cost, marked **est.** everywhere (`cost.Scanner`, fed every raw line in the same pass as replay's reader):

- **Estimate** = the call's share of the output tokens of the API message that issued it (split evenly among the message's tool calls) × that model's output price, **plus** its result size in tokens (characters / 4: transcripts carry no exact count; text blocks only, images not counted) × the price of carrying it in every later model call of the stream until the next compaction (`compact_boundary`): the cache-read price for calls that read from the cache, else the input price, each at its own model's price.
- **Cap:** a stream's estimates are scaled down if they add up to more than its real cost, so the shares never add up to more than 100%.
- **Groups:** a shell command by its family (`bash: npm test`), a file tool by its file relative to the cwd (`read src/a.go`, redacted), anything else by the tool.
- **Report tables:** "Top 10 by estimated cost" (group, runs, average output size, est. cost, est. share of the real cost) and "Largest outputs" (the 5 single results with the highest estimate).
- **Loops:** the cost of a loop is the cost of the model calls from the one that issued its first call to the one that issued its last (`replay`, `report`); "cost inside loops" is the union of those per stream.

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
| `POST /v1/events?agent=claude-code[&entry=&attended=]` | Ingest one hook payload; returns `{"stdout": "...", "exit": 0}`, already rendered by the adapter. `entry`/`attended` are Claude Code's `CLAUDE_CODE_ENTRYPOINT`/`CLAUDE_CODE_SESSION_ATTENDED`, forwarded by the shim (interactive sessions, §15) |
| `GET /v1/sessions?since=&repo=` | List sessions with totals |
| `GET /v1/sessions/{id}` | Session detail: calls, signals, interventions, usage |
| `GET /v1/pauses`, `POST /v1/pauses/reload` | The pauses in effect; re-read the `pauses` table after `loopguard pause`/`resume` wrote it (§18 M3-A) |
| `POST /v1/stop` | `{"session": "<id prefix>"}` or `{"all": true}`: stop the named live sessions' turns at their next tool call (`loopguard stop`, §7.1); answers which sessions will stop and which observe (refused); 404 none, 409 ambiguous |
| `GET /v1/usage` | Today's usage, each live session's (subagents included, and per stream), and config.yaml's budgets: tokens, API-equivalent cost, unpriced tokens, tool calls, wall minutes (`loopguard status`) |
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

CREATE TABLE usage (                     -- one row per API message per stream (§8.1)
  session_key TEXT NOT NULL,             -- the stream: claude-code:<sid> or claude-code:<sid>/<agent>
  message_id  TEXT NOT NULL,
  ts          INTEGER,
  model       TEXT,
  input_tokens INTEGER, output_tokens INTEGER, cache_read INTEGER, cache_write INTEGER,  -- maxima over the message's lines
  cost_usd    REAL,                      -- at write time; NULL: no verified price. Readers re-price.
  session_id  TEXT,                      -- migration 4: the agent session (all its streams)
  repo        TEXT,                      -- migration 4
  cache_write_1h INTEGER DEFAULT 0,      -- migration 4: the one-hour part of cache_write
  speed       TEXT,                      -- migration 4: "fast" = fast mode
  geo         TEXT,                      -- migration 4: inference_geo
  PRIMARY KEY (session_key, message_id)
);
CREATE INDEX idx_usage_ts ON usage(ts);            -- migration 4
CREATE INDEX idx_usage_session ON usage(session_id);
CREATE INDEX idx_calls_ts ON calls(ts);

-- inode holds a fingerprint of the file's first min(1 KB, offset) bytes (§8.1), not an inode number.
CREATE TABLE transcript_offsets (path TEXT PRIMARY KEY, offset INTEGER NOT NULL, inode INTEGER);

CREATE TABLE stop_requests (             -- migration 4: pending `loopguard stop` (§7.1)
  session_id   TEXT PRIMARY KEY,
  requested_at INTEGER NOT NULL,         -- unix ms; older than an hour is dropped
  recorded     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER);

CREATE TABLE pauses (                   -- migration 3: `loopguard pause`
  scope      TEXT NOT NULL,             -- session | repo | all
  target     TEXT NOT NULL DEFAULT '',  -- session id, repo root, '' for all
  until      INTEGER,                   -- unix ms; NULL = until resumed
  created_at INTEGER NOT NULL,
  PRIMARY KEY (scope, target)
);
```

- **Intervention outcome** is decided 10 completed calls after each intervention, real or would-have (§18 M3-A): `halted` if the loop was halted meanwhile (a halt itself is `halted` at once); `recovered` if the looping command itself (one of the loop's command identities) later succeeded or its error changed (D2), or the repeated action gave a different result (D1); `moved_on` if none of that, but the loop stopped signaling (no signal in the last 5 of the 10 calls: the agent did something else, which says nothing about the problem); `continued_loop` otherwise; `unknown` if the turn or the session ended first (or the daemon restarted). This is the core product metric ("nudge success rate"); `loopguard stats` shows it per detector, with recovered and moved_on apart; recovery rate = recovered / (recovered + moved_on + continued_loop + halted).
- **Wasted time** for a loop = time from the first call in the loop (`Since`) to recovery or halt.
- **Retention:** a nightly job deletes `calls`/`signals` older than `retention_days` (30); `sessions` and `interventions` aggregates are kept.

---

## 12. Configuration (`internal/config`)

`~/.loopguard/config.yaml` (global) merged with `<repo>/.loopguard.yaml` (per repo, at the git root of the session's cwd). The repo file may make LoopGuard **more protective, never less** (exact rules in §18 M3-A and M3-C): it may **raise** the level (`mode`), never lower it below config.yaml's (`enabled: false` in a repo file is ignored too: pause the repo instead), and it may turn a detector off and lower occurrence/severity thresholds and budgets (a budget config.yaml leaves off may be set; one it sets may only be lowered, never raised or turned off: §18 M4-A). One exception, by design: a repo file may **add** `slow_families` (a repo knows its slow commands, and they only lose the time-based halt). A missing file means the defaults; an invalid one is skipped with a warning (daemon log, `doctor`). When no file sets `mode`, the level is **observe**.

**Levels** (`mode`), least to most protective: `observe` (record only), `notify` (notify the human, never touch the agent), `assist` (notify and nudge; never deny or halt), `guard` (nudge → deny → halt, and notify). `enforce`, the M3-A name, is read as `guard`. The effective level of an event: `enabled` (config.yaml) → the higher of config.yaml's and the repo's `mode` (unset: observe) → `observe_until` (observe until then, whatever the level) → pauses (observe). `doctor`, `status`, and `/v1/sessions` show each session's level and why.

```yaml
mode: guard              # observe | notify | assist | guard (enforce = guard; unset: observe)
observe_days_after_install: 3
observe_until: 2026-10-23T09:00:00Z   # written by `init` on a new install: observe until then, whatever the level
enabled: true            # false in config.yaml: LoopGuard is off (a repo file can't turn it off)
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
slow_families: []        # never halt on time, only on counts; .loopguard.yaml may add to it
error_marker_families: ["pytest", "jest", "vitest", "go test", "cargo test", "cargo build", "tsc",
                        "npm test", "yarn test", "pnpm test", "bun test", "npm run test", "yarn run test",
                        "pnpm run test", "bun run test", "npx jest", "npx vitest", "npx tsc",
                        "mvn", "mvnw", "gradle", "gradlew", "make", "dotnet test"]
budgets:                 # D6 (§6); 0 or absent = off. Default: only daily soft_usd 20
  session: { soft_usd: 5, hard_usd: 15, soft_tokens: 0, hard_tokens: 0,
             soft_wall_minutes: 0, hard_wall_minutes: 180, soft_tool_calls: 0, hard_tool_calls: 0 }
  daily:   { soft_usd: 20, hard_usd: 50 }      # all sessions, local day
  repo:    { hard_tool_calls: 2000 }           # one repo's sessions, local day
notify:                  # global only; sent at the notify level and above (§15)
  on: [loop_detected, halt, budget_soft, budget_hard]   # events of channels without their own on:
  desktop:  { enabled: true }        # `desktop: true` works too
  terminal: { enabled: false }
  ntfy:     { enabled: false, server: "https://ntfy.sh", topic: "", on: [halt] }   # a topic alone turns it on
  webhook:  { enabled: false, url: "", format: "slack" }   # slack | discord | generic; a url alone turns it on
  # events: loop_detected, nudge, deny, halt, stall, budget_soft, budget_hard
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
- `--json` emits the same data with a versioned schema ([replay-json.md](./replay-json.md)).
- **Cost** (§8): usage and API-equivalent cost per stream and session (deduplicated by message id, subagents rolled up into their session), the cost of each loop (the model calls from its first call to its last), and the cost inside loops (their union per stream); models without a verified price are counted and listed, their cost unknown. Prices come from `<home>/pricing.yaml` when it exists (replay never writes it), else the shipped table. Anonymized exports keep usage (counts, model, hashed message id), compaction boundaries, and each result's original length (`chars`), so cost works on them too.

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
type Sender interface { Send(ctx context.Context, ch config.Channel, n Notification) error }
type Notification struct {
    Event, Project, Session, Detector, Family string // Session: short id (8 characters)
    Count int; Duration time.Duration
    SessionKey, LoopKey string // rate limiting only, never sent
    At time.Time
}
```

**When** (daemon actor, at the `notify` level and above; nothing at `observe`, `off`, or while paused):
- `loop_detected`: a D1-D3 loop reaches the nudge threshold, once per loop per turn (whether or not the level lets the nudge through).
- `stall`: a D5 signal (once per turn).
- `nudge`, `deny`, `halt`: such a decision **reached the agent** (assist: nudges; guard: all). A halted loop's further halts aren't notified again. The user's own stop isn't notified.
- `budget_soft`, `budget_hard`: a budget limit was passed (§6 D6), once per limit and period (session, day, or repo day), at any level from notify up, whether or not anything reached the agent. The body names the scope, metric, value, and limit, e.g. `Hard budget passed: session cost $15.40 (API-equivalent), hard limit $15.00. At the guard level, tool calls stop until the limit is raised.` Both are in the default `on:` list.

**Content** is the same on every channel, and holds only what may leave the machine: title `LoopGuard: <project folder name>`, and a body from the detector, the command family (a path is cut to its base name, secrets redacted, other characters than letters, digits, space, and `._-+:` removed, at most 40), the count, and the duration, plus `To stop it: loopguard stop <short id>` (not on a halt: the turn has stopped). Never file paths, error lines, or code. E.g. `Loop detected: "npm test" failed 6 times in a row with the same error, over 12 min. To stop it: loopguard stop 1a2b3c4d`.

**Channels:**
- **desktop:** the platform's own tool, no dependency: Windows toast through Windows PowerShell (`-EncodedCommand`, title and body passed in environment variables and XML-escaped in the script, `CREATE_NO_WINDOW`; shown under PowerShell's app id, so it is labeled "Windows PowerShell"), `osascript` on macOS (title and body as `argv`), `notify-send` elsewhere when installed. Not retried.
- **terminal:** the adapter attaches `terminalSequence` (OSC 777) to the event's hook reply, **interactive sessions only**: the shim forwards `CLAUDE_CODE_ENTRYPOINT` and `CLAUDE_CODE_SESSION_ATTENDED` (query `entry`, `attended`), and a session is interactive when the entrypoint is `cli` and attended isn't `0`. Synchronous by nature (it is part of the reply), but only a map lookup.
- **ntfy:** JSON publish (`POST {server}` with `topic`, `title`, `message`, `priority` 4 for a halt, else 3, `tags`), so non-ASCII titles need no header encoding. `loopguard notify setup ntfy` writes a topic `loopguard-` + 26 random base32 characters (128 bits): ntfy topics are public, so the name must be unguessable.
- **webhook:** Slack `{"text":…}`, Discord `{"content":…}`, generic `{source, event, title, body, project, detector, family, count, duration_seconds, session, stop_command, sent_at}`. The URL is a secret: errors and `loopguard notify` never show more than its host.

**Delivery:** `Dispatcher.Dispatch` checks the rate limits and queues (16 per channel, dropped and logged when full); one goroutine per channel sends, so a slow webhook delays nothing else and never a hook reply. ntfy and webhooks are retried 3× (1 s, 4 s, 16 s); each attempt has a 10 s timeout; failures are logged (`notification failed`, channel, attempt). **Rate limits, per channel:** at most 1 notification per loop per 10 minutes, except a halt (always sent once), and at most 5 per hour in all. A test binary never sends for real (`testing.Testing()`), whatever its config says.

**Commands:** `loopguard notify` (channels, events, and whether the level sends anything), `notify setup ntfy [--server URL] [--new-topic]`, `notify setup webhook URL [--format …]`, `notify test` (a test notification to every enabled channel, synchronously, with each result; not rate limited).

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
15. **Observe-only is enforced twice**: the policy only returns `none`, and the daemon also forces `Action = none` and `Notify = false` before rendering. Since M3-A both apply only when the session's effective mode is not enforce (decision 51).
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

### M3-A

Decisions made while switching interventions on (config loading, modes, enforce mode, pause/resume, outcome tracking) that the sections above didn't specify.

**Config** (§12)
46. **Loading**: `<home>/config.yaml`, and `.loopguard.yaml` at the git root of the session's cwd. A missing file is the defaults. An **invalid** file (YAML syntax, a value of the wrong type, an out-of-range value, a polling pattern that doesn't compile, > 1 MB) is skipped as a whole: the daemon logs a warning when it (re)reads it, `doctor` warns, and the defaults (so observe mode) apply. **Unknown keys** only warn; the file is still used, since a newer LoopGuard may have written them. Files are re-read when their mtime or size changes, checked at most every 2 s per file; the daemon keeps one pipeline per distinct set of thresholds (a hash of `config.Thresholds`).
47. **Unset `mode` means observe**, not enforce as the §12 sample suggests. An M2 install has no `config.yaml`, and it must not start enforcing when it gets the M3 binary.
48. **What a repo file may do** (`config.Tighten`; the mode and `enabled` rules are superseded by decision 78: a repo file now only raises the level): `enabled: false`; `mode: observe` (`mode: enforce` is ignored unless the global config enforces anyway); a detector's `enabled: false` (`true` is ignored when the global config turns it off); and these numbers, **only when lower** than the global value: `exact_repeat.min_count`, `repeated_failure.min_count`, `oscillation.min_revisits`, `read_thrash.min_reads`, and `policy.{nudge_threshold, deny_after_nudges_occurrences, deny_min_severity, halt_after_deny_occurrences, halt_after_minutes, max_nudges_per_turn, nudge_cooldown_calls, manual_mode_extra_occurrences}`. Global only: `observe_until`, `observe_days_after_install`, windows, `simhash_max_distance`, `max_progress`, `revisit_window`, the reset rules, the command lists, `notify`, `privacy`. Every ignored key becomes a note that `doctor` and `loopguard mode` show. A repo file is usually committed by someone else, so it must never make LoopGuard act where the user's config says it shouldn't.
49. **`observe_until`** (new key, RFC 3339): `init` writes `mode: enforce` and `observe_until: now + observe_days_after_install` on a **new install**: no `config.yaml`, no `loopguard.db`, and init added hooks rather than finding or updating them. An existing install keeps its config (and an M2 install, having none, stays in observe). `init --enforce` and `loopguard mode enforce` write `mode: enforce` and remove `observe_until`: an explicit enforce ends the grace period. `--dry-run` writes no config. Config edits (`config.EditFile`) keep comments and other keys, refuse a file that isn't valid YAML, and check that the result is a valid config before replacing the file atomically.
50. **`enabled: false` is a kill switch** (in config.yaml; since M3-C, decision 78, a repo file can't turn LoopGuard off): events of the repo are not processed or stored at all (reply `none`; the mode shows as `off`). `LOOPGUARD_DISABLE=1` still turns the shim into a no-op before it contacts the daemon (§10).
51. **Effective mode, per event**: `enabled` → `mode` (unset: observe) → `observe_until` → the repo file → pauses. The daemon sets `SessionState.Enforce` from it before running the pipeline; the policy fills `Decision.Action` only when it is set, and the daemon forces `none` again when it isn't. Doctor, status, `mode`, `daemon status`, `init`, and `/v1/sessions` (live sessions: `mode`, `mode_why`) show the mode with its reason, e.g. `observe (observe until 2026-10-23 09:00 after a new install, then enforce)`.

**Pause** (`loopguard pause`/`resume`)
52. **Stored in the `pauses` table** (migration 3), one row per (scope, target); scopes `session` (the agent's session id, so a session's subagent streams are paused with it), `repo` (the git root of the given path; matches a session whose repo is that root, or whose cwd is inside it, ignoring case on Windows), and `all`. `--for 30m|2h|1d` sets `until`; expiry is checked on every lookup, and expired rows are deleted on read. The CLI writes the table itself (so it works with the daemon down) and then calls `POST /v1/pauses/reload`; the daemon also re-reads it every 30 s and at start. A memory-only daemon has no pauses.
53. **`--session` takes a prefix** of the session id (the halt message gives the first 8 characters), a key (`claude-code:<id>`), or a subagent key; an unknown or ambiguous prefix is an error. `pause` without a scope lists the pauses; `resume --all` removes every pause of any scope.
54. **Paused = observe**: the detectors and the state machine keep running, and would-have decisions are recorded, so `resume` continues where the loop is (a loop that reached Denying while paused is denied on its next attempt).

**Enforce mode** (§3.2, §7)
55. **A halt on a pre also denies the action** (`permissionDecision: "deny"` with the one-line summary, next to `continue: false`), so the looping command doesn't run while the turn stops.
56. **A halted loop keeps halting** matching actions in enforce mode until the next human prompt, in case the agent goes on anyway (a halt reply that missed the shim's deadline is dropped). Observe mode records the halt once (§7.2).
57. **Subagent streams never halt**: their loops are nudged and denied inside the stream (deny goes on past the halt thresholds and `halt_after_minutes`), since `continue: false` would end the parent's turn. The pipeline marks a session as a subagent stream from `Event.AgentID`, so replay reports don't show "would have halted" for subagents either. A human prompt to the parent starts a new turn in its live subagent streams too.
58. **A human prompt** is a `UserPromptSubmit` whose prompt is non-empty and doesn't start with `<task-notification>`, `<system-reminder>`, or `<local-command-`. Others parse to the new phase `auto_prompt` and reset nothing.
59. **Messages** (§7.3) are built in `internal/policy/templates.go` from the loop's latest signal, now kept in `LoopState.Count`/`.Evidence` so pre-event decisions have evidence too. Up to 2 redacted error key lines (in key-line order: for jest that is the `FAIL` line and the test name, not necessarily the assertion), up to 6 edited files shown relative to the session's cwd, lines clipped at 160 characters, the whole message at 1,200. The halt message names `loopguard status` instead of `loopguard report` (M4) and has no cost yet (M4); it always ends with `To let it continue, run: loopguard pause --session <first 8 characters of the id>`. D1 got its own templates; D3's has no diff summary (only content hashes are kept); D4/D5 use a generic one. The halt's one-line reason, which is also its `systemMessage`, is in plain words. A test rejects `SYSTEM`, `you must`, `MUST`, `IMPORTANT`, and `ignore previous` in every template.
60. **Several nudges on one call** are joined into one `additionalContext` (within the cap); otherwise the strongest action is rendered.
61. **`interventions.mode`** is `enforce` when the action was returned to the agent (it may still have missed the shim's deadline), `observe` for a would-have.

**Outcomes** (§11)
62. Tracked per session by `policy.Outcomes` in the actor, for every intervention: resolved 10 completed calls later, at once for a halt, or as `halted` when a halt of the same loop comes first. `End` (human prompt, SessionEnd, actor eviction, panic, shutdown) resolves the rest as `recovered` if a recovery was already seen, else `unknown`. A daemon at start sets every outcome still NULL to `unknown`. Updates go through the writer as `OutcomeUpdate` rows (`UPDATE … WHERE session_key, seq, loop_key, action AND outcome IS NULL`), after the intervention's own row.
63. **`loopguard stats [--since 7d] [--json]`**: totals by kind (real/would-have), and per detector the actions and outcomes, with recovery rate = recovered / (recovered + continued_loop + halted), apart for real and would-have (a would-have recovery is the agent recovering on its own). **`status`** shows the mode and pauses, each session's short id (and its mode when it differs), and the last 10 decisions with real/would, detector, outcome (or `pending`), and reason.

**Tests and demo**
64. **`testdata/scenarios/stuck-npm-test.jsonl`** is the scripted stuck session (payload shapes from the 2.1.304 captures; `<CWD>` is replaced at run time), written by `go test ./cmd/loopguard-hook -run ScenarioFile -update`. `TestInterventionsThroughTheHookBinary` runs it through the real `loopguard-hook` binary and a daemon in a temp home, in enforce and observe mode; `scripts/demo-intervention.ps1` runs the same file and prints every event with LoopGuard's reply.

### M3-B

Fixes from the live enforce test in Claude Code 2.1.304 and from the M3-A review.

**Halt wording** (§7.3)
65. **"Turn", never "session"**: stopReason `LoopGuard stopped this turn: {reason}. Your next message continues normally. (To turn LoopGuard off for this session: loopguard pause --session {id})`; the `systemMessage` and a halt's deny reason start the same way. In the live test, "halted this session" plus "To let it continue, run: loopguard pause" made Claude refuse to run anything after the human's next prompt, although LoopGuard had reset. A template test checks that "session" appears only in the optional pause command.
66. **Post-halt note**: after a halt that reached the agent (enforce mode), the next human `UserPromptSubmit` reply carries `additionalContext`: "LoopGuard's earlier stop applied only to the previous turn; commands are not blocked now." It goes on the prompt itself rather than on a later tool event, since the agent decides whether to call tools before any tool event. It is not an intervention (no row, no outcome).

**Blocking the same command** (§4.3, §7.1)
67. **Command identity** replaces the family for D2 deny matching; equivalence rules live in one table (`identity.go`). They also apply to families (`npx jest` is now `jest`), so a reworded command doesn't start a new loop either. The replay golden changed only in family statistics (no loop changed).
68. **Everything after the command name is sorted** for the identity, the subcommand included: the family's "first word-like argument" rule would make `pytest tests -k a` and `pytest -k a tests` differ. The flip side: two different commands with the same multiset of arguments (`-k a -m b` vs `-k b -m a`) share an identity.
69. **PowerShell segmentation** (§4.3): the whitespace fallback now splits commands quote-aware, so `Set-Location x; npm test 2>&1 | Select-Object -Last 40` is `npm test`. Fallback words keep their quotes, so a quoted first argument is never taken for a subcommand.
70. **D2 resets come from the loop's own commands**: with identities, only one of them succeeding (or failing with another error) resets a nudged or denied loop. Found by a scenario test: `npm test -- other.test.ts` passing reset the `npm test` loop and unblocked it.

**One more try** (§7.1)
71. After at least one deny, an edit or write (successful) to a file not edited during the loop, since its last failing run, lets one matching run through (`LoopState.RetrySeq/RetryPending`). Same error → back to denying, counted like a denied attempt (`Denies++`), so retries can't postpone the halt forever; success or another error → reset. Only D2 loops get it: D1 is reset by any write already, and D3's "change" is the loop itself. Files edited between attempts (`Evidence.Files`) form the loop's file set.

**Time rule** (§7.1)
72. A time-based halt needs Denying and ≥ 1 deny; limit `max(halt_after_minutes, 3 × median run time of the command identity)` (the session keeps the last 20 run times per identity); `slow_families` (config.yaml, and a repo may add) never halt on time. Run time is the post's `duration_ms` when Claude Code sends it (new `Event.RunTime`), else the pre→post gap.
73. **Permission wait**: in permission mode `default`, per call, (pre→post gap) − `duration_ms` is added to `SessionState.PermissionWait` (with time samples, so a loop subtracts only the wait since it started). That is all that can be measured from hooks: it is mostly the human deciding, plus hook overhead; time the agent spends thinking between calls still counts.

**Outcomes** (§11)
74. **`moved_on`**: `recovered` now requires the looping command identity to succeed or its error to change; a loop that just went quiet is `moved_on`. Another command of the family passing is `moved_on` too. `stats` shows rec/moved/cont/halt/unk and counts moved_on against the recovery rate. Rows decided before M3-B keep their old `recovered` (some of which were really "went quiet"); they can't be split after the fact.

**Evidence** (§7.3)
75. **`ErrorSignature.Best`**: up to 3 lines scored per runner pattern (assertion payloads and error messages 4, assertion words 3, test names and frames 2, other key lines 1, FAIL/summary lines 0), in output order. `Lines` (and so `Exact`) are unchanged; D2's `Evidence.ErrorLines` uses `Best`. Tested on every fixture in `testdata/outputs`.

**Real prompts** (§3.1, decision 58)
76. **Checked against private data** (counts only): the private hook captures hold one `UserPromptSubmit`, plain text. In 24 private transcripts the user messages were 184 plain human text, 39 `<pasted_content …>` (origin human), 16 slash commands (`<command-name>`), 31 `<task-notification>`, 16 `<local-command-caveat>`, 8 `<local-command-stdout>`, 18 `isMeta` text, and 4 interrupts. The rule stands; test cases now pin `<pasted_content` and `<command-name>` as human. Whether Claude Code fires `UserPromptSubmit` for task notifications at all is still unverified (no capture).

### M3-C

Levels, notifications, `loopguard stop`, D4/D5, quoted evidence, and the wording fixes from the M3-B live test.

**Levels** (§7.1, §12)
77. **Four levels instead of observe/enforce**: `observe`, `notify`, `assist`, `guard`; `enforce` in a file or `loopguard mode enforce` is `guard` (`mode enforce` writes `guard` and says so), so M3-A configs keep working. `SessionState.Enforce` became `Allow` (the strongest action that reaches the agent). Below guard the state machine still runs and records what guard does (the interventions table keeps `mode = observe` for those rows, `enforce` for rows that reached the agent), so `stats` and the outcome tracking work at every level.
78. **A repo file only raises the level** (supersedes the mode and `enabled` rules of decisions 48 and 50): the higher of config.yaml's and the repo's `mode` wins; a lower one, and `enabled: false`, are ignored with a note (`doctor`, `loopguard mode`). A repo that should be quiet is paused instead (`loopguard pause --repo`), which the user does, not a committed file. `enabled: false` in config.yaml is still the global kill switch. Detector switches and thresholds keep the M3-A rules. `observe_until` applies to the repo's level too.
79. **`observe_until` still applies to new installs**, whatever the level: `init` writes `mode: guard` with it; `loopguard mode <level>` (other than observe) ends it, like `mode enforce` did.
80. **Pauses are `observe`**: nothing is sent to the agent or the human. D4/D5 nudges count toward `max_nudges_per_turn` like any nudge.

**Notifications** (§15)
81. **No new dependency**: desktop notifications run the platform's own tool (PowerShell, osascript, notify-send) instead of beeep. The text is passed as data (environment variables, argv), never spliced into a script.
82. **What is notified**: `loop_detected` is the first signal at the nudge threshold of a D1-D3 loop in a turn, at any level from notify up (at notify it is the only loop event, since nothing reaches the agent); `stall` the D5 signal; `nudge`/`deny`/`halt` only decisions that reached the agent. D4 has no event of its own (a weak signal; its nudges are notified as `nudge` where they reach the agent). The user's stop isn't notified (the user did it).
83. **Rate limits are per channel**: "1 per loop per 10 min, 5 per hour" is counted for each channel separately, since channels have their own event lists (a phone set to `halt` must not miss the halt because the desktop got the loop's `loop_detected`). A **halt always gets through the per-loop limit** (it is the loop's last word, and the human must act), but not the hourly one; repeated halts of an already-halted loop aren't notified at all.
84. **Terminal channel only for interactive sessions**, from Claude Code's own environment forwarded by the shim (`CLAUDE_CODE_ENTRYPOINT=cli`, `CLAUDE_CODE_SESSION_ATTENDED` not `0`). Checked on this machine for an interactive session (`cli`, `1`); what `claude -p` sets is not verified here, which is why anything but `cli` (and no value, from an older shim) means not interactive.
85. **Old `notify:` shapes stay valid**: `desktop: true`, `ntfy: { topic: … }` without `enabled` (a topic or URL alone turns the channel on), and `budget_soft`/`budget_hard` in `on:` (accepted for M4). Unknown events, formats, non-http(s) URLs, and topics outside `[-_A-Za-z0-9]{1,64}` make the file invalid, like any other bad value (decision 46).
86. **Tests never notify for real**: daemon `Options.Senders` injects senders; with none, a test binary (`testing.Testing()`) gets no senders at all, so `go test` can't pop a toast or call ntfy whatever a temp config says. `loopguard notify test` uses a package variable the CLI tests replace.

**Stop** (§7.1)
87. **`loopguard stop <prefix> | --all`** goes to the running daemon (`POST /v1/stop`), which matches live main sessions (not subagent streams, the doctor's session, or ended sessions) by session-id prefix (an exact id wins; 409 with the candidates when ambiguous, 404 when none). With the daemon down no session is being watched, so it fails with that explanation rather than storing a request.
88. **The request lives in the daemon's memory**, by session id, until the main stream's turn ends (Stop, a human prompt, SessionEnd) or an hour passes. Every halt-capable event of the session gets the halt until then (as a halted loop keeps halting, decision 56), subagent streams included; only the first is recorded. A human prompt cancels it (the user typed before a tool call came). At the event, an `observe` level (or a pause) drops it; the CLI refuses such sessions up front and exits 1.
89. **Desktop buttons [Stop it] / [Let it run] are not implemented.** Per-user registration is possible (a `loopguard:` URI handler under `HKCU\Software\Classes`), but not cleanly: (a) any web page or document could then open `loopguard:stop?…` links (after a browser prompt), so each notification would need a one-time secret; (b) protocol activation starts `loopguard.exe`, a console program, so a console window flashes on every click unless a separate GUI-subsystem launcher is shipped; (c) in-process toast activation needs LoopGuard's own app id, which Windows only accepts from a Start-menu shortcut (a file outside the registry); (d) none of it can be tested without writing to the real user's registry hive. `To stop it: loopguard stop <id>` in the body does the job.

**D4/D5** (§6)
90. **D4 counts re-reads, not reads**: reads of the same file minus its distinct ranges, plus one, so paging (`offset`/`limit`) isn't thrash; searches need the same result as well as the same pattern and path. Severity starts at the nudge threshold (0.55) so a firing D4 nudges; the §6 range 0.4-0.6 would have made it nudge only from the 7th read.
91. **D5 exploration score 0.2 per new thing**, counted inside D5 only (not in `Progress`, so D1-D3 resets don't change). New: a file read, a URL fetched (`WebFetch` by `url`), a web search query, a grep/glob pattern and path, a read-only bash command; tracked per session (up to 4,096 entries, then the set starts over). D5 stays quiet while a D1-D3 loop is handled, signals at most once per 25 calls, and nudges once per turn (`Capabilities.Once`). Severity 0.55 instead of the §6 sample's 0.5, which was below the nudge threshold.
92. **Private corpus** (23 sessions, 24 streams, 2,914 tool calls, no real loops; aggregate counts only): D4 and D5 signals: **0** (no loops of any detector, no below-threshold signals). Stall near misses: **45 → 5** (longest stretch 77 → 10 calls; none of the 5 had a failure or repeated read). With D5 on but exploration not counted, D5 would have fired **4** times in 4 sessions. D4 near misses (the same read twice, of 5 needed): 3. The replay near-miss report now files repeated reads and searches under `read_thrash` instead of `exact_repeat`.

**Messages** (§7.3)
93. **Quoted evidence**: tool-output lines only inside `Output excerpt:`, indented 4 spaces; tags stripped except LoopGuard's own placeholders (an exact list, so an `<IMPORTANT>` from the output is removed too); control characters removed; 160 characters, 3 lines. Tested with outputs holding `<system-reminder>`, closing tool-result tags, and "you must …" sentences: they appear only in the block, shortened.
94. **Wording from the live test**: "has failed 4 times in a row with the same error (first failure N min ago)" replaces "… within the last minute" (read as a time window); D1, D4, and D5 use the same "first … ago" form. The blocks note is sent on the first human prompt after a turn whose denies reached the agent without a halt; a halt's note wins when both apply.

**CI**
95. **govulncheck** (`make vulncheck`, `golang.org/x/vuln/cmd/govulncheck@v1.9.0`) runs on all three CI platforms, after staticcheck. It reported no vulnerabilities with Go 1.27.2 when added.

### M4-A

Cost, budgets, and `loopguard report`: decisions the sections above didn't specify.

**Usage** (§8.1)
96. **Polling, no fsnotify**: every 2 s while the session had a hook event in the last 10 minutes; one last poll after `SessionEnd` or going idle. A transcript is read line by line from its stored offset, and only complete lines count (a line being written is read next time). Checked with a 120 MB transcript of 300 KB lines (`TestTailerBigFile`).
97. **A fingerprint instead of an inode** (`transcript_offsets.inode`): an xxhash of the first `min(1 KB, offset)` bytes. Inode numbers aren't portable (Windows), and the fingerprint also notices a file rewritten in place. A shorter file or another fingerprint restarts at 0; deduplication makes that harmless.
98. **Dedup keeps the max of each counter per message id**, and a restarted daemon pre-loads the stream's stored rows, so a message whose lines straddle a restart counts once (`TestUsageSurvivesRestart`). In the private corpus 1,779 of 2,170 messages spanned several lines, and 10 lines carried a different usage than an earlier line of the same message.
99. **Subagents**: their transcripts are found next to the main one (`<session>/subagents/agent-*.jsonl`; hook events of subagents carry the parent's `transcript_path`) and kept as their own streams (`claude-code:<sid>/<agent>`); the session's totals add them up. `report`, `status`, and `/v1/usage` show both.
100. **Costs are re-priced when read**: `usage` stores the counters (with the one-hour cache-write part, speed, and geo), and `report`, `status` (daemon down), `doctor`, and the daemon's start price them with the current `pricing.yaml`. `usage.cost_usd` is only the cost at write time.

**Pricing** (§8.2)
101. **Prices from the official page**, fetched on 2026-10-21: https://platform.claude.com/docs/en/about-claude/pricing (docs.claude.com redirects there). Every shipped entry has that `source` and `checked: "2026-10-21"`; none is unverified. Fast mode, US-only inference (1.1×), and Haiku 5.5's over-100k-prompt prices are from the same page.
102. **One-hour cache writes are priced apart** (2× input; five-minute writes 1.25×). Claude Code writes almost only the one-hour cache: in the private corpus 8.98M of 9.13M cache-write tokens (after dedup). Pricing all writes at the five-minute rate would undercount the write cost by about 37%.
103. **Checked against Claude Code's own totals**: transcripts contain `cost-state` lines with Claude Code's `totalCostUSD`. One session's `modelUsage` for Opus 5.5 prices to exactly $10.6091082 here (`TestCostMatchesClaudeCode`). Over the 12 private sessions that have a `cost-state` line: Claude Code $116.61, LoopGuard $115.19 (−1.2%); 10 of 12 within 5%, the other two +14.0% and −5.0% (likely usage after the last `cost-state` line was written; not verified).
104. **Model ids** are matched exactly (ignoring a `-YYYYMMDD` date and a `[…]` suffix), not by prefix globs, so `claude-opus-5` never prices `claude-opus-5-5`, and a future `claude-opus-5-7` is unknown rather than mispriced. The ids of models not seen in the private corpus (Fable, Mythos, Sonnet 5/5.5, Opus 4.7/4.8, …) follow the naming pattern of the ones seen: their prices are verified, their ids assumed.
105. **Unknown means unknown**: a model without an entry, or with `checked: unverified`, has its tokens counted and its cost left out of every total, shown separately as "not priced" (`unpriced_tokens`); `doctor` lists such models seen in the last 30 days. USD budgets can't count them; token budgets do.
106. **The display currency** (`currency_display`, `fx_rate`) is only multiplied in for display; the rate is the user's, never fetched.
107. **`replay` and `report` never write `pricing.yaml`**; the daemon does at start, once. `doctor` warns when it is missing, invalid, or has unverified entries.

**Budgets** (§6 D6, §8.3)
108. **D6 runs outside the per-call detector loop**: its inputs are cross-session totals the daemon keeps, so `detect.Budget` takes them as arguments (it stays pure) and the actor calls it on every tool event. Replay doesn't run it.
109. **Scope periods**: session = its whole life; daily and repo = the local calendar day (the book rolls over at midnight and reloads the new day's totals from the store). **Tokens** = all four kinds, as Claude Code totals them. **Wall minutes** = first to latest event per session, summed over sessions.
110. **Hard budgets halt every tool event of the session, subagents included**, at guard, recorded once per turn; the next turn's first tool call halts again (no "commands are not blocked now" note, which would be false). Unlike loop halts (decision 57), stopping the parent from a subagent's usage is intended: the budget is the session's. Below guard: one would-have halt per period, nothing reaches the agent.
111. **Soft budgets**: one nudge per session and period, only with a tool result (a nudge can't ride on a pre), at assist and guard; one notification per period. Budget notifications are rate-limited under `bg:<scope>:<metric>:<level>`, so the hard one never waits on the soft one's 10-minute limit.
112. **Restart**: the book's totals reload from the store; announcements already recorded today (budget interventions since midnight) aren't repeated. A session-scope soft note recorded on an earlier day can come again after a restart.
113. **A repo file may set a budget that config.yaml leaves off, or lower one**; raising one or setting it to 0 is ignored with a note (`doctor`, `loopguard mode`). `soft > hard` (both set) makes the file invalid.
114. **Defaults**: only `daily.soft_usd: 20`; `budget_soft` and `budget_hard` join the default notification events (nothing is sent at the default observe level).
115. **`wall_minutes: 180`** from the earlier §12 sample is not a key (it is `hard_wall_minutes`); as an unknown key it only warns.

**Stop** (§7.1)
116. **Pending stops are stored** (`stop_requests`, migration 4) and reloaded at start (requests older than an hour are dropped). The request is written synchronously when `loopguard stop` asks; marking it recorded and deleting it happen on the hook path, so those writes run in the background (a failure is logged; the in-memory request still works). `TestStopSurvivesRestart`.

**Report** (`loopguard report`, §8.4)
117. **With a session**: the timeline comes from the transcript (replay with costs and a timeline), since the database keeps neither turns nor per-turn usage; what LoopGuard did comes from the database when it watched the session (interventions matched to the replayed loops by loop key; the rest listed as "Other"), else the replay's would-haves. Times are offsets from the session's first call, so the output doesn't depend on the time zone (golden tests in `cmd/loopguard/testdata`).
118. **Without a session**: today, 7 days, and 30 days from the database. Loops are (stream, loop key) pairs whose signals reached the nudge threshold, spanning the calls their evidence names (not split by turn: an approximation), and the stream's usage inside those spans is the estimated cost inside loops. The "Top 10 by estimated cost" comes from the transcripts changed in the last 7 days (the database has no output sizes).
119. **Estimates are estimates**: characters / 4 for a result (no exact count in transcripts; images not counted), carried at the cache-read price when the later call read from the cache. Capped per stream at its real cost. On a synthetic session where one 200,000-character output is carried by later calls, it is the top group and the largest output (`TestReportSession`).
120. **No raw text**: reports show families, normalized and redacted commands and paths, counts, and costs; never outputs or prompts (checked in the tests).

**Replay and fixtures**
121. **Replay reports cost** (`cost` in `--json`; schema version unchanged, the field is additive): totals, per session, per loop, and inside loops. On the private corpus (23 sessions, 24 streams, 2,914 calls): 736.6M tokens (725.1M of them cache reads), $358.02 API-equivalent, every model priced, $0 inside loops (it has no loops); 0.7 s.
122. **Anonymized exports keep usage** (counts, model, hashed message id), compaction boundaries, and each result's original length (`chars`). The three committed fixtures were re-exported and merged with their committed tool results (the current soft-failure rules would flip `is_error` on 23 results and change the golden's failure statistics), so the golden changed only in line counts and the new cost section ($69.26 for the three).
123. **`report` is no longer a stub**; `stubCmd` and its test are gone.
