# Cursor parity check

> **Design mapping, not implemented parity.** For the current source, recent Cursor
> quant-trade runs, verification results and remaining integration gaps, see
> [Runtime audit — 2026-10-16](CURSOR-RUNTIME-AUDIT-2026-10-16.md).

Each Cursor harness behaviour observed in the quant-trade transcripts
([cursor-comparison.md](cursor-comparison.md)) is mapped to its counterpart in
[HARNESS-SPEC.md](HARNESS-SPEC.md). The goal is to **match** Cursor wherever it works well, and to
go beyond it only where our data shows a gain.

Legend:
- **=** matched
- **+** spec goes further
- **△** deliberately different, reason given
- **?** not observable in transcripts, needs validation

| # | Cursor behaviour (observed) | Spec | Status |
|---|---|---|---|
| 1 | Small core tool set: Read, Glob, Grep, Shell, StrReplace, Write | 9 core: read, glob, grep, shell, await_shell, edit, write, todo, task (+ tool_search) | = |
| 2 | Lazy tool loading: `GetDynamicTools` → `CallDynamicTool`; MCP schemas on disk | `tool_search` / Anthropic `defer_loading`; MCP and graph tools deferred | = |
| 3 | `Read{path, offset, limit}`, negative offset reads the tail | same, plus multi-file batch and read-dedup stubs | + |
| 4 | `Glob` with `**` and `target_directory` | same, gitignore-aware, sorted by mtime | = |
| 5 | `Grep` = ripgrep, `output_mode` | rg backend, `files_with_matches` default, content/count modes | = |
| 6 | `StrReplace` with unique match; reads before editing (~99%) | `edit` multi-replace, read-before-edit **enforced**, diff plus diagnostics returned | + |
| 7 | `Write` only for new files | `write` = create or full replace only | = |
| 8 | `Shell{command, description, working_directory, block_until_ms}` | same fields; cwd also persists | = / + |
| 9 | An overrunning command is backgrounded with an id; output streams to `terminals/<id>.txt` with pid/cwd/command header and exit_code/elapsed footer | identical design: `.chronos-code/terminals/sh-N.log` | = |
| 10 | `AwaitShell` ("never sleep using shell") | `await_shell` with pattern, delta output and liveness (CPU, idle) | + |
| 11 | Long-running work is never killed by a timeout | no runtime caps; liveness and futility governance | = |
| 12 | Agent reads the **user's IDE terminals** (`terminals/1.txt`) on turn 1 | chronos-code is a TUI with no IDE terminals. Equivalent: recent background-shell list in the tail. Recent user shell history is opt-in only, for privacy. | △ |
| 13 | Large tool output spilled to `agent-tools/<uuid>.txt` and read like a normal file | head+tail ≤10k tokens plus `log_path` / `ref:` readable by `read` | = |
| 14 | User turn is thin: `<timestamp>` + `<user_query>`; @-mentions are raw paths the agent reads itself | thin turn; date/time in Zone B, per-turn state in the uncached tail; @-paths inline, not prefetched | = |
| 15 | No routing or complexity label shown to the model | routing hint removed from the prompt | = |
| 16 | No forced verification gate; the model decides when it is done | gate only after mutations, accepts any pass after the last mutation, never duplicates the answer | = (safer) |
| 17 | Verification ladder: targeted test → full suite → build | proportional verification; nearest tests via `test_map`; full suite only for shared contracts | = |
| 18 | Parallel tool calls in exploration (avg 1.4–2.4, max 9); shells serial | per-call scheduling; read-only shells parallel-safe | = / + |
| 19 | One-line bold-titled progress text before tool batches | prompt rule plus TUI rendering | = |
| 20 | Inline `python -c` experiments rather than scratch files | allowed; scratch dir `.chronos-code/scratch/` for scripts over 30 lines (heredoc failures observed) | △ |
| 21 | Subagents (Task) available but rarely used | spawn only for independent work of ≥10 calls; background; structured return | = |
| 22 | TodoWrite available, used only for complex work; Plan mode writes `.plan.md` | `todo` for ≥3 steps; plan mode exists | = |
| 23 | `ReadLints` available after edits | diagnostics after `edit` (LSP or checker), 15 languages | + |
| 24 | Background-task completion arrives as a notification, followed by a brief follow-up | background shell/task completion shows in the tail; one-line acknowledgement | = |
| 25 | Long sessions (330 msgs / 3 h) with no visible compaction | append-only context; one-shot clearing at about 60%, compaction at about 80%; served window (up to 1M) | = |
| 26 | Rules (`.mdc alwaysApply`), skills (`SKILL.md`) | AGENTS.md / CLAUDE.md / `.cursorrules` already loaded; skills catalog plus on-demand bodies | = |
| 27 | MCP: per-server instructions, broken-server STATUS.md ("inform user or continue another way") | MCP deferred; server instructions in the namespace description; errors surfaced as tool results | = |
| 28 | Semantic codebase search (Cursor indexes; +12.5% QA accuracy published) | BM25 + symbol graph + SCIP today; hybrid embeddings/RRF in spec §5.2 | = after §5.2 |
| 29 | Final answers: long markdown reports, tables, paths | outcome first, concise, no recap (Cursor's 10–19 KB reports were often over-long) | △ |
| 30 | System prompt and per-call context injection | **not stored in transcripts** | ? |
| 31 | Model-side tuning (Cursor trains or tunes for its own tools; Composer) | not replicable; compensated by an eval-driven prompt and tool-description hill-climb (spec §7) | △ |

## Where the spec deliberately goes beyond Cursor
These are areas where our own runs showed losses, or where chronos-code has an advantage:
- futility detection and the escalation ladder;
- cost-first caching: append-only context, cache-break detector, cost telemetry;
- code-graph tools (callers, implementations, impact);
- enforced read-before-edit;
- edit checkpoints and undo;
- the security floor;
- language-registry diagnostics;
- multi-hour resumability with `progress.md`.

## What cannot be confirmed from data and must be measured
- **#30:** Cursor's system prompt and hidden context. Transcripts omit tool results and the system
  prompt.
- **#31:** model-side advantages.
- **Whether "matches Cursor" holds end to end.** The only valid proof is a head-to-head run on the
  same task set: the quant-trade replay set plus SWE-bench Multilingual, per spec §7. Compare
  resolved %, wall time, tool calls, and **cost per resolved task**. Cursor's own cost needs its
  usage dashboard export, because the transcripts carry no token data.
