> See [HARNESS-SPEC.md](HARNESS-SPEC.md) — authoritative v2 spec (no runtime caps; futility governance; all languages).

Current implementation and Cursor comparison: [Runtime audit — 2026-10-16](CURSOR-RUNTIME-AUDIT-2026-10-16.md).
The spec and tool designs below are proposals; the audit distinguishes them from shipped source and historical run behavior.

# chronos-code harness: performance, subagents, tools & prompts (2026-10-15)

Companion to [cursor-comparison.md](cursor-comparison.md).
Data: all chronos-code sessions on quant-trade (`~/.chronos-code/projects/quant-trade-716662…/`,
82 telemetry sessions, 14,094 trace spans, 5,955 model rounds, 66 subagent spawns).

Files here:
- `tools.md` — proposed tool suite with exact JSON schemas + required harness changes
- `prompts/chronos-code.md` — revised main prompt (~860 words, down from 1,432) + change table
- `prompts/subagents.md` — shared subagent preamble, 6 roles, handoff-brief template
- `critique.md` — line-level critique of current prompts and tool descriptions

## 1. Subagents: is time spent on futile work? — Partly confirmed

| Metric | Value |
|---|---|
| Spawns analysed | 65 runs / 11 sessions |
| Productive / partial / futile | ~39 / ~17 / 9 (14%) |
| Real work in subagents | 12,408 s = 29% of active wall time in sessions that spawn |
| Wall time incl. stalls | 43,117 s — **31,000 s blocked on hung children** |
| Duration median / p90 | 122 s / 471 s |
| Tool calls median / p90 | 14 / 45 (1,448 total) |
| Child reads of files the parent already read | 36% of child `file_read` |
| Results missing the required STATUS block | 59% |
| Reviewer/tester ceremony | 26 runs; ≥12 clean passes followed by more unreviewed edits |
| Parallel spawns | 1 of 61 messages; even that ran serially |

Worst runs: orphaned child after compaction (Oct 15, 567 s work, never returned, 3.4 h open);
reviewer silent 2 h 22 min (Oct 10); reviewer hung on a stalled model stream 71 min (Sep 27);
debugger audit lost to compaction and redone (Oct 13, +262 s, 1.23 M cache tokens); coder hit
`max_tokens` and returned `""`, redone (+341 s).

Root causes:
1. **No liveness detection** (a time cap is NOT the fix — see HARNESS-SPEC §1) — `config.yaml` `mode: renew` + `subagent_timeout_sec: 0` → `subagents.go:229`
   uses `WithCancel` (no deadline). Window governor counts rounds; a hung stream/shell never ends one.
2. **No budget** — inherits 90 iterations / renewable 60-round windows; not scaled to task.
3. **No context handoff** — child gets a fresh agent + task string only (`harness/subagent.go:156`).
4. **Fragile return** — only text after the last tool call kept (`subagents.go:408`); empty result
   not an error; compressor shrinks 5–27 KB results to ~700 B previews; compaction keeps the preview.
5. **Serialised** — all-or-nothing `ParallelSafe` (`agent.go:1149`); cleared when hooks exist
   (`orchestrator.go:697`).
