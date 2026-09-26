# Autonomous delivery: agent-executable implementation guide

Status: implementation specification; tasks below are initially unimplemented.

This is the single implementation guide for the repository reviews, principal-engineer working model, ten research proposals, subagent redesign, and headless/TUI delivery roadmap. An implementing agent must be able to start here without the originating conversation.

**Product objective:** a user can say “build this feature end-to-end; ask me only for consequential decisions or genuine blockers,” disconnect, return later, and inspect a verified result. Routine execution-window limits must checkpoint and continue rather than terminate the delivery with `budget_exhausted`.

**Engineering principle:** enforce authority, artifact identity, evidence, and completion invariants; adapt the choice and order of engineering actions.

## Contents

1. [Execution instructions for implementing agents](#1-execution-instructions-for-implementing-agents)
2. [Baseline and code map](#2-baseline-and-code-map)
3. [Architecture and invariants](#3-architecture-and-invariants)
4. [Long-horizon budget and recovery contract](#4-long-horizon-budget-and-recovery-contract)
5. [Data, events, configuration, and surface contracts](#5-data-events-configuration-and-surface-contracts)
6. [Dependency-ordered implementation tasks](#6-dependency-ordered-implementation-tasks)
7. [Ten research implementation tasks](#7-ten-research-implementation-tasks)
8. [Verification and release gates](#8-verification-and-release-gates)
9. [Coverage matrix](#9-coverage-matrix)
10. [Execution journal and resume handoff](#10-execution-journal-and-resume-handoff)

## 1. Execution instructions for implementing agents

### 1.1 Start or resume

1. Read repository instructions in both repositories, this section, the task index, and the latest journal entry. Do not assume the original review is still current.
2. Inspect working-tree status and current revisions in Chronos Code and its resolved Chronos dependency. Preserve existing edits and untracked files. Do not reset, overwrite, or commit user work.
3. Select the first unchecked task whose dependencies are verified. Complete tasks in dependency order; independent work is allowed only with isolated artifacts and the host's delegation permission.
4. Read the selected task's implementation targets and their tests. Use graph navigation when available, then targeted source reads. Graph absence, stale summaries, or missing tools are reasons to inspect source, not to stop.
5. Record the active task and concrete acceptance criteria in section 10. Implement the smallest coherent slice of that task. If a task needs multiple sessions, record substeps under that task without creating a separate planning system.
6. Run the targeted checks, inspect the actual outputs and diff, and record evidence. A mock-only test cannot establish production startup, transport wiring, or crash recovery.
7. Mark a task complete only when its acceptance criteria, integration, and required checks pass. Record unrelated baseline failures explicitly; do not claim that an unverified gate passed.
8. Continue to the next ready task. Stop only for an actual authority/input blocker, an unsafe/unknown external effect, or a host-imposed execution boundary. Leave a recoverable handoff in all cases.

The document itself does not change today's runtime or extend the implementing agent's host context, API quota, or execution limits. Before autonomous continuation is implemented, checkpoint implementation progress in section 10 and resume from that checkpoint. Never present a planned recovery mechanism as already running.

### 1.2 Change discipline

- Prefer current implementations over new parallel subsystems. Reuse Chronos queue, graph, tool registry, and storage contracts where suitable.
- Keep generic runtime features in Chronos; keep repository analysis, engineering policy, delivery composition, and TUI/CLI adapters in Chronos Code.
- Follow `context.Context`-first APIs, explicit initialization, wrapped errors, and existing package style. Keep policy in YAML and enforcement in Go.
- Do not silently discard unknown configuration, weaken tests, disable checks, remove budgets, auto-approve tools, or mark blocked work successful.
- Commit, push, deploy, or change infrastructure only with explicit authorization. Private runtime artifact snapshots are distinct from publishing changes to a user's branch.
- Do not use `.ppd/` or introduce a second implementation-plan format merely because the product implements PPD. This guide and its journal are the implementation checklist.
- Do not spawn agents unless the current host instructions permit delegation. When delegating, provide exact scope, inputs, expected output, tests, and non-overlapping artifact ownership.
- If an interface already exists under a different name, extend it and document the mapping. Names labeled **proposed** are design targets, not evidence of existing APIs.

### 1.3 Definition of task completion

Each completed task needs: code, production wiring, error/recovery behavior, relevant automated checks, configuration/schema compatibility where applicable, operator-visible semantics, and a journal entry identifying revisions and commands. Research tasks additionally need a reproducible evaluation; implemented-but-unmeasured features remain experimental.

### 1.4 Copyable execution request

> Execute `docs/autonomous-delivery-implementation.md`. Inspect the current state of both repositories and preserve user work. Resume from section 10 and choose the first ready task. Implement, verify, and journal each coherent increment before moving on. Treat the schemas in this guide as proposed until their implementation passes. Do not skip prerequisites or remove safety/accounting to avoid budget errors. Keep the primary responsible for integration. Continue across renewable work windows once that capability is implemented; otherwise leave an exact checkpoint before the host boundary. Report only genuine blockers, consequential decisions, verified milestones, and the final result.

## 2. Baseline and code map

### 2.1 Baseline to revalidate

The reviews examined published Chronos Code `703afb1e3904817cfea9121a177371e579552112` with Chronos `49cdb6a6cbd0f2d8a1a823b628707bb441c50083`. At creation of this guide, the local checkouts were Chronos Code `557f3c6d8a70eb3c1ab6992a25b39f4bedeced0f` and Chronos `ce3bbc0657c0e034befd9729639777851aa0a22a`.

Existing local work included edits to `internal/orchestrator/orchestrator.go`, `internal/server/server.go`, and untracked `internal/server/plan_handlers.go`, `internal/server/plan_handlers_test.go`, and `docs/plan-http-api.md`. Investigate and extend this work; do not replace it from the review checkout. See [plan HTTP control documentation](plan-http-api.md): current controls operate on existing plans, do not submit jobs, and do not create a background worker.

Historical verification: the reviewed published Chronos Code race suite, Tree-sitter graph race tests, selected Chronos runtime suites, and queue race tests passed. Those results are not acceptance evidence for subsequent implementations or this local baseline.

### 2.2 Existing implementation targets

Paths below are repository-relative. `Chronos:` denotes paths in the resolved foundation repository, currently the sibling `../chronos` through `go.mod`.

| Area | Existing targets |
| --- | --- |
| Execution/identity | `internal/orchestrator/orchestrator.go`, `subagents.go`, `direct_tool.go`, `capabilities.go`, `operational_snapshot.go` |
| Budgets/results | `internal/execution/task_budget.go`, `result.go`, `envelope.go`; `internal/budget/budget.go`; `internal/orchestrator/verification_runtime.go`, `repair.go`, `context_guard.go`, `envelope.go` |
| Agent configuration | `internal/defaults/agents/*.yaml`, `config.yaml`, `routing.yaml`, `security.yaml`, `learning.yaml`; `internal/defaults/catalog.go`; `internal/config/config.go`, `paths.go` |
| Context/navigation | `internal/graph/`, `internal/incctx/`, `internal/activation/`, `internal/attention/`, `internal/toolcompress/`, `internal/projectdocs/`, `internal/lsp/` |
| Plans | `internal/plan/model.go`, `sqlstore.go`, `scheduler.go`, `controller.go`, `context.go`, `decompose.go`, `admission.go`; `internal/orchestrator/plan_runtime.go`, `plan_executor.go` |
| Artifacts/undo | `internal/worktree/manager.go`, `integrate.go`, `manifest.go`, `result.go`; `internal/orchestrator/edit_checkpoints.go`, `session_ux.go` |
| Memory/learning | `internal/memory/`, `internal/learning/`, `internal/session/`; `internal/orchestrator/runtime_memory.go`; `internal/skills/` |
| Permissions/trust | `internal/security/`, `internal/guardrail/`, `internal/mcpdiscover/`, `internal/auth/` |
| Surfaces | `internal/cli/root.go`, `plan_cmd.go`; `internal/tui/app.go`, `approval.go`, `operational.go`; `internal/server/` |
| Evaluations/operations | `internal/eval/`, `internal/integration/`, `internal/observability/`, `internal/retention/`, `benchmark/`, `.github/workflows/`, `Makefile` |
| Chronos runtime | Chronos: `sdk/agent/`, `sdk/harness/`, `sdk/team/`, `sdk/memory/`, `engine/graph/`, `engine/queue/`, `engine/tool/`, `engine/model/`, `storage/` |

### 2.3 Findings to reproduce before fixing

- Request-scoped parent providers can override configured specialist providers.
- Incremental reads close over the startup workspace rather than honoring an isolated invocation workspace.
- Shell permission checks can auto-allow `go test ./...; git push` while confirming `git push` alone. Reproduce with permission evaluation, not a real push.
- Lexical path checks can authorize a symlink beneath the root that points outside it. Default shell working-directory confinement is not filesystem/network containment.
- MCP startup trust is name-based; plan-mode mutation checks cover a narrow set of tool names.
- Configured delegation registers every peer, rather than enforcing each parent's declared child capabilities.
- Closed-loop PPD exists internally but capability admission and production callers do not establish a usable complete loop. Do not simply toggle the capability on.
- Plan heartbeat validates ownership without a timed renewal. Chronos queue has a real timed lease/heartbeat model.
- Worktree creation uses `HEAD`, while integration applies uncommitted parent changes. A dependent task needs an explicit accepted predecessor snapshot.
- Chat history/traces, plan state, edit snapshots, and an in-memory verification ledger do not constitute one operation-safe crash-recovery protocol.
- `routing.yaml` escalation/pipeline sections are descriptive rather than executable; model/tool tiers are conflated in some prompts.
- Memory/learning systems overlap; procedure applicability, correction attribution, and revision invalidation are incomplete.
- Existing token-efficiency fixtures and unexecuted PPD experiment records do not prove real-model autonomous delivery quality.

## 3. Architecture and invariants

### 3.1 One durable delivery, multiple bounded attempts

```text
CLI / TUI / HTTP
       |
authenticated admission + versioned goal + authority
       |
durable delivery record / ordered events / cumulative usage
       |
primary supervisor <-> decision inbox / specialist mailboxes
       |
rolling work graph -> Chronos queue -> bounded attempt
                                      |
                         run-bound tools + isolated snapshot
                                      |
                     operation journal + verification receipts
                                      |
                         accepted artifact / integration
                                      |
                completion certificate + knowledge capture
```

The primary owns goals, decisions, accepted claims, and synthesis. Specialists own bounded tasks and candidate artifacts. Workers own temporary execution leases. Neither a worker nor a model owns permission to redefine acceptance or grant itself authority.

### 3.2 Non-negotiable invariants

1. **Identity:** tenant, repository, delivery, node, attempt, goal revision, artifact snapshot, and policy revision come from the host, not model arguments.
2. **Workspace consistency:** reads, writes, search, graph, LSP, shell, hooks, verification, and undo use the same admitted workspace/snapshot. Missing isolated analysis falls back to isolated source, never the parent checkout.
3. **Authority:** child capabilities are an explicit authorized subset/delegation grant. A read-only parent cannot obtain undeclared write authority through another agent.
4. **Evidence:** a claim is distinct from an observation. Verification records the executed command/check, environment, result, and exact artifact identity.
5. **Artifact lineage:** dependency-ready nodes receive accepted predecessor artifacts, not merely predecessor status bits.
6. **Durability:** acknowledge admission, decisions, and completion only after their required durable writes. A dropped telemetry event cannot be the only record of a critical effect.
7. **Ownership:** stale workers cannot publish accepted results or integrate patches after losing ownership. Cancellation is not a substitute for fencing.
8. **Recovery:** unknown effect outcomes require reconciliation; never restart a whole task just because one tool/model call failed.
9. **Accounting:** record real incurred usage exactly once, including failed requests when usage is available, summaries, specialists, retries, repair, and speculative candidates. Unknown usage is explicit.
10. **Completion:** only current accepted artifacts satisfying every accepted requirement, or an explicit authorized waiver, can produce success.
11. **Continuity:** a new execution window changes attempt/window identity, not the delivery identity, cumulative spend, acceptance contract, or evidence history.
12. **Human agency:** requirements, grants, and consequential decisions are versioned and attributable. Silence is not approval unless an already authorized reversible default applies.

### 3.3 Principal-engineer behavior to encode

- Form questions and behavioral hypotheses before broad exploration; use graph outputs as leads with coverage/freshness information.
- Choose reads, experiments, delegation, or questions by expected decision value rather than ritualistic tool order.
- Plan around uncertainty, compatibility, and reviewable delivery slices; spikes and design decisions are legitimate work.
- Preserve rejected alternatives and why they failed. Remember applicability and counterexamples, not just successful prose.
- Continue independent safe work while a decision is pending. Replan only the affected frontier when requirements change.
- Measure success by verified delivered outcomes, not tool count, generated code, number of agents, or prompt brevity.

## 4. Long-horizon budget and recovery contract

### 4.1 User-visible requirement

In the new autonomous-delivery mode, exhausting a renewable attempt/window allowance must **not** emit a terminal `budget_exhausted`, a red error modal, or a failed delivery. It must save progress, relinquish ownership safely, and continue through a fresh bounded window. Foreground callers remain attached; detached callers can reconnect to the same delivery ID.

This is a real scheduling/recovery change, not an error-string rename. A delivery must never claim to be continuing unless continuation has been durably scheduled or a live worker owns it.

### 4.2 Separate four kinds of limits

| Limit | Examples | Required action |
| --- | --- | --- |
| Per-request safety | Context window, output allowance, tool output bounds, subprocess timeout | Fit/compact/split before the request; reconcile timed-out effects; retry only safe requests |
| Renewable work window | Model calls, tool calls, active compute time, working token allowance | Checkpoint and yield; automatically admit another window within authorized delivery policy |
| Cumulative delivery accounting | Total input/output/cache tokens, retries, elapsed/active time, known/unknown cost | Persist and display; never reset when compacting, delegating, retrying, or resuming |
| Explicit authority ceiling | User spending cap, hard deadline, forbidden effect, tenant quota | Do not exceed deliberately; park in a durable waiting state with a specific decision/action needed |

Do not set every existing limit to zero. Retain bounded requests, finite concurrency, cancellation, resource limits, and loop detection. A soft window renewal is not a spending-cap increase. An account-level API quota cannot be bypassed by restarting a worker.

### 4.3 Required outcomes and presentation

These are proposed delivery states/reasons. Preserve legacy execution-v1 behavior until a versioned compatibility adapter is available.

| Condition | Durable outcome | Normal presentation |
| --- | --- | --- |
| Soft window nearly full | `checkpointing` -> `queued` -> `running`; reason `window_renewal` | “Progress saved; continuing.” No error/completion event |
| Context pressure | Compact/retrieve/split with preserved goal and effect records | Optional context status; no discarded requirement |
| Retryable provider 429/503 | `waiting_retry` with persisted next-attempt time | “Provider temporarily unavailable; retry scheduled.” Honor Retry-After |
| Tool timeout | Reconcile effect; retry, replan, or park | Show recovery status rather than immediately failing the whole delivery |
| Repeated no-progress repair | `replanning`; then `waiting_decision` if no safe new strategy | Concise cause and recommendation; no infinite automatic renewals |
| Explicit spend/deadline reached | `waiting_decision`, reason `spend_authorization` / `deadline_decision` | “Progress saved; authorization needed to continue.” Never success |
| Missing credentials / exhausted provider account quota | `waiting_credentials` / `waiting_quota` | One actionable status, not a repeated modal loop |
| Required user decision | `waiting_decision` for dependent work | Primary continues independent authorized work |
| Worker dies | Lease expires; reaper schedules reconciliation and resume | Same delivery ID; no false claim of completion |
| User cancels | `cancel_requested` -> reconciled `cancelled` | Honor cancellation; do not auto-renew |
| Storage corruption, invalid authority, unrecoverable failure | Safe `failed` or explicit blocked state with retained artifacts | Honest actionable failure; do not suppress it |

Provider/model context limits, policy denials, and actual software failures still exist. The objective is that recoverable control flow does not masquerade as a fatal long-horizon task failure.

### 4.4 Admission, renewal, and accounting algorithm

1. Before a call, calculate context fit and reserve estimated input plus bounded output against applicable authority. Reserve across concurrent children atomically, not independently from the same remaining balance.
2. When a soft watermark is reached, stop admitting new effectful calls. Let owned operations reach a safe boundary, or cancel and reconcile them under bounded cleanup time.
3. Persist the window checkpoint: goal revision, accepted artifact, active node, child handles, decisions, evidence cursor, outstanding effects, provider state needed for continuation, cumulative usage, and restart context references.
4. Atomically make the continuation runnable and publish the checkpoint event. Use a shared transaction where available; otherwise use a transactional outbox and idempotent consumer. Do not assume separate plan and queue database writes are atomic.
5. Release the worker lease only under the agreed checkpoint/queue protocol. A duplicate continuation must resolve to the same checkpoint/generation and cannot execute twice concurrently.
6. A new worker verifies policy, goal revision, artifact fingerprints, and lease epoch, reconstructs bounded context, and resumes incomplete work. Do not repeat completed model/tool operations.
7. Reconcile actual usage by stable call ID. **Record actual incurred usage even when it exceeds the estimate or ceiling**, then stop new admission if required. The present `TaskBudget.AddUsage` early-return pattern must not drop already-incurred usage.
8. Renew only the soft window. Preserve total accounting, no-progress history, rejected approaches, pending decisions, and explicit hard limits.
9. Reserve enough control-plane capacity for deterministic checkpointing, reconciliation, and status. Any model-assisted summary/synthesis still counts toward authorized model usage; use a deterministic checkpoint if no model budget remains.

Prices for deployment aliases may be unknown. With no monetary cap, expose unknown cost honestly. With an explicit monetary ceiling, require trusted pricing/reservation semantics or park for pricing configuration before further billable work. Never report unknown cost as zero cost. Reservations reduce overshoot but do not guarantee a provider never bills more than an estimate.

### 4.5 All current stop paths must be audited

Do not fix only `internal/execution/task_budget.go`. Trace and test:

- `internal/budget/budget.go`: session limits, reservations, progressive compression.
- `internal/orchestrator/orchestrator.go`: task wall timeout, model-call hook, streaming repair and completion, per-turn call limits.
- `internal/orchestrator/repair.go`: repair allowance exhaustion.
- `internal/orchestrator/subagents.go`: child timeout and shared call accounting.
- `internal/orchestrator/context_guard.go`: context overflow and protected current task.
- `internal/orchestrator/plan_executor.go`: budget-to-plan-stop mapping.
- `internal/execution/result.go`, `envelope.go` and `internal/orchestrator/envelope.go`: terminal classifications.
- `internal/cli/root.go`, `internal/tui/app.go`, `internal/server/`: exit codes, retry displays, request deadlines, SSE terminal events.
- Chronos `sdk/agent/` and `engine/model/`: tool-loop iteration limits, request retries, summarization and provider limits.

Transport timeout is not delivery timeout. Durable tasks run under worker-owned context; HTTP disconnect, SSE reconnection, or TUI detach must not implicitly cancel them. Explicit cancel signals remain authoritative.

## 5. Data, events, configuration, and surface contracts

### 5.1 Proposed durable domain

Extend existing plan/execution stores where possible. Avoid duplicate sources of truth. The following logical records can be normalized tables or versioned event projections:

| Record | Required information |
| --- | --- |
| Delivery | Trusted scope; stable ID; goal revision; lifecycle; policy reference; accepted artifact; cumulative accounting; optimistic version |
| Requirement | Stable ID; statement; acceptance checks; source/user decision; status; authorized waiver if any |
| Decision | Question; options; recommendation; consequences; reversibility; deadline; blocked dependencies; actor/resolution |
| Work node | Preconditions; scope/effects; dependencies; expected artifact; verification; assumptions; recovery class |
| Attempt/window | Node and delivery IDs; lease owner/epoch; input snapshot; resolved provider; limits; checkpoint; usage |
| Operation | Stable call/effect ID; intended effect; authority; prepared/running/observed/reconciled state; result reference; replay class |
| Artifact | Content-addressed snapshot/patch; parent snapshots; declared/observed write set; provenance; retention references |
| Receipt | Artifact/environment/check identities; actual result and exit status; time; authority; supported/contradicted obligations |
| Claim | Statement; support/counterevidence; referenced symbols/config revisions; revalidation predicate; confidence basis |
| Procedure | Applicability; steps/operators; prerequisites; expected evidence; counterexamples; evaluation; approval; invalidation |

Use database uniqueness and compare-and-swap transitions for idempotency and concurrency. Add checksums, schema versions, migrations, backup/restore, and reference-aware retention. SQLite WAL is appropriate initially; do not claim shared-fleet durability for local SQLite files. Validate any supported PostgreSQL adapter separately before advertising equivalent guarantees.

### 5.2 Proposed delivery state and events

Nonterminal states: `admitted`, `queued`, `running`, `checkpointing`, `waiting_retry`, `waiting_decision`, `waiting_credentials`, `waiting_quota`, `paused`, `reconciling`, `replanning`, `cancel_requested`.

Terminal states: `succeeded`, `failed`, `cancelled`.

Lease state is separate from delivery state. Plan generations preserve immutable accepted topology; changes create a validated successor with explicit retained/invalidation mappings. A soft window yield is not a failed node or a completed delivery.

Ordered durable events include admission, goal revision, decision request/resolution, attempt ownership, operation prepared/result/reconciled, artifact accepted, verification, checkpoint, continuation scheduled, retry scheduled, procedure candidate, and final completion. Event ordering is scoped to a delivery; events carry task/attempt/operation IDs for attribution. Telemetry can be lossy; these records cannot.

### 5.3 Proposed specialist contract

```text
TaskCapsule:
  trusted identity + goal revision + input snapshot
  bounded question/scope + acceptance obligations
  relevant claims/evidence references + known counterexamples
  delegated effects + model policy + window allowance
  parent mailbox + expected output contract

TaskReceipt:
  complete | partial | blocked | failed
  artifact references + observed read/write sets
  supported and contradicted claims + verification references
  open questions + attempted approaches + remaining work
  actual provider + usage + stop/recovery classification
```

Validate the structured envelope; do not treat the child's status assertion as proof. The parent accepts evidence and artifacts, resolves conflicts, and owns user-facing synthesis. Large source/tool payloads stay in referenced artifacts, not every agent's prompt. Return partial receipts on ordinary failure/cancellation where recoverable; a crash recovers from durable records.

### 5.4 Proposed YAML profile

**Do not copy this into active configuration until F13 implements and validates it.** Values below are initial tunable defaults, not benchmark conclusions. Keep existing bounded execution available as a compatibility profile.

```yaml
delivery:
  enabled: true
  mode: autonomous
  primary: chronos-code
  persistence:
    backend: sqlite
    # Location resolved through ProjectPaths, not the process working directory.
  workflow:
    strategy: evidence_driven
    planning_horizon: 2
    replan_on:
      - requirement_changed
      - assumption_invalidated
      - repeated_no_progress
  execution:
    isolation: per_attempt
    input_snapshot: accepted_dependencies
    max_concurrent_attempts: 3
    unknown_effect: reconcile_or_wait
    detached_survives_client_disconnect: true
  budget:
    mode: checkpoint_and_continue
    window:
      max_model_calls: 32
      max_tool_calls: 128
      max_active_seconds: 900
      max_working_tokens: 250000
      checkpoint_watermark: 0.8
    cumulative:
      persist_usage: true
      # Optional explicit max_cost_microdollars / deadline_utc are authority,
      # not renewable windows. Omitted means no additional delivery ceiling.
    on_window_limit: checkpoint_resume
    on_authority_limit: await_decision
    no_progress:
      max_equivalent_windows: 2
      action: replan_then_await_decision
  collaboration:
    result_contract: evidence_receipt_v1
    decision_inbox: durable
  verification:
    mode: enforce
    bind_to_artifact: true
    require_all_acceptance: true
  learning:
    promotion: human_review
```

Validation must reject conflicting profiles, unknown keys, negative values, invalid watermarks, unsupported capabilities, or a declared autonomous profile without a live worker/recovery backend. `max_working_tokens` is a window throughput allowance; it does not change a provider's per-request context limit. Total usage and authority limits remain separate.

### 5.5 CLI, TUI, and HTTP behavior

Proposed CLI: `task start`, `task inspect`, `task attach`, `task steer`, `task pause`, `task resume`, `task cancel`, and decision list/resolve commands. Preserve `chronos-code run` as a foreground adapter; an explicit delivery profile/flag selects the new supervisor. `--detach` returns a persisted admission receipt, never a fake successful-delivery result.

Proposed HTTP: task admission returns 202 with a stable ID; task status/control and an ordered event stream share the same service as CLI/TUI. Extend existing plan endpoints rather than cloning their logic. Authenticate tenant and authorize repository/action at admission and control; request bodies cannot select arbitrary filesystem roots. Reconnect via durable event sequence. Old `/v1/chat` and execution-v1 clients retain documented semantics; introduce a versioned delivery envelope/event contract for nonterminal statuses.

TUI: show goal, accepted increments, active specialists, pending decisions, verification, renewal/retry status, remaining explicit authority, and cumulative usage. Preserve mouse capture and existing copy behavior. `/inspect` remains usable while workers run. Enter/queued messages become explicit steering events in delivery mode. Detach leaves the task running; cancellation sends an explicit durable signal.

Foreground JSON mode emits one final delivery result when terminal; while attached, progress can use stderr or a separately selected JSONL mode. A nonterminal pause may return a separately versioned handoff receipt with `terminal: false` and a documented non-success waiting exit code. It must not use `budget_exhausted`, report success, or force resubmission of the original task. A soft renewal never causes that handoff: the supervisor continues automatically.

## 6. Dependency-ordered implementation tasks

The index is the authoritative checklist. Each task may have several verified substeps recorded in section 10. `F` tasks establish correctness and delivery; `R` tasks add adaptive judgment; `Q` tasks prove and release it.

| Done | ID | Task | Dependencies |
| --- | --- | --- | --- |
| [x] | F00 | Reconcile baseline and regression fixtures | none |
| [x] | F01 | Explicit run identity and child model binding | F00 |
| [x] | F02 | Workspace-consistent reads, analysis, and effects | F01 |
| [x] | F03 | Effect authority, sandbox, MCP and tenant boundaries | F01, F02 |
| [x] | F04 | Durable delivery domain and schema | F00 |
| [x] | F05 | Durable worker lifecycle and common executor | F01, F03, F04 |
| [ ] | F06 | Operation journal and effect reconciliation | F04, F05 |
| [ ] | F07 | Accepted artifact lineage and integration | F02, F04, F06 |
| [x] | F08 | Accurate cumulative accounting and admission | F01, F04 |
| [x] | F09a | In-process window renewal for attached interactive runs | F08 |
| [ ] | F09 | Automatic budget checkpoint and continuation | F05, F06, F07, F08, F09a |
| [ ] | F10 | Acceptance-driven verification and completion | F04, F06, F07, F08 |
| [ ] | F11 | Durable specialist capsules, receipts, mailboxes | F03, F05, F07, F09, F10 |
| [ ] | F12 | Primary supervision, decisions, CLI/TUI/HTTP | F09, F10, F11 |
| [ ] | F13 | Honest YAML capabilities and aligned prompts | F10, F11, F12 |
| [ ] | F14 | Memory lifecycle, redaction, feedback and retention | F04, F06, F10 |
| [ ] | R01 | Revision-aware causal working model | F02, F10, F14 |
| [ ] | R02 | Experiment-seeking action router | R01, F08, F13 |
| [ ] | R03 | Evidence-compiled rolling work graph | R01, R02, F07, F12 |
| [ ] | R04 | Witness-selected speculative microbranches | R02, R03, F11 |
| [ ] | R05 | Semantic handoff and integration checking | R01, F07, F11 |
| [ ] | R06 | Decision-preserving context compaction | R01, F09, F14 |
| [ ] | R07 | Correction-to-procedure compiler | R01, F13, F14 |
| [ ] | R08 | Decision inbox with safe independent work | R03, F12 |
| [ ] | R09 | Witness-based adversarial review | R01, F10, F11 |
| [ ] | R10 | Harness self-debugger | R01, F06, F08, F11 |
| [ ] | Q01 | Real-model, crash and comparative evaluations | F13, F14, R01, R02, R03, R04, R05, R06, R07, R08, R09, R10 |
| [ ] | Q02 | Compatibility, packaging, operations and release | Q01 |

Q01 fixtures and instrumentation may be developed earlier, but its final acceptance requires the listed capabilities. Research dependencies do not prevent shipping a verified foundation profile after F13/F14 under explicitly narrower claims.

### F00 — Reconcile baseline and capture regressions

**Targets:** the section 2 code map, existing tests, `go.mod`, CI/release workflows, current local plan HTTP work.

**Implement:** record actual revisions and user changes; reproduce findings with focused tests at real boundaries. Inert provider/permission fixtures must not invoke paid APIs or push/deploy. Separate defects fixed upstream from defects still present. Pin a compatible Chronos revision for reproducible CI once cross-repository changes are ready; do not silently fetch or overwrite the sibling checkout.

**Accept:** baseline behavior and failing expectations are documented; test fixtures prove provider identity, workspace identity, permission decisions, and predecessor artifact propagation. Existing user files remain intact. Record toolchain and build-tag requirements. Do not leave intentionally failing tests in a published increment; keep characterization tests or pair each regression with its fix.

### F01 — Explicit run identity and provider binding

**Targets:** `internal/orchestrator/orchestrator.go`, `subagents.go`; Chronos `sdk/agent/context.go`, `agent.go`, `sdk/harness/subagent.go` or their current equivalents.

**Implement:** immutable role configuration plus per-invocation run context from section 3. Resolve a configured child's model explicitly, without losing trusted tenant/policy/usage references. Dynamic children inherit only documented model policy. Keep registries/configuration immutable during active runs; make per-invocation mutable state independent rather than copying mutex-bearing Agent structs.

**Accept:** concurrent children of the same role can use independent providers/workspaces/conversations without races; model receipts identify the actual provider; no identity is overwritten by a model argument. Preserve cancellation, recursion and capacity safeguards. Test parent frontier/child cheap and parent cheap/child frontier.

### F02 — One workspace and analysis identity

**Targets:** `internal/incctx/`, `internal/graph/`, `internal/activation/`, `internal/lsp/`, `internal/projectdocs/`, `internal/security/`, `internal/orchestrator/edit_checkpoints.go`; Chronos `engine/tool/builtins/`.

**Implement:** one admitted workspace resolver used by every read/effect and bound service. Eliminate startup-root closures on invocation-dependent paths. Key graph/cache/LSP analysis by artifact/worktree/config identity. Use root-contained opens or equivalent protections for symlink traversal; filter recursive search results through path policy. Bind file attachments and stored-result retrieval to their admitted scope too.

**Accept:** create different content at identical paths in parent and child checkouts; reads/search/graph/execution/checkpoints agree on the child. Stale/missing graph falls back to child source. Denied-file reads, symlink escapes, cache cross-contamination, and same-name symbol ambiguity are tested. Multi-language behavior is tested with and without Tree-sitter.

### F03 — Enforce effects and trust at execution boundaries

**Targets:** `internal/security/`, `internal/mcpdiscover/`, `internal/guardrail/`, `internal/server/`, `internal/auth/`; Chronos `engine/tool/`, sandbox interfaces.

**Implement:** a capability/effect vocabulary covering read, scratch write, delivery write, process execution, network and external mutation. Enforce declared delegation; read-only/planning modes apply to MCP and dynamic tools. Parse supported shell syntax or conservatively confirm/reject ambiguous compound commands; first-token allowlisting is insufficient. Install actual sandbox enforcement for autonomous shell, with filesystem, environment/secret and network scopes. Bind MCP launch trust to origin/command/arguments/endpoint/config digest, and invalidate trust when these change. Repository hooks are executable code and require admitted provenance. Authorize tenant/repository/action, not storage tenant alone.

**Accept:** inert permission tests cover compound commands and interpreters; sandbox integration tests prove read/write/network restrictions on supported platforms. Unsupported mandatory isolation fails admission rather than falling back unsandboxed. A trusted server name cannot bless a changed command. Review/test scratch outputs work without granting production write access. Broad session approval cannot override hard denials or a new configuration identity.

### F04 — Durable delivery records, goals and migrations

**Targets:** `internal/execution/`, `internal/plan/model.go`, `sqlstore.go`, `admission.go`, `internal/config/paths.go`; storage adapters as needed.

**Implement:** section 5 records and legal transitions, versioned goals/acceptance/decisions, stable IDs, scope checks, uniqueness and compare-and-swap. Persist admission before acknowledgment. Add ordered delivery events distinct from lossy telemetry. Keep existing plan APIs and migrate transactionally. Choose and document the queue/plan transaction boundary before F05, including outbox handoff if stores differ.

**Accept:** schema migration/rollback compatibility, backup/restore, duplicate admission, conflicting updates, cross-tenant guessed IDs, and replay produce deterministic state. Restart can enumerate runnable and waiting deliveries without reconstructing them from assistant prose. No new state is misrepresented as execution-v1 success.

### F05 — Worker-owned lifecycle and common executor

**Targets:** `internal/orchestrator/orchestrator.go`, `plan_runtime.go`, `plan_executor.go`, `internal/plan/controller.go`, `scheduler.go`, `internal/teambuilder/`; Chronos `engine/queue/`, `engine/graph/`, `sdk/harness/`.

**Implement:** connect durable delivery nodes to Chronos queue workers, timed leases, heartbeats, reclaim, signals and shutdown. Add epoch/fencing guarantees where needed. Use one execution service for ordinary chat work, direct tools, teams, plan nodes and specialists. Keep per-node and delivery-wide retry/concurrency controls finite. Separate client cancellation/detach from durable task cancellation. Ensure only incomplete admitted nodes execute after resume.

**Accept:** production construction plus worker start can execute a real admitted fixture. Kill/restart a worker and demonstrate reclaim; a late stale worker cannot finalize or integrate. Waiting signals survive restart. Client disconnect leaves detached work runnable; explicit cancel stops further admission. No plan is stuck merely because its owner vanished. Capability remains unavailable until its end-to-end gate passes.

### F06 — Journal and reconcile side effects

**Targets:** `internal/execution/ledger.go`, `internal/orchestrator/verification_observer.go`, tool wrappers and checkpoints; Chronos `sdk/agent/`, `engine/queue/outbox.go`, storage transaction contracts.

**Implement:** durable operation prepare/result/reconcile records with stable effect keys. Preserve tool-call/result relationships and any provider-specific continuation state required by the protocol. Classify replay safety; write tools use expected input/output fingerprints, API effects use destination idempotency or observation, unknown shell effects park. Make critical storage failures visible to execution instead of ignoring after-hook errors. Telemetry remains non-authoritative.

**Accept:** fault injection before tool execution, after effect, before result persistence, and after checkpoint detects/reconciles the actual outcome. Replay does not duplicate completed effects. An ambiguous effect cannot become a successful receipt. Outbox delivery retries require downstream idempotency; do not promise exactly-once arbitrary shell execution.

### F07 — Artifact lineage and safe integration

**Targets:** `internal/worktree/`, `internal/orchestrator/plan_executor.go`, `edit_checkpoints.go`, `internal/plan/`.

**Implement:** immutable accepted task snapshots and explicit predecessor composition. Capture authorized initial dirty content without modifying the user's index/worktree or leaking denied files. New nodes branch from accepted dependencies, not ambient `HEAD`. Validate actual read/write sets; integrate under an expected-base/version check and journal integration. Preserve candidate artifacts before cleanup. Distinguish “patch applied, cleanup pending” from “patch not applied.” Add slice-level undo with conflict detection and external-effect limitations.

**Accept:** A creates an API; dependent B can compile against A without the user committing. Concurrent disjoint changes compose; conflicting parent edits are preserved. Crash after apply before receipt does not apply twice. Cleanup failure does not trigger duplicate integration. Referenced worktrees/artifacts survive retention and restart.

### F08 — Accurate usage and authority-aware admission

**Targets:** `internal/execution/task_budget.go`, `internal/budget/budget.go`, `internal/orchestrator/verification_runtime.go`, model hooks, `internal/config/`; Chronos model/session summarization hooks.

**Implement:** separate renewable window usage from persisted cumulative usage and explicit authority. Stable call-ID reservation/reconciliation handles parallel children, provider retries, compaction, repair and speculative work. Record billed/observed usage even on an over-limit outcome. Use checked arithmetic and distinguish unknown/missing usage from zero. Persist active compute time separately from time waiting on humans/providers. Keep configuration compatibility for bounded tasks.

**Accept:** actual usage exceeding a reservation remains counted exactly once; duplicate completion/crash recovery does not double count; concurrent admissions cannot each spend the same remaining cap. Accounting survives restart and compaction. Unknown pricing behaves as section 4 requires. Verify boundaries, cache usage, cancellations, failures, refunds of unused reservation, and integer overflow cases.

### F09a — In-process window renewal for attached interactive runs

**Scope:** attached TUI/CLI/HTTP executions without a delivery identity. A live process owns the run, which satisfies section 4.1 ("or a live worker owns it") without the F05–F07 durable machinery. Delivery-worker executions are excluded and keep bounded accounting. Detach, crash-resume on another worker, and the BUD cases that need durable handoff (BUD-07, BUD-13) remain F09.

**Implemented:** Chronos SDK `ToolLoopController` (bound per agent ID in context) replaces the fixed `MaxIterations` cap at tool-round boundaries, after results are appended, and can stop with `StopReasonPaused`. `ContextConfig.PersistToolRounds` persists each completed tool round and compacts inside long turns while keeping the current user message verbatim. Loading keeps only complete tool rounds, and the summarizer never splits a round. In chronos-code, `long_running` config (`mode: renew` by default) runs a window governor. A full window (tool rounds, tool calls, active seconds, tokens, or a session budget ≥90%) renews when the window produced new file content, new verification results, or mostly new tool calls. After `no_progress_windows` consecutive idle windows the run pauses with `StopNoProgress` (not success, offered for resume). Renew mode also drops the per-execution wall-clock context and the repair.* model/tool/time/token task limits. The session token budget renews at progress windows and at each new user turn. Configured subagents get their own governor and no wall-clock limit (`subagent_timeout_sec`). `repair.max_attempts` and an explicit `repair.max_cost_microdollars` stay authoritative. `mode: bounded` restores legacy behavior.

**Not yet:** a user-visible "window renewed" status event, an explicit total-token authority ceiling that parks with a decision, and team members (the Chronos team runtime still uses the fixed SDK cap).

### F09 — Checkpoint and continue instead of budget exhaustion

**Targets:** every path in section 4.5; `internal/plan/`, Chronos queue and SDK tool loop, versioned delivery events.

**Implement:** typed nonterminal yield/control results, not string matching. Proactive soft-watermark checkpoint plus safe-boundary renewal algorithm from section 4.4. Route model-call, tool-call, repair, context and wall-time pressure appropriately. A repair allowance yields/replans; it cannot reset failure history. Preserve working context/evidence and transfer the same delivery across attempts. Do not leak internal budget exceptions as tool text prompting the model to retry indefinitely.

**Accept:** all BUD cases in section 8 pass in blocking, streaming, team, child and plan execution. A delivery larger than several windows completes without terminal budget errors. An explicit authority ceiling parks visibly and is never reset. Legacy bounded-execution clients retain their documented exit/status behavior. Automatic continuation is only advertised after a durable runnable continuation exists.

### F10 — Requirements, verification receipts and completion certificate

**Targets:** `internal/verification/policy.go`, `internal/execution/ledger.go`, `internal/orchestrator/verification_runtime.go`, `verification_observer.go`, `repair.go`, `internal/eval/`.

**Implement:** derive and persist accepted requirements from user intent with explicit ambiguity handling. Link obligations to requirements and artifact/environment identities, not only command strings. Later relevant edits stale earlier evidence. Execute checks through the same authority path; a requested command is not a passing result. Record skipped/blocked checks, tests that exercised nothing, and baseline failures. Independently verify the integrated artifact. Enforce completion for autonomous delivery, with explicit user-owned waivers.

**Accept:** model assertions cannot manufacture receipts; weakened tests or omitted deliverables do not produce success. Wrong-revision tests and irrelevant successful commands are rejected. A final result names the accepted artifact, satisfied/waived requirements, actual verification and remaining limitations. User-authorized review/planning-only tasks do not acquire inappropriate implementation obligations.

### F11 — Durable specialist protocol

**Targets:** `internal/orchestrator/subagents.go`, `capabilities.go`, `internal/teambuilder/`, `internal/execution/`; Chronos `sdk/harness/`, `sdk/team/`, `engine/queue/`.

**Implement:** section 5.3 capsule/receipt schema, scoped artifact access, task handles, durable mailboxes, status/message/wait/cancel tools, partial-result recovery, and per-invocation role state. Enforce declared delegation and typed escalation reasons. Keep depth/cycle/fanout bounds and deadlock avoidance. Aggregate parent/child usage without double counting; shared costs have one stable owner. Apply the protocol to sequential, parallel, coordinator and hierarchy patterns rather than separate execution guarantees.

**Proposed durable team design (decided 2026-09-26; not implemented):** one replay-based step log instead of a checkpoint format per strategy.

1. Chronos `sdk/team` defines a `StepJournal` (`Lookup(stepID)` / `Record(stepID, result)`). The single agent-invocation point (`executeAgent`) consults it: a recorded step returns its result without a model call; otherwise the agent runs with node identity = step ID and the result is recorded before control flow continues.
2. Step IDs are structural, never a global counter: sequential/parallel `team:<id>:member:<i>`, coordinator `plan:<iter>` and `task:<iter>:<j>`, router `route`, hierarchy/swarm graph path plus visit count (for example `sup/research#2`). Concurrent branches therefore get the same IDs on replay.
3. Recovery re-runs the strategy code. Every nondeterministic choice (coordinator plan, supervisor routing, swarm handoff) is a recorded agent output, so replay retraces the same path and executes only unrecorded steps. No per-strategy resume logic.
4. Chronos Code stores steps in an append-only delivery table, unique on (delivery, step ID) and written under the live lease. This replaces the rewritten 256 KB checkpoint blob; duplicate or stale writes are rejected or no-ops.
5. One park rule for all strategies, using `delivery_usage_calls.node_id`: a node with billed calls but no recorded step, or any unknown or unattributed call, parks the delivery.
6. Sequential and parallel teams (F05's per-strategy checkpoints) migrate onto the journal, keeping their tests as regressions.

Constraints: strategy control flow must be deterministic (audit `executePlan` and the graph runner for time, randomness and map-order dependence). Recovery granularity is one agent run; resuming inside a member's tool loop depends on F06/F09. The graph engine's own checkpointer is not a second source of truth.

**Accept:** the parent can resume after child completion, inspect partial output, answer a child's decision, or cancel it after a client restart. Same-role parallel children stay isolated. Duplicate/out-of-order mailbox delivery is idempotent. A blocked child does not erase useful results or block independent children. Real production tool registration and all four patterns are tested.

### F12 — Available primary and unified delivery surfaces

**Targets:** `internal/orchestrator/operational_snapshot.go`, `session_ux.go`, `internal/cli/`, `internal/tui/`, `internal/server/` including existing plan handlers; `internal/execution/envelope.go`.

**Implement:** primary supervision is event-driven and available for conversation while workers execute. Only the primary accepts goal/decision changes and synthesizes results; specialists send deltas. Add the section 5.5 surfaces with versioned contracts and durable events. Persist basic decision requests/resolutions. Make `@agent` explicit: preserve existing switch behavior, distinguish it from delegation, and keep the delivery supervisor identity stable. Add nonterminal progress for budget renewals/retries without error modals. Maintain legacy CLI/chat compatibility and precise exit semantics.

**Accept:** the user can start/detach/attach/inspect/steer/resolve/cancel the same task across CLI/TUI/HTTP. Steering updates a goal revision without overwriting in-flight results; stale results require validation. Ordered event replay has no duplicate terminal result. Normal renewals do not return exit code 6 or execution-v1 `budget_exhausted` in delivery mode. Completed work is not resubmitted when the user asks a progress question.

### F13 — Align prompts, YAML and actual capabilities

**Targets:** all `internal/defaults/agents/*.yaml`, `routing.yaml`, `config.yaml`, `tools.yaml`, `catalog.go`; `internal/config/`, `internal/router/`, `internal/orchestrator/capabilities.go`.

**Implement:** implement the proposed delivery profile with strict parsing, resolved provenance, schema/version validation and secret redaction. Make escalation/pipeline declarations executable through the common service or explicitly reject/export-label unsupported declarations. Enable closed-loop capability only from proven runtime wiring. Separate tool effect classes from model-cost tiers. Reconcile all agent role promises with actual grants. Remove universal “leaf-first,” “never read a full file,” graph-as-proof, and misleading stack-trace rules. Planner output allows spikes, decisions, compatible implementation increments and verification; it does not execute them itself. Primary owns integration and completion.

Role contract: primary owns the delivery; coder proposes implementation artifacts; planner/PPD planner propose work; reviewer returns evidence-backed findings and may run authorized scratch tests; debugger diagnoses and proposes isolated fixes when granted; researcher gathers evidence and authorized history; architect proposes contextual contracts/designs; explainer states supported behavior and uncertainty. Review severity follows demonstrated impact, not a fixed syntax checklist. A role lacking a needed capability requests it or hands off explicitly rather than using shell as an undeclared write path. Do not force two specialists for a task where their coordination adds no value unless the user specifically requested that collaboration.

**Accept:** effective configuration matches behavior for all nine roles. No configured key silently disappears. Default exported config round-trips. Unknown capabilities fail admission. A real startup-based acceptance test runs a dependency-ordered plan and returns primary synthesis; tests cannot manually grant a missing capability to establish this claim. Prompt tests check critical contracts without prescribing a single wording.

### F14 — Memory and learning lifecycle

**Targets:** `internal/memory/`, `internal/learning/`, `internal/session/`, `internal/skills/`, `internal/orchestrator/runtime_memory.go`, `internal/retention/`.

**Implement:** a common retrieval policy over legacy notes, layered records, summaries and learned procedures, with explicit provenance and duplicate handling. Keep working progress distinct from durable knowledge. Rank candidates across authorized scopes under one serialized context budget. Add pre-persistence redaction, source-based invalidation, contradiction links and a resume delta briefing. Connect feedback to actual candidate/agent/outcome IDs; do not equate arbitrary confidence deltas with calibrated reliability. Require a human-authorized organizational publication/promotion event, not merely a model boolean. Protect active deliveries, procedures' evidence and referenced artifacts from retention.

**Accept:** corrections replace applicability without destroying history; changed code invalidates affected facts; unrelated projects/tenants cannot recall data; sensitive fixtures do not enter retained records. Pending work is recoverable without treating an episode as instructions. Learning promotion remains review-gated and auditable. Retention cannot delete artifacts needed by a paused multi-day task.

## 7. Ten research implementation tasks

These are candidate differentiators, not claims that no other product has investigated similar ideas. Each feature needs a deterministic production contract, a bounded MVP, and a measured ablation. Expected impact is a hypothesis. Follow the index dependencies and F-task discipline.

### R01 — Revision-aware causal working model

**Problem:** symbol maps do not preserve behavioral understanding, rejected hypotheses or why a design is fragile.

**Mechanism/targets:** add a reasoning overlay alongside `internal/graph/`, persisted through the F04 domain and F14 memory. Claims link to evidence, counterevidence, decisions and dependent tasks. Begin with ownership, persistence, authorization and compatibility. Capture static and observed paths separately; key symbols by qualified identity/location/build configuration rather than bare name where possible. Build bounded `/model` inspection and context pins.

**MVP:** claim creation/update/contradiction; source/config hash dependencies; deterministic invalidation and bounded working-set selection. Smell detectors propose hypotheses for dormant runtime capabilities, duplicated policy boundaries, unexplained co-change, and contradictory ownership; they never auto-refactor or treat co-change as causality.

**Impact/accept:** reduced rediscovery and stale assumptions. Mutate a boundary between sessions: affected claims become stale, unrelated claims survive, and the agent checks evidence before reusing them. Measure wrong-claim reuse and tokens to regain a correct mental model.

### R02 — Experiment-seeking action router

**Problem:** message-length/keyword complexity and cheapest-tool-first rules do not choose the best engineering action.

**Mechanism/targets:** extend `internal/router/`, orchestrator and telemetry with bounded `ActionProposal` records: question, hypothesis, expected observations, required capability, reversibility, cost and stopping condition. Consider graph lookup, source read, experiment, implementation, specialist and human decision. Filter by authority and capability before ranking. Start with explainable ordinal scoring, not uncalibrated pseudo-probabilities.

**MVP:** rank at most a few candidates at consequential uncertainty boundaries; deterministic easy decisions require no model call. Escalation reasons distinguish knowledge gaps, conflicting evidence, missing capability, environment failures and product choices. Persist whether an action actually resolved its question.

Advertise request-local tool schemas relevant to the chosen action without mutating a shared registry or confusing schema visibility with authorization. Preserve required tools under schema budgets; do not truncate an alphabetically sorted list and accidentally remove verification or editing capabilities. Include schema size, provider round trips and result retrieval in action cost.

**Impact/accept:** fewer ritualistic reads and cheaper decisive probes. Compare against the current classifier on stale docs, missing graph coverage and short but complex requests. Measure cost per verified resolution, failure rate, and unnecessary questions; include router overhead.

### R03 — Evidence-compiled rolling work graph

**Problem:** a large up-front DAG becomes stale while free-form execution forgets commitments.

**Mechanism/targets:** extend `internal/plan/` and the F05 executor to keep stable goals but compile only the next short work frontier. Operators are investigate, decide, implement, verify and integrate, with preconditions/effects/artifacts/recovery. Use validated successor generations for changed plans. Source dependency order is distinct from compatibility/deployment order.

**MVP:** a bounded model proposal plus deterministic admission; source/classifier references are host-bound. Resolve restart context references with a live loader. Reuse completed nodes only when their assumptions/input snapshots/verification remain applicable. Support expand/contract migration obligations and optional human-readable design notes projected from decisions.

**Impact/accept:** adapt without restarting or inventing dozens of speculative steps. Inject a mid-task requirement change; only affected work is invalidated. Demonstrate a spike leading to a changed plan and a dependency chain consuming correct predecessor artifacts. Bound replan oscillation.

### R04 — Witness-selected speculative microbranches

**Problem:** committing early to the wrong mechanism causes expensive repair loops.

**Mechanism/targets:** use worktrees, F11 specialists, R02 routing and F10 verification to compare a small number of reversible candidates against a discriminating witness. Every group shares a base snapshot, question, budget and stopping rule. This is not unrestricted best-of-N full-feature generation.

**MVP:** at most two isolated local candidates for one uncertainty; execute a test/probe that distinguishes their predictions; accept a candidate only after its result and compatibility evidence pass. Losers become explanatory evidence and retained artifacts under policy, not silent discarded cost.

**Impact/accept:** eliminate wrong directions earlier. Tests show no external mutation escapes, cancellation frees resources, and all branch costs are charged. Evaluation includes both candidates and their verification costs; reject apparent improvements caused by weaker tests.

### R05 — Semantic handoff and integration checks

**Problem:** textual merge success and isolated tests do not prove independently developed changes compose.

**Mechanism/targets:** extend F11 receipts and F07 integration with observed read dependencies, API/schema/config contract changes, assumptions and claim links from R01. The primary reviews deltas, not every transcript. Trigger focused integration witnesses for incompatible assumptions even when files do not overlap.

**MVP:** support public signatures, schema versions, config keys and ownership invariants. Validate claimed writes against actual artifact differences. Tie receipts to base/output snapshots and capability grants. Fall back to stronger integration tests for unsupported semantic analysis, never claim complete conflict detection.

**Impact/accept:** safer parallelism with less repeated reading. Include two textually disjoint, individually passing patches that fail together; detect the contract conflict or fail the integration witness before acceptance. Wrong-base and forged receipt tests must fail.

### R06 — Decision-preserving context compaction

**Problem:** summaries preserve a story but lose the constraint that determines the next safe action.

**Mechanism/targets:** extend `internal/attention/`, `internal/toolcompress/`, `internal/session/`, orchestrator context guard and R01 selection. Deterministically retain current goal, authority, obligations, decisions, rejected alternatives, unknown effects and pending questions; summarize only supporting prose. Preserve retrievable references and tool/provider protocol state.

**MVP:** compaction produces an inspectable manifest of retained/omitted sources. Test a small set of task-specific decision probes against the compacted view; missing mandatory state falls back to a deterministic checkpoint/retrieval rather than another blind summary. Budget probes and summarization themselves.

**Impact/accept:** fewer repeated mistakes across windows. Force several compactions and worker restarts; forbidden changes remain forbidden, rejected approaches stay rejected, and pending effects are reconciled. Measure constraint violations and cost versus current summarization.

### R07 — Correction-to-procedure compiler

**Problem:** successful tool sequences and confidence nudges do not capture reusable causal lessons.

**Mechanism/targets:** extend `internal/learning/`, `internal/memory/`, `internal/skills/` with structured situation/assumption/action/outcome/correction episodes. Compile procedures with applicability, prerequisites, operators, expected evidence, contraindications, counterexamples and retirement conditions. Publish through existing reviewed promotion with versioned capability manifests.

**MVP:** generate a candidate from repeated verified cases; replay against held-out related tasks and counterexamples; compare against the baseline; require attributable human acceptance. Learned behavior cannot grant tools, change budgets, or rewrite security. Invalidate on relevant source/contract changes rather than every unrelated commit.

**Impact/accept:** accumulated repository experience improves behavior. Evaluate different task wording/files sharing a failure mechanism, and tasks where the procedure should abstain. Include negative transfer and total procedure-selection cost. Lack of real replay evidence leaves a candidate pending.

### R08 — Decision inbox and safe-work frontier

**Problem:** asking everything wastes human time; guessing consequential intent creates rework.

**Mechanism/targets:** extend F12 decisions and R03 scheduler with reversibility, consequence, latest responsible decision time, recommended option and dependent work. Queue signals unblock only the affected nodes. CLI/TUI show one durable actionable question rather than repeated approval popups.

**MVP:** distinguish discoverable uncertainty, authorized reversible choices and user-owned decisions. Continue nodes independent of pending decisions; invalidate dependent results when a decision changes. Silence never grants permission. Bound reminder frequency and do not let a model turn a policy denial into a default choice.

**Impact/accept:** fewer interruptions without more wrong assumptions. A compatibility decision remains pending while isolated parser/test work proceeds; after resolution the correct nodes resume. Measure human questions, waiting time and incorrect autonomous assumptions together.

### R09 — Witness-based adversarial review

**Problem:** multiple model opinions can increase cost and correlated confidence without improving correctness.

**Mechanism/targets:** reviewer/debugger roles, F11 delegation, graph impact and F10 verification exchange bounded challenges targeting specific claims. Require a failing test, counterexample, observed trace or source-grounded contradiction. Cheap agents propose; trusted execution evaluates; stronger arbitration is reserved for unresolved consequential disputes.

**MVP:** a `Challenge` references a claim, artifact and proposed witness; validate its scope and execute in scratch isolation. Cap challenge budget/count. Avoid majority voting and prevent the implementation agent from silently changing the grader. Model agreement is never itself verification evidence.

**Impact/accept:** more verified defects per review dollar. Evaluate seeded integration/security/compatibility defects against one frontier-review baseline, counting witness generation and arbitration. Track false positives and missed defects; preserve inconclusive results honestly.

### R10 — Harness self-debugger

**Problem:** the model can spend many turns diagnosing application code when its provider, context or execution environment is wrong.

**Mechanism/targets:** orchestrator hooks, `internal/execution/`, `internal/observability/`, context reports, R01 and R02 maintain an execution flight recorder. Compare intended/actual provider, workspace, snapshot, permission effects, capability availability and evidence revision. Detect repeated actions with no new evidence.

**MVP:** deterministic checks for provider leakage, wrong-root reads, stale verification and lost worker ownership. Recovery can rebuild a run context, refresh isolated analysis, reconcile an operation, or propose a new bounded strategy. Policy, grants and explicit spending caps cannot be self-modified. Harness code changes become reviewed work, not an automatic live patch.

**Impact/accept:** less wasted reasoning and better fault attribution. Inject context corruption, stale cache, missing capability and expired lease; diagnose and contain before accepting results. Measure detection latency, false alarms, recovery correctness and overhead.

## 8. Verification and release gates

### 8.1 Existing commands

Run commands in the indicated repository directory. Use focused package tests while implementing; run full checks at integration milestones and before release, not after every documentation or one-line change. Fix introduced failures without weakening assertions.

Chronos Code:

```bash
go test ./internal/execution ./internal/budget ./internal/orchestrator ./internal/plan -race -count=1
go test ./internal/security ./internal/mcpdiscover ./internal/worktree -race -count=1
go test ./internal/cli ./internal/tui ./internal/server ./internal/integration -race -count=1
go test -tags treesitter ./internal/graph -race -count=1
make test
make build
make build-core
make vet
make lint
make eval
```

Chronos (resolved foundation checkout):

```bash
go test ./sdk/agent ./sdk/harness ./sdk/team ./sdk/memory ./engine/graph ./engine/queue ./engine/tool/... ./storage/adapters/sqlite -race -count=1
go build ./...
```

Use the foundation's repository-required checks for changes outside those packages. `make eval` is the existing deterministic tool-efficiency suite, not evidence of real-model delivery quality. It writes benchmark artifacts; inspect generated changes before retaining them. Sandbox/platform, provider, database and pricing-dependent checks must be identified explicitly when the environment cannot run them.

### 8.2 Mandatory budget continuation acceptance matrix

Implement named tests or traceable equivalents; record exact names and result artifacts under F09.

| ID | Scenario | Required result |
| --- | --- | --- |
| BUD-01 | Soft model-call window of 2; task needs at least 7 calls | Multiple durable windows; one delivery; eventual verified success; no terminal budget error |
| BUD-02 | Small tool-call window exhausted inside a child | Parent stays coherent; partial child state survives; no repeated completed writes |
| BUD-03 | Token watermark exceeded by a large tool result | Externalize/compact/checkpoint with required evidence retrievable; continue |
| BUD-04 | Active-time window expires during a subprocess | Safe completion or bounded cancel plus reconciliation before continuation |
| BUD-05 | Repair allowance reached with genuine new evidence | New bounded strategy/window; preserved failure history and cumulative usage |
| BUD-06 | Same unsuccessful actions repeat across windows | No infinite renewal; replan, then durable actionable decision if no safe progress |
| BUD-07 | Process dies between checkpoint and queue handoff | Exactly one runnable continuation from durable recovery; no lost delivery |
| BUD-08 | Usage report exceeds reservation/cap | All actual usage persisted once; further unauthorized admission blocked |
| BUD-09 | Explicit user cost cap reached | Saved `waiting_decision`; no reset, automatic cap increase or success |
| BUD-10 | Missing trustworthy price under an explicit cost cap | Saved actionable pricing state; no fabricated zero-cost execution |
| BUD-11 | Provider returns transient 429/503 | Durable backoff, Retry-After respected, no whole-task replay |
| BUD-12 | Credentials absent or account quota exhausted | One durable waiting state; no hot retry loop or misleading continuation |
| BUD-13 | HTTP/SSE disconnect and TUI detach during renewal | Worker ownership unaffected; reattach to same ordered event history |
| BUD-14 | Explicit cancellation races with renewal | No new work admitted after authoritative cancellation; effects reconciled |
| BUD-15 | CLI blocking, CLI streaming, TUI, HTTP and teams | Equivalent lifecycle/usage/completion semantics; no soft-limit terminal error |
| BUD-16 | Legacy bounded execution and new delivery clients | Versioned behavior preserved; no silent reinterpretation of v1 success/error |
| BUD-17 | Concurrent siblings and summary calls near total cap | Shared atomic reservation; all billed calls attributed; no oversubscription by reset |
| BUD-18 | Context cannot fit even after safe compaction | Split/retrieve/replan or genuine blocked state; no lost goal and no retrying identical oversized requests |

Assertions must inspect durable state, actual tool effects, usage, and UI/transport events. Merely replacing `budget_exhausted` with another terminal error fails this gate. Use controlled clocks and fault injection instead of waiting hours in unit tests; add a real soak run for release evidence.

### 8.3 Mandatory delivery and subagent scenarios

- A three-node dependent feature where each node consumes its predecessor's accepted API changes.
- Sequential, parallel, coordinator and hierarchical work under the same policy/ledger.
- Same-role concurrent specialists, one blocked child, one cancelled child, and a parent that answers a user question without restarting workers.
- Requirement change and decision correction mid-run with selective artifact/evidence invalidation.
- Crash before/after model result, tool effect, receipt write, integration, checkpoint and final acknowledgment.
- Textually disjoint but semantically incompatible patches; wrong-revision passing tests; baseline failures; a model claiming success without evidence.
- Stale/unsupported graph coverage, source edits between sessions, forced compaction, and recovery of rejected hypotheses.
- Cross-tenant/session attempts, symlink escapes, compound shell commands, changed MCP identity, read-only delegation, and unsupported sandbox admission.
- Referenced-artifact retention during a paused multi-day task and explicit rollback with conflicting user changes.
- A terminal completion followed by duplicate worker/event delivery: no repeated effect or second final result.

### Q01 — Evaluation and evidence, not feature counting

**Targets:** `internal/eval/`, `internal/integration/`, `benchmark/`, test fixtures and observability.

**Implement:** reproducible corpus of unfamiliar repositories, multi-package features/refactors, rolling compatibility changes, missing graph coverage, misleading docs, failure injection, delayed decisions and forced resume. Add ablations for graph, working model, routing, compaction, specialists, speculation, witness review and procedures. Fix model/provider/revision/settings, repository snapshot, policy, pricing basis and warm/cold analysis state. Separate hidden acceptance checks from agent-authored tests. Obtain required authorization for paid model runs; unavailable credentials produce “not executed,” never synthetic success.

**Measure:** verified delivery success, false completion, total cost per verified completion, recovery success, duplicated effects, unnecessary repeated work, human questions, incorrect assumptions, integration failures, policy violations, negative procedure transfer, and overhead. Count failed and speculative runs in costs. Record repeats and uncertainty; do not cherry-pick successful tasks.

**Accept:** every F/R acceptance has a traceable scenario; BUD-01 through BUD-18 pass; real-model results exist for advertised claims. Compare with the pre-change baseline first. Competitor superiority is a hypothesis until fair controlled runs support it. Experimental features that regress quality remain disabled even if implemented.

### Q02 — Packaging, rollout, compatibility and operations

**Targets:** `.github/workflows/`, `go.mod`, `Makefile`, `release/`, docs, server operations, retention and storage adapters.

**Implement:** pin the compatible Chronos revision and schema; test core/full artifacts and supported platforms; document supported isolation. Version delivery APIs and preserve existing bounded CLI contracts. Add migration/backup/recovery instructions, per-task disk/usage monitoring, disk-full handling, reference-aware retention, graceful draining and worker ownership diagnostics. Add authenticated repository/action checks to shared deployments and document that local files are not a shared fleet database. Release adaptive features behind explicit capabilities with a rollback path that preserves user artifacts and explains any paused task.

**Accept:** new installation, existing-data upgrade, worker crash, rollback/read compatibility, detached task, hard-cap decision, artifact retention and final delivery are demonstrated from built binaries. All required commands pass on advertised platforms or support is narrowed explicitly. The release declares which capabilities are proven, experimental or unavailable. No prompt or README claims capability merely because a package exists.

### 8.4 Release increments

1. **Correct boundaries:** F00–F03; close the reproduced defects before adding autonomy.
2. **Durable continuation:** F04–F09; demonstrate uninterrupted work across soft limits and crashes.
3. **Reliable delivery/team:** F10–F14; enforce completion and expose one coherent user interface.
4. **Adaptive judgment:** R01–R10, enabled by evidence rather than a feature-count goal.
5. **Proven release:** Q01–Q02; real-model and operational gates before world-class claims.

Do not combine “implemented,” “enabled,” “evaluated,” and “released” into one checkbox. Section 10 records these separately for research/rollout milestones.

## 9. Coverage matrix

| Reviewed requirement/gap | Implementation owner |
| --- | --- |
| Primary identity, follow-ups, stable conversation partner | F01, F11, F12, F13 |
| Role boundaries, mentions, isolation, partial results and synthesis | F03, F11, F12, F13, R05 |
| Dynamic routing, model/tool tiers, executable escalation | F01, F08, F13, R02 |
| Graph navigation, source evidence, coverage and progressive disclosure | F02, R01, R02 |
| Hypotheses, architectural smell and drift | R01, R10 |
| Spikes, design decisions, incremental delivery and compatibility | F07, F10, R03 |
| Durable PPD, scheduling, recovery and checkpointing | F04, F05, F06, F07, F09 |
| No routine budget-exhaustion termination in long-horizon work | F08, F09, F12; BUD-01–BUD-18 |
| Headless CLI, TUI, HTTP, progress and human steering | F12, R08 |
| Episodic/procedural/semantic/organizational memory | F14, R01, R07 |
| Cross-session context, corrections and preserved decisions | F14, R06, R07 |
| Ambiguity, changing requirements and partial failures | F04, F06, F10, R03, R08 |
| Human-gated self-improvement and first-class learned skills | F14, R07 |
| Safety floor, MCP, shell, tenant and enterprise isolation | F01, F02, F03, F06, Q02 |
| YAML-native behavior versus decorative configuration | F13 |
| Speculative execution plus cheap verification | R04 |
| Cheaper adversarial review with evidence | R09 |
| Self-debugging planning/tool/runtime failures | R10 |
| Artifact management, undo and inspection | F06, F07, F12, F14 |
| Cost/latency awareness and fair comparison | F08, R02, R04, R09, Q01 |
| Principal-engineer-level verified autonomous delivery | All foundation gates, measured R features, Q01, Q02 |

## 10. Execution journal and resume handoff

This section is implementation progress, not runtime application data. Update it after each verified increment or before a host boundary. Keep entries concise; reference preserved test logs/artifacts instead of embedding large outputs. Do not store credentials, private source dumps or raw sensitive tool payloads.

### Current checkpoint

- Active task: F06 (the only ready task). Complete assistant/tool rounds and terminal provider-only replies have lease-fenced receipts and replacement-worker resume paths, verified by restart and race suites. Incomplete rounds and ambiguous provider/effect outcomes still park. Generic API effects still need destination-specific idempotency/observation adapters; the only configured production example remains `server.delivery_http`.
- F05 acceptance scope (decided 2026-09-26): F05's gate is read-only and scratch-write plan/team execution through production construction and worker start. Write-enabled plan admission stays disabled until F10, because F10 depends on F05 through F06/F07 and a write gate inside F05 would be circular. F05's durable team scope is sequential and parallel; coordinator, router, swarm and hierarchy teams move to F11 under the replay-based step-log design recorded there.
- Completed tasks: F00–F05, F08, F09a (F03's tested unattended isolation backend is macOS Docker Desktop only; unsupported platforms fail admission).
- Runtime budget continuation implemented: in-process for attached interactive runs (F09a, `long_running.mode: renew`); durable detached continuation (F09): no. F09a follow-ups still open: nonterminal "window renewed" event, explicit total-token ceiling, team members still use the fixed SDK cap.
- Immediate hardening: verified truthful bundled routing, role-aware model floors, tester role, structured specialist handoffs, and prompt/runtime contract tests.
- Baseline since the last entries: `1047906` fixed the previously recorded TUI model-picker pair and the integration 8-vs-9 context-source failure (and widened the CI graph-index budget); those are no longer known failures. `fcd5f81` pinned `release/chronos.version` to Chronos `a5ae191` (current sibling HEAD, clean tree), but `go.mod` still uses `replace => ../chronos`, so reproducible CI pinning (F00/Q02) is partial. `d19639b` added platform-specific worktree integration locks (`integration_lock_{unix,windows}.go`). Later commits (`5a93db2`..`c40573d`) are graph/indexer work outside this guide.
- Next action: F06 destination-specific external API effect adapters require a selected service with an authoritative idempotency or observation contract; until then unknown effects remain parked. Plan-node continuation still needs its own receipt protocol.
- Decided 2026-09-26 (user, option a): the candidate plan worker is gated on its own proven prerequisites (plan store, controller, implementation agent, worktree manager, repository root). The interactive `planning:closed-loop-ppd` capability stays with F13 and still gates the write-mode plan executor.
- Known local integration work: the documented plan HTTP handler source is absent from this checkout; `docs/plan-http-api.md` is documentation for a separate unfinished surface.
- Blocking decisions: none for baseline investigation; paid evaluation, publication, or consequential changes require existing host/user authority.

### 2026-09-24 — F00 baseline reconciliation

Date / implementing agent: 2026-09-24 / OpenCode
Task / substep: F00 / reconcile revisions and reproduce review findings
State: verified
Chronos Code revision + relevant working-tree changes: `557f3c6d8a70eb3c1ab6992a25b39f4bedeced0f`; pre-existing untracked `docs/autonomous-delivery-implementation.md` and `docs/plan-http-api.md`
Chronos revision + relevant working-tree changes: `ce3bbc0657c0e034befd9729639777851aa0a22a`; clean
User work preserved / integration constraints: both untracked documents preserved; previously described plan-handler Go files and source edits are not present
Decision / hypothesis being resolved: determine which reviewed defects remain before changing runtime contracts
Files changed: `internal/orchestrator/subagents.go`, provider/delegation tests, `internal/incctx/incctx.go` and tests, `internal/security/permissions.go` and tests, `internal/security/policy.go` and tests, `internal/mcpdiscover/runtime.go` and tests, `internal/worktree/integration_test.go`; Chronos `engine/tool/builtins/workspace.go` and tests
Production path wired: configured child provider binding; invocation-root incremental reads/search; conservative compound shell admission; exact MCP launch identity; declared child registration; canonical built-in file containment
Checks executed (exact commands, cwd, actual result): Chronos Code `make test` passed; `make build` passed; focused combined boundary race tests passed; Chronos `go test ./sdk/agent ./sdk/harness ./sdk/team ./sdk/memory ./engine/graph ./engine/queue ./engine/tool/... ./storage/adapters/sqlite -race -count=1` passed; core built-ins race test passed
Evidence / artifact references: `TestConfiguredSubagentUsesConfiguredProvider`, `TestWrapUsesRequestWorkspaceRoot`, `TestWrapRejectsSymlinkEscape`, `TestPermissionChecker_ShellPrecedence`, MCP identity/runtime tests, core built-in read/write escape tests, `TestSetupSubAgentsHonorsDeclaredChildren`, `TestDependentWorktreeCharacterizesMissingPredecessorSnapshot`
Failure or remaining uncertainty: predecessor propagation is deliberately a failing-runtime characterization until F07; graph/LSP remain startup-bound; F01 still needs host-issued invocation identity and independent same-role runtimes; plan HTTP documentation still overstates absent routes
Capabilities: implemented: baseline boundary fixes and fixtures / enabled: existing bounded runtime only / evaluated: deterministic and race suites / released: no
Next exact action: complete F01 invocation identity and immutable per-run role/provider binding

### 2026-09-24 — F01 configured provider binding

Date / implementing agent: 2026-09-24 / OpenCode
Task / substep: F01 / configured child model binding
State: verified
Chronos Code revision + relevant working-tree changes: baseline revision plus the F00/F01 working-tree changes listed above
Chronos revision + relevant working-tree changes: baseline revision plus canonical built-in workspace containment
User work preserved / integration constraints: untracked documentation preserved; no commits, pushes, provider calls, or external mutations performed
Decision / hypothesis being resolved: a request-scoped parent provider must not override an explicitly configured specialist provider
Files changed: `internal/orchestrator/subagents.go`, `internal/orchestrator/orchestrator_test.go`
Production path wired: `configuredAgentRunner.Run` binds `configured.Model` into the child request context and rejects configured children with no model
Checks executed (exact commands, cwd, actual result): targeted orchestrator provider/resource race tests passed with `-count=10`; full Chronos Code `make test` and `make build` passed
Evidence / artifact references: `TestConfiguredSubagentUsesConfiguredProvider` covers frontier-parent/cheap-child and cheap-parent/frontier-child
Failure or remaining uncertainty: none for F01 acceptance; durable delivery identity fields remain empty under execution-v1 until F04/F05 supply their host records
Capabilities: implemented: run identity, configured provider binding, concurrent same-role isolation, actual provider receipts / enabled: yes in existing runtime / evaluated: deterministic race regressions / released: no
Next exact action: bind all analysis and effect services to the F02 workspace/artifact identity

### 2026-09-24 — F01 invocation isolation

Date / implementing agent: 2026-09-24 / OpenCode
Task / substep: F01 / host run identity, immutable provider snapshots, same-role concurrency, provider receipts
State: verified
Chronos Code revision + relevant working-tree changes: baseline revision plus current F00/F01 changes
Chronos revision + relevant working-tree changes: baseline revision plus `sdk/agent` run identity, response receipts, fallback receipt propagation, and F00 containment
User work preserved / integration constraints: untracked documents preserved; no branch publication
Decision / hypothesis being resolved: shared role configuration can remain immutable during an invocation while each call carries independent identity, provider, workspace and conversation state
Files changed: Chronos `sdk/agent/context.go`, `sdk/agent/agent.go`, `engine/model/provider.go`, `engine/model/fallback.go` and tests; Chronos Code `internal/orchestrator/orchestrator.go`, `subagents.go`, `execution_test.go`, `subagents_resource_test.go`
Production path wired: root executions receive host IDs; children derive unique invocation/parent IDs; provider snapshots update atomically after explicit model switches; fallback responses identify the successful backend
Checks executed (exact commands, cwd, actual result): Chronos Code `make test` passed and `make build` passed; Chronos `go test ./sdk/agent ./sdk/harness ./sdk/team ./engine/model ./engine/tool/... -race -count=1 && go build ./...` passed; same-role concurrency focused test passed 20 repetitions under race detector
Evidence / artifact references: `TestExecuteAttachesHostRunIdentity`, `TestConfiguredSubagentDerivesInvocationIdentity`, `TestSetupSubagentsRunsSameRoleWithIsolatedInvocationState`, `TestConfiguredSubagentUsesConfiguredProvider`, fallback provider receipt tests
Failure or remaining uncertainty: one earlier full orchestrator run hit the existing stream-cancellation timing assertion; ten focused repetitions and the subsequent full race suite passed
Capabilities: implemented: yes / enabled: yes / evaluated: deterministic race tests / released: no
Next exact action: F02 request-scoped graph/LSP/security/workspace metadata and scoped attachment/result retrieval

### 2026-09-24 — F02 workspace and analysis identity

Date / implementing agent: 2026-09-24 / OpenCode
Task / substep: F02 / request-scoped IO, analysis, policy and stored-result identity
State: verified
Chronos Code revision + relevant working-tree changes: baseline revision plus current F00-F02 changes
Chronos revision + relevant working-tree changes: baseline revision plus canonical built-in path containment and F01 identity/receipt changes
User work preserved / integration constraints: untracked documentation preserved; request-specific LSP intentionally reports unavailable when no manager exists for that root
Decision / hypothesis being resolved: every read, analysis and authorization path must use the invocation checkout, never the startup checkout
Files changed: `internal/incctx/`, `internal/graph/request_scope.go`, `internal/lsp/tools.go`, `internal/workspace/workspace.go`, `internal/security/security.go`, `internal/security/permissions.go`, `internal/toolcompress/compress.go`, orchestrator graph lifecycle wiring and associated tests; Chronos `engine/tool/builtins/workspace.go` and tests
Production path wired: request-root incremental IO, canonical built-in IO, request graph cache/freshness, fail-closed foreign-root LSP, dynamic workspace metadata, request-root policy checks, workspace/artifact-scoped stored results
Checks executed (exact commands, cwd, actual result): Chronos Code `make test` and `make build` passed; `go test -race -tags treesitter ./internal/graph -count=1` passed; `go test -race -tags lsp ./internal/lsp -count=1` passed; specified Chronos race suite and `go build ./...` passed
Evidence / artifact references: request-scope graph tests cover same-path/different-symbol isolation, freshness and unavailable fallback; LSP tests cover foreign-root refusal and symlink escape; workspace, security, compressed-result and built-in file tests cover request identity and containment
Failure or remaining uncertainty: isolated LSP currently fails closed and relies on source/graph tools rather than launching a per-worktree language server; attachment ingestion remains user-selected before execution and is not treated as an ambient isolated-worktree read
Capabilities: implemented: yes / enabled: yes / evaluated: deterministic default, Tree-sitter, LSP and race tests / released: no
Next exact action: F03 capability/effect grants, autonomous shell sandbox admission, and tenant/repository/action enforcement

### 2026-09-24 — F03 authority boundary partial

Date / implementing agent: 2026-09-24 / OpenCode
Task / substep: F03 / effect vocabulary and fail-closed planning/MCP behavior
State: in_progress
Chronos Code revision + relevant working-tree changes: baseline revision plus current F00-F03 changes
Chronos revision + relevant working-tree changes: baseline revision plus current F00-F02 changes
User work preserved / integration constraints: no external MCP process or network operation used in tests
Decision / hypothesis being resolved: tool visibility is not authority; explicit host grants must gate effect classes
Files changed: `internal/security/effects.go`, `effects_test.go`, `internal/orchestrator/session_ux.go` and tests; earlier F00 MCP identity and shell parsing files
Production path wired: explicit read/scratch-write/delivery-write/process/network/external-mutation grant vocabulary; guard enforcement; unknown dynamic tools fail closed under a grant; plan mode blocks MCP tools
Checks executed (exact commands, cwd, actual result): focused security/orchestrator race tests passed ten repetitions
Evidence / artifact references: `TestGuardEnforcesExplicitEffectGrant`, `TestEffectGrantIsCopiedFromContext`, `TestPlanModeBlocksMutatingTools`, MCP launch identity tests, compound-shell tests
Failure or remaining uncertainty: F03 is not complete because `NewWorkspaceShellTool` still executes the host shell directly; `OSSandbox` has a helper-backed implementation but is not wired into production shell execution and does not yet enforce environment/secret scope. Final authorization wiring for delivery endpoints depends on F04/F12 surfaces.
Capabilities: implemented: effect grants and trust boundaries / enabled: explicit grants only / evaluated: inert deterministic tests / released: no
Next exact action: define the autonomous shell sandbox profile/config contract, wire it fail-closed at admission, and add real filesystem/network/environment restriction integration tests

### 2026-09-24 — F03 non-sandbox authority boundary

Date / implementing agent: 2026-09-24 / OpenCode
Task / substep: F03 / all currently implementable authority and trust boundaries except production OS sandbox and complete environment/secret isolation
State: verified_with_deferred_sandbox
Chronos Code revision + relevant working-tree changes: baseline revision plus current F00-F03 changes
Chronos revision + relevant working-tree changes: baseline revision plus current F00-F03 effect declarations and prior F01/F02 changes
User work preserved / integration constraints: pre-existing untracked documentation preserved; no commits, pushes, external MCP processes or network mutations performed
Decision / hypothesis being resolved: every effect must be authorized at its execution boundary, and configuration trust must bind to immutable source and content identity rather than a display name
Files changed: Chronos `engine/tool/`, built-in tools, MCP adapter, harness/team delegation tools; Chronos Code `internal/security/`, `internal/mcpdiscover/`, `internal/config/`, `internal/authorization/`, `internal/server/`, `internal/orchestrator/`, plan CLI and associated tests
Production path wired: registry-enforced declared effects; per-invocation full/read-only grants; parent-child grant attenuation; scratch-worktree and VFS authority; conservative compound/interpreter shell analysis; exact MCP source and launch digests with preflight consistency; hook source/digest admission and process authority; HTTP and local plan tenant/repository/action authorization
Checks executed (exact commands, cwd, actual result): Chronos Code `make test && make build` passed; Chronos `go test ./... -race -count=1 && go build ./...` passed; focused security, MCP, authorization, server, orchestrator, CLI, tool, harness and team race suites passed. One simultaneous cross-repository run transiently exceeded existing one-second process-test deadlines; the isolated full rerun passed.
Evidence / artifact references: registry effect and scratch-write tests; delegation attenuation test; quoted substitution/interpreter/later-segment shell tests; MCP changed-command/source and multi-agent preflight tests; hook provenance/process-effect tests; HTTP authorization and run-identity propagation tests; local plan tenant rejection test
Failure or remaining uncertainty: production shell remains intentionally unsandboxed for this increment, OS sandbox environment/secret scope remains incomplete, and delivery-route authorization cannot be attached until F04/F12 define those records and routes
Capabilities: implemented: all non-sandbox F03 boundaries available in the current runtime / enabled: yes, fail-closed for constrained executions and configured trust / evaluated: full race suites and builds / released: no
Next exact action: begin F04 durable delivery records while retaining sandbox/environment isolation as an explicit F03 release blocker

### 2026-09-24 — Immediate hardening and F04 durable foundation

Date / implementing agent: 2026-09-24 / OpenCode
Task / substep: immediate hardening; F04 delivery domain/store; F08 incurred-usage correction
State: verified
Chronos Code revision + relevant working-tree changes: `949af96e150afc8135f17c74e6ffa171e0edbf29`; current uncommitted F00-F04 and hardening changes
Chronos revision + relevant working-tree changes: unchanged from the F03 checkpoint
User work preserved / integration constraints: existing dirty worktree and untracked documentation preserved; no PPD used; no commits, pushes, provider calls, or external mutations performed
Decision / hypothesis being resolved: routing and specialist contracts must describe executable behavior, and admission identity, goals, requirements, decisions, transitions, events, and actual incurred usage must survive retries and restart before workers are added
Files changed: `internal/defaults/agents/`, `internal/defaults/config.yaml`, `routing.yaml`, `catalog.go`; `internal/router/`; role-floor selection in `internal/orchestrator/orchestrator.go`; `internal/execution/delivery_domain.go`, `delivery_store.go`, `task_budget.go`; `internal/config/paths.go`; associated tests
Production path wired: tester role and prompt/routing hardening are active; role floors prevent unintended specialist downgrades; delivery storage has scoped atomic admission, versioned goals/requirements/decisions, legal CAS transitions, ordered idempotent events, deterministic mutation retry, schema migration, and runnable/waiting projections; delivery database path is resolved through `ProjectPaths`; already-incurred token/cost usage is recorded before limit errors
Checks executed (exact commands, cwd, actual result): Chronos Code `make test` passed; `make build` passed; `make vet` passed; focused defaults/router/execution/config/orchestrator/eval race tests passed; delivery concurrency and idempotency tests passed repeated runs
Evidence / artifact references: delivery migration/admission/scope/CAS/replay/enumeration tests; mutation retry-after-later-event tests; task-budget overage/wall-time/overflow tests; bundled-agent, prompt-contract, prompt-budget, malformed-floor, role-floor, and explicit-model-override tests
Failure or remaining uncertainty: F04 is not complete until production admission, backup/restore/rollback operations, and the queue/store boundary are wired; F05-F14 are not implemented by this increment; tester shell is approval-gated but not an OS-enforced read-only sandbox; F08 lacks persisted per-delivery reservations/accounting; runtime budget continuation remains unavailable
Capabilities: implemented: hardening and standalone F04 storage foundation / enabled: hardening only; autonomous delivery remains unavailable / evaluated: deterministic race suite, vet, and build / released: no
Next exact action: connect authenticated delivery admission to this store, define the atomic queue handoff, and implement F05 expiring leases with owner epochs and stale-worker fencing

### 2026-09-24 — F04 authenticated admission slice

Date / implementing agent: 2026-09-24 / OpenCode
Task / substep: F04 / production SQLite construction, scoped HTTP admission and inspection
State: verified increment; F04 remains in progress
Chronos Code revision + relevant working-tree changes: started at `949af96`; HEAD advanced externally to `8c95ad5` during verification, incorporating the initial admission wiring; response redaction, tests, API documentation and this checkpoint remain uncommitted
Chronos revision + relevant working-tree changes: sibling checkout retains its existing F00-F03 work; no edits made for this increment
User work preserved / integration constraints: existing staged/committed changes were not staged, reset or overwritten by this agent
Decision / hypothesis being resolved: an authenticated request may safely persist a scoped delivery before a worker exists, provided the response says `admitted`, not `queued` or `succeeded`
Files changed: `internal/cli/root.go`, `internal/server/server.go`, `middleware.go`, `delivery_handlers.go`, `delivery_handlers_test.go`, `docs/delivery-admission-http.md`
Production path wired: authenticated `serve` opens `ProjectPaths.DeliveriesDB`; POST admission binds tenant/repository/actor to validated authority, enforces a stable idempotency key and persists before HTTP 201; GET reads only the same scope without exposing the admission key. The existing `AdmitRunnable`/queue use one SQLite database transaction; no production caller promotes admitted records yet.
Checks executed (exact commands, cwd, actual result): Chronos Code `go test ./internal/server ./internal/cli ./internal/execution -count=1` passed; `go test -race ./internal/server ./internal/cli ./internal/execution -count=1` passed; `make build` passed; `make test` passed; `git diff --check` passed
Evidence / artifact references: `TestDeliveryAdmissionPersistsWithoutWorkerAndIsScoped` exercises replay after restart, idempotency, conflict, scoping and no runnable queue; `TestDeliveryAdmissionRequiresAuthenticatedAuthorizedStorage` exercises authentication, authorization and fail-closed cases
Failure or remaining uncertainty: F04 backup/restore and rollback acceptance, production executor, queue promotion, detached work and autonomous completion are not yet wired; server startup was build-checked but not exercised against a live HTTP listener in this increment
Capabilities: implemented: authenticated durable admission/inspection / enabled: authenticated HTTP admission only / evaluated: deterministic and race tests / released: no autonomous execution
Next exact action: verify F04 backup/restore and transactional rollback, then promote admitted records into the SQLite queue only with an F05 production executor and worker startup

### 2026-09-25 — F03 sandbox, F04 durability, F05/F06 foundations

Date / implementing agent: 2026-09-25 / OpenCode
Task / substep: F03 full supported-platform sandbox boundary; F04 backup/restore and queue promotion; F05 read-only worker; F06 operation journal/tool/provider continuation foundations
State: F03 and F04 verified; F05 and F06 in progress, not released as autonomous delivery
Chronos Code revision + relevant working-tree changes: `8c95ad5` plus uncommitted files in `internal/security/`, `internal/execution/`, `internal/server/`, `internal/cli/`, `internal/config/`, `internal/orchestrator/` and `docs/`; unrelated credential/model-routing edits appeared concurrently in README, CLI and orchestrator tests and were preserved
Chronos revision + relevant working-tree changes: `ce3bbc0` plus pre-existing modifications and this increment's `sandbox/container.go`, tests, `sdk/agent/agent.go`, `session.go`, and new tool-call/provider-state helpers
User work preserved / integration constraints: no commits or pushes made by this agent; user chose to start Docker Desktop, which supplied a locally cached pinned Alpine image for real isolation tests; no external provider calls made
Decision / hypothesis being resolved: incomplete host `sandbox-exec` read scoping cannot authorize unattended effects, whereas a preflighted pinned container with one workspace bind, read-only rootfs, explicit environment, and network disabled can; missing daemon/image or unsupported platform must refuse shell execution
Files changed: see the exact working-tree diff; new delivery backup, operation, worker adapter, container policy and tests live in the packages named above
Production path wired: mandatory shell context now uses a preflighted Docker backend through the real tool registry on macOS; pinned image digest/config and local socket checks fail closed; legacy bounded shell remains approval-gated. Authenticated HTTP admission persists parked records, offline backup/restore verifies checksums/integrity and never overwrites a live database, and queue promotion commits state/event/queue atomically. An explicit read-only worker option binds trusted scope, read-only effects, leases, and a common orchestrator execution request while parking unverified output. Plan schema v4 renews timed node leases and parks expired/legacy owners without replay. Operation rows bind effect keys, lease epochs, fingerprints and ordered events; wrapped effectful tools prepare/begin/observe under live leases; SDK tool/model after-hook failures stop execution and provider continuation survives session/summary persistence.
Checks executed (exact commands, cwd, actual result): Chronos Code `CHRONOS_CODE_SANDBOX_TEST_IMAGE=alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 go test -race ./internal/security -run '^TestMandatoryContainerSandbox' -count=1 -v` passed with Docker 29.1.3; focused F03–F06 race suites and `make build` passed. Chronos `go test -race ./sdk/agent ./sandbox ./engine/model ./engine/tool/... -count=1` and `go build ./...` passed. `git diff --check` passed in both repositories. A full Chronos race suite passed before the later session-continuation changes; the current affected-package race suites passed after them.
Evidence / artifact references: real workspace read/write, outside read/write, symlink, network and environment denial; admission/restart/scope/idempotency; backup restore after WAL and migration; queue promotion rollback fault injection; worker reclaim/late-owner fencing and decision parking; operation prepare/crash/observe/reconcile injection and real model-tool loop; provider state round-trip across session/summary events
Failure or remaining uncertainty: `make test` under parallel load failed once on a pre-existing concurrent-identity timing assertion, later on a 45-second graph-index threshold (45.03s) and one-second hook timeouts; the isolated concurrent fixture passed 50 race repetitions, the graph fixture passed in 35s, and the hook fixtures passed 10 repetitions. The sequential full suite reached orchestrator but exceeded the 300s host command boundary. Separately, two TUI model-picker tests fail under this machine's inherited provider credentials; no TUI files were modified here. F05 still lacks a background plan reaper and a unified plan/team/child production worker. F06 still lacks full provider-call journal/reconciliation and external idempotency observations; prior-attempt effects park rather than replay. No full-suite gate is claimed.
Capabilities: implemented: F03 macOS container boundary and F04 durable storage/backup/operator commands / enabled: F04 admission and opt-in F05 read-only worker only / evaluated: deterministic, race and real Docker tests / released: no autonomous write execution
Next exact action: finish F05 background plan scanning/reclaim and common worker integration, then close F06 provider/effect reconciliation and rerun the full suite under an uncontended gate before checking either task

### 2026-09-25 — F05 plan lease fencing and F06 replay boundaries

Date / implementing agent: 2026-09-25 / OpenCode
Task / substep: F05 / plan schema v4 lease expiry and controller renewal; F06 / live tool journal integration and provider-state replay
State: verified increments; F05/F06 still in progress
Chronos Code revision + relevant working-tree changes: `8c95ad5` plus the existing F03/F04 work and plan schema/scheduler/controller, operation wrappers and tests; concurrent unrelated model-selection edits in README, CLI and orchestrator tests remain untouched
Chronos revision + relevant working-tree changes: `ce3bbc0` plus the existing F00-F03 work and this increment's SDK tool-call ID, fatal after-hook propagation and versioned provider continuation in ordinary and summary session events; Docker container backend negotiates the daemon API version and reports missing results
User work preserved / integration constraints: no commits, pushes or provider API calls; write-enabled autonomous admission remains disabled pending F07/F10
Decision / hypothesis being resolved: expired legacy plan owners and unknown tool effects must park for reconciliation rather than silently replaying; live controller work must renew a timed lease or lose fencing authority
Files changed: `internal/plan/sqlstore.go`, `scheduler.go`, `controller.go`, `lease_expiry_test.go`; `internal/orchestrator/delivery_operation_tools*`, `delivery_executor*`; `internal/execution/delivery_operations*`, `worker.go`; Chronos `sandbox/container*`, `sdk/agent/agent.go`, `session.go`, `provider_state*`, `tool_call_context.go`
Production path wired: plan leases now expire and renew, with expired/v3 leases atomically parked and late completions fenced. Running worker-owned effectful tools persist prepare/running/observed events and stop the model loop on unknown or failed observation. A new worker checks prior attempts' effects before any model replay. SDK session and summary events preserve typed Anthropic/Responses provider continuation; unknown continuation types fail persistence/recovery rather than being silently dropped.
Checks executed (exact commands, cwd, actual result): Chronos Code `go test -race ./internal/plan ./internal/integration ./internal/orchestrator -count=1` passed; `go test -race ./internal/plan -run 'TestPlanLeaseExpiry|TestPlanLegacyLease|TestControllerRenewsLeaseDuringLongNodeExecution' -count=10` passed; Chronos `go test -race ./sdk/agent ./sandbox ./engine/model ./engine/queue ./sdk/harness ./sdk/team -count=1 && go build ./...` passed before the later log-result hardening, then `go test -race ./sandbox -count=1` passed; real Docker isolation test passed again. Prior scoped tests and builds are recorded above; run remaining full gates after this increment.
Evidence / artifact references: `TestPlanLeaseExpiryParksUnknownEffectsAndFencesLateWorker`, `TestPlanLegacyLeaseIsParkedInsteadOfAutomaticallyReplayed`, `TestControllerRenewsLeaseDuringLongNodeExecution`, `TestDeliveryToolStorageFailureAfterEffectAbortsModelLoop`, `TestWorkerRestartParksPriorUnknownEffectBeforeModelReplay`, `TestProviderContinuationPersistsAcrossSessionAndSummaryReplay`
Failure or remaining uncertainty: plan recovery needs a background enumerator and reconciled replan path; the model response/tool result pair is not yet rehydrated automatically across a crash. Generic external effects still need destination idempotency/observation adapters. Existing full-suite concurrency-sensitive tests and TUI credential-bound baseline failures are noted in the preceding entry; no full-suite success is claimed.
Capabilities: implemented: lease fencing, live tool journaling, typed provider continuation / enabled: legacy plan scheduler and opt-in read-only worker / evaluated: deterministic and affected-package race tests / released: no autonomous write delivery
Next exact action: wire common team/plan/child worker execution without resubmitting completed nodes; add F06 effect adapters and recovery receipts, then run an uncontended complete race gate

### 2026-09-25 — F05 bounded background plan reaper

Date / implementing agent: 2026-09-25 / OpenCode
Task / substep: F05 / supervisor scan and park expired plan owners across restart
State: verified increment; F05 remains in progress
Chronos Code revision + relevant working-tree changes: `8c95ad5`, existing F03-F06 work plus `internal/plan/reaper.go`, plan reaper tests, `internal/orchestrator/plan_reaper.go`, and server startup wiring
Chronos revision + relevant working-tree changes: unchanged from the prior journal entry
User work preserved / integration constraints: unrelated README and credential/model-routing tests were modified by another actor during this session and were not edited by this increment; no commits or pushes
Decision / hypothesis being resolved: a server restart must identify abandoned generation leases without unscoped customer output or automatic replay of an ambiguous effect
Files changed: `internal/plan/reaper.go`, `lease_expiry_test.go`, `internal/orchestrator/plan_reaper.go`, `internal/server/server.go`, `internal/execution/delivery_store.go`, `internal/cli/plan_cmd_test.go`
Production path wired: a server with an orchestrator runs bounded internal plan reaping until shutdown; expired and pre-migration leases are parked with stop reason ambiguity, stale completions are fenced, and live leases are renewed by the controller. An optional read-only delivery request atomically queues an authenticated admission only when a worker is configured. A concurrent delivery admission conflict now rechecks the event identity after detecting a version change so the same key reports a conflict, not a spurious stale version.
Checks executed (exact commands, cwd, actual result): Chronos Code `go test -race ./internal/plan ./internal/server ./internal/orchestrator -run 'TestPlanReaper|TestPlanLease|TestControllerRenews|TestServerStartsAndStopsOptionalDeliveryWorker|TestWorkerRestartParksPriorUnknownEffect' -count=1` passed; `go test -race ./internal/execution -run '^TestDeliveryConcurrentIdentityReuseReturnsConflict$' -count=100` passed; `go test -race ./internal/cli ./internal/execution ./internal/plan -count=1` passed after correcting the CLI's schema-version expectation; repeat affected suites and build if code changes further.
Evidence / artifact references: `TestPlanReaperRecoversExpiredOwnersAcrossRestartWithoutTouchingLiveTenants`, `TestPlanLeaseExpiryParksUnknownEffectsAndFencesLateWorker`, `TestControllerRenewsLeaseDuringLongNodeExecution`, `TestDeliveryConcurrentIdentityReuseReturnsConflict`
Failure or remaining uncertainty: plans parked on unknown effects need an evidence-backed explicit reconciliation/replan transition; the shared team/plan/child executor contract remains separate and write delivery is still gated by F07/F10. Do not claim F05 or F06 complete.
Capabilities: implemented: bounded background plan reaper and CAS conflict hardening / enabled: opt-in read-only delivery service / evaluated: focused race and crash tests / released: no autonomous write delivery
Next exact action: F05 common worker execution across plan/team/child tasks, followed by F06 idempotent/observable external effect adapters and receipt-based continuation

### 2026-09-25 — F05–F08 worker identity, plan artifact chain, receipt reuse, accounting

Date / implementing agent: 2026-09-25 / OpenCode
Task / substep: F05–F08 end-to-end integration increment
State: verified increment; F05–F08 remain in progress
Chronos Code revision + relevant working-tree changes: shared worktree on `main`; new changes in `internal/orchestrator/`, `internal/plan/`, `internal/execution/delivery_usage*`, and the plan CLI schema assertion
Chronos revision + relevant working-tree changes: no changes made in the dependency
User work preserved / integration constraints: concurrent F06–F08, TUI, and claims work in the shared tree was not overwritten; no commit or push
Decision / hypothesis being resolved: worker-rooted team/child and plan executions need inherited trusted identities and shared authority; accepted predecessor patches must be loaded from committed plan records rather than ambient HEAD, and effect/usage receipts must survive a restart without duplicating successful effects or hiding incurred usage
Production path wired: direct children now use the same hook/tool boundary as direct tools; teams receive a task context, and nested executions preserve delivery/node/attempt identity. Plan schema v5 atomically binds a selected accepted artifact to fenced node completion; dependent nodes compose transitive accepted patches in private worktrees, including across validated successor generations. A patch applied with cleanup pending parks its node instead of retrying integration. Matching observed operation receipts are reused rather than replaying an effect. Unknown-priced actual usage under a cap is retained and blocks new capped spending. Default plan concurrency and attempts are finite.
Checks executed (exact commands, cwd, actual result): `go test -race ./internal/plan ./internal/worktree ./internal/execution ./internal/orchestrator ./internal/server ./internal/cli -count=1` passed; `make build` passed; `make test` passed all packages except `internal/tui` where `TestFetchModelPickerLiveCmdWithNoAuthorizedProvidersReturnsEmpty` and `TestHandleKey_TabCompletesModel` failed under the shared TUI changes; `go test -race ./internal/plan ./internal/orchestrator -run 'TestNewGenerationKeepsOnlyPreservedCompletedArtifacts|TestPlanControllerResumesDependentAgainstAcceptedArtifactNotHEAD|TestSQLMigrationIsIdempotent|TestCompletedPlanArtifactIsAtomicAndFenced' -count=1` passed after successor-generation preservation; `go test -race ./internal/plan ./internal/orchestrator ./internal/cli ./internal/server -count=1` passed after the last code change; `git diff --check` passed.
Evidence / artifact references: `TestReadOnlyDeliveryWorkerCanDelegateToConfiguredChild`, `TestPlanControllerResumesDependentAgainstAcceptedArtifactNotHEAD`, `TestNewGenerationKeepsOnlyPreservedCompletedArtifacts`, `TestCompletedPlanArtifactIsAtomicAndFenced`, `TestObservedExternalEffectReturnsStoredReceiptForSameCall`, `TestDeliveryUsagePersistsUnknownActualUnderCapAndBlocksFurtherSpend`.
Failure or remaining uncertainty: a single durable plan/team/child production worker has not been wired; prior-attempt ambiguous effects lack host-observed reconciliation and provider-state continuation; integration after apply but before the plan receipt still requires explicit recovery rather than automatic completion; accepted artifact retention and dirty-input authorization remain incomplete. The default read-only worker and write-enabled gate remain unchanged. Do not mark F05–F08 complete.
Capabilities: implemented: bounded worker-context and artifact/accounting increments / enabled: opt-in read-only worker only / evaluated: focused and affected-package race tests, build, full-suite attempt / released: no autonomous write delivery
Next exact action: connect durable plan/team/child records to one worker-owned scheduler, add host-observed effect and artifact receipt reconciliation across restart, finish scoped dirty-input and artifact retention, then rerun the full race gate and F05–F08 acceptance fixtures.

### 2026-09-25 — F08 acceptance and F05–F07 crash/recovery increments

Date / implementing agent: 2026-09-25 / OpenCode
Task / substep: F08 cumulative authority and accounting acceptance; F05 checkpointed read-only team; F06 host-observed effect receipts; F07 safe integration and undo
State: F08 verified complete; F05–F07 remain in progress and write-enabled autonomous delivery remains gated
Chronos Code revision + relevant working-tree changes: shared `main` at `2b7e14d` plus unstaged/staged concurrent work; delivery schemas v6–v9, plan schemas v5–v7 and new worker/retention/worktree/orchestrator tests
Chronos revision + relevant working-tree changes: shared `main` at `780fdf3` plus the uncommitted tool effect adapter contract; framework provider usage presence, single-attempt durable calls, hooked summarization and sequential team checkpoints were integrated concurrently
User work preserved / integration constraints: shared commits and unrelated TUI/pricing changes were left intact; no commits or pushes by this agent
Decision / hypothesis being resolved: billable calls must be reserved under a live worker epoch, accounted for even when canceled or over authority, and separated from provider wait. Completed team members and accepted artifacts must have durable receipts before a successor can run; ambiguous effects never auto-replay.
Production path wired: live-lease-fenced provider admission, atomic session/delivery budget reservation with compensation, bounded capped requests, explicit-zero versus unknown usage, cancellation-safe reconciliation, finite SDK/HTTP retries, hooked session compaction, persisted cumulative provider/active durations and attempt billing across restart. Authenticated opt-in read-only sequential teams checkpoint member results, skip persisted completed members after restart, and park uncheckpointed billed work. Host-observed file writes and opt-in destination-backed external effects reconcile under the replacement epoch. Plan artifacts/undo receipts survive cleanup; integration journals distinguish applied/unknown patches, prevent duplicate apply, serialize competing managers, snapshot only read-approved dirty inputs and reject conflicting parent edits. Retention excludes accepted patches and undo receipts.
Checks executed (exact commands, cwd, actual result): Chronos `go test -race ./...` passed; Chronos Code `go test -race ./internal/execution ./internal/orchestrator ./internal/server ./internal/plan ./internal/worktree ./internal/budget ./internal/cli -count=1` passed before the final lease-fenced reservation change; `go test -race ./internal/execution ./internal/orchestrator -run 'TestDeliveryUsageProviderAdmissionFencesReclaimedWorker|TestDeliveryUsage|TestCappedReadOnlyWorkerReservesAndReconcilesActualProviderUsage|TestCappedDeliveryAdmissionRefundsSessionReservationBeforeProviderCall|TestDeliveryModelHook' -count=1` passed afterward; `make build` passed; `make test` passed all packages except the two pre-existing TUI model-picker assertions `TestFetchModelPickerLiveCmdWithNoAuthorizedProvidersReturnsEmpty` and `TestHandleKey_TabCompletesModel`.
Evidence / artifact references: `TestDeliveryUsageProviderAdmissionFencesReclaimedWorker`, `TestCappedDeliveryAdmissionRefundsSessionReservationBeforeProviderCall`, `TestDeliveryModelHookCapsOutputBeforeReservationAndAcceptsReportedZero`, `TestDeliveryModelHookPersistsBilledUsageAfterWorkerCancellation`, `TestDeliveryUsageSeparatesProviderWaitFromActiveComputeAcrossRestart`, `TestReadOnlyTeamWorkerRestartSkipsCheckpointedMember`, `TestReadOnlyTeamWorkerParksUncheckpointedModelCallAfterRestart`, `TestWorkerRestartObservesExternalDestinationBeforeReceiptReplay`, `TestIntegrationJournalRecoversAppliedPatchWithoutDuplicateApply`, `TestDefaultRetentionNeverDeletesAcceptedPatchesOrUndoReceipts`.
Failure or remaining uncertainty: F05 plan-node handoff still lacks a production worker contract across delivery and plan stores; F06 external destinations without an explicit observation/idempotency adapter remain parked and provider-call continuation after an unknown outcome still requires evidence; F07 does not yet atomically join the plan acceptance receipt with a delivery lease. TUI model-picker tests are failing in the shared worktree. Do not mark F05–F07 complete or enable autonomous write admission.
Capabilities: implemented: F08 accounting/authority and bounded read-only team/receipt increments / enabled: opt-in read-only worker, including priced capped calls and checkpointed sequential teams / evaluated: full Chronos race suite, affected Chronos Code race suite, build, full-suite attempt / released: no write-enabled autonomous delivery
Next exact action: resolve the cross-store F05 plan claim/integration fencing contract, close F06 provider/adapter continuation gates and F07 plan/delivery receipt transaction, then rerun the full acceptance suite and mark F05–F07 only with verified evidence.

### 2026-09-25 — F05–F07 worker and recovery gate checkpoint

Date / implementing agent: 2026-09-25 / OpenCode
Task / substep: common durable plan/team worker, fenced plan integration, effect reconciliation and retained undo receipts
State: verified increments; F08 complete; F05–F07 still in progress
Chronos Code revision + relevant working-tree changes: `2b7e14d` and concurrent staged/unstaged work. Delivery schema v10 binds operation role/node identities; plan schema v7 retains accepted artifacts and operator undo receipts.
Chronos revision + relevant working-tree changes: shared Chronos branch ahead of origin with a host-supplied effect adapter contract; concurrent cost-hook/tokenizer changes were preserved
User work preserved / integration constraints: no commits, pushes or provider API calls by this agent; shared pricing, TUI and claims changes left intact
Decision / hypothesis being resolved: plan node claims and git integration must linearize under a live delivery lease even though plan and delivery records live in different WAL databases; unknown cross-store outcomes must park with a retained filesystem receipt rather than replay.
Production path wired: authenticated opt-in read-only sequential team admission checkpoints each member, skips completed members after worker restart, and parks an uncheckpointed billed call. A host-only plan worker constructor claims pre-admitted nodes and parks completed plans for F10 acceptance, without an HTTP/CLI write-enabled route. Plan claims and integration hold a fenced delivery transaction; cancellation stops further node admission, while worktree journals/undo receipts survive interruption. File-write reconciliation uses rooted observations; opted-in external mutations require destination-backed observation, and unknown shell effects reject arbitrary success assertions. Noncheckpointed ordinary model calls park on reclaim instead of being resubmitted.
Checks executed (exact commands, cwd, actual result): Chronos `go test -race ./...` passed; Chronos Code `go test -race ./internal/execution ./internal/plan ./internal/orchestrator ./internal/worktree ./internal/retention ./internal/server ./internal/cli -count=1` passed after plan worker and effect changes; `make build` passed; `make test` exceeded the 900-second command limit before affected packages completed. Isolated `go test -race ./internal/eval -count=1` and `go test -race ./internal/graph -count=1` both passed afterward. A preceding full run completed with only the two known TUI model-picker assertions failing. `go test -race ./internal/execution -run 'TestDeliveryClaimCannotStealLeaseDuringFencedIntegration|TestDeliveryIntegrationFenceRequiresLiveEpochAndExtendsOwnership' -count=20` passed.
Evidence / artifact references: `TestAdmittedPlanWorkerStartExecutesPersistedNodesAndParksForAcceptance`, `TestReplacementPlanWorkerExecutesOnlyIncompleteAcceptedDependency`, `TestExplicitDeliveryCancelStopsPlanNodeAdmission`, `TestPlanNodeExecutorCannotIntegrateAfterDeliveryLeaseExpires`, `TestDeliveryClaimCannotStealLeaseDuringFencedIntegration`, `TestReadOnlyTeamWorkerRestartSkipsCheckpointedMember`, `TestWorkerRestartObservesExternalDestinationBeforeReceiptReplay`, `TestDurableExternalMutationWithoutObserverCannotReachDestination`, `TestPlanControllerResumesDependentAgainstAcceptedArtifactNotHEAD`, `TestUndoReconcilesReverseAppliedBeforeReceipt`.
Failure or remaining uncertainty: F05 plan execution has a host-only constructor, not a public worker admission/plan-generation handoff; only sequential teams have member-level checkpointing. F06 can reconcile destinations with an installed observer, but no production external API adapter or evidence-backed automatic provider continuation is configured. F07 retains a patch and parks a crashed integration, but the verified plan-node result cannot yet be committed from that receipt without the F10 acceptance protocol; actual read-set receipts and selective artifact GC remain open. Write-enabled autonomous delivery remains unavailable. Leave F05–F07 unchecked.
Capabilities: implemented: F08 and bounded F05–F07 recovery increments / enabled: read-only delivery and sequential teams / evaluated: affected-package race and two full-suite attempts / released: no autonomous writes
Next exact action: define the host-authenticated public plan-generation handoff, implement verified receipt-to-plan reconciliation and production downstream effect observers, then run a complete uncontended race gate before marking F05–F07.

### 2026-09-25 — F05–F07 plan handoff, HTTP observation, and receipt recovery

Date / implementing agent: 2026-09-25 / OpenCode
Task / substep: authenticated plan-generation admission, configured HTTP effect observer, and operator-only recovery of an applied plan patch after a missed node receipt
State: verified increments; F05–F07 remain in progress
Chronos Code revision + relevant working-tree changes: shared branch and concurrent worktree; `internal/server/delivery_handlers.go`, `internal/orchestrator/delivery_plan.go`, `delivery_http.go`, `plan_runtime.go`, `internal/plan/reconcile.go`, `internal/worktree/plan_receipt.go` and focused fixtures. No schema change.
Chronos revision + relevant working-tree changes: no Chronos edits by this increment; sibling checkout began an unrelated in-progress SDK tool-loop edit after the initial affected-package checks.
User work preserved / integration constraints: concurrent claims, context-report, TUI, and sibling SDK work not overwritten; no commit or push and no provider API calls.
Decision / hypothesis being resolved: plan generation is persisted before atomic delivery admission and the same key/proposal repairs a cross-store crash, but this is an idempotent handoff, not a cross-database transaction. Write-capable plan admission remains unavailable in packaged serve until its F10 acceptance gate. File integration recovery requires an attempt-bound passed verification, identical verified patch hash, retained patch, matching parent revision and postimage, a parked delivery, and a versioned plan transition. Unknown downstream mutation outcomes remain parked without destination proof.
Production path wired: authenticated `plan_generation` admission rejects absent/non-plan workers and conflicting generations; `server.delivery_http` installs a bounded same-origin HTTP mutation/GET-observation contract before operation wrappers. A host-authorized `plan.reconcile` API fences the parked delivery and integration lock while committing the recovered plan artifact, without reapplying a patch. Recovery from a prepared journal after apply is covered. No public reconcile route or write-enabled CLI switch is exposed.
Checks executed (exact commands, cwd, actual result): Chronos Code `go test -race ./internal/config ./internal/execution ./internal/plan ./internal/orchestrator ./internal/worktree ./internal/server ./internal/retention ./internal/cli -count=1` passed; `make build` passed before the sibling checkout changed; `make test` completed with failures in `internal/integration` (`TestSurfacesExecuteSeededBugfix` expects eight context sources, current concurrent context-report code emits nine) and the two previously observed TUI model-picker tests. After an in-progress sibling Chronos SDK edit, a repeat `make build` and isolated integration rerun could not compile `sdk/agent/session.go` due mismatched `handleToolCalls`/`streamLoop` signatures; no SDK work was modified here. Focused `go test -race ./internal/worktree ./internal/orchestrator ./internal/plan -run 'TestIntegrateVerifiedRejects|TestVerifiedPlanReceipt|TestVerifiedIntegrationReceipt|TestAdmittedPlanWorkerStart' -count=1` passed before that sibling edit; `go test -race ./internal/worktree ./internal/plan -run 'TestVerifiedPlanReceipt|TestIntegrateVerifiedRejects|TestPlanLeaseExpiryParksUnknownEffects' -count=1` passed afterward.
Evidence / artifact references: `TestPrepareDeliveryPlanGenerationIsRetryableAndRejectsChangedProposal`, `TestPlanGenerationAdmissionRequiresExplicitPlanWorker`, `TestObservedHTTPDestinationReconcilesRestartWithoutSecondMutation`, `TestConfiguredHTTPObserverIsInstalledBeforeDeliveryJournalWrapper`, `TestVerifiedPlanReceiptRecoversApplyBeforeReceiptAndRejectsChangedPostimage`, `TestIntegrateVerifiedRejectsPatchChangedAfterVerification`, `TestVerifiedIntegrationReceiptReconcilesExpiredPlanNodeWithoutReapplying`.
Failure or remaining uncertainty: F05 still lacks a production-enabled, positive public plan worker admission gate and non-sequential team checkpointing. F06 generic API/provider continuation and unknown external effects without a destination contract remain parked. F07 actual read-set receipts and selective artifact retention remain open; F10 acceptance is not wired to the parked plan result. Full race suite is not green. Leave F05–F07 unchecked.
Capabilities: implemented: bounded handoff/HTTP destination observation/verified patch recovery / enabled: read-only worker and host-only recovery / evaluated: affected-package race, full-suite attempt and focused crash/restart fixtures / released: no autonomous write delivery
Next exact action: after the sibling SDK edit settles, rerun the build and affected-package gate; close the F10 acceptance protocol and production plan-worker admission, provider continuation, read-set evidence and artifact retention before marking F05–F07 complete.

### 2026-09-25 — F09a in-process window renewal

Date / implementing agent: 2026-09-25 / OpenCode
Task / substep: F09a, so attached interactive runs are not stopped by routine work limits before reaching the goal
State: verified
Chronos Code revision + relevant working-tree changes: `9c54b81` plus concurrent uncommitted work (left intact). New `internal/orchestrator/long_running.go` and tests, plus `internal/config/long_running_test.go`. Surgical edits to `config.go`, `defaults/config.yaml`, `orchestrator.go` (Execute wiring, stream pause handling, agent setup), `repair.go`, `subagents.go`, `execution/result.go`, `tui/app.go`.
Chronos revision + relevant working-tree changes: `5425ee5` plus `sdk/agent/tool_loop.go`, `session_rounds.go` and tests. Edits to `agent.go`, `session.go`, `engine/model/provider.go` (`StopReasonPaused`), `summarizer.go` (pair-safe split, tool-aware prompt, 1500-token summary). One test helper now detects summarization by prompt rather than `MaxTokens == 500`.
User work preserved / integration constraints: shared working tree edited surgically; no commits or pushes
Decision / hypothesis being resolved: the user decided that routine limits must not stop an attached TUI/CLI task before its goal; stop only on success, cancel, no progress, or an explicit ceiling. A fixed 90-round SDK loop that failed with an error also lost the turn's tool history, because intermediate rounds were not persisted.
Production path wired: renew mode is enabled by the embedded defaults for interactive executions; delivery workers are unchanged (see F09a).
Checks executed (exact commands, cwd, actual result): Chronos `go test -race ./... -count=1` passed. Chronos Code `go test -race ./internal/orchestrator ./internal/execution ./internal/config ./internal/server ./internal/cli ./internal/budget ./internal/plan -count=1` passed. `make build` passed. `make test` passed except the two known TUI model-picker assertions and `internal/integration` `TestSurfacesExecuteSeededBugfix`, which expects 8 context-report sources while concurrent uncommitted `context_report.go` work adds a ninth (`working_memory`). Its CLI/TUI/HTTP executions themselves succeeded under renew defaults.
Evidence / artifact references: Chronos `TestToolLoopControllerReplacesFixedIterationCap`, `TestToolLoopControllerStopReturnsPausedResponseAfterResults`, `TestToolLoopControllerIsBoundToOneAgent`, `TestStreamToolLoopControllerStopFollowsStreamProtocol`, `TestSessionPersistsToolRoundsSoPausedWorkSurvivesTheTurn`, `TestSessionWithoutPersistToolRoundsKeepsLegacyLedger`, `TestStreamSessionPersistsRoundsAndPausedMessage`, `TestRepairToolPairsKeepsOnlyCompleteRounds`, `TestSessionCompactsInsideLongTurnAndKeepsCurrentTask`, `TestSummarize_NeverSplitsAToolRound`. Chronos Code `TestWindowGovernor*`, `TestLongRunningPolicyOnlyForInteractiveRenewMode`, `TestExecuteRenewModeRunsPastEveryFixedLimit` (150 rounds past repair limits, no wall-clock deadline), `TestExecuteRenewModePausesWithoutProgress`, `TestExecuteStreamingRenewModePauseCompletesWithNoProgress`, `TestExecuteBoundedModeKeepsFixedIterationCap`, `TestConfiguredSubagentRenewModeHasNoWallClockAndOwnWindows`, `TestLongRunningConfigValidation`, `TestLongRunningOverlayMergesOnlyDeclaredFields`.
Failure or remaining uncertainty: progress is a heuristic. A model alternating new but ineffective edits keeps renewing, and nothing caps cumulative spend unless the user sets `repair.max_cost_microdollars` (a known-price model is required). Persisted tool rounds enlarge session history and storage. Team members keep the fixed SDK cap. No live-provider multi-hour soak has been run.
Capabilities: implemented / enabled: F09a renew mode for attached interactive runs / evaluated: unit, integration-package and race suites / released: no
Next exact action: add a nonterminal "window renewed" progress event for TUI/CLI and an opt-in total-token authority ceiling that pauses with a decision, then run a real-provider soak of a multi-window task.

### 2026-09-25 — Verification evidence false negatives and repair exhaustion

Date / implementing agent: 2026-09-25 / OpenCode
Task / substep: F09a follow-up for the user-reported `budget exhausted: task budget exceeded: repair_attempts used 1 of 1`
State: verified
Decision / hypothesis being resolved: any `|`, `>`, `;` or `&&`, and unrecognized read-only commands (`cat`, `head`, …), were classified as workspace mutations. That meant `go test ./... 2>&1` or `cd pkg && go test` never counted as evidence and later staled passing checks. Prompts never stated the test+diff obligation, and the repair prompt said `command=""`. So the unmet set changed after one repair, and the single repair allowance failed the turn even in report mode.
Production path wired: `verification_observer.go` classifies compound commands per simple command (`planShellEvidence`). A check is recorded only when the exit status belongs to it (all-`&&` chain, or the last command of a sequence, never piped). Read-only commands are reads, and unparseable constructs stay conservative mutations. Repair prompts name concrete commands and the no-pipe rule. The coder/chronos-code instructions state the completion-evidence rule. In report mode an exhausted repair allowance completes with the unverified checks listed; enforce mode keeps the failure. Renew mode drops the fixed repair-attempt cap: repairs continue while the unmet set changes, and a repeated set stops the loop.
Checks executed (exact commands, cwd, actual result): `go test -race ./internal/orchestrator ./internal/config ./internal/defaults -count=1` passed. `make build` passed. `make test` failed only on the known TUI model-picker pair, the integration context-source count from concurrent `working_memory` work, and `internal/plan` `TestSQLStaleTransitionHasOneWinner` (passed `-count=5` in isolation; package untouched here).
Evidence / artifact references: `TestPlanShellEvidenceCountsCommonAgentCommands`, `TestClassifyShellCommandIsConservative`, `TestRepairAllowanceExhaustionIsAdvisoryInReportMode`, `TestRepairPromptNamesConcreteCommands`, `TestRenewModeDropsTheFixedRepairAttemptCap`.
Failure or remaining uncertainty: a check piped into a filter (`go test ./... | tail`) is still not evidence, because its exit status belongs to the filter. The streaming report-mode note path has no dedicated test (shares the blocking logic).

### 2026-09-26 — F05 candidate plan worker and routed delivery executor

Date / implementing agent: 2026-09-26 / OpenCode
Task / substep: F05 / read-only+scratch plan execution through one worker service (the F05 gate scope recorded in the checkpoint)
State: verified increment; F05 remains in progress
Chronos Code revision + relevant working-tree changes: `c40573d` plus this increment; concurrent uncommitted indexer work (`internal/indexer/extract/...`, `cmd/zzprobe/`) not touched
Chronos revision + relevant working-tree changes: `a5ae191`, clean; no edits
User work preserved / integration constraints: no commits, pushes, provider calls or external mutations
Decision / hypothesis being resolved: a plan generation can run end to end under a worker lease with only read, scratch-write and sandboxed-process grants if verified node patches are retained as content-addressed candidates and dependents compose them privately, so the user's checkout is never written before F10 acceptance
Files changed: `internal/orchestrator/plan_executor.go` (candidate mode: `StoreArtifact` then worktree removal, no `IntegrateVerified`), `delivery_plan.go` (`NewCandidatePlanDeliveryExecutor`, narrowed grant, per-executor `PolicyReference`), new `delivery_router.go` (`RoutedDeliveryExecutor`), `internal/execution/delivery_domain.go` (`CandidatePlanPolicyReference`), `worker.go` (`CanRunCandidatePlan`, `CanRunCappedPlan`, `PlanPolicyReference`), `internal/server/delivery_handlers.go` (plan admission stamps the installed worker's policy; capped plans need plan-specific call admission), `internal/cli/root.go` (`serve --delivery-plan-worker`), new `internal/orchestrator/delivery_plan_candidate_test.go`
Production path wired: `serve --delivery-read-only-worker` and/or `--delivery-plan-worker` install one routed worker; the route comes from the persisted admission policy. Candidate and write plan executors each reject the other's policy. A worker without a matching executor parks the delivery instead of running it. Capped plan admission stays refused, as before.
Checks executed (exact commands, cwd, actual result): Chronos Code `go test -race ./internal/orchestrator -run 'TestCandidatePlanWorker|TestPlanExecutorsRejectTheOtherAdmissionPolicy|TestRoutedDeliveryExecutor' -count=1 -v` passed; a mutation that disabled candidate mode made `TestCandidatePlanWorkerRetainsComposedArtifactsWithoutTouchingCheckout` fail (integration receipt recorded); `go test -race ./internal/orchestrator ./internal/execution ./internal/server ./internal/cli ./internal/plan ./internal/worktree -count=1` passed; `go build ./...`, `make build` and `git diff --check` passed. Full `make test` not rerun for this increment.
Evidence / artifact references: `TestCandidatePlanWorkerRetainsComposedArtifactsWithoutTouchingCheckout` (dependent node sees the predecessor's API, two retained patches without receipts, clean `git status`, parked `waiting_decision` result, grant excludes delivery write, network and external mutation), `TestPlanExecutorsRejectTheOtherAdmissionPolicy`, `TestRoutedDeliveryExecutorParksKindsWithoutAnInstalledExecutor`
Failure or remaining uncertainty: **production blocker.** `--delivery-plan-worker` cannot start in a packaged build. `NewPlanDeliveryExecutor` and `PrepareDeliveryPlan` require `closedLoopPPDEnabled()`, but `buildRuntimeCapabilityManifest` never advertises `planning:closed-loop-ppd`, and `routing.ppd.mode: enabled` makes that capability required, which fails startup. The flag therefore fails closed with an explicit error. No positive HTTP plan-admission test exists for the same reason. Also open: shell inside candidate nodes needs a configured delivery sandbox image, otherwise it fails closed; a crash after `StoreArtifact` but before node completion leaves an unreferenced patch, and the reaper parks the node (no replay); non-sequential team checkpointing is not implemented.
Capabilities: implemented: candidate plan execution and routed worker / enabled: no in packaged builds (capability admission blocks it) / evaluated: deterministic race tests with real git worktrees / released: no
Next exact action: decide how the plan worker's capability is admitted (see checkpoint), then add the positive HTTP admission-to-worker test from a real `orchestrator.New` startup; then non-sequential team checkpointing

### 2026-09-26 — F05 candidate plan worker capability decoupled from closed-loop PPD

Date / implementing agent: 2026-09-26 / OpenCode
Task / substep: F05 / make the candidate plan worker startable in packaged builds (user decision: option a)
State: verified increment; F05 remains in progress
Chronos Code revision + relevant working-tree changes: `c40573d` plus the prior 2026-09-26 increment and this one; another actor is concurrently editing `internal/indexer/` (its build was briefly broken mid-edit during this session and recovered without changes from me)
Chronos revision + relevant working-tree changes: `a5ae191`, clean
User work preserved / integration constraints: no commits, pushes or provider calls
Decision / hypothesis being resolved: a delivery plan worker that never writes the checkout needs only plan storage, a node controller bound to an implementation role, patch retention and a repository root. It should not borrow the interactive closed-loop PPD capability, whose admission belongs to F13.
Files changed: `internal/orchestrator/orchestrator.go` (records `planImplementationAgent` at startup), `delivery_plan.go` (`candidatePlanRuntimeReady`; `NewCandidatePlanDeliveryExecutor` uses it; `PrepareDeliveryPlan` accepts closed-loop PPD or candidate wiring), `internal/cli/root.go` (error text), `internal/orchestrator/delivery_plan_candidate_test.go`, `internal/server/delivery_handlers_test.go`
Production path wired: `serve --delivery-plan-worker` now constructs from a normal `orchestrator.New` startup. Authenticated `plan_generation` admission persists the draft generation and queues the delivery with `plan-candidate-worker-v1`. The write-mode `NewPlanDeliveryExecutor` still requires `planning:closed-loop-ppd` and has no CLI path.
Checks executed (exact commands, cwd, actual result): `go test -race ./internal/orchestrator -run 'TestCandidatePlan|TestPlanExecutorsReject|TestRoutedDelivery' -count=1` passed; `go test -race ./internal/server -run TestCandidatePlanAdmission -count=1 -v` passed; `go vet` on orchestrator/server/execution/cli passed; `go test -race ./internal/orchestrator ./internal/execution ./internal/server ./internal/cli ./internal/plan ./internal/worktree ./internal/integration -count=1`: all passed except `internal/orchestrator`, which failed once under that seven-package parallel load (the failing test name was not captured) and then passed two isolated full-package race reruns (`go test -race ./internal/orchestrator -count=1`, exit 0 both times); `make build` passed.
Evidence / artifact references: `TestCandidatePlanHandoffRunsDecomposedGenerationWithoutClosedLoopCapability` (strategist JSON → draft generation → worker activates and runs both nodes; checkout untouched; runtime with neither gate refused), `TestCandidatePlanAdmissionFromProductionStartup` (real startup; write executor refused without capability; candidate executor constructed; capped plan 503; admission 202 queued with candidate policy; idempotent retry; changed proposal 409), `TestPlanExecutorsRejectTheOtherAdmissionPolicy`, `TestRoutedDeliveryExecutorParksKindsWithoutAnInstalledExecutor`
Failure or remaining uncertainty: the startup-based server test stops at admission because running the worker would call a real provider; worker execution is covered in the orchestrator package with a fake runner. There is one unidentified orchestrator failure under parallel load, consistent with the timing-sensitive tests noted in earlier entries but not proven to be one of them. Non-sequential team checkpointing and the uncontended full gate remain before F05 can be checked.
Capabilities: implemented: yes / enabled: opt-in `serve --delivery-plan-worker` (candidate artifacts only) / evaluated: deterministic race tests including a real-startup admission test / released: no
Next exact action: F05 non-sequential team member checkpointing (Chronos `sdk/team` plus `internal/orchestrator/delivery_team.go`), then an uncontended `make test`

### 2026-09-26 — F05 checkpointed parallel teams with per-member call attribution

Date / implementing agent: 2026-09-26 / OpenCode
Task / substep: F05 / member-level checkpointing for non-sequential teams (parallel slice)
State: verified increment; F05 remains in progress
Chronos Code revision + relevant working-tree changes: `c40573d` plus the 2026-09-26 increments and this one; concurrent `internal/indexer/` work untouched
Chronos revision + relevant working-tree changes: `a5ae191` plus uncommitted `sdk/team/parallel.go` and new `sdk/team/parallel_checkpoint_test.go`. **Chronos Code now needs this Chronos change.** `release/chronos.version` still pins `a5ae191`, so CI built from the pin will not compile until the Chronos change is committed and the pin is advanced.
User work preserved / integration constraints: no commits, pushes or provider calls
Decision / hypothesis being resolved: parallel members finish in any order, so the sequential "delivery-wide reconciled total" receipt cannot tell which member's billed calls a receipt covers. Attribute each model call to the host-issued member node, then resume only members without receipts. Park when a member has billed calls but no receipt.
Files changed: Chronos `sdk/team/parallel.go` (`RunParallelWithCheckpoints`, `ParallelCheckpoint`: serialized checkpoints, member node identity `team:<id>:<step>`, completed members merged in `Order` without resubmission, a checkpoint failure cancels the rest regardless of `ErrorMode`, the completion set is snapshotted before launch, and a missing member returns an error instead of a nil dereference); Chronos Code `internal/execution/delivery_store.go` (schema v11: `delivery_usage_calls.node_id`), `delivery_usage.go` (`UsageReservation.NodeID`, `UsageByNode`), `delivery_operation_context.go` (`Execution.UsageByNode`), `internal/orchestrator/delivery_usage_hook.go` (records `identity.NodeID`), `delivery_team.go` (`runDurableParallelTeam`, v2 per-member checkpoint; `durableTeam` keeps concurrency, error mode and merge for programmatic parallel teams), `internal/server/delivery_handlers.go` (read-only team admission accepts sequential or parallel teams); new `internal/orchestrator/delivery_team_parallel_test.go`, `sdk/team/parallel_checkpoint_test.go`; extended `TestAuthenticatedTeamAdmissionBindsConfiguredCheckpointedWorker`
Production path wired: authenticated `run_read_only` + `team_id` admission of a configured parallel team runs through the routed worker. Each member's receipt is persisted under the live lease with its own reconciled call count. On restart the worker checks per-node usage against receipts before any model call.
Checks executed (exact commands, cwd, actual result): Chronos `go test -race ./sdk/team -run TestParallelCheckpoint -count=50` passed; `go test -race ./sdk/team ./sdk/agent ./sdk/harness -count=1` and `go build ./...` passed. Chronos Code `go test -race ./internal/orchestrator -run 'TestReadOnlyParallelTeam|TestReadOnlyTeamWorker' -count=10` passed. Two mutations were each detected by a failing test: treating a billed member without a receipt as resumable, and dropping the completed set on resume. `go vet` passed on orchestrator/execution/server/cli. `go test -race` passed for `./internal/execution`, `./internal/server`, `./internal/cli` and `./internal/orchestrator`. `make build` and `git diff --check` passed.
Evidence / artifact references: `TestParallelCheckpointResumesOnlyUncheckpointedMembers`, `TestParallelCheckpointRejectsInvalidRequests`, `TestReadOnlyParallelTeamWorkerRecordsPerMemberReceipts`, `TestReadOnlyParallelTeamRestartRunsOnlyMemberWithoutReceipt` (first member's receipt survives restart; only the second runs; merged result in order), `TestReadOnlyParallelTeamParksBilledMemberWithoutReceipt`, `TestAuthenticatedTeamAdmissionBindsConfiguredCheckpointedWorker` (parallel team 202, router team 400)
Failure or remaining uncertainty: coordinator, router, swarm and hierarchy teams still have no member checkpoints and remain refused for durable admission. Calls recorded before schema v11 have `node_id = ''`, so a pre-upgrade in-flight parallel team parks rather than resumes (none existed, because parallel admission was refused before this change). `executeAgent`'s non-streaming fallback to `Agent.Run` after a failed call is inherited from sequential teams and still bills a second attributed call. Full `make test` not rerun.
Capabilities: implemented: yes / enabled: opt-in read-only worker for sequential and parallel teams / evaluated: deterministic race and crash/restart tests / released: no
Next exact action: commit the Chronos change and advance `release/chronos.version` (needs user authorization); decide whether coordinator/hierarchy member checkpointing belongs to F05 or to F11, whose scope already names all four patterns; then run an uncontended `make test` to close F05

### 2026-09-26 — F05 gate and completion

Date / implementing agent: 2026-09-26 / OpenCode
Task / substep: F05 / full gate and completion against its acceptance criteria (scope as decided in the checkpoint)
State: verified; F05 complete
Chronos Code revision + relevant working-tree changes: `c40573d` plus the 2026-09-26 F05 increments (uncommitted); concurrent `internal/indexer/` work present in the tree and included in the gate
Chronos revision + relevant working-tree changes: `a5ae191` plus uncommitted `sdk/team/parallel.go` and `sdk/team/parallel_checkpoint_test.go`. Commits deferred by the user; the CI pin does not include these changes yet.
User work preserved / integration constraints: no commits, pushes or provider calls
Decision / hypothesis being resolved: whether F05's acceptance holds without coordinator/hierarchy team checkpointing. User decision 2026-09-26: those patterns move to F11 under the replay-based step-log design recorded in F11.
Files changed: this guide only (F11 design section, F05 scope note, index, checkpoint)
Production path wired: unchanged from the preceding F05 entries
Checks executed (exact commands, cwd, actual result): Chronos Code `make test` (full `-race -count=1` suite, no other test load) passed every package, including orchestrator, integration, server, execution, plan, cli, tui, graph and indexer. Chronos `go test ./sdk/agent ./sdk/harness ./sdk/team ./sdk/memory ./engine/graph ./engine/queue ./engine/tool/... ./storage/adapters/sqlite -race -count=1` passed, and `go build ./...` passed.
Evidence / artifact references: acceptance mapping, with each criterion's test:
- production construction plus worker start executes an admitted fixture: `TestServerStartsAndStopsOptionalDeliveryWorker`, `TestCandidatePlanAdmissionFromProductionStartup`, `TestAdmittedPlanWorkerStartExecutesPersistedNodesAndParksForAcceptance`
- kill/restart reclaim, and a stale worker cannot finalize or integrate: `TestReplacementPlanWorkerExecutesOnlyIncompleteAcceptedDependency`, `TestPlanNodeExecutorCannotIntegrateAfterDeliveryLeaseExpires`, `TestDeliveryClaimCannotStealLeaseDuringFencedIntegration`
- waiting states survive restart and restarts resume only incomplete work: `TestReadOnlyTeamWorkerRestartSkipsCheckpointedMember`, `TestReadOnlyParallelTeamRestartRunsOnlyMemberWithoutReceipt`, `TestReadOnlyParallelTeamParksBilledMemberWithoutReceipt`
- detached work outlives the request: `TestReadOnlyDeliveryAdmissionQueuesAtomicallyAndOutlivesRequest`
- explicit cancel stops admission: `TestExplicitDeliveryCancelStopsPlanNodeAdmission`
- no plan stuck on a vanished owner: `TestPlanReaperRecoversExpiredOwnersAcrossRestartWithoutTouchingLiveTenants`
- one routed worker for chat, team and plan kinds: `TestRoutedDeliveryExecutorParksKindsWithoutAnInstalledExecutor`
- write capability stays unavailable: `TestPlanExecutorsRejectTheOtherAdmissionPolicy`
Failure or remaining uncertainty: the implementation depends on uncommitted Chronos changes, so publishing requires committing them and advancing `release/chronos.version`. Coordinator, router, swarm and hierarchy durability belongs to F11. Write-enabled delivery remains gated on F06/F07/F10. `executeAgent`'s non-streaming fallback still makes a second billed (attributed) call after a failed call.
Capabilities: implemented: yes / enabled: opt-in `serve --delivery-read-only-worker` and `--delivery-plan-worker` (read-only, sequential/parallel teams, candidate plans) / evaluated: full race suites in both repositories / released: no
Next exact action: F06, starting with automatic rehydration of an observed provider response and tool-result pair across a worker crash, plus the fault-injection matrix in F06's acceptance

### 2026-09-26 — F06 complete-round checkpoint and crash gates

State: implementation in progress; F06 not yet complete.
Chronos change: `sdk/agent` checkpoints full assistant/tool rounds and restores provider-owned continuation types when resuming blocking chat or session chat. Chronos Code change: schema v12 stores those rounds under a live delivery lease, checks operation coverage and billed-call counts before a replacement resumes, and keeps uncheckpointed/ambiguous effects parked. Sequential and parallel team member checks accept only fully checkpointed extra calls.
Checks executed: `go test -race ./sdk/agent ./sdk/team -count=1` and `go build ./...` in Chronos passed. `go test -race ./internal/execution ./internal/orchestrator -count=1` and `go build ./...` in Chronos Code passed after the concurrent graph/config migration settled. The four crash gates (`TestOperationJournalCrashBoundaries`), SDK chat/session recovery (`TestToolRoundJournalResumesWithoutRepeatingReplyOrTool`), and worker restart (read and observed effect, including billed provider calls, `TestWorkerRestartResumesCheckpointedAssistantAndToolResult`) passed. `git diff --check` passed in both repositories. No graph/config files were modified by this increment.
Next exact action: address F06's remaining destination adapters and incomplete-round/provider-only recovery decisions without treating an ambiguous effect as success; plan-node continuation still parks incomplete nodes pending its own receipt protocol.

### 2026-09-26 — F06 terminal provider reply recovery

State: verified increment; F06 remains open for destination-specific API adapters and incomplete-outcome decisions.
Chronos change: optional `AgentReplyJournal` checkpoints terminal blocking chat/session replies after output validation, and restores them before any new model admission or session message append. Chronos Code change: delivery schema v13 stores lease-fenced replies only when every provider call in the invocation has reconciled usage and preceding tool rounds have receipts. Replacement workers count both terminal replies and tool rounds against billed calls; unknown usage parks.
Evidence: `TestWorkerRestartReusesCompletedProviderReply` (plain/session, no duplicate provider call or session messages), `TestWorkerRestartParksUnknownProviderReply`, `TestAgentReplyCheckpointRequiresReconciledCallAndLiveLease`, `TestAgentReplyCheckpointFailureCannotBecomeReplayReceipt`, and `TestAgentReplyJournalDoesNotResubmitCompletedProviderCall`.
Checks executed: Chronos Code `go test -race ./internal/execution ./internal/orchestrator -count=1`, `go build ./...`, `git diff --check` passed; Chronos `go test -race ./sdk/agent ./sdk/team -count=1`, `go build ./...`, `git diff --check` passed.
Next exact action: select a destination with an authoritative observation/idempotency contract before adding an API-specific adapter. Keep unresolved external and provider outcomes parked; candidate plan-node continuation requires a separate receipt protocol.

### Per-increment record template

```text
Date / implementing agent:
Task / substep:
State: in_progress | verified | blocked
Chronos Code revision + relevant working-tree changes:
Chronos revision + relevant working-tree changes:
User work preserved / integration constraints:
Decision / hypothesis being resolved:
Files changed:
Production path wired:
Checks executed (exact commands, cwd, actual result):
Evidence / artifact references:
Failure or remaining uncertainty:
Capabilities: implemented / enabled / evaluated / released:
Next exact action:
```

### Resume checklist

- Re-read current goal and this checkpoint; confirm branch/revision and user edits.
- Verify that referenced artifacts and previous test evidence still apply.
- Reconcile any interrupted tool, migration or external effect before retrying.
- Resume incomplete substeps only; do not repeat a completed edit or integration because the conversation was compacted.
- Run checks justified by new changes or unresolved concerns, then update the task index and journal.
- Never mark the implementation complete while an accepted requirement or release gate is merely planned.
