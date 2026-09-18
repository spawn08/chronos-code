---
sidebar_position: 5
title: Production Readiness Roadmap
---

# Production Readiness Roadmap

This assessment is based on repository commit `3fd3729885edb2373c5d40bcf60aba8f8528659a` and source inspection performed on 2026-09-18. Comparisons with Claude Code and Grok Build describe capability targets, not benchmark results. Chronos Code does not yet have valid paired external-agent evidence, so this document makes no performance-parity claim.

This file is the single source of truth for the production-grade program. It replaces ad hoc feature lists with dependency-ordered work packages and measurable exit gates. It is intentionally not a PPD plan and does not require `.ppd` state.

## Scope and assumptions

- The local working tree contains changes beyond commit `3fd3729`; findings and implementation status below describe that working tree.
- Chronos Code remains YAML-first and ships as one binary. New policy, limits, agents, and skills must be configurable without recompilation.
- Chronos remains the model/tool-loop dependency. Changes needed in the sibling Chronos repository must be versioned and pinned before Chronos Code can claim reproducible builds.
- Claude Code comparisons use Anthropic's public product documentation. Grok comparisons use xAI's public Grok Build/model documentation; non-public harness internals are not assumed.
- "Competitive" means verified task success, recovery, safety, and operability under matched conditions. It does not mean matching a vendor feature checklist or an unverified benchmark claim.

## Architecture baseline

| Concern | Current implementation | Assessment |
|---|---|---|
| Execution | `internal/orchestrator.Orchestrator.Execute` is shared by CLI, TUI, and HTTP | Strong common boundary, but approximately 3,000 lines of composition and shared state increase concurrency risk |
| Agent loop | Chronos SDK owns model/tool iteration; YAML defines the primary and specialists | Good separation, but runtime completion is not tied to observed verification evidence |
| Context | Graph hints, incremental reads, selected skills, project docs, layered memory, compaction, tool-result eviction, and context guarding | Material strength; preserve and measure it rather than replacing it |
| Graph | Type-aware Go graph with incremental indexing; optional tree-sitter and LSP profiles | Strong for Go, confidence and release-profile differences need to be visible |
| Planning | Prompt-level plans, TUI plan mode, and a durable SQL DAG/controller exist | Three planning mechanisms are not connected into one normal execution path |
| Verification | Append-only ledger and obligation policy exist | Ordinary turns accept caller-supplied evidence instead of observing tools |
| Delegation | Bounded subagents and parallel-safe tool fan-out exist | Specialists share the checkout; YAML delegation allowlists are not consistently enforced |
| Server | REST/SSE, auth, tenancy middleware, rate limiting, and storage readiness exist | Lifecycle, limits, result contracts, and request-local state are incomplete |
| Security | Non-weakenable policy floor, approvals, secret guardrails, and checkpoints | TUI shell escape bypasses the common tool policy; OS sandbox is not bound to production shell execution |
| Operations | Explicit subsystem close paths, retention controls, structured request logging, and race-enabled tests | Soak evidence for bounded growth is still pending |
| Evaluation | Deterministic token-efficiency gate and task-runner infrastructure | No valid paired coding-quality baseline; checked-in PPD results are correctly marked invalid |

## Competitive target matrix

| Capability | Claude Code public behavior | Grok Build public behavior | Chronos Code target |
|---|---|---|---|
| End-to-end coding | Plans, edits multiple files, runs commands, and verifies results | Model is optimized for agentic tool use and real-world coding tasks | Runtime-enforced plan/implement/verify/repair loop with fresh evidence |
| Parallel work | Subagents, background agents, and coordinated parallel sessions | Fast tool loops and parallel tool calling are product/model goals | Read-only fan-out plus isolated mutating worktrees with deterministic integration |
| Automation | Scriptable CLI, CI integrations, SDK, and cloud/background sessions | Structured output and function calling are supported | Versioned JSON/JSONL contracts, idempotency, deadlines, budgets, and resumable task status |
| Extensibility | Project instructions, skills, hooks, MCP, custom agents | Standard tool-oriented integrations | Capability-validated skills, resilient MCP discovery, documented fleet/backend patterns |
| UX | Plan review, diffs, approvals, session history, multiple surfaces | Responsiveness and rapid iteration are emphasized | Clear plan/verification/safety state, parallel activity, actionable recovery, stable headless output |
| Evaluation | Product claims are backed by internal and external workflows | xAI reports SWE-Bench and human evaluations for its model/harness | Reproducible repository tasks with hidden deterministic graders and matched external runs |

