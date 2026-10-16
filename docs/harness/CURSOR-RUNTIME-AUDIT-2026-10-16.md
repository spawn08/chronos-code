# Chronos Code vs Cursor Agent: runtime audit and parity roadmap

**Audited and implementation updated 2026-10-16. Recommendation: validate the integrated recovery runtime with repeated live-model tasks before promoting efficiency or parity claims.** The working tree now implements interactive write-ahead journaling, supervised specialized tools, recovery-aware context assembly, deferred schemas, structured subagent handoffs, and a reproducible configured-harness evaluation path. The implementation status and remaining evidence limits are recorded in §3.1; historical run measurements below remain unchanged.

This is a **current-source audit**, distinct from the historical measurements in [cursor-comparison.md](cursor-comparison.md) and the **proposed** design in [HARNESS-SPEC.md](HARNESS-SPEC.md). A proposed counterpart in [CURSOR-PARITY.md](CURSOR-PARITY.md) does not establish runtime parity.

## 1. Scope and evidence

- Chronos Code: `main`, HEAD `8be5482`, including the six pre-existing modified files carried from `harness-v2`. Local `main` was fast-forwarded by nine commits and the modifications restored without conflicts.
- Actual dependency: `go.mod:5` replaces Chronos with `../chronos`. That checkout is on `harness-v2`, HEAD `6a4be05`, with a clean working tree. SDK findings below apply to that local dependency; a released binary needs its own versioned verification.
- Cursor: inventoried the 16 JSONL transcripts in the local `chronos-code` Cursor workspace. Four reference the recent `quant-trade` work; their visible user timestamps are October 14. These are the relevant runs found in this corpus, not a claim that every Cursor workspace/history was searched.
- Chronos run evidence: read-only queries against `~/.chronos-code/projects/quant-trade-716662ac74db1f8fdff9741ff3d491c2/{sessions,telemetry}.db`, with a start-date filter of October 13. The last located records are October 15.
- Cursor public documentation and engineering posts fetched October 16; linked in §8.

### What the transcripts can establish

They contain visible user/assistant text, tool names and arguments, and turn-end markers. **They omit system prompts, tool results, token billing, and per-call timings.** A multi-tool assistant record shows a batching opportunity, not proof of simultaneous execution. A `Read` before an edit shows requested access to that path, not a successful read, freshness, or enforced edit guard.

Repeated user requests and injected subagent catalogs occur in these exports. Raw user-role record counts are consequently not counts of distinct user tasks. Success markers are harness-reported outcomes, not independent correctness scores. No Cursor cost or model-quality advantage can be calculated from these files.

### Located Cursor runs

All paths below are under `~/.cursor/projects/Users-amarsingh-thakur-Downloads-code-projects-chronos-code/agent-transcripts/<ID>/<ID>.jsonl`. `C1`–`C4` references identify these files and their **JSONL record/line number**, not source-code lines.

| Ref | Transcript ID | Work | Assistant records | Tool calls | Calls/tool-bearing record | Largest batch |
|---|---|---|---:|---:|---:|---:|
| C1 | `b35971b2-fd15-4c31-badf-b14914ea4a00` | Explain and revamp Legacy; experiments and integration | 330 | 474 | 1.491 | 9 |
| C2 | `2525a1a9-58a8-4697-a6ac-60a354c895b6` | Add algorithm accuracy percentages to UI | 41 | 95 | 2.375 | 6 |
| C3 | `7e9a189b-6ee5-42b7-9131-6f725bf8fa4f` | Prediction changes after flow refresh; constituent weighting | 131 | 172 | 1.387 | 5 |
| C4 | `764fd9f9-a8fb-4517-ace9-82d2268f434d` | Early-hours index-trading research | 30 | 59 | 1.967 | 5 |

Combined: **800 calls across 512 tool-bearing records** (1.563 calls/record), including 401 `Read`, 203 `Shell`, 125 `StrReplace`, 37 `Glob`, 16 `Grep`, 4 `Write`, 3 `GetDynamicTools`, and 11 `CallDynamicTool`. All 11 dynamic calls are `cursor:AwaitShell`. No `Task` or `TodoWrite` calls occur in this selected set.

- **124/125 edits** have a prior `Read` request for the same path in an earlier record (99.2%). This is observed behavior, not proof of enforcement.
- **32 terminal-file reads** plus **11 await calls**: durable terminal access matters, but polling has not disappeared in Cursor.
- **116/203 shell payloads exceed 2,000 characters**. Large inline experiments are common; length alone does not establish failure.
- **34 repeated exact read requests**, with identical path/offset/limit. Some read growing terminal logs; this is not a duplicate-read waste rate.
- C4 ends with two usage-limit errors (`C4:34–35`), no edit calls, and no completed implementation in that transcript.

