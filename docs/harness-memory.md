# Harness reliability, memory, and hooks

## Runtime data and project identity

Default layout (`CHRONOS_CODE_DATA_HOME` overrides `~/.chronos-code`):

```text
~/.chronos-code/
  memory.db                         # layered memories, explicitly partitioned
  projects/<basename>-<128-bit-id>/
    project.json                    # canonical root and identifier
    sessions.db
    graph.db
    telemetry.db
    artifacts/                      # oversized TUI input and receipts
    checkpoints/                    # file-write undo artifacts
```

Identity derives from the canonical checkout/worktree path, including `.git`
files used by worktrees. Subdirectory and symlink launches resolve consistently.
Different worktrees/clones have different IDs. Moving a checkout changes its ID;
automatic relinking is not implemented. Explicit storage/graph paths override
defaults; relative overrides resolve from the project root.

The first runtime opening imports missing default databases from project
`.chronos-code/sessions.db`, `graph.db`, and telemetry `memory.db`. SQLite
`VACUUM INTO` snapshots include committed WAL changes, undergo integrity checks,
and publish without overwriting an existing destination. Originals are retained.
Existing old/new databases are not merged. CLI analytics can read legacy telemetry
before runtime migration. Avoid using old and new binaries as simultaneous writers
to the separate old and new databases: migration is a snapshot, not replication.

Project config, routing/security YAML, skills, agents, and curated legacy YAML
memories remain project files. Credentials retain the existing keychain storage.
Plans with an explicit `--db` keep that path.

## Memory architecture

Memory **kind** describes its meaning; **scope** describes who can retrieve it.

| Kind | Content and lifecycle |
|---|---|
| Episodic | Bounded task requests/outcomes, including failures and interruptions. Runtime Execute records episodes; recalled entries are past-event data. |
| Procedural | Explicit reusable steps with source/revision. Steps are required; procedures are recorded via `memory_remember`. |
| Semantic | Optional durable facts. Disabled by default; deterministic FTS retrieval does not require embeddings or a vector service. |
| Organizational | Explicitly published shared standards, bound to a configured organization ID. Private episodes are never automatically promoted. |
| Working context | Request-local task, tool-call/result sequence, selected excerpts and active plan. It is bounded separately from durable memory. |

Scopes are exact: `project`, `user`, `organization`, or explicit `tenant`.
Every partition also includes the storage tenant. User and organization partitions
span projects in the same shared user-data installation; no remote synchronization
service is implied. The host supplies identities; tool arguments cannot replace them.

```yaml
memory:
  enabled: true
  layered_enabled: true
  semantic_enabled: false
  organization_id: ""             # set explicitly to use organization scope
  context_budget_tokens: 4000      # conservative byte allowance <= token allowance
  max_records: 5                  # retrieval count, not a disk-retention limit
```

Layered memory is enabled by default when memory is enabled. Legacy YAML
project/user/feedback notes remain supported. `layered_enabled: false` disables
the new layer without deleting its database. Organization scope requires a
nonempty ID; each organization write also requires `publish: true`.

Agent tool example:

```json
{
  "scope": "project",
  "kind": "procedural",
  "content": "Verify a change to the Go harness",
  "steps": ["Run the focused package tests", "Run make test before delivery"],
  "source": "project testing instructions",
  "revision": "document revision or commit"
}
```

Send this to `memory_remember`. `memory_recall` accepts `scope`, optional `kind`,
`query`, `limit`, and `max_bytes`. `memory_forget` takes `scope` and `id` and
invalidates the entry. Corrections are new immutable records with their own
provenance. Expired/invalidated entries are excluded from recall. Entry content
plus procedural steps is capped at 8 KiB. Recall returns whole records within
the host's count and serialized-byte budget, including provenance.

Local administration makes no model calls:

```bash
chronos-code memory layers list --scope project
chronos-code memory layers search "testing" --scope user --kind procedural
chronos-code memory layers forget <id> --scope project
chronos-code memory layers --help
```

Original `memory list/search/forget` administer legacy YAML notes. There is no
automatic organization publication, embedding generation, or retention pruning.

## Input, context, and recovery

- TUI attachments have a visible selection receipt and a conservative aggregate
  prepared-input cap (at most 8 KiB, lower for smaller context windows).
- At most eight file excerpts are selected, each at most 1 KiB. Original files
  remain retrievable through ranged `file_read`.
- Oversized pasted requests are stored losslessly as artifacts. The model is
  instructed to retrieve the complete request before acting. Artifact retention
  is currently manual.
