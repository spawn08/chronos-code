# Chronos Code

YAML-native AI coding agent harness built on the [Chronos](https://github.com/spawn08/chronos) agentic framework.

Chronos Code provides a configuration-driven experience for defining, composing, and running AI coding agents — with MCP integration, tool calling, subagent delegation, persistent memory, guardrails, and a self-learning loop that distills past executions into reusable YAML definitions.

## Features

- **YAML-first configuration** — agents, skills, guardrails, security policies, and MCP servers defined entirely in YAML
- **Code graph indexing** — tree-sitter AST-based code graph for token-efficient context loading
- **Tiered model routing** — cheap models for search/explanation, frontier models for reasoning/coding
- **Self-learning loop** — analyzes execution traces and generates improved agent/skill YAML (not fine-tuning)
- **Full MCP support** — connect to external tool servers via stdio or SSE
- **Built-in agents** — coder, planner, reviewer, debugger, researcher, architect, explainer
- **Guardrail presets** — injection detection, secret scanning, PII filtering, cost caps
- **Dual-mode deployment** — interactive CLI (SQLite) or multi-tenant HTTP server (PostgreSQL)
- **Session management** — resumable, branchable, inspectable conversation sessions
- **Persistent memory** — project-scoped memory stored as inspectable YAML files with optional vector recall
- **Embedded defaults** — zero-config first run; `chronos-code init` exports editable YAML

## Quick Start

### Prerequisites

- Go 1.26+
- CGO enabled (required for SQLite)
- [Chronos](https://github.com/spawn08/chronos) cloned as a sibling directory (or adjust `go.mod` replace directive)

### Build

```bash
make build        # produces bin/chronos-code
```

### Initialize a project

```bash
chronos-code init
```

Creates a `.chronos-code/` directory with default agent definitions, skills, guardrails, and security policies — all editable YAML.

### Run

```bash
chronos-code
```

Without `init`, embedded defaults are used automatically.

## Architecture

```
┌───────────────────────────────────────────────┐
│              chronos-code CLI / Server         │
│  ┌─────────┐ ┌────────┐ ┌──────────────────┐  │
│  │   TUI   │ │ Server │ │  Self-Learning   │  │
│  │  (REPL) │ │ (HTTP) │ │  Loop            │  │
│  └────┬────┘ └───┬────┘ └────────┬─────────┘  │
│       └──────────┼───────────────┘             │
│          ┌───────▼────────┐                    │
│          │  Orchestrator  │                    │
│          └───────┬────────┘                    │
│          ┌───────▼────────┐                    │
│          │ Config Resolver│                    │
│          └───────┬────────┘                    │
├──────────────────▼────────────────────────────┤
│           Chronos Framework (library)          │
│  Agent SDK · Harness · Memory · Engine · Store │
└───────────────────────────────────────────────┘
```

### Package Layout

| Package | Purpose |
|---------|---------|
| `cmd/chronos-code` | Binary entry point |
| `internal/cli` | Command dispatch |
| `internal/config` | YAML config discovery and resolution |
| `internal/defaults` | Embedded agent/skill/guardrail YAML (`go:embed`) |
| `internal/orchestrator` | Agent lifecycle and message routing |
| `internal/tui` | Terminal interface (REPL, streaming, permissions) |
| `internal/server` | HTTP server for team/enterprise deployment |
| `internal/graph` | Tree-sitter code graph indexing |
| `internal/memory` | Local-first memory with optional vector recall |
| `internal/learning` | Self-learning loop (trace → distill → YAML) |
| `internal/session` | Session persistence and resumption |
| `internal/workspace` | Project detection, file indexing, .gitignore |
| `internal/auth` | Provider authentication (API key, OAuth, keychain) |
| `internal/security` | Security policy enforcement |
| `internal/guardrail` | YAML-configured guardrail engine |
| `internal/eval` | Token efficiency evaluation harness |
| `internal/budget` | Token budget tracking and enforcement |
| `internal/activation` | Activation buffer for context management |
| `internal/attention` | Attention budgeting |
| `internal/toolcompress` | Tool result compression |
| `internal/incctx` | Incremental context loading |

## Configuration

All configuration lives in `.chronos-code/` at the project root:

```
.chronos-code/
├── config.yaml          # Main config (model, storage, memory, learning)
├── agents/              # Agent definitions
│   ├── coder.yaml
│   ├── reviewer.yaml
│   ├── planner.yaml
│   └── ...
├── skills/              # Skill manifests
├── guardrails/          # Guardrail presets (default, strict, permissive)
├── security.yaml        # Path allowlists, shell restrictions, MCP trust
├── memory/              # Persistent memory (YAML, human-readable)
└── learned/             # Self-learning loop output
```

Config precedence (highest to lowest):
1. CLI flags
2. Environment variables (`CHRONOS_CODE_*`)
3. `.chronos-code/config.yaml` (project)
4. `~/.chronos-code/config.yaml` (user global)
5. Embedded defaults

## Default Agents

| Agent | Role | Model Tier |
|-------|------|------------|
| `coder` | Primary coding agent — read, write, test, iterate | Frontier |
| `planner` | Task decomposition and execution planning | Frontier |
| `reviewer` | Code review for bugs, security, and style | Frontier |
| `debugger` | Diagnose failures from error output and traces | Frontier |
| `researcher` | Read-only codebase exploration | Cheap |
| `architect` | High-level design and structural guidance | Frontier |
| `explainer` | Code and concept explanation | Cheap |

Switch agents explicitly with `@agent_id`:
```
@reviewer check my last commit
@debugger why is TestAuth failing
```

## Development

```bash
make build        # build binary
make test         # run tests with -race
make eval         # run token efficiency eval suite
make fmt          # format code
make vet          # static analysis
make tidy         # go mod tidy
make clean        # remove build artifacts
make install      # install to $GOPATH/bin
```

## License

Same license as [Chronos](https://github.com/spawn08/chronos).
