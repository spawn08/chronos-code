---
name: chronos-sdk-usage
description: How to use the chronos SDK library — agent, harness, engine, model, tool APIs
version: 1.0.0
triggers: [chronos, sdk, agent, harness, engine, model, provider, tool, registry]
model_hint: sonnet
tools_required: [file_read, file_grep, shell]
---
# Chronos SDK Usage

## Import Path
```go
import "github.com/spawn08/chronos"
```
Local development uses `replace github.com/spawn08/chronos => ../chronos` in go.mod.

## Key Packages Used by chronos-code

### agent
- `agent.Agent` — the core agent struct with ID, Model, Tools, Guardrails, Hooks, ContextPinsFn
- `agent.ModelConfig` — model configuration (provider, model name, temperature)
- `a.Hooks` — lifecycle hooks (pre/post tool call, etc.)
- `a.ContextPinsFn` — function returning additional context messages per turn

### engine/model
- `model.Provider` — interface for LLM providers (Chat, StreamChat, Name, Model)
- `model.ChatRequest` / `model.ChatResponse` — request/response types
- `model.Message` — with Role (RoleSystem, RoleUser, RoleAssistant) and Content
- `model.NewTokenCounter(modelID)` — token counting for budget enforcement

### tool
- `tool.NewRegistry()` — create a tool registry
- Tool definitions registered in Go code, not YAML (tools.yaml is export-only documentation)

### guardrails
- `guardrails.NewEngine()` — create guardrail engine
- Configured via YAML presets (default, strict, permissive)

### team
- `team.Team` — multi-agent team definition
- Teams configured in YAML, require explicit setup before use

## Orchestrator Wiring
The orchestrator (`internal/orchestrator/`) is the integration point:
- Builds agents from config + defaults
- Attaches providers, tools, guardrails, hooks
- Chains ContextPinsFn for skills, memory, project docs
- Routes messages via the classifier/router

## Adding a New Context Source
1. Define a new `ContextSourceKind` constant in `context_report.go`
2. Add it to the `contextSourceRegistry` slice with a budget
3. Chain into agent's `ContextPinsFn` in a new `setup*` function
4. Call `contextSourceSelected` / `contextSourceOmitted` for `/context` reporting
