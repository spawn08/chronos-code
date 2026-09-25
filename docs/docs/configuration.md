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

### Runtime Capabilities

Agent tools declared under `agents[].tools` must resolve to callable runtime tools. Startup fails before a model call if a configured tool is unavailable. Additional service requirements can be declared explicitly:

```yaml
runtime_capabilities:
  capabilities:
    - name: graph:code
    - name: tool:file_write
      agent: coder
    - name: lsp:tools
      optional: true
```

Supported capability namespaces are `tool:<name>`, `graph:code`, `lsp:tools`, `mcp:<server>`, `planning:plan-mode`, `write:files`, and `write:shell`. Tool, MCP, and write requirements should specify an `agent`. Missing required capabilities stop startup; missing optional capabilities produce a warning.

Export-only examples such as `tools.yaml` and `mcp-servers.yaml` do not count as runtime capability evidence.

### Bounded Repair

Verification-driven repair continues in the same task and session. Limits are cumulative across the original attempt, output continuation, and repair attempts. A value of `0` disables that limit.

```yaml
repair:
  max_attempts: 1
  max_model_calls: 12
  max_tool_calls: 100
  wall_time_sec: 1800
  max_tokens: 500000
  max_cost_microdollars: 0       # opt in only when every model has known pricing
```

Repair prompts contain only unmet obligations, affected paths, and remaining limits. Repeated identical verification failures stop without another model call.

Provider model-list APIs expose model or deployment identifiers, not authoritative prices. This is especially important for Azure deployment aliases and negotiated enterprise pricing. Consequently, the default USD repair limit is disabled while model-call, tool-call, wall-time, token, and repair-attempt limits remain enforced. Configuring a positive USD limit continues to fail closed if any selected model has no known price.

### Retention And Cleanup

Retention policies independently enforce age, count, and estimated byte limits. A value of `0` disables only that dimension; an all-zero policy is disabled. Cleanup is bounded to `batch_size` candidates per scope and periodic server cleanup is opt-in.

```yaml
retention:
  enabled: true
  batch_size: 100
  periodic_interval_minutes: 0
  policies:
    sessions: {max_age_days: 90, max_count: 500, max_bytes: 0}
    input_artifacts: {max_age_days: 7, max_count: 500, max_bytes: 268435456}
```

Use `chronos-code cleanup status`, `chronos-code cleanup run --dry-run`, or `chronos-code cleanup prune <scope>`. Plan records have no retention timestamps, so plan cleanup is intentionally explicit and tenant/repository scoped: `chronos-code cleanup prune plan_db --tenant <id> --repository <id> --dry-run`.

### Server and Fleet Affinity

```yaml
server:
  listen: ":8430"
  auth_type: api_key
  tenant_id: team-a
  max_concurrent: 1
  request_timeout_sec: 300
  instance_id: node-a
  fleet_instances: [node-a, node-b]
```

Set the API key through `CHRONOS_CODE_API_KEY`, not in committed YAML. For a multi-instance deployment, every node must use the same `fleet_instances` values and a distinct `instance_id`. `CHRONOS_CODE_INSTANCE_ID` and comma-separated `CHRONOS_CODE_FLEET_INSTANCES` override the YAML values. See [Deployment and Fleet Operations](./deployment.md) for probe, affinity, metrics, and permission semantics.

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

### Durable Evidence Ledger

Long-horizon work spans many turns, restarts, and resumes. With `ledger.persist` enabled, executions that carry an explicit task ID (durable plan nodes, resumed tasks) replay and extend one append-only evidence ledger instead of starting empty. Verification evidence from an earlier turn therefore still satisfies completion obligations later, as long as it is current.

```yaml
ledger:
  persist: false   # opt in; generated one-off task IDs are never persisted
```

- Storage: `<project data dir>/ledgers/<sha256(task id)>.jsonl`, one fsynced JSON record per event. Events are persisted before they are committed in memory.
- A record cut off by a crash mid-append is truncated when the ledger reopens. Any other malformed or out-of-order history fails closed and is not replayed.
- When the ledger reopens, each file the task wrote is hashed again. If a file changed outside the agent, the runtime records a synthetic write, so evidence that overlaps that file is no longer current.

### Self-Invalidating Claims

With `claims.enabled`, every ranged `file_read` (one with `start_line`/`end_line`) is recorded as a claim anchored to the content hash of the lines read. The runtime re-checks those anchors so the model is told when something it read earlier no longer matches the workspace.

