# chronos-code Harness Spec v2 — autonomous, futility-governed, language-agnostic

_Status: proposal, 2026-10-15. This document is authoritative. Where `tools.md`,
`prompts/*.md` or `README.md` conflict with it, this spec wins._

## 0. Design principles

1. **Long is fine; futile is not.** The harness never stops work because it is *long*: no
   wall-clock, iteration, tool-call or token caps on the main agent, on subagents, or on shell
   processes. It intervenes only when work is **stuck** (no liveness) or **futile** (no progress).
   The only hard ceiling is an explicit user-set cost limit (`max_cost_microdollars`).
2. **Never block the loop on one thing.** No single tool call, model stream or subagent can freeze
   the agent. Control always comes back to the model, and the model decides what to do next.
3. **Nothing is ever lost.** Every result (tool, subagent, compaction) is checkpointed before it is
   summarised. Interrupted work is resumable.
4. **Language-agnostic by construction.** Every capability (outline, search, diagnostics, tests,
   ignore rules, environment detection) is driven by a per-language registry, never by
   `if ext == ".go"`. Tier-1 languages must have parity.
5. **Cheapest per resolved task.** Cost is a first-class objective: append-only, byte-stable prompts (no in-place cache rewrites), a small working set, and cheap models for side work. See [COST.md](COST.md).
6. **Thin, high-signal context.** A stable cached prefix, tools loaded on demand, retrieval at the
   moment it is needed, and a per-turn tail rebuilt from state rather than accumulated.
7. **Measured, not asserted.** Every change ships behind a flag and is evaluated on a multi-language
   benchmark (see §7).

## 1. Execution governance: liveness and futility, never duration

The existing renewable-window governor (`internal/orchestrator/long_running.go`) is the right
foundation. It already renews windows and pauses only after `no_progress_windows` idle windows.
v2 makes it the **only** stop mechanism, sharpens its progress signal, and turns "stop" into an
escalation ladder.

### 1.1 Remove every duration or count cap
| Current | v2 |
|---|---|
| `boundedSubagentTimeout = 30m` (`long_running.go:29`) | remove; subagents use the governor |
| `subagent_timeout_sec` | remove the key (or keep it as an opt-in for CI only, default 0) |
| `defaultMaxToolIterations = 90` (`chronos/sdk/agent/agent.go:102`) | remove for interactive runs; the governor decides |
| `maxTurnsController` | only when the user passes `--max-turns` explicitly |
| `security.yaml` `shell.max_execution_time_sec: 300` (kills the process) | becomes the **yield point**: control returns, the process keeps running in the background (§2.2) |
| proposed `task.timeout_s` / `max_tool_calls` | **dropped** from the spec |
| repair caps (`repair.*`) | renew mode only (already the default); verification gate fixed in §1.5 |

### 1.2 Liveness: detect *stuck*, not *slow*
Liveness is measured separately for each long-lived activity. A failed check never kills work
silently; it either recovers automatically or surfaces the problem to the model.

| Activity | Liveness signal | On loss of liveness |
|---|---|---|
| Model stream | bytes received | after 120 s of silence: abort *that request* and retry it (idempotent; the conversation is unchanged). After 3 failed retries: fall back to another provider/model, else surface the error to the model. Today the 5-min idle timeout (`engine/model/stream_idle.go:15`) plus a hung TCP read produced a 71-min stall. |
| Shell process | output bytes, CPU time and child-process activity (`ps`/`proc` sampling) | Never killed by the harness. When it passes `block_until_ms` it is backgrounded. If there is no output and no CPU for 10 min, the next `await_shell` reports `idle: no output, 0% CPU for 10m (possible hang or waiting on stdin)`, and the model decides whether to wait, kill or move on. |
| Tool handler (Go) | handler returns | each handler runs in a goroutine under a liveness supervisor. A handler that is still running at its yield point is returned to the model as `{status:"running", handle}`; a supervisor trace always closes the span (the 9 unclosed spans in sess_d706051b). |
| Subagent | child heartbeat: one model round or tool round completed | runs in the background, non-blocking (§4). The parent sees its status in the turn context and can keep working, `await_task`, `message_task` or `cancel_task`. The child is governed by its own futility detector, not by a clock. |
| Session | assistant message persisted before tools execute | a crash or hang can always be resumed from the last persisted round. |

