# LoopGuard: Technical Design Document (TDD)

| | |
|---|---|
| Status | Draft v0.1 |
| Date | 2026-10-19 |
| Related | [HLD.md](./HLD.md) (architecture), [LLD.md](./LLD.md) (implementation detail), [plan.md](./plan.md) (execution) |

The HLD says *what* the system looks like and the LLD says *how* to build each part. This document records **why**: the key decisions, the alternatives rejected, how we'll know it works, and what could go wrong.

---

## 1. Problem statement

Autonomous coding agents (Claude Code in auto mode, Codex CLI with auto-approval) regularly get stuck in unproductive loops. The cost shows up three ways:

1. **Money:** API-key users pay per token, and a stuck overnight run can cost a lot.
2. **Quota:** subscription users burn their usage limits and get locked out for hours.
3. **Time:** the developer comes back to find nothing done, and has to read a long transcript to work out what happened.

Existing tools mostly address **spend** (budget caps in LLM gateways and agent observability tools). Few detect the **behavioral pattern** of being stuck, and fewer intervene in a way that helps the agent recover rather than just killing it.

### 1.1 Target users

| Persona | Pain | What they want from LoopGuard |
|---|---|---|
| Solo dev on a Claude/ChatGPT subscription | Wasted quota and time | "Tell me when it's stuck; unstick it if you can" |
| Solo dev / small startup on API keys | Surprise bills | Budget caps + loop kill switch |
| Developer running agents unattended (overnight, CI, background sessions) | Nobody watching | Phone notification + automatic halt |

### 1.2 Success metrics

| Metric | Target for v0.3 |
|---|---|
| Detector precision on the labeled corpus (nudge level) | ≥ 0.90 |
| Detector recall on the labeled corpus | ≥ 0.70 |
| Nudge recovery rate (`recovered` / nudges in enforce mode) | ≥ 40% |
| Hook overhead p99 | macOS/Linux: < 50 ms. Windows: within ~15 ms of the process-start floor (an empty Go program on the same machine; LLD §10) |
| Time from install to first useful output (`replay`) | < 2 min |
| Adoption after launch (first 60 days) | 300+ GitHub stars, 50+ weekly active installs (opt-in `loopguard stats --share` counts) |

**Measured hook overhead** (M2-B, Windows 11 development machine, `TestHookLatency`: 500 calls each, interleaved, against a real daemon, process start to exit):

| Run | Empty program (floor) p50 / p95 / p99 | `loopguard-hook` p50 / p95 / p99 | `loopguard hook` p50 / p95 / p99 |
|---|---|---|---|
| 1 | 28.7 / 32.5 / 37.2 ms | 37.8 / 43.6 / 49.4 ms (+12.2 ms at p99) | 73.4 / 80.1 / 90.2 ms |
| 2 | 28.9 / 39.3 / 44.9 ms | 36.9 / 49.1 / 55.8 ms (+10.9 ms at p99) | 65.1 / 77.2 / 82.4 ms |

`loopguard-hook` meets the Windows target (within ~15 ms of the floor). The floor itself moves with machine load and antivirus scanning (a quiet run measured 15 / 22 / 25 ms). macOS/Linux numbers: not measured yet; run `LOOPGUARD_BENCH_CALLS=500 go test ./cmd/loopguard-hook -run HookLatency -v` there.

---

## 2. Key decisions

### D-1. Integrate through agent hooks, not a network proxy (v1)

**Decision:** v1 uses the official hook systems of Claude Code and Codex. The proxy is optional and comes later.

| Option | Pros | Cons |
|---|---|---|
| **Hooks (chosen)** | Sees *tool calls and results*, which is exactly where loops show up. Can nudge the agent in-context. Works for **subscription users**, since there's no API key involved. Agent keeps working if LoopGuard dies. | Agent-specific adapters; hook APIs change over time. Token usage has to come from transcripts (lagged). |
| Network proxy (`*_BASE_URL`) | Exact per-request token counts and hard dollar caps. Agent-agnostic. | Sees *model traffic*, so tool results have to be reconstructed from message history. Can't easily nudge without rewriting prompts. Handling TLS and auth tokens is a security burden. Subscription auth flows may not route through a custom base URL. Proxy down = agent down. |
| Wrapping the agent process (PTY) | Agent-agnostic | Screen-scraping is fragile and can't feed context back cleanly. |

