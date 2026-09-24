---
unlisted: true
sidebar_position: 5
title: Plan
description: SQLStore.Graph, PlanScope, PlanRef, PlanGraph — durable work-plan storage
---

# Plan

The plan subsystem (`internal/plan`) provides durable DAG storage and scheduling. Plans are persisted in SQLite and survive process restarts.

## Purpose

`delivery-strategist` can propose a strict next frontier of up to six nodes. Each node has a kind (`investigate`, `decide`, `implement`, `verify`, or `integrate`), objective, scope boundary, expected artifacts, assumptions, invalidation triggers, recovery class, risks, and verification. The strategist does not execute nodes or report completion.

## Core Types

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
| ID | Unique plan identifier (UUID) |
| SessionID | Associated session ID |
| CreatedAt | Creation timestamp |
| Status | `pending` \| `in_progress` \| `completed` \| `failed` |

### PlanGraph

`PlanGraph` is the DAG (directed acyclic graph) of work items within a plan:

| Field | Description |
|-------|-------------|
| Nodes | Ordered list of `PlanNode` items |
| Edges | Dependency edges between nodes |
| Metadata | Arbitrary YAML metadata per node |

Each node carries a stable ID and lifecycle state plus the explicit work contract described above. Dependencies are admitted only as DAG edges; scope is not used as the task instruction.

## SQLStore.Graph

`SQLStore.Graph(ctx, ref)` retrieves the `PlanGraph` for a given `PlanRef` from the SQLite store. The store supports:

| Operation | Description |
|-----------|-------------|
| `Create` | Create a new plan record |
| `Graph` | Load the plan DAG |
| `UpdateNode` | Update the status of a single node |
| `Complete` | Mark the entire plan as completed |
| `List` | List plans for a session |
| `Delete` | Remove a plan record |

## CLI Commands

Plans are managed via the `plan` sub-command:

```bash
chronos-code plan --db <path>          # use a specific plan database
chronos-code plan list --db <path>     # list all plans
chronos-code plan show <id>            # show a plan graph
chronos-code plan delete <id>          # delete a plan
```

The `--db` flag specifies the SQLite database path. This allows using a separate database per project or sharing across projects.

## Delivery Strategy Integration

The public routing key remains `ppd` for compatibility. Embedded defaults use `shadow`, which records a qualifying decision without invoking `delivery-strategist` or creating a plan. Enabled routing invokes the read-only specialist for one proposal turn. A separate gated `ExecutePlan` path can parse, persist, schedule, and synthesize a supplied frontier, but it is not wired as a production rolling-replanning loop.

```bash
# Resume a plan explicitly
chronos-code --resume <session-id>
```

## Shadow Mode

When `ppd.mode: shadow`, the orchestrator records routing decisions without invoking `delivery-strategist` or creating plan records.

```bash
chronos-code eval ppd --validate-only   # validate PPD registration only
chronos-code eval ppd --report          # requires completed real-model evidence
```

## See Also

- [Data Flow Diagram](../diagrams/data-flow)
- [Orchestrator — PPD Delegation](./orchestrator#ppd-delegation)
- [Architecture Overview](./intro)
