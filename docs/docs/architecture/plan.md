---
unlisted: true
sidebar_position: 5
title: Plan
description: SQLStore.Graph, PlanScope, PlanRef, PlanGraph — durable PPD plan storage
---

# Plan

The plan subsystem (`internal/plan`) provides durable storage for PPD (Plan-Prior-Do) plans used by the `ppd-planner` specialist. Plans are persisted in a SQLite database and survive process restarts.

## Purpose

PPD plans are used for high-risk or high-complexity work where changes span multiple packages, files, or call chains. The plan provides a structured, resumable decomposition that the `ppd-planner` agent works through in order.

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

Each `PlanNode` carries:
- A unique ID within the graph
- A description of the work
- Status (`pending` / `in_progress` / `completed` / `failed`)
- Optional verification criteria

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

## PPD Routing Integration

When `ppd.mode: enabled` in `routing.yaml`, the orchestrator routes qualifying work to `ppd-planner`. The planner:

1. Creates a new `PlanRef` and `PlanGraph` via `SQLStore`
2. Works through nodes in topological order
3. Updates node status after each step
4. Marks the plan `completed` or `failed` when done

The plan survives context compaction — the `ppd-planner` can resume from `SQLStore` even if the conversation history was summarized.

```bash
# Resume a plan explicitly
chronos-code --resume <session-id>
```

## Shadow Mode

When `ppd.mode: shadow`, the orchestrator simulates PPD routing decisions and logs them without actually invoking `ppd-planner` or creating plan records. Use this to observe which tasks would be routed to PPD without changing behavior.

```bash
chronos-code eval ppd --validate-only   # validate PPD registration only
chronos-code eval ppd --report          # requires completed real-model evidence
```

## See Also

- [Data Flow Diagram](../diagrams/data-flow)
- [Orchestrator — PPD Delegation](./orchestrator#ppd-delegation)
- [Architecture Overview](./intro)