The hooks reference gives everything needed: `PreToolUse` can deny with a reason, `PostToolUse`/`PostToolUseFailure` can inject `additionalContext` next to the tool result, any hook can set `continue: false` with a `stopReason`, and `PostToolBatch` marks the end of parallel batches. Codex's hook system covers the same core events with a narrower decision set.

### D-2. Short-lived shim + long-running daemon

**Decision:** Agents call a tiny `loopguard hook` binary, which forwards to a local daemon.

- **Why not stateless hooks** (each hook invocation recomputes from SQLite)? Every tool call would pay for DB reads, the detectors need rolling state, and parallel hooks would race on the same rows.
- **Why not HTTP hooks straight to the daemon** (Claude Code supports `type: "http"`)? It's faster, but when the daemon is down, Claude Code surfaces a hook-error notice on every tool call. Silent fail-open matters more than ~5 ms. HTTP stays as an opt-in transport. Codex support for HTTP hooks is also not guaranteed.
- The shim auto-starts the daemon and fails open, so users never manage a service.

### D-3. Go

**Decision:** Go, single static binary.

- Shim cold start in Go is a few milliseconds; Python or Node would add 50–150 ms **to every tool call**, which users would feel.
- One binary for every OS makes the 2-minute install possible.
- Good concurrency primitives for the per-session actor model.
- Rust was considered: comparable speed, slower to build alone, and no meaningful benefit here.

### D-4. SQLite (pure Go driver), local only

- Zero setup, a single file, WAL mode handles one writer with concurrent dashboard reads.
- A pure-Go driver (`modernc.org/sqlite`) avoids cgo, which keeps cross-compilation and releases simple.
- Rejected: Postgres/Redis (overkill and a setup burden), flat JSONL (no queries for the dashboard).

### D-5. Deterministic detectors first, no LLM in the hot path

**Decision:** v1 detectors are rule-based, using fingerprints, simhash, and counters.

- **Latency:** an LLM call per tool call breaks the 50 ms budget.
- **Cost:** a tool that saves money must not spend it on every step.
- **Explainability:** every nudge can cite exact evidence ("failed 5 times with this error").
- **Precision:** fingerprints are predictable and testable in CI.

**Later option (v0.4+):** an *optional* "second opinion" that only runs when a rule-based detector reaches the deny stage. A small or local model reads the evidence and confirms or vetoes the deny, to cut false positives. It's off by default.

### D-6. Graduated intervention: nudge → deny → halt

Killing the agent on the first suspicion throws away good work and annoys users. Agents also often recover when told plainly what's happening, because they can't see their own repetition, especially after context compaction. So:

1. **Nudge**: factual observation injected next to the tool result.
2. **Deny**: block only the *specific* repeated action, with a reason.
3. **Halt**: end the turn and notify the human.

Each stage has to persist through more occurrences before escalating, and real progress resets it. The nudge recovery rate is measured (LLD §11 `interventions.outcome`) so the thresholds get tuned on data.

### D-7. Factual nudge wording

The Claude Code hooks documentation says to phrase `additionalContext` as factual statements: text framed as out-of-band system commands can trigger Claude's prompt-injection defenses, and Claude may then surface the text to the user instead of acting on it. So templates describe what happened and list options. They never say "SYSTEM: you must…" (templates in LLD §7.3). This gets A/B-tested in the eval phase (§6.3).

### D-8. Observe-only by default for the first days

New users get `mode: observe` for 3 days: detections are recorded and shown in `status`/dashboard as "would have nudged", but nothing is injected. Trust comes first, and the user's own data shows the value. `loopguard init --enforce` skips this.

### D-9. Cost from transcripts; label it honestly

