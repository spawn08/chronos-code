---
sidebar_position: 2
title: Request Lifecycle
description: Sequence diagram showing a complete turn from CLI through agent execution
---

# Request Lifecycle

This sequence diagram traces a complete user request through all subsystems — from CLI input to the final response.

```mermaid
sequenceDiagram
    autonumber
    actor User
    participant CLI as CLI / TUI
    participant Orch as Orchestrator
    participant Router as Router
    participant Guard as Guardrail + Security
    participant Agent as Chronos Agent
    participant Model as Model (Frontier/Cheap)
    participant Graph as Code Graph
    participant MCP as MCP Server
    participant Session as Session Store
    participant Memory as Memory Store
    participant Learning as Learning

    User->>CLI: input message
    CLI->>Orch: dispatch turn(ctx, message)

    %% Context build
    Orch->>Session: load or resume session
    Session-->>Orch: session context
    Orch->>Memory: recall(ctx, query)
    Memory-->>Orch: memory entries

    %% Routing
    Orch->>Router: classify(message, context)
    Router-->>Orch: RoutingDecision{tier, path, agent}

    %% Agent loop
    Orch->>Agent: run turn(ctx, messages, tools)

    loop Tool calls (T0 → T1 → T2)
        Agent->>Model: prompt + tool definitions
        Model-->>Agent: tool_call or text

        alt T0: Code graph tool
            Agent->>Guard: validate tool call
            Guard-->>Agent: allow / deny
            Agent->>Graph: query(symbol/file)
            Graph-->>Agent: graph result
        else T1: File read / cheap model
            Agent->>Guard: validate tool call
            Guard-->>Agent: allow / deny
            Agent->>CLI: request approval (if required)
            CLI->>User: prompt for approval
            User-->>CLI: approved
            CLI-->>Agent: approved
            Agent->>Model: execute (T1 model)
            Model-->>Agent: result
        else T2: Shell / write / frontier model
            Agent->>Guard: validate tool call
            Guard-->>Agent: allow / deny
            Agent->>CLI: request approval
            CLI->>User: prompt for approval
            User-->>CLI: approved
            CLI-->>Agent: approved
            Agent->>MCP: tool call (if MCP tool)
            MCP-->>Agent: tool result
        end

        Agent->>Agent: check context budget (ContextGuard)
    end

    Agent-->>Orch: turn result

    %% Post-turn
    Orch->>Session: persist(session, turn)
    Orch->>Learning: emit suggestion (if pattern detected)
    Learning-->>Orch: suggestion written to .chronos-code/learned/
    Orch-->>CLI: streaming response
    CLI-->>User: display response
```

## Phase Summary

| Phase | Subsystems Involved | Description |
|-------|---------------------|-------------|
| **Context build** | Session, Memory | Load prior session; recall relevant memory entries |
| **Routing** | Router | Classify intent; select tier and implementation path |
| **Agent loop** | Agent, Model, Guardrail, Security | Iterative tool calls with budget tracking |
| **T0 tools** | Code Graph | Free graph queries — no model call, no approval |
| **T1 tools** | Model (cheap), Files | Ranged file reads, cheap model calls |
| **T2 tools** | Model (frontier), Shell, MCP | Writes, shell, MCP server calls — approval gated |
| **Post-turn** | Session, Learning | Persist session; emit learning suggestions |

## Tool Tier Costs

| Tier | Examples | Approval |
|------|----------|----------|
| T0 | `graph_query`, `codebase_search`, `find_callers` | Never |
| T1 | `file_read` (ranged), cheap model inference | Sometimes |
| T2 | `file_write`, `shell`, MCP tools | Always (unless `--yolo`) |

## See Also

- [Architecture Overview](./architecture-overview) — subsystem map
- [Context Budget](./context-budget) — how ContextGuard manages the token window
- [MCP Discovery](./mcp-discovery) — how MCP tools become available
