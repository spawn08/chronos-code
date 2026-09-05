---
sidebar_position: 5
title: Data Flow
description: Plan.Graph, Memory.Recall, and Learning feedback loops
---

# Data Flow

This diagram shows how data flows between the Plan store, Memory store, and the Learning feedback loop — the three stateful subsystems that persist across sessions.

```mermaid
graph LR
    classDef store fill:#dbeafe,stroke:#1d4ed8,color:#1e3a8a
    classDef process fill:#dcfce7,stroke:#16a34a,color:#14532d
    classDef decision fill:#fef9c3,stroke:#ca8a04,color:#713f12
    classDef feedback fill:#f3e8ff,stroke:#7c3aed,color:#3b0764
    classDef user fill:#f1f5f9,stroke:#64748b,color:#334155

    User(["User / Turn"]):::user
    Orch["Orchestrator"]:::process

    subgraph PlanSubsystem["Plan Subsystem (internal/plan)"]
        PlanStore[("SQLite\nPlan Store")]:::store
        PlanGraph["PlanGraph\n(DAG of nodes)"]:::process
        PlanNode["PlanNode\n(status tracking)"]:::process
    end

    subgraph MemorySubsystem["Memory Subsystem (internal/memory)"]
        ProjectMem[("project.yaml")]:::store
        UserMem[("user.yaml")]:::store
        FeedbackMem[("feedback.yaml")]:::store
        Recall["Store.Recall\n(text search)"]:::process
    end

    subgraph LearningSubsystem["Learning Subsystem (internal/learning)"]
        Tracer["Session Tracer\n(pattern detection)"]:::feedback
        SuggestionFiles[(".chronos-code/learned/\n*.yaml")]:::store
        HumanReview{"Human Review\n(learn accept/reject)"}:::decision
        PatternInjector["Pattern Injector\n(approved patterns → context)"]:::feedback
    end

    subgraph SessionSubsystem["Session (internal/session)"]
        SessionDB[("SQLite\nSession DB")]:::store
        Summary["Session Summary\n(on compact/end)"]:::process
    end

    %% Plan flow
    User -->|PPD-qualifying task| Orch
    Orch -->|create plan| PlanStore
    PlanStore --> PlanGraph
    PlanGraph --> PlanNode
    PlanNode -->|update status| PlanStore
    PlanStore -->|graph on resume| Orch

    %% Memory read flow
    Orch -->|recall at turn start| Recall
    Recall --> ProjectMem
    Recall --> UserMem
    Recall --> FeedbackMem
    Recall -->|entries injected| Orch

    %% Memory write flow
    User -->|"remember <cat>: <fact>"| Orch
    Orch -->|write intent| ProjectMem
    Orch -->|write intent| UserMem
    User -->|"forget: <mem_ID>"| Orch
    Orch -->|delete entry| ProjectMem

    %% Session → memory feedback
    Summary -->|summarized facts| ProjectMem

    %% Learning flow
    Orch -->|trace turn| Tracer
    Tracer -->|detect pattern| SuggestionFiles
    SuggestionFiles --> HumanReview
    HumanReview -->|accept| PatternInjector
    HumanReview -->|reject| SuggestionFiles
    PatternInjector -->|inject at turn start| Orch

    %% Session persistence
    Orch -->|persist turn| SessionDB
    SessionDB --> Summary
    Summary -->|recall_prior_summaries| Orch

    %% Feedback loop
    HumanReview -->|accepted pattern| FeedbackMem
```

## Data Stores

| Store | Format | Location | Description |
|-------|--------|----------|-------------|
| Plan DB | SQLite | `--db <path>` | PPD plan DAGs and node status |
| Session DB | SQLite | `.chronos-code/sessions.db` | Session history and summaries |
| Project memory | YAML | `.chronos-code/memory/project.yaml` | Project-scoped facts |
| User memory | YAML | `~/.chronos-code/memory/user.yaml` | User preferences |
| Feedback memory | YAML | `.chronos-code/memory/feedback.yaml` | Accepted learning patterns |
| Suggestion files | YAML | `.chronos-code/learned/*.yaml` | Pending suggestions (pre-review) |

## Feedback Loops

### Memory → Context Loop

On every turn start:
1. `Store.Recall(ctx, query)` searches project + user + feedback YAML
2. Matching entries are injected into the context (budget-permitting)
3. High-relevance entries survive context trimming longer

### Learning Loop

1. Session tracer detects patterns during tool calls and model interactions
2. Patterns are written as YAML suggestion files
3. Human runs `chronos-code learn accept <id>` to activate
4. Accepted patterns are injected via `PatternInjector` on future turns
5. Accepted patterns are also written to `feedback.yaml` for recall

### Plan Resumption Loop

1. PPD-qualifying task creates a `PlanGraph` in SQLite
2. `ppd-planner` updates `PlanNode` status as it works
3. On `/resume` or `--resume <session-id>`, the orchestrator loads the plan from SQLite
4. Remaining nodes continue from where execution stopped

## See Also

- [Memory Architecture](../architecture/memory)
- [Plan Architecture](../architecture/plan)
- [Context Budget](./context-budget) — how memory entries compete for budget
