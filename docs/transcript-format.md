# Claude Code transcript format (as observed)

What `loopguard replay` reads. This describes what was **observed** in a sample of real Claude Code transcripts (23 session files and 1 subagent file across 6 projects, ~17.6k lines, written in Oct 2026), not a spec. Rows marked *(not in sample)* are handled defensively from the documented client behavior but were not seen. The format is undocumented and changes between versions, so the reader is defensive: unknown line types and fields are ignored, malformed lines are counted and skipped. All values below are made up.

## Files

```
~/.claude/projects/                       # or $CLAUDE_CONFIG_DIR/projects
  <encoded-cwd>/                          # cwd with every non-alphanumeric char replaced by "-"
    <session-id>.jsonl                    # one main session; file name == sessionId
    <session-id>/subagents/agent-<agent-id>.jsonl   # a subagent's own stream
    <session-id>/subagents/agent-<agent-id>.meta.json  # {agentType, description, spawnDepth, toolUseId}
    <session-id>/tool-results/<id>.txt|.jpg          # large or binary tool outputs, referenced from results
    <session-id>/custom-title.json
    memory/*.md                           # project memory, not a transcript
```

Example of the cwd encoding: `C:\Users\dev\work\api` → `C--Users-dev-work-api`. It is lossy (`a-b` and `a/b` collide), so the cwd recorded in the lines is the source of truth.

## Lines

One JSON object per line (JSONL). Lines can be large: the longest observed was ~1.3 MB (a tool result), so the reader must not use a line-length limit. No malformed lines were observed, but the reader still tolerates them.

Every line has a `type`. Observed types, most common first:

| `type` | Meaning | Used by replay |
|---|---|---|
| `assistant` | One content block of a model response | yes: `tool_use` blocks |
| `user` | A human prompt, a tool result, or injected context | yes: prompts and `tool_result` blocks |
| `attachment` | Context injected by the client (reminders, environment, edited-file notes, queued commands, ...) | no |
| `system` | Client events; `subtype` is one of `stop_hook_summary`, `turn_duration`, `away_summary`, `local_command`, `api_error`, `compact_boundary`, `informational` | `compact_boundary` noted only |
| `permission-mode` | `{permissionMode}` change | yes: permission mode |
| `last-prompt`, `ai-title`, `custom-title`, `agent-name`, `mode`, `atis-latch` | Session metadata, no timestamp | no |
| `queue-operation` | Prompt queue `enqueue`/`dequeue`/`remove` | no |
| `file-history-snapshot`, `file-history-delta` | Checkpoint bookkeeping | no |
| `pr-link`, `cost-state` | PR links; Claude Code's running cost totals (`totalCostUSD`, `modelUsage` per model) | no (LoopGuard prices usage itself; `cost-state` was used to check its prices, LLD §18 M4-A) |

Common fields on `user`/`assistant`/`system`/`attachment` lines:

```json
{"type":"user","uuid":"u-1","parentUuid":"a-0","sessionId":"0000-…","timestamp":"2026-10-01T09:00:00.123Z",
 "cwd":"C:\\Users\\dev\\work\\api","gitBranch":"main","version":"2.x.y","isSidechain":false,"userType":"external","entrypoint":"cli"}
```

- `timestamp` is RFC 3339 with milliseconds, UTC (24 chars).
- `cwd` can change within a session (up to 5 distinct values were seen in one file).
- Subagent lines also carry `agentId` and `isSidechain: true`. In the observed data, sidechain lines appear only in subagent files, never in the main file.

## Assistant lines: tool calls

```json
{"type":"assistant","timestamp":"…","requestId":"req_…","message":{
  "id":"msg_01…","model":"claude-…","role":"assistant","stop_reason":"tool_use",
  "content":[{"type":"tool_use","id":"toolu_01…","name":"Bash","caller":{"type":"direct"},
              "input":{"command":"npm test","description":"Run tests","timeout":120000}}],
  "usage":{"input_tokens":3,"output_tokens":120,"cache_creation_input_tokens":0,"cache_read_input_tokens":5000, "…":"…"}}}
```

- **One content block per line.** A single API message (`message.id`) is spread over several lines (1 to 6+ observed): `thinking`, `text`, and `tool_use` blocks each get their own line, and **each line repeats the message's `usage`**. Cost accounting (M4) must deduplicate by `message.id` (LLD §8.1).
- **Usage** (what cost accounting reads, LLD §8.1): `usage.input_tokens`, `output_tokens` (thinking included: `output_tokens_details.thinking_tokens` is a part of it), `cache_creation_input_tokens` with `cache_creation.{ephemeral_5m_input_tokens, ephemeral_1h_input_tokens}` (in the sample almost all writes went to the one-hour cache), `cache_read_input_tokens`, `speed` (`standard`; `fast` for fast mode), `service_tier`, `inference_geo` (`global` or `not_available` in the sample), and `iterations`, `server_tool_use`, `fallback_credit` (not used). Models seen: `claude-opus-5-5`, `claude-opus-5`, `claude-opus-4-6`, `claude-haiku-5-5`, `claude-haiku-4-5-20251001`, and `<synthetic>` (messages the client writes itself, zero usage). Of 2,170 messages, 1,779 spanned several lines, and 10 lines repeated a message with different counters: keep the maximum of each.
- Tool names seen: `Bash`, `PowerShell`, `Read`, `Edit`, `Write`, `Grep`, `Glob`, `WebFetch`, `WebSearch`, `ToolSearch`, `TaskStop`, `Monitor`, `Agent`, `SendUserFile`, `SendFeedback`, and `mcp__<server>__<tool>`.
- Inputs seen: `Bash`/`PowerShell` `{command, description, timeout?, run_in_background?}`; `Read` `{file_path, offset?, limit?}`; `Edit` `{file_path, old_string, new_string, replace_all}`; `Write` `{file_path, content}`; `Grep` `{pattern, path, output_mode, glob?, -n?, -i?, -A?, head_limit?}`; `Glob` `{pattern, path?}`; MCP tools: arbitrary.