### Chronos historical run evidence, October 13–15

The queried window contains 566 model spans (353 `gpt-6.1-sol`, 202 `claude-sonnet-5-5`, 11 `claude-haiku-5-5`) and 1,050 tool spans. Among them: 414 shells, 236 reads, 105 writes, 29 stored-result fetches, 20 skill calls, 17 subagent calls, and 32 plan updates.

- Three tool spans lack a valid end timestamp: two shells and one subagent. This proves incomplete observations; it does not alone identify why they ended incompletely.
- Ten `chat_summary` events.
- Nested `<turn_context>` reaches **depth 14**, at `sess_f8da9d7bdd95e0df`, event sequence 31.
- Eight telemetry sessions, including one shell-only session; all have zero reported cost and no end timestamp in the historical database's old schema.
- Trace usage totals: 1,053,576 uncached input, 919,557 cache-creation, 18,441,710 cache-read, and 328,363 output tokens. The 58,638 one-hour cache-creation tokens are a subset of creation, not an extra class to add again.

These are **historical runs**, preceding some fixes now present in source. They identify regressions to replay, not proof those faults persist in today's executable. They are not paired runs of identical tasks/models with Cursor.

## 2. What Cursor's harness does well—and where these runs lose efficiency

### Adopt these patterns

1. **Discover context at the point of need.** `C1:2–5` narrows from filenames/grep to docs and ranged source. `C3:33–35` discovers `AwaitShell` only when a long experiment needs it. Cursor's engineering post describes file-backed MCP schemas, history, tool outputs, and terminals, reducing up-front context.
2. **Let action follow investigation in the same workflow.** `C2:17–39` applies UI edits; `C2:40–43` runs tests/build, inspects a test file, edits it, then reruns. Tools are integrated into delivery rather than producing only a plan.
3. **Use exact edits with local evidence.** The 99.2% prior-read-path rate and batched edits support adopting a small, unambiguous edit interface with freshness checks.
4. **Recover past context from durable history.** `C4:2–3` reads a previous transcript, including negative offsets. Cursor explicitly documents history-file references after summarization. Compaction should reduce working context without destroying the evidence trail.
5. **Treat user terminals as discoverable context.** `C2:2` and `C3:2` read an IDE terminal at startup. For a CLI, expose known task/shell logs and allow explicit attachment of external logs.

### Improve beyond the observed behavior

- `C1:23–30` repeatedly reads background logs; `C3:34–39` alternates await calls and log reads. Return useful deltas and completion notifications so waiting does not require extra lookup rounds.
- `C3:20–29` revises similar experiments while changing guessed symbol APIs/formatting; `C4:14–33` repeatedly reconstructs sizeable analysis scripts. Reuse a versioned experiment artifact and inspect APIs/schema before running the full computation.
- `C2:3–10` emits long, repetitive progress prose. Show one informative line per meaningful milestone; do not make that verbosity the parity target.
- C4 keeps researching until account limits interrupt it. Persist executable experiments and partial findings before the next expensive run so interruption preserves a useful deliverable.

The visible `<timestamp>`/`<user_query>` envelope is small, but `C2:11` also contains an injected subagent catalog. **It is incorrect to infer that Cursor's entire prompt contains only the user query, or that no compaction occurred because none is visible in the export.**

## 3. Initial implementation comparison

This table preserves the initial audit snapshot. The implementation update in §3.1 supersedes its remaining-work statements for the changed areas. Evidence paths are relative to Chronos Code, except `../chronos/...` for the local SDK.