### 1.3 Progress: a richer signal
A window counts as **progress** if any of these hold. Items marked ✓ already exist; the rest are
new.
- ✓ new file content (write hash not seen before)
- ✓ new verification outcome (command × pass/fail × exit code not seen before)
- ✓ mostly new tool-call fingerprints (≥25%)
- **information gain**: new files or ranges read, new symbols resolved, new search hits, measured
  against a per-session *knowledge set*. Re-reading the same range with an unchanged hash counts
  as zero.
- **error-signature change**: the normalised compiler/test error set changed (fewer, or
  different). The same failure set three times in a row counts as zero.
- **plan movement**: a todo item moved to `completed` with evidence attached.
- **external progress**: a background shell or subagent reported new output or finished.

### 1.4 Futility detectors (run at every round boundary, cheap)
| Detector | Trigger | Real case it catches |
|---|---|---|
| Repetition | same call fingerprint ≥3× with no state change in between | `data/actusflow-migration.log` read 14×; `git status` 5× |
| Oscillation | a file's content hash cycles A→B→A | edit/undo loops |
| Stuck failure | same normalised error signature after ≥3 fix attempts | re-running pre-existing failing tests |
| Redundant verification | same check re-run with an unchanged tree hash | `cargo test` ×4 |
| Re-exploration | ≥60% of reads in a window are already in the knowledge set | child re-reading 36% of the parent's files |
| Denied-path loop | the same permission or sandbox denial ≥2× | `/tmp` writes, `~/.cursor` reads |
| Empty spin | rounds with no tool calls and no final answer | `max_tokens` loops returning `""` |

### 1.5 Escalation ladder (replaces "stop")
When a detector fires or a window shows no progress, the harness climbs one rung at a time and
resets to rung 0 as soon as progress resumes.
1. **Nudge (rung 1).** Inject a one-line, specific steer into the uncached tail. Example:
   `Futility: you have read migration.log 6× with no change; use await_shell sh-3 pattern:"DONE|ERROR"`.
   Duplicate tool calls are answered from cache with `unchanged since round N` instead of being
   re-executed.
2. **Reflect (rung 2).** Force one no-tool reasoning turn: "State the hypothesis, what was tried,
   why it failed, and a different approach." Record the answer in the progress file (§3.4).
3. **Re-strategise (rung 3).** Compact into a fresh context carrying the structured state (§3.3),
   or delegate the stuck sub-problem to a `debug` or `plan` task with a clean context. The new
   context is briefed on what failed.
4. **Ask (rung 4).** Only now pause for the user, with a precise question and the evidence. The
   work is kept and "continue" resumes it. In unattended mode, rung 4 parks the task as `blocked`
   and moves on to other ready tasks.

### 1.6 Verification gate
- Fires only if the turn mutated something: file writes, or shell commands classified as mutating.
- Accepts any passing check run after the last mutation of the affected files. Never re-runs a
  command against an identical tree hash.
- `git diff --stat` stops being mandatory evidence.
- Repair output is an addendum. The user never sees two full final answers.

## 2. Tools

The core set from `tools.md` stands, with the changes below. Everything else is loaded on demand
via `tool_search`.

### 2.1 Performance requirements (measured on quant-trade)
| Requirement | Target |
|---|---|
| grep, root of a 120k-file repo | ≤50 ms p50, results complete (ripgrep backend, gitignore plus ecosystem ignores) |
| glob `**` | correct, ≤20 ms; 0% false-empty (today 41% of calls return null) |
| read | ≤5 ms; batch of up to 10 paths in one call |
| harness overhead per round | ≤15 ms (WAL `sessions.db`, batched span and event writes, shared token cache) |
| compression refetch rounds | 0 for results ≤10k tokens (today 290 rounds, 3,747 s) |
| parallelism | per-call scheduling; read-only shell commands classified as parallel-safe |