## User lines

`message.content` is either a string or an array of blocks (`text`, `image`, `tool_result`).

### Tool results

```json
{"type":"user","timestamp":"…","sourceToolAssistantUUID":"a-7","promptId":"p-…",
 "message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01…",
   "content":"Exit code 1\n…output…","is_error":true}]},
 "toolUseResult":"Error: Exit code 1\n…"}
```

- `tool_result.content` is a string (most results) or an array of `text`/`image`/`tool_reference` blocks. This is the text the model saw, and what replay uses as the output.
- **Errors:** `is_error: true` marks a failed call. It is present on most results and absent on the rest (absent means not an error). A failed shell command's content starts with `Exit code N`.
- `toolUseResult` holds the client's structured result, with a shape per tool: `{stdout, stderr, interrupted, isImage, noOutputExpected, …}` for a successful shell command; **a plain string** for a failed one; `{filePath, oldString, newString, structuredPatch, originalFile, …}` for `Edit`; `{type, file: {…}}` for `Read`; and so on. Replay does not depend on it.
- Large outputs are replaced by a `<persisted-output>` pointer to `tool-results/<id>.txt`.
- Every `tool_result` joined to a `tool_use` in the same file (no orphans either way in the sample). A result's timestamp was never before its tool use: under 1 s for about a third of calls, under 1 min for most, and over 1 min for long-running commands.

### Human prompts and other user lines

Not every `user` line without a `tool_result` is a person typing:

| Shape | What it is | Turn boundary? |
|---|---|---|
| `origin.kind == "human"` (newer clients; also `promptSource: "typed" \| "sdk"`, `turnOrigin: "human"`, `permissionMode`) | A real prompt | **yes** |
| No `origin`, plain text, not `isMeta` | A real prompt from an older client | **yes** |
| Content starts with `<command-name>` | A slash command the user ran | **yes** |
| Content starts with `<bash-input>` *(not in sample)* | A `!` shell command the user ran | **yes** |
| Text block starting with `[Request interrupted by user` *(not in sample)* | The user interrupted the agent | **yes** |
| `origin.kind == "task-notification"` / `turnOrigin: "task_notification"`, or content starting with `<task-notification>` | A background task finished | no |
| `isMeta: true` (e.g. `<local-command-caveat>`) | Injected context | no |
| Content starting with `<local-command-stdout>`, `<system-reminder>`, `<bash-stdout>`, `<bash-stderr>` | Output shown to the model | no |
| `isCompactSummary: true` (with `isVisibleInTranscriptOnly`) | The summary written at compaction | no |

### Compaction

A `system` line with `subtype: "compact_boundary"` and a `compactMetadata` object, followed by a `user` line with `isCompactSummary: true`. The session continues in the same file afterwards. Replay treats compaction as neither a turn boundary nor a reset.

## Timestamps and order

Lines are in causal order, but timestamps are **not monotonic**: about 5% of consecutive timestamped line pairs go backwards, by up to an hour or more. Every regression observed came from metadata lines (`attachment`, `pr-link`, `queue-operation`, `file-history-delta`) written with their own clocks. `tool_use`/`tool_result` timestamps were consistent with each other. Replay uses only `tool_use`, `tool_result`, and prompt timestamps, and clamps each stream to non-decreasing time.

## Subagents

A subagent (the `Agent` tool) writes its own file under `<session-id>/subagents/`. Its lines have the parent's `sessionId`, plus `agentId` and `isSidechain: true`, and contain the subagent's own tool calls and results. The parent's `Agent` result carries `toolUseResult.agentId` and the subagent's totals. Replay reads each subagent file as a separate stream (TDD §3.4: detectors run per agent stream).

## What replay cannot recover

- **File contents.** Transcripts don't contain the file after an edit. For `Write`, replay uses the hash of the written `content` as the content hash after the write. For `Edit`, it records no content hash, so oscillation by state revisit (D3b) only sees writes; inverse-edit detection (D3a) works from the `old_string`/`new_string` pairs.
- **Exit codes** beyond the `Exit code N` text, and **durations** beyond the timestamp difference between `tool_use` and `tool_result`.
- **Pre-hook context** such as the permission mode at each call: the latest `permissionMode` seen in the stream is used.