- Ranged reads run before result compression. An outline no longer marks an
  entire file as delivered. Range/binary/scan-limit errors are explicit.
- Model budgets use the current model, configured ceiling, actual schemas, and
  output allowance. Large tool payloads shrink before complete older turns are
  dropped; the latest task and call/result relationships are preserved.
- SQLite session summaries contain a replay boundary and preserved tail, so
  restart does not resurrect summarized turns. Full historical events remain.
- Transient recovery is bounded to the failed SDK model request. The TUI and
  orchestrator do not automatically resubmit the whole task after tool actions.
  A partially emitted stream is not replayed. `/compact` remains an explicit
  action; terminal failures never claim recovery is already underway.

`codebase_context` performs a composite graph retrieval in one call:

```json
{
  "query": "context guard",
  "include": ["definitions", "callers", "tests", "excerpts"],
  "max_tokens": 4096
}
```

It also accepts explicit `symbols` and `ranges`. Results include revisions,
source locations, freshness and omission information. The complete JSON result
has a token budget. This is bounded read-only composition, not an embedded
general-purpose scripting language.

## TUI inspection and undo

- `/session list`: searchable, asynchronously loaded session picker; Enter
  resumes; Tab inspects metadata; Esc cancels. Sessions page 100 at a time,
  up to the latest 1,000 current-agent sessions.
- `/inspect`, `/inspect context`, `/inspect changes`: scrollable snapshots of
  current/latest-turn tool, context and captured edit information. Up/Down and
  PageUp/PageDown scroll; Left/Right select fields; Ctrl+Shift+C copies; Esc closes.
- `/inspect changes` shows captured edit details, not a complete working-tree diff.
- `/rewind` (`/undo`) restores the last confirmed `file_write` checkpoint only
  when the current file hash and permissions still match. Intervening user edits
  cause a conflict and preserve the checkpoint. Same-session restart can recover
  confirmed checkpoints; unconfirmed crash records are not auto-applied.
- Checkpoints stream to disk with a 64 MiB snapshot limit and bounded metadata.
  Shell/MCP mutations are not automatically checkpointed.
- Mouse-wheel capture remains enabled by default. Shift-drag or `/copy` copies.

Inspection is bounded (1 MiB/value, 4 MiB captured tool details per turn).
Rendering retains bounded raw blocks for resize reflow and only materializes
the visible transcript tail. Historical session transcript paging is not yet
available through the inspector.

## User hooks

Chronos Code supports three YAML hook points:

```yaml
hooks:
  user_prompt_submit:
    - name: prompt-check
      command: './scripts/prompt-check.sh {{user_message}}'
      timeout_ms: 2000
  pre_tool_call:
    - name: tool-check
      command: './scripts/tool-check.sh {{tool_name}} {{tool_args}}'
      timeout_ms: 5000
  post_tool_call:
    - name: tool-audit
      command: './scripts/tool-audit.sh {{tool_name}} {{tool_output}}'
      timeout_ms: 2000
```

Supply the referenced scripts yourself. Hooks run through `/bin/sh -c` from
the canonical workspace root. Supported placeholders are `tool_name`,
`tool_args`, `tool_output`, `session_id`, `agent_id`, and `user_message`.
Tool arguments/output are JSON; substitutions are shell-word quoted by the
runner. Use only placeholders relevant to the selected hook point. This is an
argument-template interface, not a JSON-stdin protocol.

Names must be unique within each hook point, and timeouts must be 1–300,000 ms.
Failed prompt/pre-tool hooks block the operation. Post-tool failures are recorded
as activity and do not replace the tool's own result. Hook stdout/stderr are bounded.
Internal SDK hooks additionally support model/tool lifecycle, budgets, telemetry,
guardrails and context management; these are not additional YAML hook points.

Startup, late MCP tools, and final logical file/graph implementations receive the
same result-processing pipeline. Shared MCP connections preserve per-registry
policy and approval. Delegates use fresh request-local history, inherited runtime
services, bounded inputs/results, and shared finite capacity; nested requests fail
fast if waiting would deadlock an ancestor.

## Build and measurement

`make build` and `make install` retain the full tree-sitter profile. `make
build-core` produces a separate portable core binary. `make size-check-core`
enforces 40 MiB, and `make size-check-full` enforces 70 MiB. Portable release
archives use the core profile. CI reads the Go toolchain version from `go.mod`.

The offline eval now validates actual requested source or reconstructible
compressed evidence. It does not award correctness credit for a misleading
`unchanged` marker. Its reported token savings cover immediate fixture tool
responses, not complete live model trajectories.