Public references:

- Claude Code overview: https://code.claude.com/docs/en/overview
- xAI Grok Build model documentation: https://docs.x.ai/docs/models/grok-code-fast-1
- xAI Grok Code Fast 1 announcement: https://x.ai/news/grok-code-fast-1

## Executive assessment

Chronos Code already has stronger foundations than a typical early coding harness: a shared CLI/TUI/HTTP execution boundary, durable sessions, bounded tool loops, safe output continuation, context compaction, incremental file reads, a type-checked Go graph, specialist agents, MCP discovery, approval policy, edit checkpoints, layered memory, and extensive race-enabled tests.

The primary problem is integration, not absence of components. Several mechanisms exist but do not form a closed autonomous loop. In particular, ordinary coding turns do not derive verification evidence from runtime tool events, enabled PPD can replace implementation with a read-only planning response, specialists share one checkout, and server execution shares mutable agent state. These defects matter more than adding another agent or tool.

## Gap analysis

| Priority | Area | Repository evidence | Production gap | Required outcome |
|---|---|---|---|---|
| P0 | Completion reliability | Verification policy and append-only evidence ledger exist, but normal CLI/TUI/HTTP requests do not populate obligations and events | An agent can claim completion without runtime-observed fresh build/test/diff evidence | Derive obligations from task kind and writes; record tool outcomes automatically; reject or explicitly mark unsupported completion |
| P0 | Complex-task routing | `Orchestrator.Execute` switches qualifying enabled PPD requests to the read-only `ppd-planner` | Complex implementation can terminate with a plan instead of a patch | Keep PPD shadowed by default until planner output is parsed, persisted, scheduled, executed, and returned to the primary loop |
| P0 | Prompt/runtime contract | Runtime prompts required `update_plan`, `fs_write`, and `fs_read`, which production does not install | Agents waste turns attempting impossible actions and lose plan continuity | Validate runtime-active prompts against installed capabilities and provide a real durable plan tool before requiring it |
| P0 | Server correctness | API-key startup omits required tenant identity; graceful shutdown and `MaxConcurrent` are not wired | Default server startup and unattended shutdown are unreliable | Validate complete server config, handle signals, drain requests, enforce concurrency and request deadlines |
| P0 | Concurrent request safety | Model routing mutates shared `agent.Agent` state outside a full-turn lease | Concurrent requests can leak model choice or mutable state | Make model/agent selection request-scoped or serialize unsafe shared execution |
| P0 | Shell security | TUI `!command` executes directly rather than through the registered shell tool | Plan mode, permission policy, hooks, audit, timeout, and sandbox can be bypassed | Route every shell path through one policy-enforced execution contract |
| P1 | Specialist isolation | Subagents have bounded concurrency and fresh conversation state but share the parent checkout | Parallel mutation can overwrite user or sibling work | Add per-task Git worktrees, patch handoff, conflict detection, crash manifests, and cleanup |
| P1 | Headless contract | `run --json` uses an ad hoc object while startup diagnostics can write to stdout | Automation cannot rely on one stable machine-readable stream | Add a versioned result envelope, typed errors, JSONL events, stderr-only diagnostics, and stable exit semantics |
| P1 | Resource limits | Call limits default to unlimited; token accounting resets after compaction; no overall execution deadline | Unattended tasks can run without an effective cumulative cap | Add durable per-task token/cost/call/time ceilings with explicit terminal reasons |
| P1 | Retention | Session rows, telemetry, edit snapshots, artifacts, logs, and rate-limit clients lack comprehensive pruning | Long-running hosts accumulate disk and memory state | Add YAML retention controls, dry-run/status commands, safe startup/periodic pruning, and metrics |
| P1 | Evaluation | Offline eval measures synthetic tool-output savings; PPD benchmark is explicitly invalid | Coding quality and autonomous recovery are unmeasured | Run versioned repository fixtures with hidden tests and report verified success, cost, calls, latency, retries, and unsupported claims |
| P2 | Skills | Discovery and BM25 selection are strong; `tools_required` and `model_hint` are advisory and one malformed skill can disable discovery | Selected skills may be impossible to execute | Validate capabilities, isolate malformed skills, explain selection, and publish an authoring contract |
| P2 | MCP | Namespacing, pooling, and policy are strong; discovery is all-or-nothing and docs diverge from schema | One malformed source can suppress healthy servers; operators receive misleading guidance | Isolate source errors, align docs, integrate watching, and retain per-agent connection status |
| P2 | TUI operations | Streaming, approvals, interruption, and bounded rendering are mature | Parallel status is incomplete and permission mode is not prominent | Show all active specialists, current safety mode, budgets, verification state, and actionable recovery |
| P2 | Graph coverage | Go graph is type-aware; non-Go support is syntactic and release builds differ from full builds | Confidence and available tools vary by language/build artifact | Expose graph confidence, make release coverage explicit, and fall back to source evidence automatically |

