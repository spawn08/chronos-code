# Chronos Code

AI coding agent harness built on the Chronos framework.

## Build
```bash
make build        # produces bin/chronos-code
make test         # run tests with -race
make tidy         # go mod tidy
```

## Module
`github.com/spawn08/chronos-code` — imports `github.com/spawn08/chronos` as library dependency.

## Architecture
- `cmd/chronos-code/main.go` — entry point
- `internal/cli/` — command dispatch (manual, no Cobra)
- `internal/config/` — YAML config discovery and resolution
- `internal/defaults/` — embedded agent/skill/guardrail YAML (`go:embed`)
- `internal/orchestrator/` — agent lifecycle and routing
- `internal/tui/` — terminal interface (REPL, streaming display, permissions)
  - Mouse wheel capture is **on by default** (alt-screen transcript). Copy is
    shift+drag / Ctrl+Shift+C / `/copy`. Do not default mouse capture off.

## Conventions
- Follow Chronos conventions: no `init()`, wrap errors with `fmt.Errorf`, `context.Context` first param
- YAML-first: all config in YAML, not Go code
- Token-efficient: use code graph (T0) before file reads (T1) before shell (T2)
