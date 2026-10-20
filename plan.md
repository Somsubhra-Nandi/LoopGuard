# LoopGuard: Build Plan

> A circuit breaker for AI coding agents. It detects when Claude Code / Codex is stuck in a loop, nudges it out, and halts it if needed, while tracking tokens, cost, and wasted time.

Design docs: [docs/HLD.md](docs/HLD.md) · [docs/LLD.md](docs/LLD.md) · [docs/TDD.md](docs/TDD.md)

**Time budget:** ~7 weeks part-time (~15–20 h/week) to a public launch, then iterate on user feedback.
**Stack:** Go, SQLite (`modernc.org/sqlite`), cobra, go:embed + htmx dashboard, goreleaser.
**Rule for the whole project:** the agent must never break because of LoopGuard. Fail-open, always.

---

## Milestone overview

| # | Milestone | Weeks | Ships |
|---|---|---|---|
| M0 | Setup + validate the idea on real data | 0.5 | Repo, CI, name decision, a hand-labeled sample of your own loops |
| M1 | Replay engine (offline detection) | 1.5 | `loopguard replay`: a report of loops in past sessions. **First demo-able thing** |
| M2 | Live hooks for Claude Code (observe mode) | 1 | `init`, shim, daemon, store, `status` |
| M3 | Interventions | 1 | Nudge → deny → halt, notifications, `pause` |
| M4 | Cost, budgets, dashboard | 1 | Usage from transcripts, budgets, local web UI |
| M5 | Codex adapter + packaging | 1 | Codex support, Claude Code plugin, Homebrew, install script |
| M6 | Benchmark + launch | 1 | Benchmark write-up, demo video, launch posts → **v0.3.0 public** |
| M7 | Post-launch | ongoing | Proxy mode, compaction memory, more agents, based on feedback |

Build order is **replay first**. It needs no hooks, gives you a labeled dataset to tune detectors on, and makes a great first-run experience for users.

---

## M0: Setup and validation (days 1–3)

- [ ] Check the name: GitHub org/repo, Homebrew formula, a `.dev` domain. Pick a fallback name if taken.
- [ ] `go mod init github.com/<you>/loopguard`; cobra skeleton with empty subcommands: `hook`, `daemon`, `init`, `uninstall`, `doctor`, `status`, `report`, `replay`, `pause`.
- [ ] CI (GitHub Actions): `go vet`, `staticcheck`, `go test -race`, build matrix (linux/mac/windows).
- [ ] Add `docs/` (these design docs), `LICENSE` (Apache-2.0 or MIT), `CONTRIBUTING.md`.
- [ ] **Validate with real data:** open 5–10 of your own Claude Code transcripts (`~/.claude/projects/**/*.jsonl`) where you remember the agent looping. Write down, by hand, what the loop looked like (which command, which error, how many times). This is your first ground truth.
- [ ] Capture golden hook payloads: add a temporary hook that dumps stdin to a file (`cat > /tmp/hook-$RANDOM.json`) for each event in Claude Code, and save a few of each event type into `testdata/hooks/claude-code/`.

**Done when:** CI is green, and you have ≥5 real loop examples written down plus golden payloads.

---

## M1: Replay engine (week 1–2)

Goal: `loopguard replay --since 30d` prints a correct report of loops in your past sessions.

- [ ] `internal/event`: core types (LLD §2).
- [ ] `internal/normalize`
  - [ ] text normalizer (timestamps, durations, IDs, temp paths, ANSI) with table tests
  - [ ] bash parsing + command family (`mvdan.cc/sh`) + polling-family list
  - [ ] error signature: key-line extraction, xxhash, simhash, `SameError()`; tests with real pytest / jest / go test / cargo outputs
  - [ ] action fingerprints per tool
- [ ] `internal/session`: SessionState, ring buffer, FileHistory, ProgressTracker
- [ ] `internal/detect`: D1 exact repeat, D2 repeated failure, D3 oscillation (inverse edits + state revisit). Scripted-session tests for each.
- [ ] `internal/policy`: state machine in observe mode only (records would-have actions)
- [ ] `internal/replay`
  - [ ] Claude Code transcript reader: `tool_use` → pre, `tool_result` → post, `is_error`
  - [ ] terminal report: sessions scanned, loops found, wasted minutes, top 5 loops with evidence
  - [ ] `--json` output
  - [ ] `--label` mode (simple TUI or prompt-based: y/n per detected loop; mark missed loops) → `labels.jsonl`
