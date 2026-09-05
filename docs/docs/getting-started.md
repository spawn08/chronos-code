---
sidebar_position: 2
title: Getting Started
description: Build, configure, and run Chronos Code for the first time
---

# Getting Started

## Prerequisites

- **Go 1.26+**
- **CGO enabled** — required for SQLite (`modernc.org/sqlite`)
- [Chronos](https://github.com/spawn08/chronos) as a sibling checkout, or update the `go.mod` `replace` directive to point to your local copy

## Build

Clone the repository and build:

```bash
git clone https://github.com/spawn08/chronos-code
cd chronos-code
make build        # produces bin/chronos-code
```

Available make targets:

```bash
make build        # bin/chronos-code
make test         # go test -race
make eval         # token-efficiency eval vs baseline.json
make fmt          # gofmt -s -w .
make vet          # go vet ./...
make tidy         # go mod tidy
make clean        # remove bin/
make install      # $GOPATH/bin
```

### Optional build tags

| Tag | What it adds |
|-----|-------------|
| `treesitter` | Tree-sitter code graph (in addition to Go AST graph) |
| `postgres` | PostgreSQL storage adapter |
| `lsp` | LSP tools: diagnostics, hover, references, rename preview |

Example with LSP:

```bash
go build -tags lsp ./...
```

## Initialize a Project

```bash
chronos-code init
```

This writes `.chronos-code/` into the current directory with agent YAML, skills, guardrails, routing, and security policy. Skip this step to run on embedded defaults — first run works without any config files.

```text
.chronos-code/
├── config.yaml          # model, storage, memory, learning, verification
├── routing.yaml         # intent, models, complexity paths, PPD
├── security.yaml        # path allowlists, shell restrictions, MCP trust
├── agents/              # chronos-code.yaml, coder.yaml, …
├── skills/
├── guardrails/
├── memory/              # project.yaml, user.yaml, feedback.yaml
└── learned/             # pending learning suggestions
```

## Run

```bash
chronos-code                      # interactive TUI (REPL)
chronos-code run "your message"   # one-shot, then exit
chronos-code serve                # HTTP API on :8430
```

### Useful CLI flags

| Flag | Description |
|------|-------------|
| `-c` / `--config` | Path to a specific config file |
| `--debug` | Enable debug logging |
| `--stream` / `--no-stream` | Control streaming output |
| `--permission-mode` | Permission enforcement level |
| `--yolo` | Auto-approve policy-allowed tools (never overrides deny rules) |
| `--budget <usd>` | Hard USD spending cap |
| `--resume <session-id>` | Resume a prior session |
| `--json` | Machine-readable output (headless mode) |

## Interactive TUI

Once in the REPL, these slash commands and shortcuts are available:

| Command | Effect |
|---------|--------|
| `/login` or `Ctrl+L` | Authenticate with a provider |
| `/whoami` | Show effective credential source |
| `/context` | Show context sources, counts, and budgets |
| `/resume` | Continue the latest session |
| `/compact` | Summarize and compress history |
| `/rewind` | Undo the last `file_write` |
| `/plan on` / `/plan off` | Block writes and shell until plan mode exits |
| `/learn` | Review pending learning suggestions |
| `/model` | List models; Tab to autocomplete |
| `/mouse` | Toggle mouse capture mode |
| `/copy` | Copy last assistant reply to clipboard |
| `/think` | Enable native thinking mode for this turn |
| `!<command>` | Run a local shell command in the workspace; output stays in the chat |

**Clipboard shortcuts:** `Ctrl+Y` / `Ctrl+Shift+C` copy the last reply. `/copy code` and `Ctrl+Shift+X` copy the last fenced code block.

**Mouse:** Wheel scroll is on by default. Shift-drag to select text; then copy with your terminal shortcut (`Cmd+C` on macOS).

## Commands Reference

```text
chronos-code                          Start interactive REPL
chronos-code run <message>            One task, then exit
chronos-code init                     Export .chronos-code/ into the project
chronos-code login / logout / whoami  Provider credentials
chronos-code providers                List resolvable providers
chronos-code agents list              List resolved agents
chronos-code config show|validate     Show or validate resolved config
chronos-code session list|delete|export
chronos-code memory list|search|forget
chronos-code mcp add|list|test|remove
chronos-code learn suggest|list|show|accept|reject
chronos-code eval run|ppd
chronos-code team list|run
chronos-code plan … --db <path>       Durable plan database ops
chronos-code skills list|show
chronos-code serve                    HTTP server
chronos-code version
```

## Language Server Tools (Optional)

Build with `-tags lsp` to register `lsp_diagnostics`, `lsp_hover`, `lsp_references`, and `lsp_rename_preview`. Supported servers:

- `gopls` (Go)
- `typescript-language-server --stdio`
- `pyright-langserver --stdio`
- `rust-analyzer`

Servers start lazily on first use. A missing server is non-fatal.

## Next Steps

- [Configuration](./configuration) — full YAML config reference
- [Architecture Overview](./architecture/intro) — how subsystems connect