### 2.2 Shell: no kill, no cap
`shell{command, description, working_dir?, block_until_ms?}`
- **Persistent session**: `cd` and exported env vars persist across calls.
- **Yield, don't kill.** `block_until_ms` (default 30 s, maximum = policy yield) only decides when
  control returns. An overrunning process keeps running as `sh-N`, and output streams to
  `.chronos-code/terminals/sh-N.log` with a header (pid, cwd, command) and a footer (exit_code,
  elapsed_ms).
- `await_shell{shell_id, pattern?, timeout_ms?}` returns on exit, on a pattern match, or when the
  await window ends. The await window only bounds *that call*; the process keeps running. It
  returns only the output delta, plus liveness (`cpu%`, `idle_for`).
- `kill_shell{shell_id}` kills the whole process group.
- Running shells are listed in the turn tail (`Background: sh-2 'pytest -x' running 14m, last
  output 3s ago`).
- At session end, the user chooses which background processes to keep (servers) and the rest are
  reaped. The harness never times them out.

### 2.3 Task (subagents)
`task{agent, description, prompt, background?: true}` with **no `timeout_s` and no
`max_tool_calls`**. Governance is entirely §1, applied to the child with its own knowledge set,
which is seeded from the handoff (§4).
Companion tools: `await_task{id, timeout_ms?}`, `message_task{id, text}`, `cancel_task{id}`.

## 3. Context engineering

> Cost design, measurements and targets: **[COST.md](COST.md)**. It found that 57% of today's spend ($238 of $419) is cache writes, and 75% of those are full-prompt rewrites caused by re-trimming every round above about 150k tokens.

### 3.1 Layout (cache-optimal)
```
[tools: core 9]                      cache bp1 (1h)   — deferred tools via tool_search
[system: identity + rules]           cache bp1 (1h)   — ~860 words, no tool lists
[project instructions + skill catalog] cache bp2      — AGENTS.md/CLAUDE.md/.cursorrules…
[conversation]                       cache bp3/bp4    — rolling tail breakpoints (existing)
[uncached tail, rebuilt every call]  — env delta, todo, background shells/tasks, futility steer
```
The `<turn_context>` is **rebuilt from state** on each turn and never stored in recallable user
text, which fixes the depth-14 recursion. The complexity/routing hint is used for routing and
telemetry only and is never shown to the model.

### 3.2 Just-in-time retrieval over prefetch
Repository context is fetched by tools (`code_search`, `grep`), not prepended. Recalled
cross-session memories are injected only with provenance and age, and are dropped when the
current workspace contradicts them (git presence, framework, file existence).

### 3.3 Compaction = structured state + clearing
- **Tool-result clearing first**: replace old tool results with handles
  (`[read research/recommend.py:120-180 @hash, round 14]`). Results are re-fetchable but cost no
  context. This alone delays most compactions.
- When a summary is needed it uses a template: goal and acceptance criteria · decisions and
  rationale · files changed (+why) · knowledge set (files/symbols already understood) · checks
  and last results · failed approaches (from rung-2 reflections) · open todos · background
  shells/tasks · next step.
- The summary budget scales with the window (≥4k, roughly 3% of the window). On `max_tokens` the
  harness continues the summary instead of truncating it.
- Threshold: clear tool results at about 60% of the window, down to about 35% (hysteresis); summarise at about 80%. Use the served window, not a 128k default. The context guard is a safety net only (COST.md §3.1).
- Subagent results and user instructions are **pinned verbatim** across compactions.

### 3.4 Long-horizon memory (multi-hour and multi-session)
Following Anthropic's long-running-agent harness pattern:
- `.chronos-code/progress.md` holds the task goal, a feature/acceptance checklist, the progress
  log and failed approaches. The agent updates it at milestones, and a resumed or compacted
  context reads it first.
