---
sidebar_position: 3
title: MCP
description: ManagedServer lifecycle, Discover/Load/Runtime phases, credential requirements
---

# MCP (Model Context Protocol)

The MCP subsystem (`internal/mcpdiscover`) manages external tool servers defined in `.mcp.json`. It handles discovery, validation, startup, namespacing, and cleanup.

:::warning Known Issue
There is a known credential validation bypass in the MCP subsystem. See [Known Issues](../known-issues) — issue #1 for details.
:::

## Supported Transports

| Transport | Description |
|-----------|-------------|
| `stdio` | Local process over stdin/stdout |
| `sse` | Remote HTTPS Server-Sent Events |

Only these two transports are accepted. HTTP (non-HTTPS) SSE is rejected.

## ManagedServer Lifecycle

Each entry in `.mcp.json` becomes a `ManagedServer` that progresses through three phases:

### 1. Discover Phase

At startup, `mcpdiscover` reads `.mcp.json` and constructs a candidate list of `ManagedServer` entries. For each candidate:

- Parse transport type (stdio / sse)
- Check required fields are present
- Validate credential references (`${ENV_VAR}` format)
- Run `ValidateManagedServer` — structural validation gate

Candidates that fail `ValidateManagedServer` are marked **denied** and excluded from loading. Denied servers do not block the startup of healthy servers.

### 2. Load Phase

Candidates that pass the Discover phase enter the Load phase:

- Resolve `${ENV_VAR}` references from the environment
- Run `validateRuntimeConfig` — runtime credential gate (verifies env vars are set)
- Check MCP trust policy from `security.yaml`

Servers failing `validateRuntimeConfig` (e.g., unset env var) are marked **unavailable** at load time. This is the credential bypass boundary — see [Known Issues](../known-issues).

### 3. Runtime Phase

Servers that pass Load are started:

- stdio: spawn subprocess, connect pipes
- sse: connect HTTPS endpoint

Once running, tools are:

1. **Namespaced** with the server name prefix (e.g., `myserver_tool_name`)
2. **Registered** in the Chronos tool registry
3. **Approval-gated** by default (`require_approval: true` in `security.yaml`)

## Credential Reference Requirement

All credential-like values in `.mcp.json` must use `${ENV_VAR}` references:

```json
{
  "servers": [
    {
      "name": "my-server",
      "transport": "stdio",
      "command": "my-mcp-server",
      "args": ["--token", "${MY_SECRET_TOKEN}"]
    }
  ]
}
```

**Hardcoded credentials are rejected** during the Discover phase. The `mcp list` and `mcp test` commands redact credential values in their output.

## Failure Isolation

The MCP subsystem is designed to be non-blocking:

- **Denied** servers (structural validation failure) → excluded, warning logged
- **Unavailable** servers (runtime credential failure) → excluded, warning logged
- **Failed** servers (startup crash) → excluded, other servers continue
- **Healthy** servers → registered and available

A complete failure of all MCP servers does not block chat or non-MCP tools.

## CLI Management

```bash
chronos-code mcp add     # add a server entry to .mcp.json
chronos-code mcp list    # list configured servers (credentials redacted)
chronos-code mcp test    # test connectivity for each server
chronos-code mcp remove  # remove a server entry
```

## Atomic Backup

Every `.mcp.json` mutation writes a backup file (`.mcp.json.bak`) in the same directory before applying changes. To revert a mutation:

```bash
cp .mcp.json.bak .mcp.json
```

## Disabling MCP Discovery

```yaml
# .chronos-code/config.yaml
mcp:
  discovery_enabled: false
```

Restart required. When disabled, `.mcp.json` is not read and no MCP tools are registered.

## See Also

- [MCP Discovery Diagram](../diagrams/mcp-discovery)
- [Configuration — MCP section](../configuration#mcp-configuration-mcpjson)
- [Rollback Controls](../rollback)
- [Known Issues](../known-issues)
