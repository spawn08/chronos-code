---
slug: /
sidebar_position: 1
title: Introduction
description: Chronos Code — YAML-native AI coding agent harness built on the Chronos framework
---

# Chronos Code

[![CI](https://github.com/spawn08/chronos-code/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/spawn08/chronos-code/actions/workflows/ci.yml)
[![Release](https://github.com/spawn08/chronos-code/actions/workflows/release.yml/badge.svg)](https://github.com/spawn08/chronos-code/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/spawn08/chronos-code?sort=semver)](https://github.com/spawn08/chronos-code/releases/latest)

**Chronos Code** is a YAML-native AI coding agent harness built on the [Chronos](https://github.com/spawn08/chronos) agentic framework. It is a single Go binary that loads YAML agents, skills, and policies, then runs them through Chronos.

The product the user talks to is the `chronos-code` primary agent. Specialists are spawned on demand, or when the user `@mention`s them. Persistent memory, a Go code graph, MCP tools, guardrails, and a review-gated learning loop sit around that execution path.

## Key Features

| Feature | Description |
|---------|-------------|
| **YAML-first configuration** | Agents, skills, guardrails, security policies, routing, and MCP servers defined in YAML |
| **Primary agent + specialists** | `chronos-code` stays the conversation partner; coder, planner, reviewer, debugger, researcher, architect, and explainer run via `spawn_subagent` or `@agent_id` |
| **Go code graph** | Default indexer uses `go/packages` and the Go AST; tree-sitter via `treesitter` build tag |
| **Tiered routing** | T0 graph tools → T1 cheap models → T2 frontier models |
| **Self-learning loop** | Traces sessions into reviewable YAML suggestions; auto-distillation off by default |
| **MCP** | stdio and HTTPS SSE servers from `.mcp.json`; tools namespaced and approval-gated |
| **Guardrails** | Injection detection, secret scanning, PII filtering, cost caps |
| **Two surfaces** | Interactive TUI/CLI and `chronos-code serve` HTTP API |
| **Sessions and memory** | Resumable SQLite sessions; project/user/feedback memory as git-diffable YAML |
| **Embedded defaults** | First run works without any config files |

## Quick Install

### Prebuilt binary (recommended)

```bash
# macOS / Linux
curl -fsSL https://raw.githubusercontent.com/spawn08/chronos-code/main/scripts/install.sh | bash
```

```powershell
# Windows
irm https://raw.githubusercontent.com/spawn08/chronos-code/main/scripts/install.ps1 | iex
```

### Build from source

```bash
git clone https://github.com/spawn08/chronos-code
cd chronos-code
make build        # produces bin/chronos-code
```

## Default Agents

| Agent | Role | Tier |
|-------|------|------|
| `chronos-code` | Primary conversation agent; orients, routes, synthesizes | Frontier |
| `coder` | Implement, test, iterate | Frontier |
| `planner` | Task decomposition | Frontier |
| `ppd-planner` | Read-only durable DAG for multi-package / high-risk work | Frontier |
| `reviewer` | Bugs, security, style | Frontier |
| `debugger` | Failures from errors and traces | Frontier |
| `researcher` | Read-only search | Cheap |
| `architect` | Design and structure | Frontier |
| `explainer` | Explain code and concepts | Cheap |

## Next Steps

- [Getting Started](./getting-started) — build, initialize, and run your first session
- [Configuration](./configuration) — YAML config reference
- [Architecture Overview](./architecture/intro) — how the subsystems connect
- [Diagrams](./diagrams/architecture-overview) — visual system diagrams