Without a proxy, token usage comes from the agent's local transcript files, deduplicated by message ID. Consequences:
- Budgets are enforced at the **next tool call** after crossing, not mid-request. The docs say so.
- For subscription users, cost is labeled **"API-equivalent"**, since they don't actually pay per token. Tokens and time are shown alongside.
- Prices live in a user-editable `pricing.yaml`, never hard-coded, because model prices change.

---

## 3. Detection design rationale

### 3.1 What a "loop" means here

A loop is **repeated effort without a change in outcome**. Repeated *actions* alone aren't enough (running tests after each fix is normal). The outcome signal has to stay flat too. That's why the headline detector (D2) keys on **same command family + same error signature**, and deliberately ignores the edits made in between.

### 3.2 Why fingerprints and simhash

- Outputs differ trivially between runs (timestamps, durations, temp dirs, PIDs). Normalization removes these.
- After normalization, the same error can still differ slightly (a line number shifts after an edit). Simhash with a small Hamming threshold catches near-duplicates; key-line extraction keeps the signature focused on the error, not on test-runner chatter.
- It's cheap: O(output size) per call, with no models involved.

### 3.3 Why per-file content history for oscillation

Comparing diffs is fiddly; comparing **content hashes after each write** is exact and simple. Revisiting an earlier state (A → B → A) is the definition of flip-flopping. Inverse edit pairs (`old=A,new=B` then `old=B,new=A`) catch the common case directly from the Edit tool input, without reading the disk.

### 3.4 Known false-positive sources and mitigations

| Legitimate behavior | Mitigation |
|---|---|
| Polling CI or a server (`gh run watch`, `curl /health`, `sleep 30`) | Polling-family allowlist; D1/D2 skip them |
| Flaky test that passes on retry | D2 requires *no success* in between; a single pass resets the loop |
| TDD cycle: run tests → fix → run tests (failing differently each time) | Error signature changes → progress 0.6 → resets |
| Large refactor reading many files repeatedly | D4 requires low progress and never goes past nudge |
| Long builds repeated after dependency changes | Family success resets; `progress` from new files touched |
| User deliberately asked for retries ("keep trying until it passes") | Per-repo or per-session `loopguard pause`; the stall detector is lenient; v0.4: detect this intent in `UserPromptSubmit` and relax thresholds for the turn |
| Subagents with independent work | State keyed by `agent_id` within the session; detectors run per agent stream |

---

## 4. Failure modes and resilience

| Failure | Behavior |
|---|---|
| Daemon not running | Shim exits 0 silently and starts the daemon in the background; only that event is lost |
| Daemon slow (GC pause, disk) | Shim timeout (≤ 800 ms) → no decision; the agent continues |
| Daemon panics | Actor-level recover → decision `none`, state rebuilt from DB |
| Corrupt or locked DB | In-memory mode; `doctor` reports it |
| Agent updates its hook schema | Adapter parse error → `none` + logged; golden-file tests per agent version catch it in CI; `doctor` warns about unknown versions |
| Transcript format changes | Unknown lines are skipped; usage shows "unknown" rather than wrong numbers |
| Wrong detection in enforce mode | Deny/halt only after a nudge plus persistence; `loopguard pause`; `LOOPGUARD_DISABLE=1`; each halt message says how to pause |
| Many parallel sessions | One actor per session; bounded memory (ring buffers); idle eviction |

**Invariant:** no LoopGuard failure may block or slow an agent beyond the shim timeout.

---

## 5. Security and privacy

- **Local-only data.** No telemetry. Anonymous stats sharing is explicit opt-in (`loopguard stats --share`) and sends aggregate counts only.
- **Minimal storage.** Raw hook payloads stay in memory only. Persisted: hashes, families, redacted error key lines, and truncated input digests. `store_outputs: none` stores fingerprints only.
- **Secret redaction** happens before hashing and storing (LLD §13).
- **Localhost API** with a bearer token in a 0600 file; the dashboard uses a one-time login link. No CORS. Host header checked against DNS rebinding.
- **Config edits** to agent settings are backed up (`settings.json.loopguard.bak`), idempotent, and reversible (`uninstall`).
- **No new capabilities granted to agents.** LoopGuard can only deny or stop; it never approves (`permissionDecision: "allow"` is never emitted), so it can't weaken the agent's permission system.
- **Supply chain.** Reproducible builds, checksums + signatures (cosign) on releases, minimal dependencies.