```yaml
claims:
  enabled: false   # opt in
```

- Claim states: `live` (the lines still match; a span that only moved is relocated), `stale` (the lines changed or were removed), and `doubted` (the lines match but a claim it was derived from is no longer live).
- Re-checks run after `file_write` (the written file), after mutating or unclassified shell commands (all claims), and when an execution continues a task with the same explicit task ID (all claims, catching edits made outside the agent).
- A mutating shell command whose effects invalidate claims returns a `claims_invalidated` list in its tool result.
- When a task is continued, a `[Working claims]` block is added to the turn: outdated spans first (re-read before relying on them), then spans to re-check, then spans that are still current.
- Every status change is appended to the evidence ledger as a `claim` event (audit only; it does not affect verification). Claim stores are kept in memory, with at most 256 claims per task and 64 continued tasks.

### Model Catalog and Pricing

Model prices, context windows, and `/model` picker entries come from the [models.dev](https://models.dev) catalog (the open database opencode also uses), cached at `~/.chronos-code/cache/models.json`.

```yaml
models_catalog:
  auto_refresh: true            # refresh a cache older than 24h in the background
  url: "https://models.dev"     # catalog is fetched from <url>/api.json
```

- Startup applies the cached catalog before the first model call and never waits on the network. A missing or stale cache is refreshed in the background and used from the next start (or immediately for later calls in a long session).
- `chronos-code models refresh` fetches the catalog on demand; it ignores `auto_refresh` and `CHRONOS_CODE_DISABLE_MODELS_FETCH`, which only stop automatic fetches.
- Only providers chronos-code can build are cached (anthropic, openai, google → `gemini`, mistral, deepseek, groq, together, fireworks, perplexity, openrouter). When a model ID appears under several providers, the first-party provider's price and window win. Picker entries exclude deprecated and non-tool-calling models.
- Context windows use the model's usable input limit when models.dev publishes one smaller than the full window (for example gpt-5: 272K of 400K). `defaults.context.max_tokens` (128000 by default) still caps compaction and the context guard.

Prices resolve in this order, later layers winning per model ID:

1. Bundled `pricing.yaml` (the offline fallback)
2. The models.dev catalog
3. `~/.chronos-code/pricing.yaml` (user)
4. `<project>/.chronos-code/pricing.yaml` (project)

```yaml
# pricing.yaml: USD per 1M tokens
models:
  my-model: { input: 3, output: 15, cache_read: 0.3, cache_write: 3.75, cache_write_1h: 6 }
  long-context-model:
    input: 5
    output: 30
    tiers:
      - { above: 272000, input: 10, output: 45 }   # whole call billed at these rates past 272K prompt tokens
```

Omitted `cache_read`/`cache_write` default to `input`, and `cache_write_1h` defaults to `cache_write`. models.dev does not publish Anthropic's 1-hour cache-write rate, so catalog Anthropic models use 2× input. A model with no price in any layer is reported as `unpriced` (or `≥$…` when only some calls were priced), never as $0.

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
  mode: shadow             # enabled | shadow | disabled
  # enabled  — delegate one proposal turn to delivery-strategist
  # shadow   — observe routing decisions without invoking delivery-strategist
  # disabled — skip PPD policy entirely
```

`ppd` is retained as the public compatibility key. Shadow remains the default because a production rolling-replanning loop is not implemented.

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
  "mcpServers": {
    "local-files": {
      "transport": "stdio",
      "command": "mcp-files",
      "args": ["--token", "${MCP_FILES_TOKEN}"]
    },
    "remote-search": {
      "transport": "sse",
      "url": "https://mcp.example.com/events?token=${MCP_SEARCH_TOKEN}"
    }
  }
}
```

This example is validated by `internal/mcpdiscover/testdata/mcp.json`.

Discovery parses `.mcp.json`, Cursor, VS Code, Claude, `package.json`, and the user config independently. A malformed source is reported without suppressing healthy siblings. During file watching, only the malformed source falls back to its last-known-good entries.

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
| PPD policy | `shadow` by default; `enabled` is rejected until closed-loop durable execution is available |
| Verification | `report` by default; `enforce` is opt-in |
| Learning suggestions | On, human review required; `auto_distill: false` |
| Vector recall and branchable sessions | Roadmap |

## See Also

- [Rollback Controls](./rollback) — per-switch disable controls
- [Security Architecture](./architecture/security) — guardrail stack details
- [MCP Architecture](./architecture/mcp) — MCP discovery and runtime