## Delivery principles

1. Correctness and safety gates are constraints; token and latency reduction are optimization objectives.
2. A feature is not production-ready until CLI, TUI, and HTTP share its semantics and tests.
3. Parallelism follows file ownership and integration contracts. Read-only fan-out can share a checkout; mutation requires isolation.
4. New autonomous behavior starts in report or shadow mode and advances only with measured evidence.
5. Every limit ends with a typed, observable result rather than silent truncation or reset.
6. Graph and memory output are context candidates, not authority; current source and executed checks remain proof.

## Priority model

Work is ordered by the following rule:

1. Prevent false success, data loss, security bypass, and request cross-talk.
2. Close the normal coding loop before adding more orchestration modes.
3. Isolate mutations before increasing parallelism.
4. Bound unattended operation before advertising server scale.
5. Add retention and observability before long-running fleet deployment.
6. Gate competitive claims on verified task evidence.

Priority definitions:

- `P0`: can produce an incorrect result, unsafe mutation, broken default, or cross-request corruption.
- `P1`: required for reliable unattended use and meaningful coding-quality improvement.
- `P2`: required for ecosystem maturity, broad language support, and polished operation.
- `P3`: optimization after correctness and measurement are established.

## Phased roadmap

### Phase 0: stop correctness regressions

Target: safe zero-config behavior and honest capability contracts.

- Keep PPD in shadow mode until its planner output reaches the durable scheduler and implementation loop.
- Remove required references to unavailable tools from runtime-active prompts.
- Make coder guidance evidence-sensitive: targeted reads and tests by default, broader checks when risk or graph uncertainty requires them.
- Add startup validation for required prompt capabilities.
- Route TUI shell escape through the ordinary tool permission path.

Exit gate: a complex implementation request cannot silently become planning-only; prompts require only installed tools; security-negative and default-contract tests pass under `-race`.

### Phase 1: close the coding loop

Target: plan, implement, verify, repair, and finish based on runtime evidence.

- Derive verification obligations when a coding task writes files.
- Observe file writes, shell commands, diagnostics, targeted tests, builds, and final diff in the execution ledger.
- Invalidate earlier verification after subsequent writes.
- Add a bounded repair loop with explicit retry categories and no whole-task replay after side effects.
- Implement a real request-scoped plan service or remove plan-tool language entirely.

Exit gate: seeded multi-file bug fixes and features cannot report success without fresh evidence; failed checks either trigger bounded repair or produce a typed blocked/failed result.

### Phase 2: isolate and parallelize

Target: safe specialist concurrency for real coding work.

- Add a Git worktree manager with task IDs, private temp roots, base revision capture, and recovery manifests.
- Run read-only specialists against the parent checkout and mutating specialists in isolated worktrees.
- Return structured patch, verification, changed-path, and base-revision results.
- Apply or merge results only after conflict and stale-base checks.
- Clean worktrees on success/cancel and prune crash leftovers by retention policy.

Exit gate: two concurrent mutating specialists cannot alter the same checkout; cancellation leaves no registered worktree; conflicts do not modify the parent tree.

### Phase 3: unattended execution and server hardening

Target: bounded, recoverable operation across CLI and HTTP.

- Make routing and model choice request-scoped.
- Enforce server concurrency, request-body limits, execution deadlines, graceful drain, and panic recovery.
- Add durable cumulative token/cost/call budgets and idempotency keys.
- Validate API-key/OIDC tenant configuration before listening.
- Add structured request logs, correlation IDs, retry metadata, and health/readiness distinctions.

