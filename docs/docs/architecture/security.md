---
sidebar_position: 6
title: Security
description: Guardrail stack, validateSecretReferences, credential validation
---

# Security

Chronos Code has a multi-layer security architecture. The **embedded security floor** cannot be weakened by project config, `--yolo`, or any runtime switch. Project policy can add restrictions on top of the floor but cannot remove them.

## Security Layers

```
User request
     ↓
┌─────────────────────────────┐
│  1. Guardrail Engine        │  injection, PII, secret detection
│     internal/guardrail      │
├─────────────────────────────┤
│  2. Security Policy         │  path allowlist, shell restrictions
│     internal/security       │
├─────────────────────────────┤
│  3. Verification Policy     │  report / enforce completion gate
│     internal/verification   │
├─────────────────────────────┤
│  4. Budget Cap              │  token and USD limits
│     internal/budget         │
├─────────────────────────────┤
│  5. Auth                    │  API keys, OAuth, keychain
│     internal/auth           │
└─────────────────────────────┘
     ↓
Tool execution / model call
```

## Guardrail Engine (`internal/guardrail`)

The guardrail engine loads YAML guardrail rules from `.chronos-code/guardrails/` (or embedded defaults). Each rule specifies:

- **Scope**: which tool calls or message types to inspect
- **Pattern**: what to detect (regex, keyword list, or semantic check)
- **Action**: `warn`, `redact`, or `deny`

Built-in guardrail categories:

| Category | Default Action | Description |
|----------|---------------|-------------|
| Injection detection | `deny` | Prompt injection in tool results or user input |
| Secret scanning | `deny` | Credentials, API keys, tokens in outbound content |
| PII filtering | `warn` + `redact` | Personal data in prompts and tool results |
| Destructive operations | `deny` (require confirm) | `rm -rf`, `DROP TABLE`, etc. |

## Security Policy (`internal/security`)

`security.yaml` configures the security policy applied to every tool call:

```yaml
security:
  path_allowlist:
    - "."                    # project root and below
    - "/tmp/chronos-*"       # temp scratch space

  shell_restrictions:
    blocked_commands:
      - "curl"               # example: block direct curl
      - "wget"
    blocked_prefixes:
      - "sudo"
      - "su "

  mcp_trust:
    require_approval: true   # all MCP tools need user approval
    namespace_prefix: mcp_
```

### Path Policy

File and shell operations are validated against the `path_allowlist`. Any attempt to access a path outside the allowlist is denied at the security layer, before the tool executes.

### Shell Policy

The shell policy applies to `shell` tool calls. Blocked commands and prefixes are rejected before subprocess creation.

## `validateSecretReferences`

The `validateSecretReferences` function (in `internal/security`) enforces that credential-like values in config and MCP definitions use `${ENV_VAR}` references. It runs:

1. During MCP Discover phase (validates `.mcp.json` server args and headers)
2. During config load (validates any inline credential fields)

A secret reference validation failure causes the affected MCP server to be marked **denied** at the Discover phase — not at runtime. This is the first validation gate.

## Credential Validation

The second gate, `validateRuntimeConfig`, runs during the MCP Load phase. It verifies that every `${ENV_VAR}` referenced in the server config is actually set in the environment. An unset variable causes the server to be marked **unavailable**.

**Recommendation:** set all referenced environment variables before starting Chronos Code so that both validation gates can fully evaluate MCP server credentials at startup.

## `--yolo` Mode

`--yolo` auto-approves all **policy-allowed** tools without requiring user confirmation. It does not affect:

- Deny rules (both embedded floor and project policy)
- Destructive operation confirmations (explicit double-confirm)
- Guardrail blocks

## USD Budget Cap

```yaml
# config.yaml
budget:
  usd: 5.00              # hard stop at $5.00 per session
  tokens: 100000         # optional token cap
```

If pricing for a model is unavailable, a positive USD cap **fails closed** — the provider call is not made. Unknown models only run without a USD cap (`--budget 0` or unset).

## Permission Modes

| Mode | Description |
|------|-------------|
| `default` | Interactive approval for risky operations |
| `auto` | Auto-approve policy-allowed tools (equivalent to `--yolo`) |
| `strict` | Deny all shell and write operations not in allowlist |

## Audit and Hooks

`internal/security` supports pre/post tool hooks for audit logging. Hooks run outside the guardrail path — a hook failure produces a warning, not a tool denial.

## See Also

- [Configuration — Security section](../configuration#security-config-securityyaml)
- [MCP Architecture](./mcp) — credential requirement detail