| Area | Current Chronos Code evidence | Assessment and remaining work |
|---|---|---|
| Recursive filename search | `internal/incctx/glob.go:23–66` | Implemented: `**`, alternation, ignore rules, newest-first, explicit truncation. Replay real patterns and measure latency; the old “no recursive glob” gap is closed in source. |
| Content search | `internal/incctx/incctx.go:320–427`, `internal/incctx/ripgrep.go` | Implemented rg path plus bounded fallback and files/content/count modes. Current Cursor docs describe **Instant Grep**, not necessarily rg underneath. Compare correctness and latency rather than identical implementation. |
| Background shells | `internal/security/background.go:125–218,239–307` | Yield, await, kill, output delta, idle time and session isolation exist. Process state is in-memory, shell calls use fresh `sh -c`, and CPU/child activity is not measured. Restart adoption and persistent environment are not implemented here. |
| Artifact accessibility | `internal/orchestrator/orchestrator.go:365`, `internal/config/paths.go:27–55`, `internal/incctx/incctx.go:182–213` | **Integration gap:** shell logs live in the default external data home, but the source reader rejects outside-workspace paths. A returned `log_path` is not reliably readable through `file_read`. |
| General large outputs | `internal/toolcompress/compress.go:81–110,126–191` | Durable storage and chunk retrieval exist, but a separate `read_stored_result` abstraction remains. Consolidate result/history/log access behind bounded artifact reads. |
| Compression policy | `internal/orchestrator/orchestrator.go:3554–3610` | Improved: 4k-token minimum; skills, subagent results, awaits and targeted ranges are exempt. This is not the proposed universal 10k-token head/tail policy; nor does “not compressed” mean “survives compaction verbatim.” |
| Tool scheduling | `../chronos/sdk/agent/agent.go:1106–1169`, `internal/orchestrator/orchestrator.go:701–739` | Contiguous parallel-safe groups now run concurrently, with writes/shells as ordered barriers. User pre/post hooks deliberately clear safety flags. Resource/conflict-aware scheduling and parallel read-only shell classification remain refinements. |
| Tool discovery | `../chronos/sdk/agent/agent.go:512–524`, `internal/mcpdiscover/runtime.go:147–170` | Agent serializes all registered definitions. MCP discovery/connection is not lazy **schema exposure**. No deferred-tool dispatch implementation was found in the inspected runtime scope. |
| Main prompt | `internal/defaults/agents/chronos-code.yaml:8–45` | Substantial progress: action bias, proportional checks, batching, bounded reads, BSD hygiene, restrained delegation and one final answer. Intent/evidence additions in the carried modifications explicitly protect short follow-ups. |
| Skill loading | `internal/orchestrator/orchestrator.go:1653–1678` | Catalog-first/on-demand bodies are implemented. The older comment above this function still describes BM25 body selection; read the implementation. |
| Recall and prefetch | `internal/orchestrator/orchestrator.go:1047–1097,1367–1417,2115–2138` | Recall sanitization/provenance for prior sessions exists; ordinary memory notes omit age/anchors. Repository prefetch is still appended to the prepared user message. Just-in-time retrieval is only partial. |
| Verification | `internal/verification/policy.go:72–115`; `internal/orchestrator/verification_observer.go:75–107,245–297` | Mutation-based derivation, optional diff obligation, compound-command handling and background-check exit handling exist. Async mutation ordering and targeted coverage remain gaps (§4). |
| Completion/repair | `internal/orchestrator/completion_gate.go:25–99`, `internal/orchestrator/repair.go:88–139` | Text-pattern/plan nudges still exist, and blocking nudge replies are concatenated. The repair prompt says separate commands are required although the observer accepts some compounds. Single-final/addendum behavior needs end-to-end validation. |
| Subagents | `internal/orchestrator/subagents.go:199–308,392–439` | Capacity/cycle protection, task-local governor, empty-result error and partial-report fallback exist. Ordinary delegation waits for the child; handoff is a task string, not a harness-built knowledge packet. No background task lifecycle/validated artifact return is implemented on this path. |
| Stream liveness | `../chronos/engine/model/stream_idle.go:11–69` | Two-minute byte-silence body closure exists. This is a liveness bound, not a total runtime cap. General tool handlers still execute synchronously (`../chronos/sdk/agent/agent.go:1219–1226`). |
| Durable tool rounds | `../chronos/sdk/agent/tool_loop.go:102–165` | Completed rounds persist and checkpoint. The session path records **after** tool execution; appending assistant intent in memory before execution is not durable write-ahead journaling. Delivery-operation machinery should not be assumed to cover ordinary interactive calls. |
| Futility governance | `internal/orchestrator/futility.go:62–123`, `long_running.go:148–201` | Repetition/check/failure nudges and escalating idle-window guidance exist. No learned-information set, normalized error-set comparison or guaranteed no-tool reflection turn on this path. New argument fingerprints alone can look like progress. |
| Compaction | `../chronos/engine/model/summarizer.go:137–253`; `../chronos/sdk/agent/session.go:836–861` | Structured summaries, scaled budget and two continuations replace the old fixed 1,500-token approach. Still: each tool argument/result is cut to 1,000 characters before summarization; final truncation after continuation allowance can be returned as a normal summary. Recoverable artifact references remain important. |
| Context/cache guard | `internal/orchestrator/context_guard.go:126–228`; `internal/defaults/config.yaml:141–148` | Measured schema/output reserve, provider calibration, remembered trim and headroom exist. Default 128k ceiling/0.45 summary threshold remain; this is not the proposed served-window clearing/80% design. Tune as a coherent policy, not by raising one threshold. |
| Edit interface | `../chronos/engine/tool/builtins/file.go:48–175`, `internal/orchestrator/edit_checkpoints.go:53–167` | Unique exact replacement, no-op rejection and checkpoints exist. Checkpoint race detection is not “the model read the relevant current source.” No enforced source-read freshness or post-edit diagnostics in this builtin receipt. |
| Language diagnostics | `internal/lsp/manager.go:1,89–109`, `lsp_off.go:1–32`; `Makefile:20–41` | LSP is build-tagged, four-language server selection; default build tags omit `lsp`. Multi-language indexing does not imply default post-edit diagnostic parity. Large-file read outlines are still Go-specific (`incctx.go:133–145`). |
| Scoped rules | `internal/projectdocs/projectdocs.go:24–36,97–123` | Root-to-cwd AGENTS/CLAUDE/legacy rules exist. `.cursor/rules/*.mdc` frontmatter/glob activation is absent from this loader; path-scoped activation should follow files being worked on, not just startup cwd. |
| Cost telemetry | `internal/learning/telemetry.go:400–406,488–509`; `telemetry_cost_test.go:17–89` | Source now writes cache classes, price/unpriced information and timestamps. Historical DBs have not magically acquired old missing costs. `ended_at` is updated after each call, so it is not a true session terminal-state event. |
| Outcome evaluation | `internal/eval/harness.go:50–61,159–185,212–214`; `Makefile:67–72` | Existing deterministic eval measures tool contracts/token representations with a stub model and synthetic shell. Useful regression tests; **not** evidence of agent completion or Cursor parity. |

