---
sidebar_position: 5
title: Known Issues
description: Tracked issues with severity, component, and status
---

# Known Issues

This page tracks known issues found during code review and analysis. Each issue includes severity, the affected component, a description with source file references, and current status.

## Issue Table

| # | Severity | Component | Description | Status |
|---|----------|-----------|-------------|--------|
| [1](#issue-1-mcp-credential-bypass) | **High** | MCP / Security | MCP credential validation bypass at Discover → Load boundary | Open |
| [2](#issue-2-context-guard-budget-boundary) | **Medium** | ContextGuard / Context | Context budget boundary condition near `effectiveLimit` | Open |
| [3](#issue-3-rollback-documentation-gap) | **Medium** | Documentation / Config | Per-consumer rollback controls are undocumented and not yet exposed | Open |
| [4](#issue-4-untracked-test-file) | **Medium** | Testing | Test file not tracked in CI coverage reporting | Open |

---

## Issue 1: MCP Credential Bypass {#issue-1-mcp-credential-bypass}

| Field | Value |
|-------|-------|
| **Severity** | High |
| **Component** | `internal/mcpdiscover`, `internal/security` |
| **Source** | `internal/mcpdiscover/` — `validateSecretReferences`, `validateRuntimeConfig` |
| **Status** | Open |

### Description

There is a boundary condition between the two MCP validation gates:

1. **`validateSecretReferences`** (Discover phase) — validates that credential-like values use `${ENV_VAR}` format
2. **`validateRuntimeConfig`** (Load phase) — validates that referenced env vars are actually set in the environment

A server with an `${ENV_VAR}` reference that passes `validateSecretReferences` format checking but has its referenced env var unset can enter the Load phase before `validateRuntimeConfig` detects the missing value. Depending on timing and the specific server transport, this may allow a partially-configured server to attempt startup before the credential check completes.

### Impact

An MCP server that should be marked **unavailable** (due to unset credentials) may instead reach the Runtime phase startup attempt and fail there rather than at the intended Load gate. This can produce misleading error messages and may briefly expose an unauthenticated connection attempt to the remote server.

### Workaround

Ensure all `${ENV_VAR}` references are exported in the environment before starting Chronos Code:

```bash
export MY_API_TOKEN="..."
chronos-code
```

Use `chronos-code mcp test` to validate server connectivity before interactive use.

### Related

- [MCP Architecture](./architecture/mcp)
- [MCP Discovery Flow](./diagrams/mcp-discovery)
- [Security Architecture](./architecture/security#credential-validation)

---

## Issue 2: Context Guard Budget Boundary {#issue-2-context-guard-budget-boundary}

| Field | Value |
|-------|-------|
| **Severity** | Medium |
| **Component** | `internal/activation`, `internal/incctx`, `internal/toolcompress` |
| **Source** | `internal/activation/`, `internal/toolcompress/` |
| **Status** | Open |

### Description

The `ContextGuard` uses `effectiveLimit = min(modelContextWindow, configuredBudget)` to determine the trim/reject boundary. At the exact boundary (token count == `effectiveLimit`), the guard's `NearLimit` threshold check (80% of `effectiveLimit`) and the `OverLimit` check can produce different behaviors depending on whether the token count is computed before or after tool output is appended.

This produces a one-turn "over-budget" state that resolves on the next trim cycle rather than being caught proactively, potentially causing the agent to produce a slightly truncated tool result on the boundary turn.

### Impact

At the token budget boundary, one tool result may be silently truncated rather than compressed via `toolcompress`. This is correctness-adjacent: the result is still usable, but the truncation is not surfaced in the `ContextReport`.

### Workaround

Set `budget.tokens` to a value at least 10% below the model's context window to avoid operating at the exact boundary:

```yaml
budget:
  tokens: 180000   # for a 200k context window model
```

### Related

- [Context Budget Diagram](./diagrams/context-budget)
- [Orchestrator — ContextGuard](./architecture/orchestrator#contextguard-budget-math)

---

## Issue 3: Rollback Documentation Gap {#issue-3-rollback-documentation-gap}

| Field | Value |
|-------|-------|
| **Severity** | Medium |
| **Component** | Documentation, `internal/config` |
| **Source** | `internal/config/`, `.chronos-code/config.yaml` |
| **Status** | Open (TBD) |

### Description

Per-consumer (per-agent or per-surface) rollback controls are not yet exposed in the configuration surface. The [Rollback Controls](./rollback) page documents global switches, but there is no mechanism to:

- Disable MCP for a specific agent while leaving it enabled for others
- Restrict memory recall to specific agents
- Apply different budget caps per agent role

This means the only granularity available is global per-subsystem switches.

### Impact

Operators cannot selectively disable features for high-risk specialist agents (e.g., disabling MCP for `coder` while leaving it enabled for `researcher`) without disabling it globally.

### Planned Resolution

Per-agent capability configuration is on the roadmap. The expected mechanism is an `agent.capabilities` YAML stanza in each agent definition file.

### Related

- [Rollback Controls](./rollback)
- [Configuration](./configuration)

---

## Issue 4: Untracked Test File {#issue-4-untracked-test-file}

| Field | Value |
|-------|-------|
| **Severity** | Medium |
| **Component** | Testing, CI |
| **Source** | `benchmark/ppd/results.json`, `.github/workflows/ci.yml` |
| **Status** | Open |

### Description

`benchmark/ppd/results.json` is marked `invalid` in the repository — no real model was invoked when generating it. The CI workflow does not exclude this file from test reporting, which means:

1. The file can be cited as PPD test evidence when it is not
2. The `chronos-code eval ppd --report` command fails closed on this file (correct behavior), but the file's presence in the repo is misleading

From the README:
> `benchmark/ppd/results.json` is `invalid`: no real model was invoked. It must not support a PPD quality or efficiency claim. Invalid runs are excluded from successful-task denominators.

### Impact

A developer or CI consumer reading the benchmark directory could misinterpret the invalid results as real PPD performance evidence.

### Recommended Fix

1. Add a `_INVALID` suffix or `README` to the benchmark directory clearly marking the file as a placeholder
2. Add a CI check that asserts `results.json` is marked `invalid` and skips it in any summary reporting
3. Remove the file entirely and only commit real benchmark results

### Related

- `benchmark/eval/baseline.json` — real baseline (used by `make eval`)
- `benchmark/eval/report.md` — synthetic fixture-replay result (also not a real comparison)
- `.github/workflows/ci.yml` — CI workflow

---

## Reporting New Issues

To report a new issue, open a [GitHub issue](https://github.com/spawn08/chronos-code/issues) with:
- Component name and source file path
- Steps to reproduce or evidence from code review
- Proposed severity (High / Medium / Low)
