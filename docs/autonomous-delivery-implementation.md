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
| [ ] | F03 | Effect authority, sandbox, MCP and tenant boundaries | F01, F02 |
| [ ] | F04 | Durable delivery domain and schema | F00 |
| [ ] | F05 | Durable worker lifecycle and common executor | F01, F03, F04 |
| [ ] | F06 | Operation journal and effect reconciliation | F04, F05 |
| [ ] | F07 | Accepted artifact lineage and integration | F02, F04, F06 |
| [ ] | F08 | Accurate cumulative accounting and admission | F01, F04 |
| [ ] | F09 | Automatic budget checkpoint and continuation | F05, F06, F07, F08 |
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

- Active task: F04 has durable authenticated HTTP admission/inspection wired into `serve`, with admitted records intentionally parked until F05 production executor wiring. F03 remains deferred at production sandbox/environment isolation.
- Completed tasks: F00, F01, F02.
- Runtime budget continuation implemented: no.
- Immediate hardening: verified truthful bundled routing, role-aware model floors, tester role, structured specialist handoffs, and prompt/runtime contract tests.
- Next action: finish F04 backup/restore and rollback checks; connect F05 production executor and atomically promote admitted records to the existing SQLite queue. Then wire worker lifecycle while retaining the F03 sandbox/environment release blocker.
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
