---
sidebar_position: 2
title: Getting Started
description: Install Chronos Code, connect a model provider, and run your first task.
---

# Getting Started

This page takes you from nothing to a working session in a few minutes.

## 1. Install

**macOS / Linux**

```bash
curl -fsSL https://raw.githubusercontent.com/spawn08/chronos-code/main/scripts/install.sh | bash
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/spawn08/chronos-code/main/scripts/install.ps1 | iex
```

The installer puts `chronos-code` in `~/.local/bin`. If it warns that this folder isn't on your `PATH`, add it. To install a specific release, set `VERSION` (for example `VERSION=v0.7.0`). To install somewhere else, set `INSTALL_DIR`.

Check that it works:

```bash
chronos-code version
```

## 2. Connect a model provider

Chronos Code uses Anthropic (Claude) models by default. Pick **one** of these ways to give it access:

**Use an environment variable** (simplest):

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
```

**Store a key** in your system keychain, so you don't have to export it every time:

```bash
chronos-code login anthropic --api-key sk-ant-...
```

**Reuse an existing login.** If you already use Claude Code or the Codex CLI, open the app and run `/login`. It offers to reuse that login.

Other providers work the same way: OpenAI, Azure OpenAI, Google Gemini, Mistral, Groq, DeepSeek, OpenRouter, Together, Fireworks, Perplexity, and local Ollama. For example:

```bash
export OPENAI_API_KEY="sk-..."
chronos-code --provider openai
```

If you have credentials for exactly one provider, Chronos Code picks it automatically. Run `chronos-code whoami` to see which credential is in use. See [Choosing models](./configuration#choosing-models) to make your choice permanent.

## 3. Start a session

Open a terminal in your project folder and run:

```bash
chronos-code
```

Then type what you want, for example:

```text
Add input validation to the signup handler and a test for it
```

Chronos Code explores the code, explains what it plans to do, and asks before editing files or running commands:

| Key | Meaning |
|---|---|
| `y` / `Enter` | Allow this once |
| `a` | Always allow this tool for the rest of the session |
| `A` | Allow everything for the rest of the session |
| `n` / `Esc` | Deny |

Press `Ctrl+C` to stop the current task. Press it again when idle, or type `/quit`, to exit.

## 4. Try a few things

```text
@explainer how does authentication work in this project?
@reviewer review my uncommitted changes
/plan on
Refactor the config loader to support environment overrides
```

The last two lines use **plan mode**: Chronos Code investigates, shows you a plan, and starts implementing only after you approve it. See [Using Chronos Code](./using-chronos-code) for the full workflow.

## 5. (Optional) Add project settings

Chronos Code works without any setup. When you want to customize it for a project, run:

```bash
chronos-code init
```

This creates a `.chronos-code/` folder with editable copies of the default settings, agents, and safety policy. It trims the shell command allowlist to the tools your project uses. Commit the folder to share the setup with your team. See [Configuration](./configuration).

It also helps to add an `AGENTS.md` (or `CLAUDE.md`) file at the root of your repository that describes your conventions: how to build, how to test, coding style. Chronos Code reads it automatically at the start of every task. See [Project instructions](./agents-and-skills#project-instructions).

## Run a single task and exit

```bash
chronos-code run "update the README with the new install steps"
```

See [Headless and Automation](./headless) for scripting and CI use.

## Troubleshooting

| Problem | What to do |
|---|---|
| "no credential" or authentication errors | Run `chronos-code whoami`, then set an API key or use `/login` |
| A command keeps asking for approval | Press `a` to allow it for the session, or see [Permissions and Safety](./security) |
| Startup fails with a `security.yaml` error | Your project policy tries to allow more than the built-in policy. See [What you can and can't change](./security#what-you-can-and-cant-change) |
| The model is wrong or unavailable | Run `/model` in the app, or see [Choosing models](./configuration#choosing-models) |
| Sessions get slow or expensive | Run `/compact`. See [Best Practices](./best-practices#keep-long-sessions-healthy) |