Browser-driven visual verification is a documented Cursor capability. The selected transcripts do not exercise it. For Chronos, first prove a configured browser MCP plus screenshots/diagnostics works through the real harness; inventory or a server connection alone is not visual-verification parity.

## 3.1 Implemented runtime update and evidence limits

The update builds on the staged and unstaged recovery work already present when implementation began. Source provenance is Chronos Code HEAD `8be54822f039fb039ca38eade253fb9f713e901a` plus the working-tree changes; the local SDK remains clean at `6a4be05546bf829be608235ade04fbc82f34484f`. No SDK files were changed for this update. A released dependency and binary still need separate verification.

| Area | Implemented source | Evidence and remaining limits |
|---|---|---|
| Interactive tool journal | `internal/orchestrator/interactive_journal.go` | Persists native assistant intent and planned call IDs before execution, then writes individual started/result receipts. Resume restores pending groups and preserves unresolved-effect replay protection. Exact file writes can reconcile against the expected hash. General external side effects require destination-specific reconciliation. Compaction requests leave the tool journal intact; recovery retains the fresh mutable tail and drops historical transient annotations. |
| Host-crash recovery | `internal/orchestrator/journal_crash_integration_test.go` | A subprocess runs a tool through the SDK, writes and syncs its effect, then the test forcibly terminates the host before its result. A new host reads the same SQLite journal, identifies the unknown outcome, rejects a duplicate invocation, and observes exactly one effect. This establishes the local crash-window contract; it does not prove remote-service exactly-once delivery. |
| Stuck specialized tools and subagents | `internal/orchestrator/tool_supervisor.go` | Slow specialized handlers yield a session-owned run ID. Await, cancellation, steering and terminal receipts remain accessible; active fingerprints prevent duplicate starts and a bounded gate contains non-cooperative handlers. Cancellation requests cooperative shutdown and retains uncertainty until a terminal receipt. Heartbeats describe host observations, and idle time describes lifecycle observations. Go cannot forcibly stop arbitrary in-process handler code. Admission is process-local; overlapping hosts need a distributed lease. Late completion retains its receipt and does not re-enter every agent hook. Active process adoption after host restart remains out of scope. |
| Durable artifacts and reload integration | `internal/orchestrator/artifacts.go`, `internal/security/shell_artifact.go`, `internal/toolcompress/compress.go`, `internal/orchestrator/artifact_pipeline_test.go` | Tool findings and shell logs have bounded session-owned readers, byte/line continuation and tail access outside the workspace source reader. Compression retains the artifact ID. Initial, connected and reloaded MCP tools share the tool pipeline and supervision. Full terminal process state is still in memory. |
| Recovery-aware compaction | `internal/orchestrator/working_context.go`, `internal/orchestrator/context_recovery_test.go` | Saves compaction source and summary fragments, adds a recovery contract, flags output-limited fragments and preserves discoverable references. Context-guard reductions retain artifact metadata and protect live task/tool progress. The SDK still clips each tool argument/result in the summarizer input; durable original artifacts and history supply the recovery path. Repeated live-model recovery quality remains unmeasured. |
| Context assembly and cache attribution | `internal/orchestrator/working_context.go`, `internal/orchestrator/request_metrics.go`, `internal/orchestrator/context_guard.go` | Bounded per-call tail carries live task/source/check state, shell/tool status and provenance hashes; user requests stay separate from repository/routing annotations. The guard measures exposed schemas, pinned messages, a YAML-configured finite unary output allowance and calibration together, retaining remembered trims and headroom. Its effective request budget is recorded; SDK compaction triggering still uses a separate trigger calculation. Request traces record block hashes and effective budgets. The default compaction threshold remains unchanged; cache savings have no live A/B evidence yet. |
| Deferred schemas | `internal/orchestrator/deferred_tools.go`, `internal/defaults/config.yaml` | YAML defines an ordered stable core. Specialized namespaces expose bounded on-demand schemas through `discover_tools`; `call_tool` re-enters hooks, effect grants and registry authorization. Unknown names/namespaces are explicit errors. Registry availability is reported in a mutable catalog. Connection readiness and approval remain separate from availability. |
| Prompt consolidation and model profiles | `internal/defaults/agents/chronos-code.yaml`, `internal/defaults/context-engineering.yaml`, `internal/defaults/model-profiles.yaml`, `internal/orchestrator/model_profiles.go` | Shared intent/navigation policy has a canonical runtime source; YAML model-family guidance augments it. The embedded artifact catalog includes the model profiles. Supported-family prompt behavior has deterministic tests; comparative model-family task resolution remains an evaluation requirement. |
| Subagent knowledge handoffs | `internal/orchestrator/subagent_handoff.go`, `internal/orchestrator/subagents.go` | Bounded handoff packets carry relevant current source observations and hashes, task/acceptance state, prior failures and unresolved work. Delegation preserves the original task separately; handoff instructions require explicit report status and evidence. Full oversized and interrupted reports are persisted before reduction, and SDK-visible errors include bounded partial findings and recovery references. Hash checks reject stale parent observations. Source changes during handoff still require child freshness checks. |
| Reproducible evaluation | `internal/eval/live.go`, `internal/eval/evidence_test.go`, `internal/cli/eval_tasks_cmd.go` | A configured orchestrator runs in an isolated checkout and hidden graders determine correctness. Run metadata records revision/config/model/runtime fingerprints and request context; model/tool observations retain usage classes and evidence completeness. The offline fixture drives the real harness with a scripted local model endpoint. It proves integration and objective grading; model quality, provider billing and Cursor parity need paid live runs with controlled inputs. The adapter explicitly marks unavailable emitted-call correlation, retry/repair counters, per-call pricing/usage-known evidence, complete partial traces and durable session-terminal events. Host toolchains and inherited environment remain non-hermetic; oracle failures may obscure an earlier execution classification. |