Exit gate: race tests cover concurrent sessions and model routes; SIGTERM drains or cancels within a fixed deadline; budget and timeout exits are deterministic.

### Phase 4: operational lifecycle

Target: bounded disk/memory use and diagnosable recovery.

- Add retention controls for sessions, traces, telemetry, memory, checkpoints, artifacts, worktrees, logs, and limiter entries.
- Provide `cleanup status`, `cleanup run --dry-run`, and scoped prune operations.
- Make exports atomic and private by default.
- Surface dropped telemetry, cleanup failures, and stale runtime resources.
- Add rate-limit and provider-failure backoff without replaying committed mutations.

Exit gate: a soak test demonstrates bounded growth; interrupted cleanup is idempotent; recovery preserves active sessions and user work.

### Phase 5: automation and ecosystem

Target: stable integration for CI, orchestrators, and reusable extensions.

- Define one versioned execution envelope for CLI JSON and HTTP.
- Add ordered JSONL events for content, tools, subagents, usage, verification, errors, and completion.
- Enforce or clearly diagnose skill tool/model requirements; isolate malformed skills.
- Align MCP docs and schema, isolate malformed sources, and integrate bounded hot reload.
- Publish CI, backend orchestration, and fleet deployment examples with safe permission profiles.

Exit gate: schema fixtures remain backward compatible; CI can distinguish success, blocked approval, retryable provider failure, budget exhaustion, and verification failure without parsing prose.

### Phase 6: measured competitiveness

Target: improve only where verified task evidence supports the change.

- Build a versioned task corpus spanning multi-file features, debugging, refactors, tests, recovery, and non-Go repositories.
- Grade patches with hidden tests or deterministic artifact assertions.
- Record verified success, tokens per success, model/tool calls, wall time, cost, retries, and unsupported claims.
- Run ablations for graph context, skills, specialists, PPD, verification repair, and worktree parallelism.
- Compare external agents only under matched repository revision, model class, permissions, timeout, and success criteria.

Exit gate: release decisions cite reproducible reports; no market claim is based on synthetic token compression alone.

## Exact work packages

The IDs below define implementation order and dependencies. A package is complete only when its acceptance tests pass and its user-facing behavior is documented.

### P0.1 Runtime capability contract

Depends on: none.

Files:

- `internal/defaults/agents/*.yaml`
- `internal/defaults/catalog.go`
- `internal/defaults/catalog_test.go`
- `internal/orchestrator/capabilities.go`
- `internal/orchestrator/orchestrator_test.go`

Actions:

1. Enumerate installed tools, planning services, graph profile, LSP profile, MCP namespaces, and writable execution modes at startup.
2. Validate runtime-active prompt requirements against that manifest.
3. Fail configuration validation for required nonexistent tools; warn only for optional capabilities.
4. Keep PPD `shadow` unless the closed-loop PPD capability is present.

Acceptance:

- Embedded defaults validate with zero warnings.
- A fixture requiring a nonexistent planning or write tool fails before a model call.
- Export-only YAML is never treated as runtime capability evidence.

### P0.2 Runtime verification ledger

Depends on: P0.1.

Files:

- `internal/execution/ledger.go`
- `internal/verification/policy.go`
- `internal/orchestrator/orchestrator.go`
- `internal/orchestrator/tool_pipeline_test.go`
- `internal/integration/surfaces_test.go`

Actions:

1. Allocate one ledger per task and attach it to the execution context.
2. Record successful file writes with normalized workspace-relative paths and content hashes.
3. Record shell checks with command class, exit status, affected scope, timestamp, and post-write revision.
4. Derive obligations from task kind and observed mutations instead of trusting adapters.
5. Mark evidence stale after a later overlapping write.
6. Return the verification decision and unmet obligations in `ExecutionResult`.
7. Keep `report` as the default rollout; make `enforce` reject unsupported success identically across CLI, TUI, and HTTP.

Acceptance:

- A write followed by no check reports unmet verification.
- A passing test followed by a write is stale.
- A failing test cannot satisfy an obligation.
- Blocking and streaming paths produce the same final decision.
- Existing caller-supplied events are either removed or explicitly marked trusted test/adapter input.

