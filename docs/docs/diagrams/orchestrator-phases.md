---
sidebar_position: 5
title: Orchestrator Phases
description: State machine diagram of the tool-calling loop and ToolPhase lifecycle
---

# Orchestrator Phases

This diagram shows the tool-calling loop state machine inside the Chronos orchestrator,
including `ToolPhase` states and the interaction with guardrails, the context guard, and
the model.

## ToolPhase State Machine

```mermaid
stateDiagram-v2
    direction LR

    classDef phase font-weight:bold

    [*] --> Idle : session starts

    Idle --> TurnStart : user message received

    state TurnStart {
        [*] --> BuildContext
        BuildContext --> InjectMemory : memory enabled
        BuildContext --> SkipMemory : memory disabled
        InjectMemory --> CheckBudget
        SkipMemory --> CheckBudget
        CheckBudget --> [*] : budget OK
        CheckBudget --> TrimContext : near limit
        TrimContext --> [*] : trimmed
    }

    TurnStart --> ModelCall : context ready

    state ModelCall {
        [*] --> AwaitingResponse
        AwaitingResponse --> ResponseReceived : model responds
        AwaitingResponse --> ModelError : timeout / error
        ModelError --> [*] : error propagated
    }

    ModelCall --> CheckResponse

    state CheckResponse {
        [*] --> ParseResponse
        ParseResponse --> TextOnly : no tool calls
        ParseResponse --> HasToolCalls : tool calls present
    }

    CheckResponse --> StreamText : text only
    CheckResponse --> ToolLoop : has tool calls

    state ToolLoop {
        direction TB
        [*] --> PENDING : tool call received

        PENDING --> GuardrailCheck : intercept

        state GuardrailCheck {
            [*] --> EvalRules
            EvalRules --> GuardrailAllow : all rules pass
            EvalRules --> GuardrailDeny : rule matched
        }

        GuardrailCheck --> EXECUTING : guardrail allow
        GuardrailCheck --> FAILED : guardrail deny

        EXECUTING --> CapResult : result received
        EXECUTING --> FAILED : tool error

        state CapResult {
            [*] --> SizeCheck
            SizeCheck --> Pass : ≤ 100 KB
            SizeCheck --> Truncate : > 100 KB
        }

        CapResult --> COMPLETED : result capped/ok
        FAILED --> [*] : tool call settles
        COMPLETED --> [*] : tool call settles
    }

    ToolLoop --> AggrResults : all tools settled
    AggrResults --> ContextGuardCheck

    state ContextGuardCheck {
        [*] --> CountTokens
        CountTokens --> UnderLimit : total ≤ effectiveLimit
        CountTokens --> TrimMessages : total > effectiveLimit
        TrimMessages --> UnderLimit : trimmed successfully
        TrimMessages --> RejectTurn : still over limit after trim
    }

    ContextGuardCheck --> ModelCall : under limit (next round)
    ContextGuardCheck --> ErrorOut : reject turn — /clear required

    StreamText --> EmitLearning
    EmitLearning --> PersistSession
    PersistSession --> Idle : turn complete

    ErrorOut --> Idle : error surfaced to user
```

## Phase Descriptions

| Phase | Description |
|-------|-------------|
| **PENDING** | Tool call received from model; awaiting guardrail check |
| **EXECUTING** | Guardrail approved; tool handler is running |
| **COMPLETED** | Tool returned a result; result is capped to ≤ 100 KB |
| **FAILED** | Tool error or guardrail deny; error is included in tool result message |

## Key Design Decisions

### Guardrails Intercept at PENDING → EXECUTING

The guardrail engine intercepts **before** the tool handler runs. A guardrail deny sets state
to `FAILED` without any side effects from the tool. This prevents injection, secret leakage,
and destructive operations from being attempted.

### Tool Result Capping

Every tool result is capped at `maxToolResultBytes` (100 KB) by `wrapToolResultCap`. This is a
**last-resort safety net** after `toolcompress` has already compressed eligible results. It
prevents token-explosion from large tool outputs (e.g., MCP tools, context injections) that
bypass compression.

Source: `internal/orchestrator/context_guard.go` — `wrapToolResultCap`

### ContextGuard Fires After Every Tool Round

Unlike the SDK's `enforceContextBudget` (which runs once at session start), the
`contextGuardHook` fires **before every model call** via the `hooks.EventModelCallBefore` hook.
This catches token accumulation in follow-up rounds of the tool-calling loop.

Source: `internal/orchestrator/context_guard.go` — `contextGuardHook.Before`

### EmitLearning Before Session Persist

Learning suggestions are emitted **after** the turn completes but **before** the session is
persisted. This ensures that if session persistence fails, the learning suggestion is not lost
(it is written to disk independently).

## See Also

- [Context Budget State Machine](./context-budget) — ContextGuard detail
- [Orchestrator Subsystem](../subsystems/orchestrator) — full orchestrator reference
- [Request Lifecycle](./request-lifecycle) — end-to-end sequence diagram
