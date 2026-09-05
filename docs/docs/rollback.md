---
sidebar_position: 4
title: Rollback Controls
description: Per-subsystem disable switches for Chronos Code
---

# Rollback Controls

Chronos Code exposes independent YAML switches to disable individual subsystems. Sessions, memories, learned patterns, and `.mcp.json` files remain on disk when a subsystem is disabled — they can be re-enabled at any time without data loss.

## Available Switches

Set any of these in `.chronos-code/config.yaml` (or `~/.chronos-code/config.yaml`) and **restart** the process:

```yaml
session:
  recall_prior_summaries: false   # stop injecting prior session summaries
  context_report: false           # hide /context source breakdown

learning:
  pattern_injection: false        # stop injecting approved patterns into context
  enabled: false                  # stop emitting new learning suggestions

memory:
  enabled: false                  # stop persist and recall entirely

mcp:
  discovery_enabled: false        # skip .mcp.json loading at startup
```

:::caution Restart required
Changes to these switches take effect only on the next process start. The running process does not hot-reload configuration.
:::

## MCP File Recovery

If a `.mcp.json` mutation needs to be reversed, restore from the atomic-write backup that is written to the same directory:

```bash
# The backup is written alongside .mcp.json before every mutation
ls .mcp.json*
# .mcp.json  .mcp.json.bak   ← restore from .bak if needed
```

## Embedded Security Floor

The embedded security floor **cannot be disabled** by any YAML switch, `--yolo`, or project policy. It enforces:

- Path access controls
- Injection detection
- Secret scanning
- Destructive operation confirmations

## Per-Consumer Rollback Controls

:::note TBD
Per-consumer (per-agent or per-surface) disable controls are not yet exposed in the configuration surface. Individual agents inherit the global switches above. Fine-grained per-agent capability restrictions are on the roadmap.
:::

## Capability Status by Switch

| Switch | Effect when `false` / `disabled` |
|--------|----------------------------------|
| `session.recall_prior_summaries` | Prior session context not injected; fresh context each run |
| `session.context_report` | `/context` command returns empty; context breakdown hidden |
| `learning.pattern_injection` | Approved patterns not added to prompts |
| `learning.enabled` | No new suggestions emitted; existing suggestions unaffected |
| `memory.enabled` | No reads or writes to project/user/feedback YAML |
| `mcp.discovery_enabled` | `.mcp.json` not loaded; no MCP tools available |

## See Also

- [Configuration](./configuration) — full config reference
- [MCP Architecture](./architecture/mcp) — MCP discovery lifecycle
- [Known Issues](./known-issues) — known gaps in rollback documentation
