---
sidebar_position: 1
title: Orchestrator
description: Agent lifecycle, ToolPhase, ContextGuard budget math, and ContextReport
---

# Orchestrator

The orchestrator (`internal/orchestrator`) is the central coordinator of Chronos Code. It is
constructed once per process and owns agent lifecycle, routing, context management, and
turn execution.

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

Guardrails intercept at the `PENDING → EXECUTING` boundary. A guardrail deny produces `FAILED`
without executing the tool. The orchestrator collects phase results for the `ContextReport`.

See the [Orchestrator Phases Diagram](../diagrams/orchestrator-phases) for a visual state machine.

## ContextGuard Budget Math {#contextguard-budget-math}

The orchestrator enforces a context budget through a `contextGuardHook` that fires **before
every model call** — including follow-up rounds in the tool-calling loop.

Source: `internal/orchestrator/context_guard.go`

The effective limit computation reserves a margin from the model's context window to leave room
for output, then subtracts an overhead estimate per registered tool. When total context exceeds
this effective limit, the guard runs `trimMessages`, which:

1. **Protects** the system/pinned prefix (never trimmed)
2. **Drops** oldest conversation messages, sweeping orphaned tool-result messages with each dropped assistant turn
3. **Restores** the most recent user turn if all turns were dropped
4. **Rejects** with an error if messages still exceed budget after trimming — the user must `/clear`

| Parameter | Default | Description |
|-----------|---------|-------------|
| `defaultToolReserveTokens` | 150 tokens/tool | Per-tool overhead estimate |
| `contextGuardMargin` | 15% | Fraction kept free for model output |
| `maxToolResultBytes` | 100 KB | Hard cap per tool result before truncation |

## ContextReport

The `/context` TUI command and `context_report: true` config expose a `ContextReport` showing:

- **Source names** — which context sources are active
- **Token counts** — tokens consumed per source
- **Budget utilization** — percentage of effective limit used
- **Omission reasons** — which sources were trimmed and why

:::note Privacy guarantee
`ContextReport` never exposes memory body content or credential values — only source names,
counts, and omission reasons.
:::

Source: `internal/orchestrator/context_report.go`

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

Each layer is merged; higher layers win on conflict. The orchestrator validates the merged config
before starting.

## Routing Integration

The orchestrator delegates intent classification to `internal/router`. The router returns a
`RoutingDecision` containing:

- **Tier** — T0 (graph tools only), T1 (cheap model), or T2 (frontier model)
- **Path** — `low`, `medium`, or `high` complexity
- **Agent** — which agent to invoke (default: `chronos-code`; override: specialist or `ppd-planner`)

## PPD Delegation

When `ppd.mode: enabled` in `routing.yaml`, the orchestrator delegates qualifying work to
`ppd-planner`:

- **Qualifying criteria**: high-risk or high-complexity tasks, explicit PPD keywords, resume
  operations, or breadth exceeding file/package/call thresholds
- **Shadow mode** (`ppd.mode: shadow`): routing decisions are recorded but `ppd-planner` is not
  invoked
- **Disabled** (`ppd.mode: disabled`): PPD policy is skipped entirely

## See Also

- [Request Lifecycle Diagram](../diagrams/request-lifecycle)
- [Context Budget Diagram](../diagrams/context-budget)
- [Orchestrator Phases Diagram](../diagrams/orchestrator-phases)
- [Architecture Overview](../diagrams/architecture-overview)
