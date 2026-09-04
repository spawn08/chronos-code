---
name: token-efficiency
description: Token efficiency patterns for chronos-code — routing tiers, budget tracking, context compression
version: 1.0.0
triggers: [token, efficiency, budget, cost, tier, routing, compress, context, activation]
model_hint: sonnet
tools_required: [file_read, file_grep, shell]
---
# Token Efficiency

## Tier System (T0 → T1 → T2)
Always use the cheapest tier that can answer the question:
- **T0** (graph queries): codebase_search, codebase_symbol, codebase_callers — near-zero tokens
- **T1** (file reads): read specific files with line ranges — moderate tokens
- **T2** (shell): go test, go build, grep — expensive, full output captured

## Key Packages

### internal/budget
- `budget.Tracker` — tracks token usage per session
- USD budget enforcement: unknown model + positive cap = fail closed before invocation

### internal/activation
- `activation.Buffer` — manages which context gets activated per turn
- Prioritizes most relevant context within token limits

### internal/attention
- `attention.Budgeter` — allocates attention budget across context sources

### internal/toolcompress
- `compress.go` — compresses tool outputs to fit token budgets
- Applied to large shell outputs, file contents

### internal/incctx
- Incremental context loading — adds context progressively rather than all at once
- Deduplication: `dedup.go` prevents duplicate context injection

## Eval Suite
- `make eval` runs deterministic offline fixture-replay
- Compares against `benchmark/eval/baseline.json`
- Fails on contract violation or >10% regression in optimized tokens
- Report at `benchmark/eval/report.md` (synthetic, not real model benchmark)

## Router / Classifier
- `internal/router/classifier.go` — determines task complexity
- `internal/router/router.go` — routes to appropriate model tier
- `internal/router/t1.go` — T1 tier routing logic
- `internal/router/ppd.go` — PPD (Plan-Prompt-Delegate) routing (shadow mode by default)

## Context Budget Allocation
Each context source has a byte budget in `context_report.go`:
- Skills: 32000 bytes
- Memory, project docs, prior sessions: each has its own budget
- `/context` TUI command shows actual usage and omission reasons

## Best Practices
- Measure before optimizing: run `make eval` as baseline
- New tools should report token counts via the budget tracker
- Context sources should respect their budget and report via contextSourceSelected/Omitted
- Tool outputs over threshold should be compressed before injection