- Commit or checkpoint at each green milestone. Edit checkpoints already exist; add an optional
  `git commit` on a scratch branch, or `git stash`-style snapshots.
- A resume reconstructs state from the progress file, checkpoints and the event log, never from a
  lossy summary alone.

## 4. Subagents that converge

| Problem (audit) | v2 mechanism |
|---|---|
| no handoff → 36% duplicate reads | `prompt` brief plus a harness-attached **handoff packet**: knowledge set (paths, ranges, hashes), relevant snippets, failed approaches. The child's read cache is seeded, and re-reads return `unchanged since parent read`. |
| parent blocked for 31,000 s | background by default; parent continues; status in the tail; `await_task` |
| results lost or compressed | structured JSON return (`status, summary, findings[path:line], changed_files, checks[cmd,exit], unresolved, next_action`) validated; written to `.chronos-code/tasks/<id>.json` incrementally; **never compressed**; pinned across compaction |
| empty `""` result | not a success: the harness asks the child to finalise; if it still fails, returns `status:"failed"` with the last partial findings |
| ceremonial review | reviewer only on request or for risky shared-contract diffs; "clean" is a valid result; reviewing your own fresh diff is forbidden |
| serial spawns | independent tasks are launched in one batch and run concurrently (per-call scheduling) |
| futile children | the child runs the same futility ladder; at rung 3/4 it returns `partial` or `blocked` with findings rather than spinning |
| over-spawning | prompt rule: spawn only for independent work of roughly 10+ calls whose raw output you won't reuse, or for parallel fan-out |

## 5. Language-agnostic support

### 5.1 Tier-1 languages (full parity required)
Python, TypeScript/JavaScript (incl. TSX/JSX), Go, Rust, Java, Kotlin, C, C++, C#, Ruby, PHP,
Swift. Tier-2 (best-effort): Scala, Dart, Bash, Lua, Elixir, Zig, Objective-C, SQL, HCL/YAML/JSON.

### 5.2 Capability matrix: what must be registry-driven
| Capability | Today | v2 |
|---|---|---|
| Read outline for large files | **Go only** (`incctx.go:148` uses `go/parser`) | tree-sitter outline from the indexer's 18 packs (`indexer/extract/packs/*`) for every language; `go/parser` stays as the Go fast path |
| Code index / search | Go plus 17 tree-sitter languages, BM25; type coverage weak for Py/Go/Rust (0.27–0.45) | keep; add SCIP auto-run per language when the indexer is installed (scip-python, scip-typescript, rust-analyzer scip, scip-java, scip-go, scip-clang); hybrid BM25 + embeddings with RRF |
| Diagnostics after edit | LSP for go/ts/py/rust only, behind the `lsp` build tag | on by default. Server registry, auto-detected on PATH: gopls, basedpyright/pyright, typescript-language-server, rust-analyzer, jdtls, kotlin-language-server, clangd, csharp-ls/OmniSharp, ruby-lsp, intelephense, sourcekit-lsp, metals, dart, bash-language-server, lua-language-server. **Fallback** to the project's own fast checker when no LSP is present (below). |
| Fast checker fallback | none | `go vet`/`go build`, `ruff`+`mypy`/`pyright`, `tsc --noEmit`/`eslint`, `cargo check`, `gradle compileJava`/`mvn -q compile`, `kotlinc`/gradle, `cmake --build`/`make -n` + compile_commands, `dotnet build`, `ruby -c`/rubocop, `php -l`, `swift build` |
| Test discovery and mapping | generic | ecosystem detectors: go test, pytest/unittest, jest/vitest/mocha/node --test, cargo test, JUnit via gradle/maven, kotest, ctest/gtest, dotnet test, rspec/minitest, phpunit/pest, XCTest/swift test. Changed file → nearest tests via indexer `test_map`. **Targeted first, full suite only for shared contracts.** |
| Ignore rules (grep/glob/index) | partial skip list; misses `target/`, `.next/`, `out/` | `.gitignore` + `.ignore` + an ecosystem defaults table: node_modules, .next, dist, build, out, coverage; .venv, venv, __pycache__, .mypy_cache, .pytest_cache, .tox; target; vendor (Go/PHP, configurable); bin/obj; .gradle, .idea; Pods, DerivedData, .build; .dart_tool; _build, deps |
| Environment banner | langs + date + OS | languages, package managers and lockfiles (npm/pnpm/yarn/bun, pip/uv/poetry, cargo, go mod, maven/gradle, nuget, bundler, composer, swiftpm), toolchain versions, userland (BSD/GNU), available CLIs (rg, gtimeout, gsed, docker, kubectl), git branch and dirty count |
| Edit validation | exact-match replace | same, plus a per-language syntax check (tree-sitter parse error count must not increase) before the edit is accepted, with a closest-match hint on mismatch |
| Monorepos | — | the nearest manifest decides the toolchain per path; multi-language repos (quant-trade = Python + Rust + TypeScript) are first-class |

