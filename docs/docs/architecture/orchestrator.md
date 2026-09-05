---
sidebar_position: 2
title: Orchestrator
description: Agent lifecycle, ToolPhase, ContextGuard budget math, and ContextReport
---

# Orchestrator

The orchestrator (`internal/orchestrator`) is the central coordinator of Chronos Code. It is constructed once per process and owns agent lifecycle, routing, context management, and turn execution.

## Responsibilities

| Concern | Description |
|---------|-------------|
| Config resolution | Merges CLI flags, env vars, project YAML, user YAML, and embedded defaults |
| Agent wiring | Loads agent YAML, skills, and guardrails; wires them into Chronos |
| Graph indexing | Triggers workspace graph indexing at startup |
| Session management | Loads or creates a session; persists after each turn |
| Memory integration | Reads project/user/feedback memory into context |
| Routing | Applies Router decisions to select model tier and implementation path |
| Turn execution | Drives the Chronos agent loop; applies guardrails around tool calls |
| Learning | Emits pending suggestions after turns when learning is enabled |

## ToolPhase Lifecycle

Each tool call during a turn moves through a defined phase:

```
PENDING → EXECUTING → COMPLETED
                   ↘ FAILED
```

Guardrails intercept at the `PENDING → EXECUTING` boundary. A guardrail deny produces `FAILED` without executing the tool. The orchestrator collects phase results for the `ContextReport`.

## ContextGuard Budget Math

The orchestrator enforces a context budget through the `ContextGuard` (implemented across `internal/activation`, `internal/attention`, `internal/incctx`, and `internal/toolcompress`).

The effective token limit is:

```
effectiveLimit = min(modelContextWindow, configuredBudget)
```

When the running token count approaches `effectiveLimit`, the orchestrator:

1. **Trims** low-priority context sources (tool results, history) using `toolcompress`
2. **Rejects** new tool output if trimming is insufficient to fit within budget

Priority order for trimming (lowest first):

| Priority | Source |
|----------|--------|
| 1 (trim first) | Historical tool results (non-current turn) |
| 2 | Prior session summaries (when `recall_prior_summaries: true`) |
| 3 | Memory injections (project/user/feedback) |
| 4 | Skill context |
| 5 (last resort) | Current turn tool output |

## ContextReport

The `/context` TUI command and `context_report: true` config expose a `ContextReport` showing:

- **Source names** — which context sources are active
- **Token counts** — tokens consumed per source
- **Budget utilization** — percentage of `effectiveLimit` used
- **Omission reasons** — which sources were trimmed and why

:::note Privacy guarantee
`ContextReport` never exposes memory body content or credential values — only source names, counts, and omission reasons.
:::

## Config Load Order

```
CLI flags
    ↓
Environment variables (ANTHROPIC_API_KEY, etc.)
    ↓
.chronos-code/config.yaml  (project)
    ↓
~/.chronos-code/config.yaml  (user global)
    ↓
internal/defaults/  (embedded)
```

Each layer is merged; higher layers win on conflict. The orchestrator validates the merged config before starting.

## Routing Integration

The orchestrator delegates intent classification to `internal/router`. The router returns a `RoutingDecision` containing:

- **Tier** — T0 (graph tools only), T1 (cheap model), or T2 (frontier model)
- **Path** — `low`, `medium`, or `high` complexity
- **Agent** — which agent to invoke (default: `chronos-code`; override: specialist or `ppd-planner`)

The orchestrator applies the `RoutingDecision` when constructing the Chronos turn.

## PPD Delegation

When `ppd.mode: enabled` in `routing.yaml`, the orchestrator delegates qualifying work to `ppd-planner`:

- **Qualifying criteria**: high-risk or high-complexity tasks, explicit PPD keywords, resume operations, or breadth exceeding file/package/call thresholds
- **Shadow mode** (`ppd.mode: shadow`): routing decisions are recorded but `ppd-planner` is not invoked
- **Disabled** (`ppd.mode: disabled`): PPD policy is skipped entirely

## See Also

- [Request Lifecycle Diagram](../diagrams/request-lifecycle)
- [Context Budget Diagram](../diagrams/context-budget)
- [Architecture Overview](./intro)
