---
sidebar_position: 4
title: Plan
description: SQLStore.Graph, PlanScope, PlanRef, PlanGraph — durable PPD plan storage
---

# Plan

The plan subsystem (`internal/plan`) provides durable storage for PPD (Plan-Prior-Do) plans
used by the `ppd-planner` specialist. Plans are persisted in a SQLite database and survive
process restarts.

## Purpose

PPD plans are used for high-risk or high-complexity work where changes span multiple packages,
files, or call chains. The plan provides a structured, resumable decomposition that the
`ppd-planner` agent works through in topological order.

## Core Types

All types are defined in `internal/plan/model.go`.

### PlanScope

`PlanScope` defines the boundary of a plan:

| Field | Description |
|-------|-------------|
| Root | Project root path |
| Packages | Package paths in scope |
| Files | Specific files in scope (optional) |
| MaxDepth | Maximum call-chain depth to consider |

### PlanRef

`PlanRef` is a reference to a specific plan record:

| Field | Description |
|-------|-------------|
| ID | Unique plan identifier (`PlanID`) |
| TenantID | Tenant identifier |
| TaskID | Task identifier |
| Generation | Current generation ID |
| State | `draft` \| `active` \| `paused` \| `replanning` \| `completed` \| `failed` \| `canceled` |

### PlanGraph (Plan)

`Plan` is the full DAG of work items within a plan, including nodes, dependencies, attempts,
context references, evidence, leases, and events:

```go
type Plan struct {
    TenantID     TenantID
    TaskID       TaskID
    ID           PlanID
    Generation   GenerationID
    State        PlanState
    Nodes        []Node
    Dependencies []Dependency   // DAG edges: NodeID cannot run until DependsOn completes
    ContextRefs  []ContextRef
    Evidence     []Evidence
    Leases       []Lease
    Events       []Event
}
```

## Node State Machine

Each `Node` within a `Plan` has its own lifecycle:

```
Proposed → Pending → Ready → Leased → Running → Completed
                                    ↘ RetryWait → Ready
                                    ↘ Blocked → Ready | Canceled
                                    ↘ Failed
                                    ↘ Canceled
```

Valid transitions are enforced by `Node.Transition(next NodeState)`. Invalid transitions return
`ErrInvalidNodeTransition`.

## Plan State Machine

The plan itself follows a parallel state machine:

```
Draft → Active → Paused ↺ Active
               ↘ Replanning → Active
               ↘ Completed
               ↘ Failed
               ↘ Canceled
```

`Plan.Transition(next PlanState)` enforces valid transitions. `Plan.ValidateDAG()` rejects
duplicate node IDs, dangling dependencies, and cycles.

## SQLStore.Graph

`SQLStore.Graph(ctx, ref)` retrieves the `Plan` (PlanGraph) for a given plan ID from the SQLite
store. The `SQLStore` in `internal/plan/sqlstore.go` supports:

| Operation | Description |
|-----------|-------------|
| `Create` | Create a new plan record |
| `Graph` | Load the full plan DAG |
| `UpdateNode` | Update the status of a single node |
| `Complete` | Mark the entire plan as completed |
| `List` | List plans for a tenant/task |
| `Delete` | Remove a plan record |

## CLI Commands

```bash
chronos-code plan --db <path>          # use a specific plan database
chronos-code plan list --db <path>     # list all plans
chronos-code plan show <id>            # show a plan graph
chronos-code plan delete <id>          # delete a plan
```

## PPD Routing Integration

When `ppd.mode: enabled` in `routing.yaml`, the orchestrator routes qualifying work to
`ppd-planner`. The planner:

1. Creates a new `Plan` (Draft → Active) via `SQLStore`
2. Works through nodes in topological order
3. Updates node status after each step (`Running → Completed | Failed`)
4. Marks the plan `completed` or `failed` when all nodes settle

The plan survives context compaction — the `ppd-planner` can resume from `SQLStore` even if the
conversation history was summarized.

```bash
# Resume a plan explicitly
chronos-code --resume <session-id>
```

## Shadow Mode

When `ppd.mode: shadow`, routing decisions are logged without invoking `ppd-planner` or
creating plan records:

```bash
chronos-code eval ppd --validate-only   # validate PPD registration only
chronos-code eval ppd --report          # requires completed real-model evidence
```

## Stop Reasons

When a node or plan halts before completion, a `StopReason` is recorded:

| StopReason | Cause |
|------------|-------|
| `ambiguity` | Task specification was ambiguous |
| `approval_denied` | User rejected a required approval |
| `budget_exhausted` | Token or USD budget exceeded |
| `verification_failed` | Verification gate rejected the output |
| `capability_missing` | Required tool or skill not available |
| `retry_exhausted` | All retry attempts failed |
| `user_decision_required` | Blocking decision needs human input |

## See Also

- [Orchestrator — PPD Delegation](./orchestrator#ppd-delegation)
- [Architecture Overview](../diagrams/architecture-overview)
