---
sidebar_position: 3
title: Using Chronos Code
description: The day-to-day workflow in the interactive app, including plan mode, approvals, sessions and commands.
---

# Using Chronos Code

Run `chronos-code` in your project folder to open the interactive app. This page covers everything you need for daily work.

## Asking for work

Write requests the way you'd brief a colleague: say what you want, where, and how you'll know it's done.

```text
The /orders endpoint returns 500 when the cart is empty. Find out why, fix it,
and add a regression test. Run the order tests when you're done.
```

Chronos Code explores the code, makes changes, runs checks, and summarizes what it did. For bigger or riskier work, use [plan mode](#plan-mode).

### Point at files with `@`

Mention a file anywhere in your message to attach it:

```text
Why does @internal/config/load.go ignore the user config on Windows?
```

You can attach up to 8 files per message, with paths relative to your project. Text is also extracted from `.pdf` and `.docx` files. Large pastes are collapsed into a short placeholder so your input stays readable.

### Call a specialist

Start your message with `@` and an agent name to hand the conversation to that specialist:

```text
@reviewer look at my last commit for bugs and security issues
@debugger TestCheckout fails intermittently, find out why
@explainer walk me through how caching works here
```

The specialist **stays active** for the following messages. Switch back with `@chronos-code …`, `/agent chronos-code`, or pick an agent with `Ctrl+A`. Run `/agents` to list all agents, including your own [custom agents](./agents-and-skills).

You don't have to name specialists. The main agent delegates to them on its own when it makes sense.

## Plan mode

Plan mode separates thinking from doing. Use it for anything that touches several files, changes behavior, or that you'd want to review before it happens.

1. Turn it on with `/plan on`. The status bar shows `plan`. To start in plan mode, launch with `chronos-code --plan-mode`.
2. Describe the task. Chronos Code investigates using read-only tools. It can't edit files or run commands in this phase.
3. When it has a plan, you'll see it in the conversation with a review prompt:

   | Key | What happens |
   |---|---|
   | `y` | Approve. Plan mode turns off and implementation starts right away. |
   | `a` | Approve, and also allow file edits without asking for the rest of the session |
   | `n` | Keep planning. Type your feedback and it revises the plan. |

4. After approval, Chronos Code implements the plan step by step. You can still approve or deny individual commands as usual.

If you ask a plain question in plan mode, it just answers. It only asks for approval when it has a plan to implement. Turn plan mode off manually at any time with `/plan off`. Run `/plan` to see the current state.

:::tip
Plan mode works well together with `@planner` or `@architect` when you want a second opinion on the approach before anything is written.
:::

## Approvals

By default, Chronos Code reads freely but asks before it edits files, runs most shell commands, or uses external (MCP) tools.

| Key | Meaning |
|---|---|
| `y` / `Enter` | Allow once |
| `a` | Allow this tool for the rest of the session |
| `A` | Allow everything for the rest of the session |
| `n` / `Esc` | Deny. The agent is told and adapts. |

Common safe commands (for example `go test`) run without asking. Dangerous ones (for example `sudo` or `rm -rf /`) are always refused. See [Permissions and Safety](./security) to adjust this for your project.

## While a task is running

You don't have to wait for a task to finish:

| Action | Key |
|---|---|
| Add information to the running task (it picks it up after its current step) | Type and press `Enter` |
| Queue a follow-up message for after the task finishes | `Alt+Enter` |
| Stop the task | `Ctrl+C` |

## Seeing what it did

Tool calls appear as short lines under each reply. Each line shows the command, file, or search pattern and whether it succeeded. Below it you see a short preview of the real output, such as the last lines of a test run or the matching lines of a search. A command that fails shows `✗` and its exit code.

| To see… | Use |
|---|---|
| The full output of every tool call | `Ctrl+O` (press again to collapse) |
| A scrollable view of everything from the last turn | `/inspect` (`/inspect changes` shows only edits) |
| The current changes in your working tree | `/diff` |

Every edit is shown as a diff. To undo the last file edit, run `/rewind`.

## Running your own commands

Prefix a line with `!` to run a shell command yourself without asking the agent:

```text
!git status
!make test
```

The output appears in the conversation. The same safety policy applies.

## Sessions

Every conversation is saved automatically.

| Command | What it does |
|---|---|
| `/resume` | Continue your previous conversation (`/resume <id>` for a specific one) |
| `chronos-code --resume <id>` | Start the app in a specific session |
| `/session list` | Show recent sessions |
| `/clear` | Start a fresh session |
| `/compact` | Summarize the conversation so far to make later turns faster and cheaper |

Long tasks keep going on their own as long as they make progress. If a task stalls, it pauses and asks you how to continue. `/task resume` or `/task retry` picks it up again.

## Memory

Save facts you want Chronos Code to remember in future sessions by starting a one-line message with `remember:`:

```text
remember project: we use sqlc for all database access, never raw SQL strings
remember: I prefer table-driven tests
```

Use `remember project:` for facts about the codebase, `remember user:` for your personal preferences, and plain `remember:` for general feedback. `recall-past: <topic>` searches what was saved. `/memory` shows the memory state. Memory is stored as readable YAML files in `.chronos-code/memory/`.

## Models, thinking and cost

| Command | What it does |
|---|---|
| `/model` | Show or switch the model (press `Tab` to autocomplete, or `Ctrl+M` to open a picker) |
| `/think low\|medium\|high\|off` | Let the model think longer on hard problems |
| `/usage` | Tokens and cost for this session |
| `/budget` | Remaining budget |
| `/context` | What is being sent to the model, and how much space each part takes |
| `/status` | Current agent, model, mode and settings at a glance |

To cap spending for a session, start with `chronos-code --budget 5` (US dollars).

## Copying

| What | How |
|---|---|
| Last reply | `/copy`, `Ctrl+Y`, or `Ctrl+Shift+C` |
| Last code block | `/copy code` or `Ctrl+Shift+X` |
| Everything on screen | `/copy visible` (`/copy all` for the whole conversation) |
| Select text with the mouse | Hold `Shift` while dragging |

## Command reference

| Command | Description |
|---|---|
| `/help` | Show commands and shortcuts |
| `/plan [on\|off]` | Plan mode: plan first, approve, then implement |
| `/agent [name]` · `/agents` | Show or switch the active agent · list agents |
| `/model [provider] [model]` | Show or switch the model |
| `/think off\|low\|medium\|high` | Set the thinking level |
| `/login` · `/logout` · `/whoami` | Manage provider credentials |
| `/resume` · `/session` · `/clear` · `/compact` | Session management |
| `/rewind` | Undo the last file edit |
| `/diff` · `/inspect` | Review changes and tool details |
| `/context` · `/usage` · `/budget` · `/status` · `/workspace` | Inspect cost, context and state |
| `/memory` · `/skills` · `/mcp` | Memory, skills and external tool servers |
| `/learn` | Review suggestions Chronos Code has learned from your sessions |
| `/<skill-name> <task>` | Run a task with a specific [skill](./agents-and-skills#skills) |
| `/copy` · `/mouse` · `/stream` | Copy, toggle mouse scrolling, toggle streaming |
| `/quit` | Exit |

## Keyboard shortcuts

| Key | Action |
|---|---|
| `Enter` | Send (while busy: add to the running task) |
| `Alt+Enter` · `Ctrl+J` | New line (while busy, `Alt+Enter` queues a follow-up) |
| `Ctrl+C` | Stop the task, or exit when idle |
| `↑` / `↓` | Message history |
| `Ctrl+R` | Search history |
| `Tab` | Accept a completion |
| `Ctrl+/` | Command palette |
| `Ctrl+A` | Agent picker |
| `Ctrl+L` | Log in |
| `Ctrl+O` | Expand or collapse tool output |
| `Page Up` / `Page Down` | Scroll |
| `Ctrl+End` | Jump back to live output |