- [ ] `make eval`: replay the labeled transcripts → precision/recall per detector

**Done when:** replay over your own history finds the loops you wrote down in M0, precision ≥ 0.85 on your labeled set, and a full month of history runs in under 10 s.

📣 **Share early:** post a screenshot of the replay report ("my agent wasted 6 hours in loops last month") on X / r/ClaudeAI. Collect feedback and early stars before the live product exists.

---

## M2: Live hooks for Claude Code, observe mode (week 3)

- [ ] `internal/store`: SQLite + migrations (LLD §11), batched async writer
- [ ] `internal/daemon`: localhost HTTP, token auth, PID file, session manager + per-session actors, `/v1/events`, `/healthz`, `/metrics`
- [ ] `internal/adapter/claudecode`: Parse (all events in LLD §3.1), Render (none only for now), golden tests
- [ ] `internal/shim`: stdin → daemon with timeout → stdout/exit; fail-open; daemon auto-start with lock file
  - [ ] tests: daemon down / slow / garbage → exit 0, empty stdout
  - [ ] benchmark: round-trip p99 < 50 ms
- [ ] `loopguard init`: merge hooks into `~/.claude/settings.json` (backup, idempotent, absolute binary path), `--project` option; `loopguard uninstall`
- [ ] `loopguard status`: active sessions, recent signals, "would have nudged" list
- [ ] `loopguard doctor`: binary on PATH, hooks installed, daemon healthy, a test event round-trips
- [ ] **Dogfood:** run it on all your own Claude Code work for the rest of the project

**Done when:** a day of normal Claude Code use creates no visible errors or slowdown, and `status` shows correct live detections.

---

## M3: Interventions (week 4)

- [x] Policy: full state machine: nudge → deny → halt, cooldowns, reset on progress, permission-mode awareness (LLD §7)
- [x] Claude Code rendering: `additionalContext` on PostToolUse/PostToolUseFailure, `permissionDecision: "deny"` on PreToolUse, `continue:false` + `stopReason` for halt
- [x] Message templates (factual tone, LLD §7.3) with the evidence filled in
- [ ] D4 read thrash, D5 stall
- [x] Intervention outcome tracking (`recovered` / `continued_loop`) 10 calls after each intervention
- [ ] `internal/notify`: desktop, terminal sequence, ntfy, Slack/Discord webhook
- [x] `loopguard pause [--session|--repo|--all] [--for]` / `resume`; `LOOPGUARD_DISABLE=1`; `enabled: false` in `.loopguard.yaml`; `loopguard mode`, `loopguard stats`
- [x] Observe-mode-for-3-days default; `init --enforce` to skip
- [ ] **Stuck-task test repo** (TDD §6.1): 4–5 tasks that reliably make agents loop. Use it for manual and e2e testing.
- [ ] e2e test: `claude -p` on a stuck task → assert the nudge is in the transcript and a halt happens

**Done when:** on the stuck-task repo, LoopGuard nudges, denies, and halts correctly, the phone notification arrives, and in normal work there are no false halts over a week of dogfooding.

---

## M4: Cost, budgets, dashboard (week 5)

- [ ] `internal/cost`: transcript tailer (offsets, dedupe by message id), `pricing.yaml` (fill in current prices from the provider pricing pages), cost per session/day/repo
- [ ] D6 budget detector: soft → notify, hard → halt at the next tool call
- [ ] `loopguard report [session]`: timeline of a session: calls, loops, interventions, tokens, cost, wasted time
- [ ] Dashboard (go:embed + htmx, no build step):
  - [ ] overview: today / 7d / 30d: tokens, API-equivalent cost, loops caught, time saved estimate
  - [ ] sessions list → session detail timeline (calls colored by progress, loop spans highlighted)
  - [ ] settings view (read-only display of the effective config)
- [ ] `loopguard dashboard` opens the browser with a one-time login link

**Done when:** cost numbers match your provider console within ~5% for API-key sessions, and the dashboard is screenshot-worthy.

---

## M5: Codex adapter + packaging (week 6)

- [ ] Verify the current Codex hooks docs (developers.openai.com/codex/hooks) for your installed version; capture golden payloads like in M0
- [ ] `internal/adapter/codex`: parse (Bash, apply_patch → files/hunks, mcp__*), render (deny on PreToolUse; nudge/halt via PostToolUse/Stop as supported)
- [ ] Codex installer: enable the hooks feature flag in `~/.codex/config.toml`, write `~/.codex/hooks.json`, backup + uninstall
- [ ] Codex usage tailer (verify the session file format first)
- [ ] Replay support for Codex sessions
- [ ] **Packaging**
  - [ ] goreleaser: binaries for mac/linux/windows, checksums, cosign signatures
  - [ ] Homebrew tap: `brew install <you>/tap/loopguard`
  - [ ] `curl -fsSL https://.../install.sh | sh`
  - [ ] Claude Code plugin wrapping the binary (`hooks/hooks.json` using `${CLAUDE_PLUGIN_ROOT}`)
