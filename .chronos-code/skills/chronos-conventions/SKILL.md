---
name: chronos-conventions
description: Go conventions for the chronos-code project — error handling, context, YAML-first config
version: 1.0.0
triggers: [go, convention, error, context, yaml, config, style, pattern]
model_hint: sonnet
tools_required: [file_read, file_write, file_grep, shell]
---
# Chronos Code Conventions

## Module Structure
- Module: `github.com/spawn08/chronos-code`
- Imports `github.com/spawn08/chronos` as library dependency (local replace in go.mod)
- Entry point: `cmd/chronos-code/main.go`
- All internal packages under `internal/`

## Hard Rules
1. **No `init()` functions** — explicit initialization only
2. **Wrap errors with context**: `fmt.Errorf("context: %w", err)` — never bare `return err`
3. **`context.Context` is always the first parameter** when present
4. **YAML-first**: all user-facing config in YAML, not Go code. Use `gopkg.in/yaml.v3`
5. **CGO_ENABLED=1** required for SQLite (`modernc.org/sqlite`)

## Error Handling Pattern
```go
result, err := doThing(ctx, arg)
if err != nil {
    return fmt.Errorf("do thing for %s: %w", arg, err)
}
```
Never `_ = fn()` on fallible calls unless explicitly justified with a comment.

## Config Resolution (highest → lowest)
1. CLI flags
2. Environment variables
3. `.chronos-code/config.yaml` (project)
4. `~/.chronos-code/config.yaml` (user global)
5. Embedded defaults (`internal/defaults/`)

## Embedded Defaults
- All default YAML lives in `internal/defaults/` with `go:embed`
- Every embedded file MUST have a catalog entry in `catalog.go` with an `Activation` and `Rationale`
- `RuntimeActive` = loaded at startup; `ExportOnly` = only for `chronos-code init`

## Token Efficiency (T0 → T1 → T2)
Use the cheapest tool tier first:
- **T0**: Code graph queries (codebase_search, codebase_symbol, codebase_callers)
- **T1**: File reads (targeted, with line ranges)
- **T2**: Shell commands (go test, go build)

## Build & Test
```bash
make build        # produces bin/chronos-code
make test         # go test ./... -race -count=1
make lint         # golangci-lint run ./...
make fmt          # gofmt -s -w .
make eval         # token efficiency eval suite
```

## Package Boundaries
- `internal/orchestrator/` owns agent lifecycle — other packages don't import it
- `internal/skills/` is self-contained — depends only on `chronos/engine/model` for token counting
- `internal/config/` provides `Config` struct — read by orchestrator, CLI, and TUI
- `internal/defaults/` provides embedded FS — read by orchestrator and CLI init
