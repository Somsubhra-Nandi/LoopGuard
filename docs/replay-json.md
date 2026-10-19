# `loopguard replay --json` schema

Version 1 (`schema_version: 1`). New fields can be added without a version bump. Removing or changing a field bumps the version.

```jsonc
{
  "schema_version": 1,
  "generated_at": "2026-10-19T12:00:00Z",   // UTC
  "path": "C:\\Users\\dev\\.claude\\projects", // transcripts directory
  "since": "2026-09-19T12:00:00Z",          // null with --since all
  "sessions_scanned": 23,                   // main sessions
  "streams_scanned": 24,                    // main sessions + subagent streams
  "tool_calls": 2914,                       // completed calls replayed
  "sessions_with_loops": 0,
  "total_loops": 0,
  "loops_by_detector": {"repeated_failure": 0},  // detectors with at least one loop
  "below_threshold_signals": 0,             // loops that never reached the nudge threshold (not counted above)
  "wasted_seconds": 0,                      // union of loop spans, per stream
  "wasted_tool_calls": 0,                   // union of loop call ranges, per stream
  "lines": {"lines": 17586, "malformed": 0, "ignored": 11512, "tool_uses": 2914,
            "tool_results": 2914, "unjoined_results": 0, "prompts": 246},
  "errors": [],                             // unreadable files
  "top_loops": [ /* Loop, most wasted time first; length set by --top (-1: all) */ ],
  "elapsed_seconds": 0.47
  // Cost (API-equivalent USD) arrives in M4.
}
```

`Loop`:

```jsonc
{
  "session_id": "1d3a0fe0-…",
  "agent_id": "a3b69e6a62251c21b",          // omitted for the main session
  "project": "C--Users-dev-work-shop",      // Claude Code's project directory
  "repo": "C:\\Users\\dev\\work\\shop",     // working directory at the first signal
  "detector": "repeated_failure",           // exact_repeat | repeated_failure | oscillation
  "loop_key": "rf:npx jest:94c22cd962ff5726",
  "family": "npx jest",                     // D1/D2
  "file": "src/flags.ts",                   // D3, relative to repo when inside it
  "count": 5,                               // calls that are part of the loop
  "first_call": 1,                          // calls are numbered from 1 per stream
  "last_call": 9,
  "first_signal_call": 7,
  "started_at": "…", "ended_at": "…",
  "duration_seconds": 176,
  "wasted_tool_calls": 9,                   // last_call - first_call + 1
  "max_severity": 0.95,
  "error_lines": ["…", "…"],                // up to 2, normalized and redacted
  "would_have": [{"action": "nudge", "call": 7, "reason": "…"}]   // observe mode: what enforce mode would have done
}
```

A loop is one `loop_key` within one human turn of one stream. Call numbers match `loopguard replay --session <id> --calls` and the labels file.
