---
sidebar_position: 6
title: Security Policy
description: Credential-reference policy, ValidateManagedServer validation, and operator security reference
---

# Security Policy

This page is the **operator-facing security reference** for Chronos Code. It covers what the
security system enforces, what cannot be overridden, and how to configure restrictions.

For the implementation internals, see [Security Subsystem](./subsystems/security).

## What Cannot Be Overridden

The **embedded security floor** is baked into the binary. No project config file, `--yolo` flag,
or runtime switch can weaken it:

| Protection | Detail |
|------------|--------|
| Credential-reference enforcement | Hardcoded secrets in config or MCP are rejected at load |
| Path traversal prevention | Access outside `path_allowlist` is denied before tool execution |
| Guardrail deny rules | Inject/PII/secret-scan denials are always active |
| USD fail-closed | Unknown model + USD cap → provider call blocked |

## Credential-Reference Policy

All credential-like values in any Chronos Code config file or `.mcp.json` **must** use
`${ENV_VAR}` reference syntax:

```yaml
# .chronos-code/config.yaml — correct
providers:
  anthropic:
    api_key: "${ANTHROPIC_API_KEY}"
```

```json
// .mcp.json — correct
{
  "servers": [
    {
      "name": "my-server",
      "transport": "stdio",
      "command": "my-server-binary",
      "args": ["--token", "${MY_SERVER_TOKEN}"]
    }
  ]
}
```

Hardcoded values matching credential patterns are rejected by `validateSecretReferences` during
config load and MCP discovery. The rejected server or config field is marked **denied** and
excluded — other config fields and servers continue loading normally.

### Why `${ENV_VAR}` Is Required

1. **Prevents accidental commit** of secrets to source control
2. **Enables rotation** without modifying config files
3. **Enables audit** — the config file shows which env vars are used, not their values
4. **Allows redaction** — CLI commands like `mcp list` and `mcp test` can safely display configs

## ValidateManagedServer {#validate-managed-server}

`ValidateManagedServer` is the structural validation gate run during MCP discovery on every
candidate server entry.

Source: `internal/mcpdiscover/config.go`

It enforces:

| Check | Detail |
|-------|--------|
| Name non-empty | Server must have a name |
| Transport valid | Only `stdio` or `sse` are accepted |
| stdio requires Command | `command` must be set for stdio transport |
| sse requires HTTPS URL | `url` must start with `https://` |
| No credential literals | All credential-like values must be `${ENV_VAR}` references |

A server failing any check is assigned `StateInvalid` or `StateDenied` and excluded from the
Load phase. Other servers continue loading normally (failure isolation).

```bash
# Test validation without connecting
chronos-code mcp test
```

## MCP Credential Validation Gates

There are two sequential gates for MCP credential checking:

```
.mcp.json
    │
    ▼ Gate 1: validateSecretReferences (Discover phase)
    │  Checks: credential format (${ENV_VAR} required)
    │  On fail: server → StateDenied, excluded
    │
    ▼ Gate 2: validateRuntimeConfig (Load phase)
    │  Checks: env vars are actually set in the environment
    │  On fail: server → StateInvalid, excluded
    │
    ▼ Runtime (connect + tool registration)
```

**Recommendation:** always set all referenced environment variables before starting Chronos Code
so that both gates can fully validate MCP server credentials at startup.

## Security Configuration Reference

### `security.yaml`

```yaml
# .chronos-code/security.yaml
security:
  # Files and directories accessible to tool calls
  path_allowlist:
    - "."                      # project root (recommended minimum)
    - "/tmp/chronos-*"         # scratch space

  # Shell tool restrictions
  shell_restrictions:
    blocked_commands:
      - "curl"
      - "wget"
    blocked_prefixes:
      - "sudo"
      - "su "

  # MCP server trust policy
  mcp_trust:
    require_approval: true     # users must approve each MCP tool call
    max_connections: 10        # maximum simultaneous MCP connections
    namespace_prefix: "mcp_"
```

### `config.yaml` — Budget and Permission Mode

```yaml
# .chronos-code/config.yaml
budget:
  usd: 5.00                    # hard stop at $5.00 per session (fails closed)
  tokens: 100000               # optional token cap

permission_mode: default       # default | auto | strict
```

| Permission Mode | Behavior |
|----------------|----------|
| `default` | Interactive approval for risky operations |
| `auto` | Auto-approve policy-allowed tools (`--yolo` equivalent) |
| `strict` | Deny all shell and write operations outside explicit allowlist |

## Guardrail Configuration

Built-in guardrails are in `internal/defaults/` (embedded YAML). To customize:

```yaml
# .chronos-code/guardrails/custom.yaml
rules:
  - name: block-gh-token
    scope: tool_result
    pattern: "ghp_[A-Za-z0-9]+"
    action: deny
    message: "GitHub PAT detected in tool result — blocking to prevent credential leakage"
```

Place custom guardrail files in `.chronos-code/guardrails/`. They are merged with embedded
defaults; conflicting rule names in project files take precedence.

## Disable Controls

All security features can be **tightened** but not **removed**. The following toggles reduce
functionality:

```yaml
# Disable MCP entirely
mcp:
  discovery_enabled: false

# Disable memory recall (reduces context injection attack surface)
memory:
  enabled: false

# Disable learning suggestions
learning:
  enabled: false
```

See [Rollback Controls](./rollback) for the full per-subsystem disable reference.

## See Also

- [Rollback Controls](./rollback)
- [Configuration](./configuration)
- [Security Subsystem Internals](./subsystems/security)
- [MCP Subsystem](./subsystems/mcp)
