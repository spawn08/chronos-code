---
sidebar_position: 5
title: Configuration
description: Where Chronos Code settings live, and the settings you are most likely to change.
---

# Configuration

Chronos Code works without any configuration. When you want to change something, you edit plain YAML files. This page explains where they live and covers the settings most people change.

## Where settings live

| Location | Use it for | Commit it? |
|---|---|---|
| `~/.chronos-code/config.yaml` | Your personal defaults for every project (preferred model, thinking level) | No, it's yours |
| `<project>/.chronos-code/config.yaml` | Settings for one repository that the whole team should share | Yes |
| `-c path/to/file.yaml` | A one-off overlay for a single run (for example a CI profile) | Up to you |
| Command-line flags and environment variables | Temporary overrides (`--model`, `--budget`, `CHRONOS_CODE_MODEL`) | — |

Later layers win: built-in defaults, then your user file, then the project file, then `-c`, then flags. You only need to write the settings you want to change. Everything else keeps its default.

Run `chronos-code init` in a project to get an editable copy of all default files in `.chronos-code/`. Besides `config.yaml`, that folder can contain:

| File or folder | Purpose | Details |
|---|---|---|
| `agents/*.yaml` | Custom agents and changes to built-in agents | [Agents, Skills and Instructions](./agents-and-skills) |
| `skills/<name>/SKILL.md` | Reusable how-to guides the agent can load | [Skills](./agents-and-skills#skills) |
| `security.yaml` | Which paths and commands are allowed | [Permissions and Safety](./security) |
| `routing.yaml` | Which model handles which kind of request | [Automatic model selection](#automatic-model-selection) |
| `guardrails/default.yaml` | Input/output size limits and the session token cap | [Cost limits](#cost-limits) |
| `pricing.yaml` | Prices for models that aren't in the public catalog | [Prices](#prices) |
| `memory/` | What Chronos Code has been asked to remember | [Using Chronos Code](./using-chronos-code#memory) |

External tool servers are configured separately in `.mcp.json`. See [MCP Servers](./mcp).

:::tip Check your settings
`chronos-code config show` prints the combined result of all layers. `chronos-code config validate` checks that the files load. Misspelled keys are silently ignored, so if a setting has no effect, check its spelling against this page.
:::

## Choosing models

Out of the box, every agent uses Anthropic models: `claude-sonnet-4-6`, with `claude-haiku-4-5` for the researcher and explainer.

**Change the main agent's model.** This is the agent you talk to, so it's usually all you need:

| How | Lasts for |
|---|---|
| `/model` in the app (or `Ctrl+M`) | This session |
| `--provider openai --model gpt-4o` | This run |
| `export CHRONOS_CODE_PROVIDER=openai` and `CHRONOS_CODE_MODEL=gpt-4o` | Your shell |

`chronos-code models` lists the models available to you. `chronos-code models openai` lists them for one provider.

**Change a specialist's model, or make it permanent.** Each agent has its own `model:` block. Run `chronos-code init`, open `.chronos-code/agents/<agent>.yaml`, and edit it:

```yaml
# .chronos-code/agents/coder.yaml
model:
  provider: openai     # anthropic, openai, azure, gemini, mistral, groq, deepseek,
  model: gpt-4o        # openrouter, together, fireworks, perplexity, ollama, ...
```

Put the file in `~/.chronos-code/agents/` instead to apply it to all your projects.

:::note Using a provider other than Anthropic
The specialists keep their Anthropic models unless you change them. If you only have credentials for another provider, update the `model:` block in each agent file you use. Otherwise delegated work to that specialist will fail to authenticate.
:::

`defaults.model` in `config.yaml` only applies to agents that don't set a model of their own, such as custom agents you write without a `model:` block.

### Automatic model selection

By default, Chronos Code picks a model for each request based on how complex it looks. Simple lookups go to a small, fast model and hard debugging or design work goes to a stronger one. It only switches to a provider you have credentials for. The choices are defined in `routing.yaml` under `model_routing`.

If you'd rather always use exactly the model you configured:

- Pin it for the session with `--model`, `CHRONOS_CODE_MODEL`, or `/model`. A pinned model is never switched.
- Or turn automatic selection off completely:

  ```yaml
  router:
    enabled: false
  ```

### Gateways, proxies and self-hosted models

Send all calls for a provider through a gateway:

```yaml
providers:
  anthropic:
    base_url: https://llm-gateway.example.com/anthropic
```

Point an agent at a local or OpenAI-compatible server by editing its `model:` block:

```yaml
# .chronos-code/agents/explainer.yaml
model:
  provider: ollama
  model: qwen2.5-coder:14b
  base_url: http://localhost:11434
```

For **Azure OpenAI**, set `AZURE_OPENAI_API_KEY`, `AZURE_OPENAI_ENDPOINT`, and `AZURE_OPENAI_DEPLOYMENT` (plus `AZURE_OPENAI_API_VERSION` if needed), and use `provider: azure`.

## Credentials

Keep keys out of committed files. In order of preference:

1. **Environment variables**, such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `AZURE_OPENAI_API_KEY`, `MISTRAL_API_KEY`, `GROQ_API_KEY`, `DEEPSEEK_API_KEY`, or `OPENROUTER_API_KEY`.
2. **The system keychain**, with `chronos-code login <provider> --api-key <key>`, or `/login` in the app. `/login` can also reuse an existing Claude Code or Codex login.
3. **A reference in YAML**, when a config file must name the key: `api_key: "${MY_KEY_VARIABLE}"`.

`chronos-code whoami` shows which credential is being used. `chronos-code logout <provider>` removes a stored one.

## Thinking

Let supported models (Claude, OpenAI reasoning models) think before answering. This helps on hard problems, but uses more tokens:

```yaml
defaults:
  reasoning:
    native: true
    effort: medium       # low | medium | high
    summary: true        # show a short summary of the model's thinking in the app
```

You can also change it any time with `/think low|medium|high|off`.

## Long conversations

Chronos Code summarizes older parts of a conversation automatically when it gets close to the model's limit:

```yaml
defaults:
  context:
    max_tokens: 128000          # upper bound on how much context is used
    summarize_threshold: 0.8    # summarize when 80% full
    preserve_recent_turns: 6    # always keep the latest turns word for word
```

Run `/compact` to summarize on demand.

## Long-running tasks

Large tasks keep going for as long as they make progress. Chronos Code checks in at regular intervals, and pauses to ask you how to continue only when a stretch of work produced nothing new:

```yaml
long_running:
  mode: renew                  # "bounded" makes the limits below hard stops instead
  window:
    tool_calls: 100
    seconds: 900
  no_progress_windows: 2       # pause after this many unproductive stretches in a row
```

## Checking its own work

Chronos Code tracks what it should verify (for example, that changed code still builds and passes tests):

```yaml
verification:
  mode: report     # report: list anything unverified in the summary (default)
                   # enforce: don't report success until the checks have passed
```

## Cost limits

- **Per session, in dollars:** start with `--budget 5`. Calls stop before the budget would be exceeded, including when a model's price is unknown.
- **Per session, in tokens:** set `cost.max_tokens_per_session` in `guardrails/default.yaml` (default 500,000; you're warned at 80%).
- **Per task, in dollars:** `repair.max_cost_microdollars: 2000000` caps a single task at $2. This only works when every model involved has a known price.

Check spending any time with `/usage` and `/budget`.

### Prices

Model prices come from the public [models.dev](https://models.dev) catalog, which is refreshed automatically in the background. For private deployments or negotiated rates, add a `pricing.yaml` (USD per million tokens) to `~/.chronos-code/` or `.chronos-code/`:

```yaml
models:
  my-deployment: { input: 3, output: 15, cache_read: 0.3 }
```

Set `models_catalog.auto_refresh: false` (or `CHRONOS_CODE_DISABLE_MODELS_FETCH=1`) to stop automatic catalog downloads. `chronos-code models refresh` updates the catalog on demand.

## Sessions and memory

```yaml
session:
  auto_resume: false        # true: reopen your last conversation on start

memory:
  enabled: true             # false: don't save or recall memories
  auto_extract: true        # allow "remember: ..." messages in chat
```

Sessions are stored per project under `~/.chronos-code/projects/`. Manage them with `chronos-code session list | delete <id> | export <id> <file>`.

## Hooks

Run your own commands automatically, for example a formatter after every edit or a check before any shell command:

```yaml
hooks:
  post_tool_call:
    - name: format
      command: "make fmt"
      timeout_ms: 30000
  pre_tool_call:
    - name: audit
      command: "echo {{tool_name}} >> .chronos-code/audit.log"
      timeout_ms: 2000
  user_prompt_submit:
    - name: branch-context
      command: "git branch --show-current"
      timeout_ms: 2000
```

| Hook | Runs | If it fails |
|---|---|---|
| `pre_tool_call` | Before each tool call | The tool call is blocked |
| `post_tool_call` | After each tool call | Recorded, the task continues |
| `user_prompt_submit` | When you send a message; its output is added to your message | The turn stops |

Available placeholders: `{{tool_name}}`, `{{tool_args}}`, `{{tool_output}}`, `{{session_id}}`, `{{agent_id}}`, and `{{user_message}}`. Values are quoted safely for you.

Hooks in a **project** file run only after you trust them, so a cloned repository can't run commands on your machine without your consent. On first start, Chronos Code stops and prints a digest. Add it to your personal `~/.chronos-code/security.yaml`:

```yaml
hooks:
  trusted_digests: ["<digest printed at startup>"]
```

## Learning from your sessions

Chronos Code can suggest improvements (a new skill, a refined agent, a project pattern) based on your past sessions. Suggestions are never applied automatically. Review them with `/learn` or `chronos-code learn list | show | accept | reject`.

```yaml
learning:
  enabled: true      # false: stop creating new suggestions
```

## Housekeeping

Old sessions, logs and temporary files are cleaned up according to retention rules (by default, sessions older than 90 days). Run `chronos-code cleanup status` to see what's stored, and `chronos-code cleanup run --dry-run` to preview a cleanup.

## Environment variables

| Variable | Purpose |
|---|---|
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, … | Provider credentials |
| `CHRONOS_CODE_PROVIDER`, `CHRONOS_CODE_MODEL` | Override the main agent's model |
| `CHRONOS_CODE_DISABLE_MODELS_FETCH` | Stop automatic model catalog downloads |
| `CHRONOS_CODE_DATA_HOME` | Store sessions and runtime data somewhere other than `~/.chronos-code` |

## Global flags

| Flag | Purpose |
|---|---|
| `-c`, `--config <file>` | Add a config file on top of the others |
| `--provider <name>`, `--model <id>` | Choose the main agent's model |
| `--plan-mode` | Start in plan mode |
| `--budget <usd>` | Spending cap for the session |
| `--resume <session-id>` | Continue a previous session |
| `--yolo` | Allow file edits and allowlisted commands without asking |
| `--dangerously-skip-permissions` | Allow everything that would ask (sandboxes and CI only) |
| `--no-stream` | Show replies when complete instead of streaming |
| `--json` | Headless: print one JSON result |
| `--debug` | Verbose logging |

## A complete example

```yaml
# .chronos-code/config.yaml, shared with the team
defaults:
  reasoning:
    native: true
    effort: medium

verification:
  mode: enforce

hooks:
  post_tool_call:
    - name: format
      command: "make fmt"
      timeout_ms: 30000
```

```yaml
# ~/.chronos-code/config.yaml, personal
session:
  auto_resume: true
```