### 5.3 Prompt implications
The system prompt names **no language**. Language-specific guidance comes from the environment
banner and from optional language skills (`skills/lang-python`, `lang-rust`, …), which load only
when that language is detected and relevant to the task.

## 6. Prompt changes (delta vs `prompts/`)
- Remove from the subagent preamble: "tool-call budget {max_tool_calls}", "time limit
  {timeout_s}s", and "80% of your budget". Replace with: *"There is no time or call limit. Work
  until DONE WHEN is met. If you are not learning anything new (re-reading, repeating the same
  failing fix, waiting without a plan), stop and return `partial` or `blocked` with what you
  established. The harness will also tell you when it detects this."*
- Main prompt adds: *"Long tasks are expected; keep going while you are making progress. Progress
  means new information, new passing checks, or completed plan items. If you notice you are
  repeating yourself, change approach before trying again."*
- Keep: action bias, batching, read-once, background + await, proportional verification, a
  concise outcome-first final answer, and asking only for genuinely blocking decisions.

## 7. Evaluation (must gate every change)
- **Multi-language suites:** SWE-bench Verified (Python), SWE-bench Multilingual and Multi-SWE-bench
  (Java, TS/JS, Go, Rust, C/C++), Terminal-Bench (shell and long-horizon), plus a private replay
  set from the real sessions (quant-trade: Python/Rust/TS; chronos: Go).
- **Long-horizon suite:** multi-hour tasks (migration, feature build-out) that score completion,
  wall time, and *futile time*: time in windows the detector marks non-progress, which must trend
  to ≈0.
- **Metrics per run:** resolved %, tool calls, model rounds, tokens (uncached, cache-read,
  cache-write, output), **cost per resolved task**, cache-break count, wall time, futile-time %, stall count (liveness losses not recovered), duplicate
  read %, verification-repair rate, subagent productive %.
- **A/B by flag** against today's harness; each roadmap item ships only if resolved % does not drop
  and at least one efficiency metric improves.

## 8. Implementation order
0. Cost telemetry + cache-break detector, then the compaction-threshold/clearing fix (COST.md §5). This is the largest single saving.
1. Liveness supervisor (stream retry, tool goroutine supervisor, span always closes, persist
   before execute). Then remove the subagent/iteration caps.
2. Background shell + `await_shell`/`kill_shell`; the shell timeout becomes a yield point.
3. Futility detectors + escalation ladder (extend `windowGovernor`); duplicate-call cache.
4. Verification gate fix; `<turn_context>` rebuild; hide the routing hint.
5. ripgrep grep, `**` glob, ecosystem ignore table, multi-file read, no compression ≤10k tokens.
6. Background subagents with handoff packet, structured return, pinned results.
7. Language registry: tree-sitter outlines, LSP plus checker fallback, test detectors, banner.
8. Compaction v2 (clearing → structured summary → progress.md).
9. Prompts v2; deferred tools via `tool_search`.
10. Perf: WAL, batched persistence, shared token cache, per-call parallel scheduling.
11. Eval harness (§7) — built in parallel with step 1 so that every later step is measured.
