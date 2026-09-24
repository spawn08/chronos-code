---
sidebar_position: 4
title: Plan
description: SQLStore.Graph, PlanScope, PlanRef, PlanGraph — durable work-plan storage
---

# Plan

The plan subsystem (`internal/plan`) provides durable DAG storage and scheduling. Plans are
persisted in SQLite and survive process restarts.

## Purpose

For high-risk or broad work, `delivery-strategist` can propose the next bounded frontier. It is
read-only and does not execute nodes. A frontier contains at most six `investigate`, `decide`,
`implement`, `verify`, or `integrate` nodes with explicit objectives, expected artifacts,
assumptions, invalidation triggers, recovery classes, risks, and verification.

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

## Delivery Strategy Integration

The `ppd` routing key remains for compatibility. Enabled mode delegates one turn to
`delivery-strategist`; shadow mode records the same decision without invoking it. The durable
controller can execute an explicitly admitted generation, but no production closed loop asks
the strategist for successive rolling frontiers.

```bash
# Resume a plan explicitly
chronos-code --resume <session-id>
```

## Shadow Mode

When `ppd.mode: shadow`, routing decisions are logged without invoking `delivery-strategist` or
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
