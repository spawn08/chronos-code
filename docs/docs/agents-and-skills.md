---
sidebar_position: 6
title: Agents, Skills and Instructions
description: Teach Chronos Code about your project with instruction files, reusable skills and custom agents.
---

# Agents, Skills and Instructions

You can shape how Chronos Code works in three ways. From simplest to most powerful:

| | What it is | Use it for |
|---|---|---|
| [Project instructions](#project-instructions) | A markdown file in your repository | House rules: how to build and test, conventions, things to avoid |
| [Skills](#skills) | Step-by-step guides the agent loads when relevant | Repeatable procedures such as "add a database migration" or "cut a release" |
| [Custom agents](#custom-agents) | A named specialist with its own instructions, tools and model | A distinct role you call often, such as `@security-auditor` or `@docs-writer` |

## Project instructions

Create an `AGENTS.md` file at the root of your repository. Chronos Code reads it at the start of every task. `CLAUDE.md`, `AGENT.md`, `.cursorrules`, and `.github/copilot-instructions.md` also work, so if your team already has one of these, you're set.

A good instruction file is short and practical:

```markdown
# Project notes

## Build and test
- Build: `make build`
- Run all tests: `make test`. Run a single package: `go test ./internal/billing/...`
- Always run `make lint` before finishing.

## Conventions
- Errors are wrapped with context: `fmt.Errorf("load invoice: %w", err)`.
- New HTTP handlers go in `internal/api/` and need a table-driven test.
- Never edit files under `gen/`; run `make generate` instead.
```

Instruction files in subfolders are picked up too. Put `services/payments/AGENTS.md` next to the code it describes, and it's added when you work from that folder. Changes are picked up immediately. `/context` shows which files were loaded.

## Skills

A skill is a markdown guide for one specific job. Chronos Code sees the name and description of every available skill, and reads the full guide only when a task calls for it. You can have many skills without slowing down every conversation.

Create a folder with a `SKILL.md` file:

```text
.chronos-code/skills/
└── add-migration/
    └── SKILL.md
```

```markdown
---
name: add-migration
description: Create and apply a new database migration safely
---

1. Run `make migration name=<short_name>` to create the files.
2. Write both the up and the down migration.
3. Run `make migrate-test` and confirm the down migration restores the schema.
4. Never modify a migration that has already been merged.
```

To use a skill:

- **Let Chronos Code decide.** It loads a skill when the task matches the description, so make the description say *when* to use it.
- **Ask for it explicitly** in the app with `/add-migration create a table for invoices`.
- List skills with `/skills` or `chronos-code skills list`. Show one with `chronos-code skills show <name>`.

Skills are also found in the folders other tools use (`.claude/skills`, `.agents/skills`, `.cursor/skills`, `.github/skills`, and others), both in your project and in your home folder. Existing skills keep working. Put personal skills in `~/.chronos-code/skills/`.

## Custom agents

Add an agent by creating a YAML file in `.chronos-code/agents/` (for the project) or `~/.chronos-code/agents/` (for you). Use one file per agent:

```yaml
# .chronos-code/agents/docs-writer.yaml
id: docs-writer
name: Docs Writer
description: Writes and updates user documentation
model:
  provider: anthropic
  model: claude-sonnet-4-6
system_prompt: |
  You write clear, friendly user documentation. Focus on what users can do
  and how to do it. Avoid implementation details.
instructions:
  - Keep pages short and task-oriented, with examples.
  - Update the docs sidebar when you add a page.
tools:
  - name: file_read
  - name: file_list
  - name: file_glob
  - name: file_grep
  - name: file_write
```

Then use it:

```text
@docs-writer document the new --plan-mode flag
```

**Tools you can give an agent:**

| Tool | Allows |
|---|---|
| `file_read`, `file_list`, `file_glob`, `file_grep` | Reading and searching (always safe) |
| `file_write` | Creating and editing files |
| `shell` | Running commands |

Leave out `file_write` and `shell` for read-only roles such as reviewers or advisors. Whether a tool needs your approval is decided by your [permission settings](./security), not by the agent file. You can set `permission: deny` on a tool to take it away from an agent.

To let an agent delegate work to other agents, list them under `sub_agents:`.

### Changing a built-in agent

Create a file with the same `id` as a built-in agent, for example `coder`. It **replaces** that agent's definition, so start from a copy of the original:

1. Run `chronos-code init`. This writes every built-in agent to `.chronos-code/agents/`.
2. Edit the one you want to change: its model, instructions, or tools.
3. Delete the agent files you didn't change, so future improvements to the built-in versions still reach you.

If your replacement leaves out `system_prompt`, the built-in prompt is kept.

## Teams

A team runs several agents on one task in a fixed pattern: one after another, in parallel, or with a coordinator. Define teams in `config.yaml`:

```yaml
teams:
  - id: review-panel
    name: Review panel
    strategy: parallel          # sequential, parallel, router, coordinator
    agents: [reviewer, tester, architect]
```

Run a team with `chronos-code team run review-panel "review the changes on this branch"`. List teams with `chronos-code team list`.

## Which one should I use?

- Start with **`AGENTS.md`**. Most teams get the biggest improvement from a short, accurate instruction file.
- Add a **skill** when you notice yourself explaining the same procedure twice.
- Create a **custom agent** only when you want a distinct persona with its own tools or model that you'll call by name.