### P0.3 Bounded repair loop

Depends on: P0.2.

Files:

- `internal/orchestrator/orchestrator.go`
- `internal/apierror/apierror.go`
- `internal/config/config.go`
- `internal/defaults/config.yaml`
- `internal/orchestrator/execution_test.go`

Actions:

1. Add YAML limits for repair attempts, model calls, tool calls, wall time, tokens, and USD.
2. Continue in the same durable session after verification failure; never replay the original turn after side effects.
3. Supply only failed obligations, relevant diagnostics, changed paths, and remaining limits to the repair prompt.
4. Stop on success, non-retryable policy/auth errors, repeated identical failure, cancellation, or exhausted limits.
5. Emit a typed terminal reason for every stop.

Acceptance:

- A seeded fix that initially fails a test gets one bounded repair and passes.
- An unchanged repeated failure stops without a third identical attempt.
- Cancellation and budget exhaustion prevent further provider/tool calls.
- No mutation executes twice because of outer-loop replay.

### P0.4 Server safety and request isolation

Depends on: none for containment; P1.2 for parallel execution above one.

Files:

- `internal/cli/root.go`
- `internal/config/config.go`
- `internal/server/server.go`
- `internal/server/middleware.go`
- `internal/server/server_test.go`
- `internal/orchestrator/orchestrator.go`

Actions:

1. Wire tenant, OIDC, concurrency, rate, and listen configuration from YAML, environment, and CLI in documented precedence order.
2. Handle SIGINT/SIGTERM and drain in-flight requests with a fixed shutdown deadline.
3. Default `max_concurrent` to one while model and agent selection mutate shared state; reject excess execution requests with a structured retryable response.
4. Add body limits, strict JSON decoding, panic recovery, execution deadlines, and correlation IDs.
5. Replace shared model mutation with request-scoped provider selection, then permit safe configured concurrency.
6. Add idempotency keys before automatic client retries are recommended.

Acceptance:

- API-key mode starts only with both key and tenant and returns actionable validation errors otherwise.
- SIGTERM stops acceptance and drains or cancels within the deadline.
- Concurrency never exceeds the configured cap.
- Race tests issue different model/session requests concurrently without cross-talk.
- Health remains available while execution capacity is full; readiness reports draining state.

### P0.5 Unified shell security

Depends on: P0.1.

Files:

- `internal/tui/app.go`
- `internal/security/permissions.go`
- `internal/security/sandbox.go`
- orchestrator tool registration and shell tests

Actions:

1. Remove direct `exec.CommandContext` from the TUI shell-escape path.
2. Invoke the same registered shell service used by agents.
3. Apply policy, approval, timeout, hooks, audit, plan mode, and optional OS sandbox identically.
4. Redact command arguments in audit records using one common redactor.

Acceptance:

- A `never_allow` command is denied through both model and TUI paths.
- Plan mode blocks shell escape.
- Timeout and cancellation terminate the child process tree.
- Audit output contains no configured secret fixture.

### P1.1 Durable planning integration

Depends on: P0.1, P0.2, P0.3.

Files:

- `internal/plan/*`
- `internal/router/ppd.go`
- `internal/orchestrator/orchestrator.go`
- `internal/integration/delegation_test.go`

Actions:

1. Define a strict planner output schema and reject prose-only plans for execution.
2. Parse, validate, and persist a generation-scoped DAG.
3. Execute nodes through the ordinary bounded execution contract.
4. Feed node evidence into the task ledger and run final synthesis through the primary agent.
5. Resume from persisted leases/attempts after interruption.
6. Promote PPD from `shadow` only after benchmark and recovery gates pass.

Acceptance:

- A multi-package fixture produces a persisted DAG, patch, and fresh verification.
- Restart resumes incomplete nodes without rerunning committed effects.
- A planner-only response can never be returned as successful implementation.

### P1.2 Request-scoped runtime state

Depends on: P0.4 containment.

Files:

- `internal/orchestrator/orchestrator.go`
- Chronos agent request/provider seam in the sibling repository
- `internal/server/scaling_test.go`

Actions:

1. Make requested agent, model/provider, permission profile, and routing metadata immutable per execution context.
2. Stop assigning request-selected providers to shared `agent.Agent.Model`.
3. Serialize only same-session history mutation, not unrelated agents or sessions.
4. Coordinate top-level and delegated leases through one runtime scheduler.