6. **Prompts invite spawning** — `chronos-code.yaml:39` ("tester gives independent execution
   evidence"), PPD directives `orchestrator.go:2631-2653`.

## 2. Tool-call performance

Harness per-call overhead is small (tool-end → next model call p50 37 ms, ~0.1% of wall time).
The real cost is **tools returning wrong/incomplete results, forcing extra model rounds (~5 s each)**.

| Tool | n | p50 | p90 | Problem |
|---|---|---|---|---|
| shell | 3,687 | 840 ms | 17.6 s | p99 240 s; hung calls; 1,103 s serialised |
| spawn_subagent | 63 | 126 s | 535 s | see §1 |
| file_grep | 256 | 4 ms | 98 ms | walks `backend/target` (6.9 GB), `.next`, `out`; hits 32 MiB cap → incomplete |
| file_glob | 96 | 2.7 ms | 7.7 ms | **41% return null** — `filepath.Glob` has no `**` |
| file_read | 1,337 | 4 ms | 15 ms | fine; 168 compressed → refetch |
| read_stored_result | 358 | 2 ms | 8 ms | 290 refetch-only rounds = **3,747 s model time (6%)** |

grep vs ripgrep on quant-trade: 130–811 ms vs 18–49 ms, and incctx returned wrong matches
(e.g. `backend` "overnight": 12 matches all in `target/` vs rg's 6 real ones). `rg` is installed
but unused.

Speedups (priority): (1) ripgrep backend + gitignore-aware fallback; (2) `**` glob;
(3) don't compress results <16 KB or ranged reads; (4) `sessions.db` WAL + `synchronous=NORMAL`
(span write 2.8 → 0.3 ms), single end-of-span write; (5) batch trace/event persistence
(`scale.go` already has `InsertTraces`/`AppendEvents`); (6) shared token cache incl. SDK counter
(`session.go:686`); (7) per-call parallel scheduling + read-only shell classification;
(8) batch edit-checkpoint fsyncs.

## 3. Tools & prompts (research-backed)

Key evidence: Anthropic tool-writing / advanced-tool-use (deferred loading −85% definition tokens,
accuracy 49→74%); SWE-agent ACI ablations (linting edits +3 pts, capped summarised search +6 pts);
Codex prompting guide (batch all reads, background exec, no plans for trivial tasks, concise
finals); Claude 5.x guidance (tone down "never stop", explicit parallel block, avoid reviewer
subagents unless asked: ~⅓ cost cut); multi-agent costs ~15× tokens. Full URLs in `tools.md`.

Proposed core tools: `read` (multi-file, negative offset, read-tracking), `grep` (rg),
`glob` (`**`, gitignore, mtime sort), `edit` (multi-edit, read-before-edit, returns diff +
diagnostics), `write`, `shell` (persistent cwd, `block_until_ms` → background id),
`await_shell`, `kill_shell`, `code_search` (consolidates 12 graph tools), `todo`,
`task` (6 types, background, handoff packet, validated JSON return; no caps), `tool_search`
(deferred: diagnostics, web_search, web_fetch, impact, MCP).

Prompt changes: remove mandatory tests + `git diff --stat`, triple "never stop", duplicated tool
lists, self-review; add batching, read-once, background+await, BSD userland, proportional
verification, spawn only for independent ≥10-call work with 5-field brief, outcome-first final
answer. Subagents: 9 → 6 roles, <300 words each, no nested spawns, futility-based early exit (no budgets),
JSON-only return, reviewer "clean is acceptable".

## 4. Consolidated roadmap

| # | Change | Fixes | Expected gain |
|---|---|---|---|
| 1 | Liveness supervisor (stream retry, tool supervisor) + background subagents + futility ladder — **no time caps** | 31,000 s blocked | removes multi-hour stalls without limiting long work |
| 2 | Tool-call watchdog + persist assistant msg before tools | 95-min session stall | bounded worst case |
| 3 | Verification gate only after mutations; accept post-edit passes | forced re-runs, duplicate answers | 2–4 calls + minutes/task |
| 4 | ripgrep grep + `**` glob + gitignore | incomplete/empty searches | 4–18× grep; fewer rounds |
| 5 | Stop compressing <16 KB / ranged reads; spawn results ≥4 KB pinned across compaction | 3,747 s refetch, lost audits | ~6% model time |
| 6 | Background shell + await/kill | polling, hangs | 14 polls → 1–2 calls |
| 7 | Structured subagent handoff/return (files+ranges in, JSON out, empty = error) | 36% dup reads, 59% no STATUS | fewer redos |
| 8 | Revised prompts (main + subagents), drop ceremonial reviewer/tester | over-spawning, ceremony | ~⅓ cost per Anthropic guidance |
| 9 | Fix `<turn_context>` recursion; hide complexity hint | stale/nested context | cleaner prefix |
| 10 | Structured compaction 4–8 k tokens, retry on max_tokens | truncated summaries | no lost work |
| 11 | Per-call parallel scheduling; parallel spawns | serialised rounds | ~45–1,100 s |
| 12 | sessions.db WAL, batched traces, shared token cache | per-round ms | 36 → ~15–20 ms/round |
| 13 | Deferred tool loading (`tool_search`) | ~30 schemas/request | −6–10 k prefix tokens |
| 14 | Real paired eval on quant-trade prompts before/after | synthetic benchmarks | measurable |