- [ ] `doctor` covers Codex

**Done when:** a fresh machine goes from zero to working (both agents) in under 2 minutes using only the README.

---

## M6: Benchmark + launch (week 7)

- [ ] **Benchmark** (TDD §6.4): run each stuck task 10× with and without LoopGuard in Claude Code auto mode and Codex. Record tokens, wall time, outcome, and how often the nudge alone led to a solution. Turn it into charts.
- [ ] **README** that sells:
  - [ ] one-line pitch + 20-second GIF of a stuck agent getting nudged and recovering
  - [ ] "Try it in 30 seconds": `brew install … && loopguard replay`
  - [ ] benchmark charts + honest limitations (budgets enforced at the next tool call, API-equivalent cost for subscriptions)
  - [ ] privacy section: 100% local
- [ ] Docs site (GitHub Pages / plain markdown): install, config reference, how detection works, FAQ
- [ ] **Launch** (spread over a week):
  - [ ] Show HN: "LoopGuard – a circuit breaker for Claude Code and Codex"
  - [ ] r/ClaudeAI, r/ChatGPTCoding, r/LocalLLaMA (if a proxy/local angle exists), relevant Discords
  - [ ] X/Twitter thread with the demo video and benchmark chart
  - [ ] Blog post: "I analyzed a month of my AI agent sessions: here's how much time loops wasted" (the replay data is the hook)
- [ ] Triage issues daily for the first 2 weeks; ship fixes fast

**Done when:** v0.3.0 is tagged, launched, and you've replied to every issue and comment.

---

## M7: After launch (pick based on feedback)

- [ ] **Compaction memory:** after compaction, inject a short summary of approaches that already failed (TDD §8 Q4)
- [ ] **Optional proxy mode** for API-key users: exact per-request hard caps, support for agents without hooks (Cursor, Aider, custom Agent SDK apps)
- [ ] More adapters: Gemini CLI, Copilot CLI, OpenCode
- [ ] Optional LLM "second opinion" before deny (off by default)
- [ ] MCP server so the agent can ask "how much have I spent / have I tried this already?"
- [ ] Team mode: shared reports, Slack bot, org budgets (possible paid tier later)

---

## Week-by-week checklist (quick view)

| Week | Focus | End-of-week proof |
|---|---|---|
| 0.5 | M0 setup + hand-labeled loops | CI green, 5+ written-down real loops |
| 1–2 | M1 replay | Replay report on your own history, precision ≥ 0.85 |
| 3 | M2 live hooks (observe) | Dogfooding with zero slowdown |
| 4 | M3 interventions | Stuck-task repo: nudge → deny → halt works, phone notification arrives |
| 5 | M4 cost + dashboard | Dashboard screenshot, cost matches the console |
| 6 | M5 Codex + packaging | 2-minute install on a fresh machine |
| 7 | M6 benchmark + launch | v0.3.0 public, posts live |

---

## Risks to watch while building

| Risk | Early warning | Response |
|---|---|---|
| False positives annoy users | You get a wrong nudge while dogfooding | Raise thresholds, add the case to the eval corpus, ship observe-mode-first |
| Hook APIs change | Golden tests fail on a new agent version | Adapter fix; pin supported versions in `doctor` |
| Nudges don't help (low recovery rate) | Recovery < 25% in your data | Iterate templates (TDD §6.3); rely more on deny + halt |
| Scope creep (proxy, dashboard polish) | M4 slipping past week 5 | Cut dashboard to overview + session list; ship |
| Competition ships loop detection | A competitor announcement | Lead with benchmark data, nudge recovery, and the replay first-run, which others don't have |

---

## Resume line (target)

> **LoopGuard**: open-source circuit breaker for AI coding agents (Go). Detects stuck loops in Claude Code and Codex via lifecycle hooks using fingerprinting and simhash-based error signatures, with graduated in-context interventions. Cut wasted tokens by **X%** on a stuck-task benchmark; **N** stars, **M** weekly active users.

Fill in X, N, and M with real numbers after launch.