The roadmap below continues to describe acceptance goals. Implemented substrate does not establish every broader exit condition: production process adoption, remote-effect reconciliation, multi-language/live-model holdouts, browser-MCP evidence and paired product measurements remain open.

## 4. Highest-priority runtime gaps

### P0.1 — accessible, durable artifact/history substrate

**Why first:** shells already produce the files needed for dynamic discovery, but their paths are outside the reader's allowed root by default. This makes the output recovery design incomplete.

Implement session-owned artifact IDs backed by the existing data home; bounded line/byte reads and search; head/tail previews; explicit continuation; tool-output, terminal, subagent and transcript artifacts with the same interface. Resolve only authorized artifacts or explicitly attached readable roots, keeping ordinary workspace path checks.

Acceptance: a command produces output beyond the in-memory tail; the agent retrieves its beginning and failure location via the advertised interface. This works with the default external data home, compaction and restart, and does not expose another session's artifacts.

### P0.2 — ordinary interactive write-ahead journaling and tool liveness

Persist the assistant tool intent and call ID **before** invoking effects, then journal each result independently. Expose pending/running/reconciled states and heartbeat/idle information. A stuck handler must not prevent steering or usable partial results from returning. Reuse the delivery operation/journal abstractions where their contracts fit.

An elapsed deadline cannot make an unknown side effect safe to repeat. Do not relaunch a potentially mutating tool simply because its handler stopped responding. Recover or reconcile the original call; contain abandoned work and concurrency resources. Keep productive long-running work alive.

