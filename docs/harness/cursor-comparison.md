# chronos-code vs Cursor Agent — Harness Comparison & Improvement Plan

_Date: 2026-10-15. Evidence: 4 Cursor agent transcripts on quant-trade (Oct 2026,
`~/.cursor/projects/…-chronos-code/agent-transcripts/`), 7 chronos-code sessions on quant-trade
Oct 13–15 (`~/.chronos-code/projects/quant-trade-716662…/sessions.db` + `telemetry.db`), and the
chronos-code / chronos source._

## 1. Headline

chronos-code is **not behind on raw mechanics**: it parallelises *more* than Cursor (≈2.5 vs ≈1.5
tool calls/turn), has better prompt caching (cache-read ≈17× uncached input), a code-graph index,
subagents, skills, undo checkpoints and a richer security floor.

It loses on **harness overhead and robustness**. Cursor's harness is *thin*: a small, sharp core
tool set, a plain user turn (`<timestamp>` + `<user_query>`), lazy tool loading, and real
background shells. chronos-code wraps every turn in layers (classifier hint, recalled
cross-session context, plan, repo-context, verification gate) and several of those layers
actively misfire.

## 2. Side-by-side

| Aspect | Cursor (observed) | chronos-code (observed) | Verdict |
|---|---|---|---|
| Core tools | Read, Glob, Grep, Shell, StrReplace, Write (+ lazy namespace) | ~25 tools pinned (file_*, shell, 12 graph tools, plan, skill, memory, subagent…) | Cursor leaner |
| Tool loading | `GetDynamicTools` → `CallDynamicTool`; MCP schemas on disk, loaded on demand | All schemas in every request | Cursor |
| Glob | `**` recursive, `target_directory` | `filepath.Glob`, **no `**`** | Gap |
| Grep | ripgrep, `output_mode` | pure-Go walker, 500 matches | Gap (speed/ignore rules) |
| Read | `offset/limit`, **negative offset = tail** | `start_line/end_line`, Go outline default | Small gap |
| Edit | `StrReplace` (unique match), reads-before-edit ~99% | `file_write old/new_content`, no read-before-edit check | Parity; add guard |
| Shell | `block_until_ms`; overruns become background shell w/ id, output streamed to `terminals/<id>.txt` (header + `exit_code`, `elapsed_ms`); `AwaitShell` to wait | fresh `sh -c`, fixed 300 s, killed on timeout; no background | **Major gap** |
| Hung tool | not observed | trace never closed → 95-min stall (sess_d706051b) | **Major gap** |
| Large output | spilled to `agent-tools/<uuid>.txt`, model can `Read` it | `{"compressed":true,preview}` + `read_stored_result` (7% of outputs, 29 refetches) | Cursor simpler (file path + Read) |
| Parallelism | 1.4–2.4 avg, max 9; batched reads/globs/edits; shells serial | 2.5–2.9 avg, max 9; **but any shell/write/glob/list in batch ⇒ whole batch serial** | chronos-code fine; fix all-or-nothing |
| User-turn context | `<timestamp>` + `<user_query>` only; @-files as paths | `Path: complexity=low kind=edit…` + `<turn_context>` (prior sessions, last 4 requests, memories, patterns, diagnostics, plan, repo-context) | **chronos-code overloaded & buggy** |
| Live terminals | agent reads user's IDE terminal files (`terminals/1.txt`) on turn 1 | none | Gap (CLI equivalent: recent shell history) |
| Completion | model decides; final answer | verification gate injects "Verification did not support completion…" (4/7 sessions, incl. read-only Q&A) | **chronos-code harmful** |
| Compaction | none visible across 330-msg / 3 h sessions (large window) | at 45% of 128k; generic prompt; 1,500-token cap → 3/10 truncated; lost a subagent audit | **Major gap** |
| Planning | TodoWrite available, unused; Plan mode writes `.plan.md` w/ todos | `update_plan` used 32× | Parity |
| Subagents | available, unused in these runs | 17 calls, ~2,900 s; one re-spawn after compaction loss | Use less, cheaper |
| Thinking/progress | short bold-titled 1st-person progress text before tool batches | — | Cursor UX better |
| Final answer | long markdown reports (3–19 KB) | 4–6 KB, plus duplicate post-repair summaries | Remove duplicates |

## 3. chronos-code failure modes (evidence from quant-trade runs)

1. **Verification-repair gate misfires.** Read-only "compare to Cursor" got a forced `git diff --stat`
   turn; sess_01f6b50e re-ran `cargo test`/`npm test`/`e2e` 3–4× each (~13 extra calls) for
   "test status=stale"; users read near-duplicate final answers.
2. **Recursive, stale `<turn_context>`.** Recalled user messages already contained `<turn_context>`
   → nesting depth 14 (sess_f8da9d7b). Recalled facts from Sep 27 were false ("no git repo",
   "vanilla JS in backend/web/").
3. **Task classifier mislabels.** "complexity=low kind=edit suggested_plan=skip" on both a yes/no doc
   question and the AKS + App Gateway + Postgres migration.
4. **Compaction truncation.** 10 compactions; 3 summaries hit `max_tokens` (1,500); debugger's 7.8 KB
   audit compacted away → "Prior truncated audit lost…" re-spawn (+262 s).
5. **Hung shell → silent stall.** sess_d706051b: shell trace never closed (ended_at = zero), no
   assistant message persisted, idle 95 min; 9 unclosed tool traces in that session. Polling a log
   by `file_read` 14× instead of a wait primitive.
6. **Redundant work.** `options.rs` read 10×; overlapping ranged reads; `git status --short` 5×.
7. **Analysis instead of action** on "devise an algorithm" (user had to re-ask), over-hedging.
8. **Workspace sandbox blocks legit paths** (`/tmp/...`, `~/.cursor/...`), making the "compare to
   Cursor" task impossible.
