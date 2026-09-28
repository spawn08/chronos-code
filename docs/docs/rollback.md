---
sidebar_position: 9
title: Turning Features Off
description: Switch individual Chronos Code features off without losing any data.
---

# Turning Features Off

Every major feature can be switched off on its own. Nothing is deleted when you do this: sessions, memories and suggestions stay on disk, and come back when you switch the feature on again.

Add the switches to `.chronos-code/config.yaml` (one project) or `~/.chronos-code/config.yaml` (all your projects), then restart Chronos Code:

```yaml
memory:
  enabled: false                 # don't save or recall memories

session:
  recall_prior_summaries: false  # don't bring summaries of earlier sessions into new ones

learning:
  enabled: false                 # stop creating improvement suggestions
  pattern_injection: false       # stop using previously accepted patterns

mcp:
  discovery_enabled: false       # ignore all MCP server configuration

router:
  enabled: false                 # always use each agent's configured model

workspace:
  index_on_start: false          # don't index the code in the background at startup
```

| Switch | Turn it off when… |
|---|---|
| `memory.enabled` | You don't want anything carried over between sessions |
| `session.recall_prior_summaries` | Each session should start completely fresh |
| `learning.enabled` | You don't want suggestions at all |
| `learning.pattern_injection` | Accepted patterns are giving unhelpful advice |
| `mcp.discovery_enabled` | You're in CI, or you want no external tools |
| `router.enabled` | You want predictable model choice and cost |
| `workspace.index_on_start` | Background indexing uses too many resources on a very large repository |

:::note
Switching features off never weakens safety. The permission rules in [Permissions and Safety](./security) stay in force no matter what you disable.
:::