Acceptance:

- Concurrent requests selecting different models use the intended provider on every call under `-race`.
- Same-session turns are ordered; different sessions can overlap.
- Subagent execution cannot race a top-level turn on the same mutable agent/session.

### P1.3 Git worktree isolation

Depends on: P1.2.

New package: `internal/worktree`.

Integrations:

- `internal/orchestrator/subagents.go`
- `internal/plan/controller.go`
- `internal/config/config.go`
- `internal/cli/` cleanup/status commands

Actions:

1. Capture repository root, HEAD, dirty-state policy, and task ID before delegation.
2. Create private worktrees under the Chronos Code data directory with collision-safe branch/ref names.
3. Mount each mutating specialist on its own workspace and storage namespace.
4. Return base revision, changed paths, diff, checks, and verification evidence as structured output.
5. Apply patches only after stale-base, overlap, symlink, and conflict checks.
6. Persist a cleanup manifest before creation; remove worktree and refs on success/cancel; prune expired crash leftovers.

Acceptance:

- Two mutating specialists never write the parent checkout directly.
- Conflicting patches leave the parent unchanged and return a typed conflict.
- Cancellation and simulated process death leave a discoverable, safely prunable manifest.
- Existing user changes are neither included nor discarded unless explicitly selected.

### P1.4 Stable autonomous result contract

Depends on: P0.2, P0.3, P0.4.

Files:

- `internal/orchestrator` result types
- `internal/cli/root.go`
- `internal/server/handlers.go`
- new schema fixtures under `internal/integration/testdata`

Actions:

1. Define a versioned envelope containing task, session, agent, status, stop reason, content, usage, cost, changed paths, verification, and typed error.
2. Send diagnostics to stderr and reserve stdout for JSON in headless mode.
3. Add JSONL events for content, tool lifecycle, subagents, retries, usage, verification, and completion.
4. Define stable exit codes and HTTP status mapping for success, blocked approval, invalid request, retryable provider error, timeout, budget exhaustion, and verification failure.

Acceptance:

- `run --json` emits exactly one valid document on stdout.
- JSONL event ordering and terminal event are deterministic.
- CLI and HTTP fixtures deserialize into the same schema version.

### P1.5 Retention and cleanup controller

Depends on: P1.3 for worktree cleanup.

Files:

- new `internal/retention` package
- storage adapters and migration files
- `internal/config/config.go`
- `internal/cli/cleanup_cmd.go`
- startup/periodic server lifecycle

Actions:

1. Inventory sessions, events, traces, audit logs, telemetry, memory, compressed results, checkpoints, input artifacts, logs, limiter entries, and worktrees.
2. Add independent age/count/byte policies with safe defaults and `0` meaning explicitly documented behavior.
3. Implement transactional or rename-first deletion, active-resource exclusion, dry-run reports, and bounded batches.
4. Run lightweight startup recovery and periodic server cleanup.
5. Expose reclaimed bytes, failures, and oldest retained resource.

Acceptance:

- A generated soak fixture remains within configured bounds.
- Cleanup is idempotent after interruption.
- Active sessions/worktrees and user source files are never removed.

### P2.1 MCP and skills hardening

Depends on: P0.1.

Actions:

1. Parse MCP sources independently and retain healthy/last-known-good servers when one source is malformed.
2. Integrate bounded watch/reload with per-agent status and reconnect backoff.
3. Validate skill `tools_required`, model hints, size, and referenced resources against the capability manifest.
4. Isolate malformed skills and expose selection rationale without injecting it into model context.
5. Generate MCP and skill examples from parser fixtures so documentation cannot drift.

Acceptance:

- One malformed MCP or skill file cannot disable unrelated valid entries.
- Reload failure preserves the previous healthy runtime.
- Every bundled example is parsed in CI.

### P2.2 TUI operational polish

Depends on: P0.2, P0.5, P1.2.

Actions:

1. Keep safety mode, remaining limits, plan state, and verification state visible.
2. Show all active specialists and worktree identities without unbounded rendering.
3. Add actionable retry/resume controls for typed terminal reasons.
4. Persist bounded command history and rotate private TUI logs.
5. Show VCS-derived changes separately from checkpoint history.