9. Minor: GNU flags on macOS, heredoc Python guessing JSON keys, ambiguous `old_content` (3 matches),
   no-op edits, invalid regex. Sessions never marked closed; `cost_usd` always 0.

## 4. Improvement plan (prioritised)

### P0 — remove harm (days)
1. **Gate the verification gate.** Skip entirely when the turn made no file writes / mutating shell
   calls. When tests already ran *after the last edit*, accept them; never require re-running the
   same command with identical tree hash. Never emit a second full summary — repair output should
   be an addendum.
2. **Fix `<turn_context>` recursion.** Strip `<turn_context>…</turn_context>` (and the `Path:` hint)
   from user text before storing it as recallable memory and before injecting "last 4 requests".
   Add a staleness check: drop recalled claims older than N days or contradicted by current
   workspace facts (git present, frontend framework).
3. **Drop or demote the complexity hint.** Either remove it from the prompt (Cursor sends none) or
   only emit it when confidence is high; never `suggested_plan=skip` for prompts mentioning
   deploy/migrate/multi-system.
4. **Compaction:** raise summary cap to ≥4–6k tokens, use a structured prompt (goal, decisions,
   files touched + why, commands run + results, open todos, subagent findings verbatim), detect
   `stop_reason=max_tokens` and retry/continue. Pin subagent results as artifacts that survive
   compaction. Raise `summarize_threshold` from 0.45 → ~0.75–0.8 on 200k+ models.
5. **Watchdog for hung tools.** Every tool call has a hard wall-clock deadline enforced *outside*
   the tool (context + goroutine); on expiry, close the trace, return an error result, and persist
   the assistant message. Always persist the assistant message *before* executing tools.

### P1 — Cursor parity on tools (1–2 weeks)
6. **Background shell.** `shell{command, description, working_dir, block_until_ms}`: if the command
   outruns `block_until_ms`, keep it running, return `{shell_id, output_file}`; stream output to
   `.chronos-code/terminals/<id>.txt` with header (pid, cwd, command) and footer (exit_code,
   elapsed_ms). Add `await_shell{shell_id, block_until_ms, pattern?}` and `kill_shell`. Prompt:
   "Never poll by sleeping or re-reading logs; use await_shell." Persist cwd/env across calls
   (optional).
7. **Glob with `**`** (`doublestar` or hand-rolled walker respecting .gitignore), plus
   `target_directory`. **Grep via ripgrep** when available (fallback to Go), with
   `output_mode: content|files_with_matches|count`, `-A/-B/-C`, `glob`, `type`, head limit.
8. **Read:** negative `offset` for tails; `offset/limit` aliasing; dedupe — if the same range was read
   and the file is unchanged, return a short "unchanged since your read at turn N" note.
9. **Read-before-edit guard:** reject `file_write` with `old_content` on files not read (or changed
   since read) this session; this is cheap and eliminates ambiguous/stale edits.
10. **Offload large output to a real file** the model can `file_read` with offset/limit (like
    `agent-tools/<uuid>.txt`), instead of an opaque storage key + separate tool.
11. **Per-call parallelism.** Replace all-or-nothing `toolCallsParallelSafe`: run parallel-safe calls
    concurrently and serialise only conflicting ones (writes to same path, shells). Mark
    `file_glob`, `file_list`, graph tools, `codebase_search` as `ParallelSafe`.

### P2 — context & prompt efficiency
12. **Lazy tool loading.** Keep ~8 core tools pinned (read, glob, grep, shell, await_shell, edit,
    write, update_plan). Put graph tools, memory, MCP servers, LSP behind a
    `list_tools{namespace}` / `call_tool{namespace,name,args}` pair; store MCP schemas on disk.
    Shrinks the cached prefix and reduces tool-choice confusion.
13. **Thin user turn.** `<timestamp>` (with time+TZ, not just date) + `<user_query>`; move
    repo-context prefetch to on-demand (`codebase_context` tool) unless the query is
    navigation-heavy. Add git branch/status snapshot to the environment banner.
14. **Skill relevance threshold.** Don't load `analyze-stock` (11.9 KB) for a supertrend question —
    require the skill description to match, or let the model decide via catalog only.
15. **Progress text.** Prompt the model to emit a one-line "**Title** — what I'm doing" before tool
    batches (Cursor style), improving UX in the TUI without extra tokens.
16. **Action bias for build requests.** Prompt rule: when the user asks to devise/build/implement,
    produce a working artifact + backtest numbers, then caveats — not analysis only.
17. **Shell hygiene in prompt:** detect OS and state "macOS/BSD userland — no GNU flags"; prefer
    writing scratch scripts to `.chronos-code/scratch/` over long heredocs; inspect JSON keys
    before indexing.

### P3 — measurement
18. Close sessions (status) and fill `cost_usd`; store the system prompt hash + tool schema hash per
    model call so regressions are attributable.
19. Run a **real** paired eval (current benchmarks are synthetic/invalid): replay the quant-trade
    prompts above + SWE-bench-Verified subset with Cursor-style harness flags on/off; track tool
    calls, wall time, tokens, verification-repair rate, stall rate.
20. Allow configurable extra readable roots (e.g. `~/.cursor/projects`) and a writable scratch dir
    so tasks like this comparison are possible.

## 5. What to keep (chronos-code is ahead)
- Prompt caching layout (4 breakpoints, uncached tail) — strong.
- Code-graph index (callers/implementations/impact) — Cursor has no equivalent in these runs.
- Edit checkpoints + `/rewind`, security floor, sandbox options, steering inbox.
- Higher natural parallelism.
- Honest, evidence-based final answers (keep, but trim duplicates and hedging).