---

## 6. Evaluation methodology

### 6.1 Corpus

1. **Own sessions:** the author's real Claude Code and Codex transcripts (a few months is plenty), replayed with `loopguard replay --label`.
2. **Synthetic stuck tasks:** a small repo of deliberately hard or impossible tasks that reliably induce loops:
   - a test asserting a value that comes from a read-only fixture,
   - a dependency version conflict that can't be resolved,
   - a linter rule that contradicts a type checker rule (forces oscillation),
   - a flaky network test (a false-positive check).
3. **Community contributions (post-launch):** users can submit anonymized labeled loops (`loopguard replay --export-anonymized`).

### 6.2 Offline metrics (CI-gated)

- Per detector: precision, recall, and mean **detection delay** (calls between loop start and first signal).
- Corpus-level: the share of wasted time that would have been saved at each threshold setting.
- A detector or threshold change merges only if precision doesn't drop below target.

### 6.3 Online metrics (local, from the user's own data)

- Nudge recovery rate, per detector and per nudge template.
- Deny → recovery vs deny → halt.
- Time saved estimate: halted loops × the median time similar loops ran in observe mode.
- Template experiments: rotate between 2 nudge phrasings per detector locally and compare recovery rates (results are only shared if the user opts in).

### 6.4 Benchmark write-up for the README

Run the synthetic stuck tasks 10× each with and without LoopGuard, in Claude Code auto mode and in Codex. Report: tokens used, wall time, outcome (solved / gave up / halted), and how often the nudge alone led to a solution. This is the headline demo and the evidence for "saves X% of wasted tokens".

---

## 7. Alternatives considered (product level)

| Alternative | Why not (now) |
|---|---|
| Build a general LLM gateway with budgets | Crowded market (LiteLLM, Portkey, Bifrost, Helicone); can't see tool-level behavior |
| Agent observability dashboard only | Exists already (e.g. TokenJam) and is passive; the differentiator is **detection + intervention** |
| Claude Code plugin only | Good distribution channel, but it ties us to one agent; the plugin becomes a packaging option around the same core binary |
| Ask the agent to self-monitor via CLAUDE.md instructions | The agent can't reliably see its own repetition, especially after compaction; external counters are ground truth |

---

## 8. Open questions

1. **Codex hook API stability:** event names, the decision fields allowed per event, and the feature-flag key have changed between releases. Need a version matrix and golden payloads before v0.3.
2. **Codex usage data:** confirm where token counts appear in Codex session files and how reliable they are.
3. **Subagent attribution:** should a loop inside a subagent halt only the subagent (deny its next calls), or the parent turn? Proposal: deny inside the subagent; halt the parent only on the budget detector.
4. **Context compaction:** after `PreCompact`/`PostCompact`, the agent forgets earlier failed attempts and loops resume. Should LoopGuard proactively inject a summary of failed approaches after compaction (`SessionStart` with `source: compact` supports `additionalContext`)? This could be a strong feature; test it in eval.
5. **Naming and trademark:** confirm that "LoopGuard" is free to use, or pick another name.
6. **Monetization (later, optional):** a free local core, with a possible paid team edition (shared dashboards, org-wide budgets, Slack bot). Out of scope until there's adoption.

---

## 9. Glossary

| Term | Meaning |
|---|---|
| Action fingerprint | Hash of a normalized tool call (tool + canonical input) |
| Command family | Normalized "what kind of command" key, e.g. `npm test` |
| Error signature | Exact hash + simhash of the key error lines of a failed output |
| LoopKey | Stable identifier for one detected loop instance, used for escalation state |
| Nudge | Factual context injected into the agent after a tool result |
| Deny | Blocking a specific repeated tool call before it runs |
| Halt | Ending the agent's turn (`continue: false`) and notifying the human |
| Observe mode | Detection runs and records "would-have" interventions without acting |