Acceptance: terminate the host between a tool's side effect and returned result; resume identifies the pending call and does not duplicate the effect. A hung custom handler permits steering/status recovery and yields a terminal or reconciliable trace observation.

### P0.3 — verification must follow actual file versions and task intent

Two concrete issues need tests:

1. `verification_observer.go:89–107` assumes background mutations happened before later checks. A background generator can write source **after** a test passes; no new mutation event is appended on exit. Track observed file hashes/change epochs throughout execution or invalidate overlapping evidence conservatively until the mutator is quiescent. Separately retain non-source-changing dev servers without forcing perpetual retests.
2. The ordinary derivation at `orchestrator.go:2563–2565` passes changed paths but no test map/build/check commands. It can create one generic test obligation across changed languages. Supply affected-test coverage and per-manifest tooling so a Python check does not stand in for a changed Rust/TS contract.

Also distinguish declared read-only analysis scripts from unknown mutations, and align repair instructions with accepted shell syntax. Link open plans to the live task rather than assuming any open item or a phrase like “next step” requires more execution. Buffer the final until the completion decision, then publish one final or a short repair addendum.

Acceptance: read-only comparison/status questions produce no unrequested edits or repair tests; passing targeted checks are reused until their inputs change; a late async source write invalidates earlier evidence; a genuine blocker/finished analysis is not resumed by a wording heuristic.

### P1.1 — deferred tool schemas with a small stable core

Keep core file/search/edit/shell/wait/plan tools available. Give graph, memory, MCP and browser capabilities a namespace catalog with status/instructions and on-demand schema discovery. Keep discovered tools callable through a stable dispatch interface or provider-supported deferral, without changing authorization or bypassing wrappers.

Avoid arbitrary “nine tools proves parity.” Measure schema tokens, ambiguous-tool selection and success rate. Cursor reports a 46.9% total-token reduction in its MCP-using A/B cohort; that is evidence for trying progressive discovery, **not a predicted Chronos saving**.

Acceptance: many configured MCPs do not inflate every model request; selecting one loads the relevant tools; reconnect/reload preserves status, approval, evidence, compression and audit behavior. Unknown tools are explicit errors. Core prefix bytes remain stable across unrelated discovery.

### P1.2 — recovery-aware compaction and lean context assembly

Store artifact references before reducing outputs. Summaries must carry goal/constraints, decisions, changed-file versions, accepted checks, failed hypotheses, active tasks/shells and next action. If a summary still truncates, preserve a recoverable partial state and signal it. Critical findings beyond the current 1,000-character tool-summary prefix must remain discoverable.

Use one effective request budget for schemas, pinned context, tokenizer calibration and output reserve. Benchmark the existing remembered trim before replacing it. Change thresholds/clearing with hysteresis behind flags; do not blindly switch 0.45 to 0.8 or assume every deployment serves its model's maximum window.

Move mutable recall/task state to a bounded tail with explicit provenance and anchors. Keep raw user requests separate from repository prefetch and harness annotations. Preserve freshness/fallback guidance for the graph; selected context and old memory cannot establish current repository state.

Acceptance: an important finding at the end of a long subagent result is recovered after compaction without rerunning the audit; short follow-ups use the latest task; normal long turns do not repeatedly rewrite the cached prefix; cache breaks are attributed to changed blocks.

### P1.3 — model-aware prompts and sharper tools

Use YAML profiles for model-family tool guidance/effort/output behavior over a shared small policy core. Current main and runtime index guidance duplicate some intent/navigation instructions; maintain one canonical policy and expose task-specific advice only when needed. Keep the current action bias and proportional verification.

Tool improvements: `file_read` offset/limit and tail support; portable multi-edit with hash/freshness checks and useful diff receipts; post-edit diagnostics selected by nearest manifest; safe parallel groups for independent reads; structured command/experiment metadata.

Read dedup needs context awareness: if source was compacted away, “unchanged” is not the requested source. Return the source or an explicit recoverable artifact on demand. Do not treat a repeated await that returns new output as futile merely because its arguments match.

Acceptance: a recorded set of Python/Rust/TS/Go tasks passes on the supported model families; source-read freshness prevents stale edits; diagnostics catch a broken edit before a full build; model-specific prompt variants improve efficiency without reducing resolution.

