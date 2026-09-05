---
sidebar_position: 1
title: Architecture Overview
description: High-level diagram of Chronos Code subsystems and their interconnections
---

# Architecture Overview Diagram

This diagram shows the major subsystems of Chronos Code and how they interconnect.

```mermaid
graph LR
    classDef surface fill:#dbeafe,stroke:#1d4ed8,color:#1e3a8a
    classDef core fill:#dcfce7,stroke:#16a34a,color:#14532d
    classDef context fill:#fef9c3,stroke:#ca8a04,color:#713f12
    classDef safety fill:#fee2e2,stroke:#dc2626,color:#7f1d1d
    classDef integration fill:#f3e8ff,stroke:#7c3aed,color:#3b0764
    classDef external fill:#f1f5f9,stroke:#64748b,color:#334155

    subgraph Surfaces["Surfaces"]
        TUI["TUI\n(Bubble Tea REPL)"]:::surface
        HTTP["HTTP Server\n(/v1/chat)"]:::surface
        CLI["CLI\n(cmd dispatch)"]:::surface
    end

    subgraph Core["Core Runtime"]
        Orchestrator["Orchestrator\n(agent lifecycle)"]:::core
        Router["Router\n(T0/T1/T2 routing)"]:::core
        Config["Config\n(YAML merge)"]:::core
        Defaults["Defaults\n(go:embed)"]:::core
    end

    subgraph Workspace["Workspace & Intelligence"]
        Graph["Code Graph\n(Go AST / tree-sitter)"]:::core
        LSP["LSP\n(optional)"]:::core
        ProjectDocs["ProjectDocs\n(doc watcher)"]:::core
    end

    subgraph ContextLayer["Context & Memory"]
        Session["Session\n(SQLite)"]:::context
        Memory["Memory\n(YAML)"]:::context
        Plan["Plan\n(SQLite DAG)"]:::context
        Skills["Skills"]:::context
        ContextGuard["ContextGuard\n(budget + trim)"]:::context
    end

    subgraph Safety["Safety & Security"]
        Guardrail["Guardrail Engine\n(YAML rules)"]:::safety
        Security["Security Policy\n(path/shell)"]:::safety
        Budget["Budget Cap\n(tokens / USD)"]:::safety
        Auth["Auth\n(keys/OAuth)"]:::safety
        Verification["Verification\n(report/enforce)"]:::safety
    end

    subgraph Integrations["Integrations"]
        MCP["MCP\n(stdio / SSE)"]:::integration
        Learning["Learning\n(suggestion YAML)"]:::integration
        TeamBuilder["TeamBuilder"]:::integration
        Eval["Eval Harness"]:::integration
    end

    Chronos["Chronos Library\n(agent loop + tools)"]:::external

    CLI --> Orchestrator
    TUI --> Orchestrator
    HTTP --> Orchestrator

    Orchestrator --> Router
    Orchestrator --> Config
    Orchestrator --> Chronos
    Orchestrator --> Session
    Orchestrator --> Memory
    Orchestrator --> ContextGuard

    Config --> Defaults

    Chronos --> Graph
    Chronos --> MCP
    Chronos --> Guardrail
    Chronos --> Security

    ContextGuard --> Session
    ContextGuard --> Memory
    ContextGuard --> Skills

    Orchestrator --> Learning
    Orchestrator --> Plan
    Orchestrator --> TeamBuilder

    Router --> Plan
```

## Key Relationships

| Relationship | Description |
|-------------|-------------|
| **CLI/TUI/HTTP → Orchestrator** | All surfaces funnel through a single Orchestrator instance |
| **Orchestrator → Chronos** | Orchestrator drives the Chronos agent loop; Chronos is a library, not a service |
| **Chronos → MCP** | Chronos tool runtime manages MCP server connections |
| **Chronos → Guardrail/Security** | All tool calls pass through the guardrail and security layers |
| **ContextGuard** | Mediates between Session, Memory, and Skills to enforce the token budget |
| **Orchestrator → Learning** | After each turn, emits pending suggestions for human review |

## See Also

- [Request Lifecycle](./request-lifecycle) — turn-by-turn sequence
- [MCP Discovery](./mcp-discovery) — MCP startup flow
- [Context Budget](./context-budget) — ContextGuard state machine
- [Data Flow](./data-flow) — Plan and Memory feedback loops
