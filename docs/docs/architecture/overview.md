---
slug: /architecture
sidebar_position: 1
title: Architecture Overview
description: High-level architecture of Chronos Code — subsystems, request path, and package layout
---

# Architecture Overview

Chronos Code is a thin orchestration harness on top of the
[Chronos](https://github.com/spawn08/chronos) agentic framework library. The framework handles
the agent loop, tool runtime, streaming, and storage adapters. Chronos Code adds:
YAML-driven configuration, a Go code graph, MCP integration, persistent memory, a
learning loop, and interactive surfaces (TUI and HTTP).

## High-Level Diagram

```mermaid
graph LR
    classDef surface  fill:#dbeafe,stroke:#1d4ed8,color:#1e3a8a
    classDef core     fill:#dcfce7,stroke:#16a34a,color:#14532d
    classDef context  fill:#fef9c3,stroke:#ca8a04,color:#713f12
    classDef safety   fill:#fee2e2,stroke:#dc2626,color:#7f1d1d
    classDef infra    fill:#f3e8ff,stroke:#9333ea,color:#3b0764

    CLI["CLI\ninternal/cli"]:::surface
    TUI["TUI\ninternal/tui"]:::surface
    HTTP["HTTP API\ninternal/server"]:::surface

    Config["Config\ninternal/config"]:::core
    Orch["Orchestrator\ninternal/orchestrator"]:::core
    Router["Router\ninternal/router"]:::core
    Agent["Chronos Agent\nloop + tools"]:::core

    Graph["Graph\ninternal/graph"]:::context
    Memory["Memory\ninternal/memory"]:::context
    Session["Session\ninternal/session"]:::context
    Plan["Plan\ninternal/plan"]:::context

    Security["Security\ninternal/security"]:::safety
    Guardrail["Guardrails\ninternal/guardrail"]:::safety
    Budget["Budget\ninternal/budget"]:::safety

    MCP["MCP\ninternal/mcpdiscover"]:::infra
    Learning["Learning\ninternal/learning"]:::infra

    CLI --> Config
    CLI --> TUI
    CLI --> HTTP
    TUI --> Orch
    HTTP --> Orch
    Config --> Orch
    Orch --> Router
    Orch --> Agent
    Orch --> Memory
    Orch --> Session
    Orch --> Plan
    Agent --> Graph
    Agent --> MCP
    Agent --> Security
    Security --> Guardrail
    Security --> Budget
    Orch --> Learning
```

## Request Path

```mermaid
sequenceDiagram
    actor User
    participant CLI as CLI / TUI
    participant Orch as Orchestrator
    participant Router
    participant Agent as Chronos Agent
    participant Tools as Tools (Graph/MCP/Shell)

    User->>CLI: message / command
    CLI->>Orch: Turn(ctx, msg)
    Orch->>Router: Classify(msg)
    Router-->>Orch: RoutingDecision{tier, path, agent}
    Orch->>Agent: Run(ctx, turn)
    loop Tool calling
        Agent->>Tools: call tool
        Tools-->>Agent: result
    end
    Agent-->>Orch: response
    Orch->>Orch: persist session + emit learning
    Orch-->>CLI: streamed reply
    CLI-->>User: display
```

## Subsystems

### Entry & Surfaces

| Package | Role |
|---------|------|
| `cmd/chronos-code` | Binary `main` — thin entry point |
| `internal/cli` | Command dispatch (no Cobra); handles all sub-commands |
| `internal/tui` | Bubble Tea REPL: streaming display, approvals, slash commands |
| `internal/server` | HTTP API (`/v1/chat`, sessions, memory, teams) |

### Core Runtime

| Package | Role |
|---------|------|
| `internal/orchestrator` | Agent lifecycle, routing application, turn execution |
| `internal/config` | YAML discovery and merge across CLI/env/project/user/embed layers |
| `internal/defaults` | Embedded agents, skills, guardrails, routing (`go:embed`) |
| `internal/router` | Intent patterns, model routing, complexity paths, PPD policy |

### Workspace & Code Intelligence

| Package | Role |
|---------|------|
| `internal/workspace` | Project root, ignore rules, file indexing |
| `internal/graph` | Go AST graph (default); tree-sitter behind `treesitter` build tag |
| `internal/projectdocs` | Watches project docs for context injection |
| `internal/lsp` | Optional `lsp` tag: diagnostics, hover, references, rename preview |

### Context & Memory

| Package | Role |
|---------|------|
| `internal/session` | Session persistence and resume (SQLite) |
| `internal/memory` | Local YAML memory (project / user / feedback) |
| `internal/plan` | Durable PPD plan store and scheduler (SQLite) |
| `internal/skills` | Skill discovery and selection |
| `internal/activation` | Context window activation decisions |
| `internal/attention` | Attention-based relevance scoring |
| `internal/incctx` | Incremental context management |
| `internal/toolcompress` | Tool result compression for context budget |

### Safety & Security

| Package | Role |
|---------|------|
| `internal/security` | Path/shell policy, permissions, hooks, sandbox |
| `internal/guardrail` | YAML guardrail engine (injection, PII, secrets) |
| `internal/verification` | Report/enforce verification policy |
| `internal/budget` | Token and USD caps |
| `internal/auth` | API keys, OAuth, keychain, SSO |

### Integrations

| Package | Role |
|---------|------|
| `internal/mcpdiscover` | `.mcp.json` load, test, redact, runtime management |
| `internal/learning` | Trace → suggestion YAML; apply only after human review |
| `internal/teambuilder` | Multi-agent team definitions |
| `internal/eval` | Offline token-efficiency and PPD eval harness |

## Chronos vs Chronos Code

Chronos itself (`github.com/spawn08/chronos`) is a **library**: agent SDK, harness, tool
runtime, streaming, and storage adapters. Chronos Code does not reimplement the agent loop.

```
github.com/spawn08/chronos        ← library (agent loop, tool runtime, streaming)
github.com/spawn08/chronos-code   ← harness (YAML config, graph, MCP, TUI, learning)
```

## Subsystem Deep-Dives

- [Orchestrator](./orchestrator.md) — ToolPhase, ContextGuard, ContextReport
- [MCP](./mcp.md) — ManagedServer, Discover/Load/Runtime, security validation
- [Memory](./memory.md) — Recall store, session summaries, learning loop
- [Planning](./planning.md) — SQLStore.Graph, PlanScope, PlanRef
- [CLI](./cli.md) — Command dispatch, manual routing
- [Security](./security.md) — Guardrail stack, path policy, MCP trust