### P2 — background task lifecycle, scoped rules and IDE-equivalent evidence

- Add background subagent start/status/await/message/cancel with durable structured results; provide a harness-built handoff packet of relevant ranges/hashes, failed approaches and acceptance criteria. Seed child knowledge without assuming parent text is still valid. Use independently valuable delegation, not ceremonial reviewer/tester fan-out.
- Activate nested instructions and `.mdc`-equivalent path rules as affected files enter context. Keep catalog bodies lazy and avoid copying full guides into the prompt.
- Make diagnostic servers/checker fallbacks available in the normal build; extend outlines using the existing language packs. Add an end-to-end browser-MCP verification fixture for UI tasks.
- Track terminal process IDs/exit state across task turns and restart; expose bounded attached external logs. Persistent cwd/env is optional ergonomics, not a prerequisite for correctness.

Acceptance: parent continues while a child runs; interruption returns partial findings, not an empty success; two disjoint tasks run concurrently and conflicting edits serialize; UI changes produce visible browser evidence; path-specific rules activate in the correct project subtree.

## 5. A practical rollout

| Stage | Deliverable | Exit condition |
|---|---|---|
| Baseline, first | Versioned live eval runner + task snapshots; runtime prompt/schema/block hashes; explicit session terminal events | Each run identifies code/SDK/model/config, has an objective oracle and complete usage classes |
| Recovery | Artifact access, write-ahead journaling, stuck-tool recovery, async verification fix | Crash/hang/late-write fixtures preserve effects and invalidate stale checks correctly |
| Efficiency | Deferred schemas, recovery-aware compaction, bounded context tail | Equal task resolution with fewer schema tokens/model rounds/cache rewrites |
| ACI and product parity | Freshness-aware multi-edit/read-tail, normal-build diagnostics, async subagents/scoped rules/browser fixture | Python/Rust/TS/Go and UI workflows pass through the real configured harness |
| Promote | Repeated paired evaluations and canary runs | Improvements hold on unseen tasks and cost per successful task falls |

This sequence avoids rebuilding recursive glob, rg search, batching, empty-child detection, skill progressive disclosure or structured summaries that already exist. Keep checkpoints, effect controls, graph freshness/SCIP coverage reporting, steering and renewable progress governance; test their integration instead of counting features.

## 6. How to establish “on par”

Use isolated working-tree/data snapshots at a recorded base revision. Never replay experiments against today's live trading databases; dataset freshness changes the question and invalidates comparisons.

Start with six representative tasks:

1. Explain Legacy vs Playbook from source and measured reports; oracle checks required facts/citations and no edits.
2. Add accuracy labels using fixture metrics; verify UI text, data provenance and frontend tests/build.
3. Explain prediction changes after flow refresh; pinned before/after dossier fixtures and a reproducible experiment.
4. Improve an algorithm with a small executable change and evaluation artifact; separate research findings from statistical and implementation acceptance.
5. Mixed Python/Rust/TS contract change; all affected contract tests must pass.
6. Long-running interrupted task; preserve tool effects, findings and plan through a hang, compaction and restart.

Run a controlled same-model harness A/B where model access permits, and a separate best-available product comparison. Keep task inputs, permissions, attached context, toolchains and acceptance checks equivalent; report model differences rather than attributing them to the harness. Cursor runs can be executed/exported through the IDE; actual billing requires its usage export.

Record resolution and partial/blocked outcomes, model rounds, emitted **and executed** tool calls, p50/p95 duration, duplicate unchanged-source reads, artifact-only fetch rounds, unnecessary repair rate, unrecovered stalls, cached/uncached/output usage, unpriced calls, and **cost per resolved task**. Repeated tasks and unseen holdouts matter more than a single run. Fail deterministic correctness/recovery regressions; promote efficiency changes only if resolution is preserved.

The historical 57% cache-write cost share in [COST.md](COST.md) covers a different, longer interval. It is not the new runtime's measured cost profile, and projected savings must not be presented as achieved. Its rewrite-vs-cached-round arithmetic has been corrected: full-prefix cache reads still cost money on every additional round.

## 7. Reproduction and checks performed

The companion analyzer exports metadata/counters, not source, command payloads or transcript bodies. SQLite connections are read-only. To reproduce the selected inventory:

