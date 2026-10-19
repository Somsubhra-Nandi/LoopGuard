# Capturing golden hook payloads

The Claude Code adapter (milestone M2) is tested against real hook payloads stored in `testdata/hooks/claude-code/`. This page explains how to collect them with a temporary capture hook. It doesn't need the `loopguard` binary.

## 1. Install the capture hook (temporarily)

1. Requirements: `sh` (on Windows, Git Bash's `sh` must be on the PATH Claude Code uses). `jq` is optional; with it, payloads get pretty-printed.
2. Open a scratch project you don't mind experimenting in, and create or edit `.claude/settings.local.json` there. Project-local settings keep the capture out of your other sessions. To capture everywhere, use `~/.claude/settings.json` instead, and **back it up first**.
3. Copy the `hooks` block from [`scripts/capture-settings.example.json`](../scripts/capture-settings.example.json) into that file. If the file already has a `hooks` key, merge the events into it.
4. Replace every `/ABSOLUTE/PATH/TO/loopguard` with the absolute path of your checkout (on Windows, a forward-slash path such as `C:/Users/you/src/loopguard` works).
5. Start Claude Code in that project. Run `/hooks` to confirm that the eight events are registered.

Every event now writes one file to `~/.loopguard-capture/<hook_event_name>-<timestamp>-<random>.json`.

## 2. Use Claude Code for about 15 minutes

Try to trigger every event and the common tool shapes:

- Start a new session, then `--resume` one and run `/compact` (`SessionStart` with different `source` values).
- Send a few prompts (`UserPromptSubmit`).
- Ask for work that uses `Bash`, `Read`, `Edit`, `Write`, `Grep`, and `Glob`, plus an MCP tool if you have one (`PreToolUse` / `PostToolUse`).
- Make a command fail, e.g. a test that can't pass (`PostToolUseFailure`).
- Ask for something that triggers several tool calls in parallel (`PostToolBatch`).
- Let a turn finish (`Stop`), then exit with `/exit` (`SessionEnd`).

## 3. Anonymize and copy samples into the repo

1. Run `claude --version` and create `testdata/hooks/claude-code/<version>/` (e.g. `testdata/hooks/claude-code/2.3.1/`).
2. Pick 2–3 files per event type, covering different tools and both success and failure.
3. Anonymize each file before copying it into the repo:
   - usernames and home directories in `cwd`, `transcript_path`, and file paths → `/home/user/project/...`
   - `session_id`, `tool_use_id`, and `prompt_id` → stable fake values (keep the format, and keep IDs consistent within one session so pre/post pairs still join)
   - prompt text, file contents, and command output that is private → neutral replacements of a similar shape
   - **any secrets** (API keys, tokens, `.env` values) → removed
4. Keep the field structure exactly as captured. Don't drop unknown fields: the golden tests exist to catch schema changes.
5. Name the files `<event>-<short-description>.json`, e.g. `PostToolUseFailure-bash-pytest.json`.

## 4. Remove the capture hook

Delete the `hooks` entries you added (or restore your backup), then delete `~/.loopguard-capture/`. It's in `.gitignore`, but it contains raw, un-anonymized data.
