---
sidebar_position: 3
title: Configuration
description: Complete YAML configuration reference for Chronos Code
---

# Configuration

Chronos Code is YAML-first: all user-facing configuration lives in YAML files, not Go code. An embedded default set ships inside the binary so the first run works without any files.

## Config Discovery & Precedence

Settings are merged from highest to lowest priority:

1. **CLI flags** — `--budget`, `--debug`, `--yolo`, etc.
2. **Environment variables** — provider and server variables (e.g., `ANTHROPIC_API_KEY`)
3. **`.chronos-code/config.yaml`** — project-level config (in the repo root)
4. **`~/.chronos-code/config.yaml`** — user-global config
5. **Embedded defaults** — shipped inside the binary (`internal/defaults/`)

Run `chronos-code config show` to see the fully resolved config. Run `chronos-code config validate` to check for errors.

## Directory Layout

```text
.chronos-code/
├── config.yaml          # model, storage, memory, learning, verification
├── routing.yaml         # intent patterns, model tiers, complexity paths, PPD
├── security.yaml        # path allowlists, shell restrictions, MCP trust
├── agents/
│   ├── chronos-code.yaml
│   ├── coder.yaml
│   └── …
├── skills/
├── guardrails/
├── memory/
│   ├── project.yaml     # project-scoped facts
│   ├── user.yaml        # user-scoped facts
│   └── feedback.yaml    # feedback memory
└── learned/             # pending learning suggestions (human review required)
```

MCP servers live in **`.mcp.json`** in the project root — not under `.chronos-code/`.

## Core Config Keys (`config.yaml`)

### Model and Storage

```yaml
defaults:
  model: claude-3-5-sonnet-20241022   # default model ID
  provider: anthropic                  # anthropic | openai | gemini | …

storage:
  driver: sqlite                       # sqlite | postgres
  path: .chronos-code/sessions.db     # SQLite path (sqlite only)
  # dsn: postgres://...               # PostgreSQL DSN (postgres build tag)
```

### Memory

```yaml
memory:
  enabled: true              # false stops persist and recall entirely
  project_file: .chronos-code/memory/project.yaml
  user_file: ~/.chronos-code/memory/user.yaml
  feedback_file: .chronos-code/memory/feedback.yaml
```

### Learning

```yaml
learning:
  enabled: true              # emit pending suggestions
  auto_distill: false        # NEVER auto-apply; human review required
  pattern_injection: true    # inject approved patterns into context
  suggestions_dir: .chronos-code/learned/
```

### Session

```yaml
session:
  recall_prior_summaries: true    # include prior session summaries in context
  context_report: true            # expose /context source breakdown
```

### Verification

```yaml
verification:
  mode: report   # report — log issues | enforce — refuse completion on unmet obligations
```

`enforce` refuses a successful completion when the runtime has verification obligations without current evidence. It does not invent checks.

### Native Thinking

Off by default. Enable in YAML or with `/think` in the TUI:

```yaml
defaults:
  reasoning:
    strategy: cot
    native: true           # Anthropic extended thinking / OpenAI reasoning effort
    effort: medium         # low | medium | high
    budget_tokens: 4096
    summary: true          # stream thinking summaries in the TUI
```

## Routing Config (`routing.yaml`)

```yaml
router:
  intent_patterns: []      # YAML regex rules for T0 routing (no model call)
  default_tier: t1         # t0 | t1 | t2

  complexity_paths:
    low:    { model: claude-haiku-... }
    medium: { model: claude-sonnet-... }
    high:   { model: claude-opus-... }

ppd:
  mode: enabled            # enabled | shadow | disabled
  # enabled  — qualifying work delegated to ppd-planner
  # shadow   — observe routing decisions without invoking ppd-planner
  # disabled — skip PPD policy entirely
```

## Security Config (`security.yaml`)

```yaml
security:
  path_allowlist:
    - "."                  # relative to project root
  shell_restrictions: []   # blocked shell commands / prefixes
  mcp_trust:
    require_approval: true
    namespace_prefix: mcp_ # tool namespace prefix
```

## MCP Configuration (`.mcp.json`)

MCP servers are defined in `.mcp.json` in the project root. Use `${ENV_VAR}` references for credentials — never hardcode secrets:

```json
{
  "servers": [
    {
      "name": "my-server",
      "transport": "stdio",
      "command": "my-mcp-server",
      "args": ["--token", "${MY_API_TOKEN}"]
    },
    {
      "name": "remote-server",
      "transport": "sse",
      "url": "https://api.example.com/mcp",
      "headers": {
        "Authorization": "Bearer ${REMOTE_API_KEY}"
      }
    }
  ]
}
```

:::warning Credential requirement
All credential-like arguments and header values **must** use `${ENV_VAR}` references. Hardcoded credentials are rejected. `mcp list` and `mcp test` output redacts credential values.
:::

Manage MCP servers with:

```bash
chronos-code mcp add     # add a server to .mcp.json
chronos-code mcp list    # list configured servers
chronos-code mcp test    # test a server's connectivity
chronos-code mcp remove  # remove a server
```

## Credential Injection

Provider API keys are read from environment variables and merged as credentials — never stored in config files:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."
```

Use `chronos-code login` for OAuth and enterprise credential flows. `chronos-code whoami` shows the effective credential source.

## Capability Status

| Capability | Status |
|-----------|--------|
| Go code graph, SQLite sessions, deterministic YAML memory | **Default** |
| Tree-sitter graph | Optional `treesitter` build tag |
| PostgreSQL storage | Optional `postgres` build tag |
| LSP tools | Optional `lsp` build tag |
| PPD policy | `enabled` in embedded `routing.yaml`; `shadow` observes; `disabled` skips |
| Verification | `report` by default; `enforce` is opt-in |
| Learning suggestions | On, human review required; `auto_distill: false` |
| Vector recall and branchable sessions | Roadmap |

## See Also

- [Rollback Controls](./rollback) — per-switch disable controls
- [Security Architecture](./architecture/security) — guardrail stack details
- [MCP Architecture](./architecture/mcp) — MCP discovery and runtime
