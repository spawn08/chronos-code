---
slug: /
sidebar_position: 1
title: Introduction
description: Chronos Code is an AI coding agent for your terminal, configured with plain files you can commit.
---

# Chronos Code

[![CI](https://github.com/spawn08/chronos-code/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/spawn08/chronos-code/actions/workflows/ci.yml)
[![Latest release](https://img.shields.io/github/v/release/spawn08/chronos-code?sort=semver)](https://github.com/spawn08/chronos-code/releases/latest)

**Chronos Code** is an AI coding agent that runs in your terminal. You describe what you want: fix a bug, add a feature, explain some code, review a change. It reads your project, proposes or makes the changes, runs your tests, and reports back. You approve anything risky before it happens.

It ships as a single binary. Everything about how it behaves lives in plain YAML files that you can read, edit, and commit with your code.

## What you can do with it

- **Build and fix things.** Ask for a change in plain language. Chronos Code finds the relevant code, edits it, and runs your build and tests to check the result.
- **Plan before you change anything.** Turn on [plan mode](./using-chronos-code#plan-mode) to get a reviewed plan first. Once you approve it, Chronos Code implements it.
- **Call in a specialist.** Mention `@reviewer`, `@debugger`, `@planner`, `@architect`, `@explainer`, `@researcher`, or `@tester` when you know what kind of help you need.
- **Automate.** Run one-off tasks from scripts and CI with `chronos-code run`, and get a structured JSON result.
- **Keep your team on the same page.** Commit your agents, skills, instructions, and safety rules to the repository so everyone gets the same setup.

## Where to go next

| If you want to… | Read |
|---|---|
| Install it and run your first task | [Getting Started](./getting-started) |
| Learn the day-to-day workflow | [Using Chronos Code](./using-chronos-code) |
| Run it from scripts or CI | [Headless and Automation](./headless) |
| Change models, limits, and behavior | [Configuration](./configuration) |
| Add your own agents, skills, and project instructions | [Agents, Skills and Instructions](./agents-and-skills) |
| Control what it may do without asking | [Permissions and Safety](./security) |
| Connect external tools | [MCP Servers](./mcp) |
| Get better results | [Best Practices](./best-practices) |

## Built-in agents

You always talk to the main **Chronos Code** agent. It hands focused work to specialists when that helps, and you can also call a specialist directly with `@name`.

| Agent | Best for |
|---|---|
| `chronos-code` | Your main partner. It understands the request, does the work or delegates it, and sums up the result. |
| `coder` | Implementing changes and iterating until tests pass |
| `planner` | Breaking a large task into steps (read-only) |
| `architect` | Design and structure decisions (read-only) |
| `reviewer` | Finding bugs, security issues, and style problems in a change |
| `debugger` | Tracking down the cause of a failure from errors and logs |
| `tester` | Writing and running tests |
| `researcher` | Fast, read-only searches through the codebase |
| `explainer` | Explaining code and concepts in plain language |