```bash
python3 docs/harness/analysis/audit_cursor_runs.py \
  --cursor-root "$HOME/.cursor/projects/Users-amarsingh-thakur-Downloads-code-projects-chronos-code/agent-transcripts" \
  --ids b35971b2-fd15-4c31-badf-b14914ea4a00 2525a1a9-58a8-4697-a6ac-60a354c895b6 7e9a189b-6ee5-42b7-9131-6f725bf8fa4f 764fd9f9-a8fb-4517-ace9-82d2268f434d \
  --chronos-dir "$HOME/.chronos-code/projects/quant-trade-716662ac74db1f8fdff9741ff3d491c2" \
  --since 2026-10-13 --summary-only
```

Executed successfully on the audited working trees:

- Analyzer inventory and selected-run audit above.
- `go test -race ./internal/incctx ./internal/security ./internal/learning ./internal/orchestrator -run '(Glob|Ripgrep|Background|AwaitShell|Futility|ConversationPins|IndexGuidance|EvidenceGuidance|TelemetryCost|NeverCompress)'`: matched checks passed; `internal/learning` had no tests matching this filter.
- `go test -race ./internal/learning -run 'Test(TelemetryRecordsCacheTokensAndCost|MigrateUpgradesVersionOneDatabase)$'`: cost/cache accounting and old-schema migration tests passed.
- In `../chronos`: `go test -race ./sdk/agent ./engine/model -run '(Parallel|Summar|StreamIdle)'`: matched SDK checks passed.

These initial checks verify selected implementation contracts. Subsequent implementation verification is recorded in §7.1. Paired live-model measurements, production process adoption and a browser workflow remain roadmap exit conditions.

### 7.1 Implementation verification, 2026-10-16

The final working-tree implementation passed:

- `make test`: complete repository suite with normal grammar/LSP build tags, `-race -count=1`; includes `internal/integration`, orchestrator recovery, configuration, security, evaluation and CLI tests.
- `make build`: normal binary build succeeded. The final `make eval-fixture` also rebuilt the final source with the same normal tags.
- `make eval-tasks-validate`: validated the checked-in runtime task manifest.
- `make eval-fixture EVAL_OUTPUT=bin/eval/runtime-verified.jsonl`: two isolated, configured-harness runs passed the external exact-content/scope oracle. Both completed the scripted provider sequence, executed three tool calls over four model rounds, and recorded four request-context observations. JSONL assertions also verified matching configured-input hashes.
- Targeted race-enabled regressions cover native interactive host termination, unresolved-effect replay blocking, hung handlers, receipt-write failures, bounded capacity, cancellation/steering, deferred delivery effects, long/partial subagent artifacts, compaction/journal isolation, fresh mutable tails, and tenant/session shell isolation.
- `git diff --check` and `git diff --cached --check`: clean.

Reproduce the offline runtime baseline with:

```bash
make eval-tasks-validate
make eval-fixture EVAL_OUTPUT=bin/eval/replay-001.jsonl
make test
make build
```

Each replay output path must be new; the evaluator rejects overwrites. See [benchmark/runtime/README.md](../../benchmark/runtime/README.md) for fixture inputs, recorded baseline, model configuration and explicit live-run commands. `benchmark/runtime/baseline-fixture.jsonl` preserves an earlier versioned fixture run with its own code/SDK/config/binary fingerprints; `bin/eval/runtime-verified.jsonl` records the final local replay and is a generated build artifact.

These fixture results use synthetic zero usage and establish deterministic harness correctness. They establish no live-model cost, unseen-task resolution or Cursor advantage. The host-crash regressions use local subprocesses and SQLite; production remote-effect reconciliation, process adoption, distributed admission and browser-MCP evidence remain unverified. The SDK checkout stayed unchanged.

## 8. Public Cursor references

- [Dynamic context discovery, January 6, 2026](https://cursor.com/blog/dynamic-context-discovery): file-backed long results/history/terminals/skills/MCP descriptions; MCP-cohort A/B token result.
- [Agent tools and lifecycle](https://cursor.com/docs/agent/tools): model-specific harness optimization, tools, browser, checkpoints and steering.
- [Search](https://cursor.com/docs/context/semantic-search): current Instant Grep and Explore subagent documentation. The historical URL now describes exact indexed search; do not infer embeddings are mandatory for current Cursor parity.
- [Rules](https://cursor.com/docs/context/rules): scoped `.mdc` frontmatter, lazy rule references, and nested AGENTS.md.

The recovery substrate and reproducible configured-harness baseline are implemented in the working tree. Promote them after repeated live-model and holdout evaluations establish preserved task resolution and measured efficiency. Production process adoption, distributed admission, remote-effect reconciliation and browser evidence retain their separate acceptance requirements.