Acceptance:

- Cancellation, resize, approval, and parallel activity tests remain race-clean.
- The UI never labels a verification-blocked task successful.

### P2.3 Observability and fleet readiness

Depends on: P1.4, P1.5.

Actions:

1. Add structured logs with request/task/session/tenant correlation and redaction.
2. Publish metrics for active/queued work, provider latency/retries, tool failures, budgets, verification, MCP health, cleanup, and disk use.
3. Separate liveness, readiness, and draining state.
4. Document single-host, CI, backend-orchestrated, and multi-instance deployment patterns.
5. Pin the sibling Chronos revision and add release provenance/SBOM/signing.

Acceptance:

- Operators can explain any terminal task result from logs and metrics without prompt contents.
- A multi-instance test demonstrates explicit session affinity or durable ownership.

### P1.6 Coding-quality evaluation harness

Depends on: P0.2 and P1.4; runs continuously through later phases.

Files:

- `internal/eval/taskrunner.go`
- `internal/eval/grader.go`
- `benchmark/tasks/`
- CI nightly workflow and versioned reports

Actions:

1. Implement a production Chronos Code adapter using isolated fixture clones.
2. Start with deterministic Go bug-fix, multi-file feature, refactor, and test-generation tasks; then add TypeScript, Python, and Rust.
3. Grade with hidden tests and artifact assertions, not model self-report.
4. Record success, unsupported completion, tokens per success, cost, wall time, model/tool calls, retries, and repair attempts.
5. Run ablations for graph, skills, specialists, verification repair, and worktrees.
6. Add matched Claude Code and Grok Build runs only where licensing, automation interfaces, models, permissions, timeout, and repository revision can be controlled.

Acceptance:

- Reports are reproducible from a pinned task manifest and include failed attempts.
- No feature is promoted from shadow/report mode based only on synthetic token savings.
- Regression thresholds use confidence intervals or repeated-run variance, not a single stochastic run.

## Evaluation ladder

| Level | Purpose | Gate |
|---|---|---|
| L0 | Unit contracts | Deterministic package tests and race detector |
| L1 | Runtime integration | Fake-provider CLI/TUI/HTTP parity, cancellation, retries, and evidence freshness |
| L2 | Repository fixtures | Isolated real Git repositories with hidden tests and patch grading |
| L3 | Soak and fault injection | Provider rate limits, tool failures, cancellation, restart, disk pressure, and concurrent sessions |
| L4 | Matched external comparison | Same task revision, model class, permissions, timeout, warm/cold context policy, and grader |

Release-blocking metrics after L2 is stable:

- Verified task success rate.
- Unsupported-success rate; target is zero.
- Regression rate on previously passing tasks.
- Tokens and cost per verified success.
- Median and tail wall time per verified success.
- Recovery success after an injected transient failure.
- Residual worktrees/artifacts and disk growth after cleanup.

## Implementation order

1. Finish P0.1 capability validation.
2. Implement P0.2 runtime verification evidence.
3. Add P0.3 bounded repair.
4. Complete P0.4 server limits and P0.5 shell unification in parallel.
5. Build P1.6 evaluation fixtures while the loop stabilizes.
6. Integrate P1.1 durable planning only after verification and repair are real.
7. Complete P1.2 request-scoped state before raising server concurrency.
8. Add P1.3 worktrees before enabling mutating specialist fan-out.
9. Stabilize P1.4 machine contracts and P1.5 retention before fleet documentation.
10. Deliver P2 ecosystem, TUI, and observability work, then run L4 comparisons.

## Implementation status

### Slice A: runtime prompt and routing contract

The current working tree contains these runtime-active default and contract changes:

- Bundled PPD mode is `shadow`; explicit configurations can still choose `enabled`.
- Primary and coder prompts no longer require unavailable planning or scratch-file tools.
- Coder guidance no longer treats graph output as infallible or narrowly targeted tests as always sufficient.
- Tests lock the PPD rollout default and reject reintroduction of unavailable planning-tool requirements.

Durable PPD execution and runtime verification are now implemented behind their rollout gates. PPD remains in `shadow` until valid benchmark evidence permits promotion.

P0.1 runtime capability validation was completed on 2026-09-18:

- Startup now builds an agent-scoped manifest from callable tool registrations and live graph, LSP, MCP, planning-mode, and write capabilities.
- Every configured agent tool must resolve to a supported runtime implementation; export-only tool examples do not satisfy the contract.
- Additional required and optional capabilities can be declared under `runtime_capabilities` in `config.yaml`.
- Missing required capabilities fail startup before project-document compression or another model call; optional misses produce deterministic warnings.
- Explicit PPD `enabled` mode is rejected until `planning:closed-loop-ppd` is implemented. The default remains `shadow`.
- Embedded defaults and missing-capability startup fixtures are covered by focused tests.

The dependency-ordered runtime ledger, repair, isolation, and result-contract packages are implemented in the current working tree.

### Batch C: runtime evidence, bounded repair, and containment

Implementation is present in the working tree and passed the consolidated test phase:

- Task ledgers now carry typed write/check evidence, normalized scopes, hashes, timestamps, provenance, and mutation revisions.
- File and shell tools feed runtime evidence through the common tool pipeline; unknown shell commands conservatively invalidate workspace evidence.
- Verification obligations are derived from observed mutation and final decisions are available to blocking and streaming callers.
- Repair limits cover attempts, model calls, tool calls, wall time, tokens, and cost; repair continues in the same session with a bounded failure-only prompt.
- Repeated identical verification failure stops without another model call and terminal reasons are typed.
- HTTP handlers now have strict bounded JSON decoding, deadlines, panic recovery, correlation IDs, and draining readiness.
- TUI shell escape now uses the registered shell tool and the ordinary hook, policy, approval, and evidence path.

These behaviors passed the repository-wide race suite on 2026-09-18.

### Slice B: HTTP safety containment

Implemented on 2026-09-18:

- `serve` now resolves API-key tenant identity and OIDC settings from CLI, environment, or YAML-backed server configuration.
- SIGINT/SIGTERM now enters the existing ten-second graceful shutdown path.
- Agent execution endpoints enforce `max_concurrent`; the conservative default remains one even though provider selection is now request-scoped.
- Excess execution requests return HTTP 429 with `Retry-After: 1`; health and non-execution endpoints remain available.
- Invalid concurrency and rate-limit flags fail with actionable errors.
- Permission-setup failure closes the partially built orchestrator.

Verification performed:

```text
go test ./internal/server ./internal/config ./internal/cli
go test ./... -race -count=1
cd docs && npm run build
```

Result: passed. The documentation build emitted the existing warning that the inline author in `2025-01-01-welcome.md` is not declared in `authors.yml`; it did not fail the build and is unrelated to this slice.

The previously listed P0.4 items are implemented and passed the consolidated test run.

### P2.3 observability and fleet readiness

Implementation is present in the working tree and passed package, race, binary, and documentation verification:

- JSON request/task logs carry correlation, task, session, tenant, and agent identifiers without prompt or tool payloads.
- The authenticated Prometheus endpoint reports work, provider, tool, budget, verification, MCP, cleanup, and disk-use state.
- Liveness, readiness, and draining are independent endpoints; draining rejects new execution work.
- Multi-instance mode uses an explicit deterministic affinity contract and rejects a session on every non-owner node before execution.
- Deployment guidance covers single-host, CI, backend-orchestrated, and fleet modes with conservative permission profiles.
- Release automation creates an SPDX SBOM, SHA-256 checksums, GitHub provenance, and a keyless Sigstore signature and verification.

Release remains externally blocked until the local Chronos changes on top of `62817f39b00bc8ee860ec8a0dbd37e5d6065dcfd` are published as a compatible revision. The workflow rejects the explicit sentinel in `release/chronos.version` rather than silently consuming an arbitrary local sibling checkout.

### Consolidated verification

Completed on 2026-09-18:

```text
chronos:      go test ./... -race -count=1
chronos-code: go test ./... -race -count=1
chronos-code: make build
chronos-code: go test -tags lsp ./internal/lsp ./internal/orchestrator ./internal/tui
docs:         npm run build
evaluation:   go run ./cmd/chronos-code eval tasks --validate-only --manifest benchmark/tasks/manifest-v1.yaml
```

All commands passed. The docs build retained the existing non-fatal inline-author warning. The evaluation command validated four deterministic task manifests; it did not make an external-agent performance claim.
