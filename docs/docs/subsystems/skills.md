---
sidebar_position: 3
title: Skills
description: Isolated discovery, capability validation, and selection decisions
---

# Skills

Chronos Code discovers `SKILL.md` files from project, provider, user, and plugin directories. A malformed, oversized, unreadable, or non-regular file produces a diagnostic and is skipped; valid sibling skills remain available. A directory contributes at most 256 skill entries and each `SKILL.md` is limited to 256 KiB.

## Authoring Contract

```markdown
---
name: focused-review
description: Review a focused change for correctness.
version: 1.0.0
triggers: [review, correctness]
model_hint: sonnet
tools_required: [file_read, file_grep]
---
# Focused Review

Inspect the changed code and report concrete correctness defects.
```

The parser fixture for this example is `internal/skills/testdata/review/SKILL.md`.

- `name` is required and cannot contain control characters or angle brackets.
- `triggers` is limited to 64 entries.
- `tools_required` is limited to 64 exact runtime tool names.
- `model_hint`, when present, must match the selected agent model ID.
- Unknown frontmatter fields are rejected to catch misspellings.

## Selection

BM25 ranking produces a `SelectionResult` with separate `Decisions`, `Selected`, and `Context` fields. Each decision exposes the numeric score, acceptance state, and rejection reason. Rejection reasons, diagnostics, paths, and scores are not rendered into model context.

Before injection, each candidate is checked against the target agent's live tool registry and model ID. Missing tools, denied tools, model mismatches, top-K overflow, and token-budget overflow reject only that candidate. Explicitly requested skills pass through the same capability gate.
