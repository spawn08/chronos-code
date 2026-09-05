---
sidebar_position: 3
title: MCP Discovery
description: Flowchart of the MCP Discover → Load → Runtime startup path and validation gates
---

# MCP Discovery Flow

This flowchart shows the full lifecycle of MCP server startup, including both validation gates.

```mermaid
flowchart TD
    classDef decision fill:#fef9c3,stroke:#ca8a04,color:#713f12
    classDef process fill:#dbeafe,stroke:#1d4ed8,color:#1e3a8a
    classDef success fill:#dcfce7,stroke:#16a34a,color:#14532d
    classDef failure fill:#fee2e2,stroke:#dc2626,color:#7f1d1d
    classDef gate fill:#f3e8ff,stroke:#7c3aed,color:#3b0764

    START([Orchestrator starts]):::process
    READ[Read .mcp.json]:::process
    PARSE{Parse entries}:::decision

    subgraph DISCOVER["Phase 1: Discover"]
        CHECK_FIELDS{Required fields\npresent?}:::gate
        CHECK_TRANSPORT{Transport is\nstdio or HTTPS SSE?}:::gate
        VALIDATE_CREDS[validateSecretReferences\ncheck ENV_VAR format]:::gate
        VALID_MANAGED{ValidateManagedServer\npasses?}:::gate
    end

    DENIED_STRUCT([DENIED — structural\nvalidation failure]):::failure

    subgraph LOAD["Phase 2: Load"]
        RESOLVE_ENV[Resolve ENV_VAR\nreferences from environment]:::process
        RUNTIME_CREDS{validateRuntimeConfig\nenv vars set?}:::gate
        TRUST_POLICY{MCP trust\npolicy allows?}:::gate
    end

    UNAVAILABLE([UNAVAILABLE — runtime\ncredential failure]):::failure
    POLICY_DENIED([DENIED — trust\npolicy blocked]):::failure

    subgraph RUNTIME["Phase 3: Runtime"]
        START_SERVER{Start server process\n/ connect SSE}:::process
        REGISTER[Register tools with\nnameespace prefix]:::success
        GATE_APPROVAL[Gate: require_approval\n= true by default]:::process
        AVAILABLE([AVAILABLE — tools\nregistered]):::success
    end

    FAILED([FAILED — startup\ncrash]):::failure

    NO_MCP_FILE([No .mcp.json — \nno MCP tools]):::failure

    %% Happy path
    START --> READ
    READ -->|file found| PARSE
    READ -->|not found| NO_MCP_FILE

    PARSE --> CHECK_FIELDS
    CHECK_FIELDS -->|yes| CHECK_TRANSPORT
    CHECK_FIELDS -->|no| DENIED_STRUCT

    CHECK_TRANSPORT -->|stdio or HTTPS SSE| VALIDATE_CREDS
    CHECK_TRANSPORT -->|other| DENIED_STRUCT

    VALIDATE_CREDS --> VALID_MANAGED
    VALID_MANAGED -->|pass| RESOLVE_ENV
    VALID_MANAGED -->|fail| DENIED_STRUCT

    RESOLVE_ENV --> RUNTIME_CREDS
    RUNTIME_CREDS -->|all set| TRUST_POLICY
    RUNTIME_CREDS -->|var unset| UNAVAILABLE

    TRUST_POLICY -->|allowed| START_SERVER
    TRUST_POLICY -->|blocked| POLICY_DENIED

    START_SERVER -->|success| REGISTER
    START_SERVER -->|crash| FAILED

    REGISTER --> GATE_APPROVAL
    GATE_APPROVAL --> AVAILABLE

    %% Isolation note
    DENIED_STRUCT -.->|other servers unaffected| PARSE
    UNAVAILABLE -.->|other servers unaffected| PARSE
    FAILED -.->|other servers unaffected| PARSE
```

## Validation Gates

| Gate | Phase | What it checks | Failure outcome |
|------|-------|---------------|-----------------|
| `ValidateManagedServer` | Discover | Transport type, required fields, `${ENV_VAR}` format | Server marked **denied** |
| `validateRuntimeConfig` | Load | All referenced env vars are set in environment | Server marked **unavailable** |
| Trust policy | Load | Server allowed by `security.yaml` MCP trust config | Server marked **denied** |
| Startup | Runtime | Process starts / SSE connects successfully | Server marked **failed** |

## Failure Isolation

All failure outcomes are isolated — a failed server does not block healthy servers or chat. Failed servers produce a warning log entry at startup.

**Tip:** run `chronos-code mcp test` before starting a session to verify that all MCP servers pass both validation gates and connect successfully.

## See Also

- [MCP Architecture](../architecture/mcp) — detailed description
- [Configuration — MCP section](../configuration#mcp-configuration-mcpjson)
- [Security Architecture](../architecture/security) — `validateSecretReferences` detail
