---
sidebar_position: 2
title: MCP
description: ManagedServer lifecycle, Discover/Load/Runtime phases, and credential validation
---

# MCP (Model Context Protocol)

The MCP subsystem (`internal/mcpdiscover`) manages external tool servers defined in `.mcp.json`.
It handles discovery, validation, startup, namespacing, and cleanup.

## Supported Transports

| Transport | Description |
|-----------|-------------|
| `stdio` | Local process over stdin/stdout |
| `sse` | Remote HTTPS Server-Sent Events |

Only these two transports are accepted. HTTP (non-HTTPS) SSE is rejected.

## ManagedServer

A `ManagedServer` is the in-memory representation of a server entry from `.mcp.json`:

```go
// internal/mcpdiscover/config.go
type ManagedServer struct {
    Name       string
    Transport  string   // "stdio" | "sse"
    Command    string   // stdio only
    Args       []string // stdio only
    URL        string   // sse only
    Permission string   // "allow" | "deny" | "require_approval"
    Env        map[string]string
}
```

## Discover / Load / Runtime Phases

Each `.mcp.json` entry progresses through three gates. Failure at any gate stops further progress.

### 1. Discover Phase

At startup, `mcpdiscover` reads `.mcp.json` and constructs candidate `ManagedServer` entries:

1. Parse transport type (`stdio` / `sse`)
2. Verify required fields are present
3. **`validateSecretReferences`** — credential format gate: all credential-like values must use
   `${ENV_VAR}` form (hardcoded credentials are rejected here)
4. **`ValidateManagedServer`** — structural validation gate

Candidates failing `ValidateManagedServer` are marked **denied** and excluded from loading.

### 2. Load Phase

Candidates passing Discover enter the Load phase:

1. Resolve `${ENV_VAR}` references from the environment
2. **`validateRuntimeConfig`** — runtime credential gate (verifies env vars are set)
3. Check MCP trust policy from `security.yaml`

Ensure all referenced environment variables are set before starting Chronos Code so that both
validation gates can fully evaluate MCP server credentials at startup.

### 3. Runtime Phase

Servers passing Load are started via `mcpdiscover.Start`:

- **stdio**: spawn subprocess, connect pipes
- **sse**: connect HTTPS endpoint, maintain SSE stream

Once running:

1. Tools are **namespaced** with the server name prefix (`mcp__<server>__<tool>`)
2. Tools are **registered** in the Chronos tool registry
3. All tools default to **`require_approval`** permission regardless of server trust level

Source: `internal/mcpdiscover/runtime.go` — `connectLocked` function

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

Hardcoded credentials are rejected at the Discover phase. The `mcp list` and `mcp test` commands
redact credential values in their output.

## Server State Machine

Each server progresses through these states:

| State | Meaning |
|-------|---------|
| `connected` | Successfully running, tools registered |
| `denied` | Rejected at Discover (structural/format validation) |
| `approval_required` | Policy requires user approval before connecting |
| `invalid` | Runtime config validation failed |
| `connection_limit_reached` | `MaxMCPConnections` exceeded |
| `connection_failed` | Startup or connect error |
| `tool_registration_failed` | Tools collided or failed to register |

## Failure Isolation

The MCP subsystem is designed to be non-blocking:

- **Denied** servers → excluded, warning logged
- **Unavailable** servers → excluded, warning logged
- **Failed** servers → excluded, other servers continue
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

Every `.mcp.json` mutation writes a backup file (`.mcp.json.bak`) before applying changes:

```bash
cp .mcp.json.bak .mcp.json  # revert a mutation
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
