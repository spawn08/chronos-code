---
sidebar_position: 3
title: Memory
description: Store.Recall, session summaries, and learned patterns
---

# Memory

The memory subsystem (`internal/memory`) provides persistent, git-diffable YAML storage for
project facts, user preferences, and feedback. Memory is read at context-build time and
written in response to explicit memory intents.

## Memory Stores

Chronos Code maintains three distinct YAML memory files:

| Store | File | Scope | Description |
|-------|------|-------|-------------|
| Project memory | `.chronos-code/memory/project.yaml` | Project | Facts about the codebase, conventions, decisions |
| User memory | `~/.chronos-code/memory/user.yaml` | User-global | Preferences, working style, credentials context |
| Feedback memory | `.chronos-code/memory/feedback.yaml` | Project | Accumulated feedback from learning suggestions |

All memory files are plain YAML — human-readable and git-diffable. They can be inspected and
edited directly.

## Store.Recall

`Store.Recall(ctx, query)` performs **deterministic text recall**: a keyword-based search over
the active memory stores. It returns matching memory entries ranked by relevance.

Source: `internal/memory/recall.go`

Recall characteristics:

- **Deterministic** — same query always returns the same results (no vector embeddings by default)
- **Scoped** — searches only enabled stores
- **Budget-aware** — the orchestrator may trim memory injections when context budget is tight

:::note Roadmap
Vector recall (semantic similarity search) is on the roadmap but not yet available. Current
recall is keyword/text-based only.
:::

## Memory Intents

Memory is written through **explicit intents** in the conversation. Casual uses of "remember",
"always", or "never" do **not** persist unless phrased as a recognized intent.

Supported intent patterns:

```text
remember <category>: <fact>      # write a fact to memory
forget: <mem_ID>                 # remove a memory entry by ID
recall-past: <query>             # search memory explicitly
```

Examples:

```text
remember project: Use table-driven tests for all Go tests
remember user: I prefer concise explanations without preamble
forget: mem_a1b2c3d4
```

## Session Summaries

When `session.recall_prior_summaries: true` (default), the orchestrator injects summaries of
prior sessions into the context at the start of each turn. This provides continuity across
sessions without replaying the full transcript.

Session summaries are generated at the end of each session and stored in the session database
(SQLite). The `/compact` command triggers early summarization of the current session's history.

## Learned Patterns

The learning subsystem (`internal/learning`) emits **pending suggestions** (YAML files in
`.chronos-code/learned/`) based on patterns observed across sessions.

Patterns are injected into context when:
- `learning.enabled: true` (default)
- `learning.pattern_injection: true` (default)
- The pattern has been **accepted** via `chronos-code learn accept`

```bash
chronos-code learn list          # list pending suggestions
chronos-code learn show <id>     # show a suggestion
chronos-code learn accept <id>   # accept and activate a pattern
chronos-code learn reject <id>   # reject and discard
```

:::caution Auto-distillation is off
`learning.auto_distill: false` by default. Patterns are **never** automatically applied.
Human review via `learn accept` is always required.
:::

## Disabling Memory

```yaml
# .chronos-code/config.yaml
memory:
  enabled: false          # stops all persist and recall
```

```yaml
learning:
  pattern_injection: false  # stop injecting approved patterns
  enabled: false            # stop emitting new suggestions
```

Existing YAML files are preserved on disk when memory is disabled. See
[Rollback Controls](../rollback) for per-subsystem disable toggles.

## CLI Commands

```bash
chronos-code memory list            # list all memory entries
chronos-code memory search <query>  # search memory
chronos-code memory forget <mem_ID> # remove a memory entry
```

## See Also

- [Configuration — Memory section](../configuration#memory)
- [Rollback Controls](../rollback)
