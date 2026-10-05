---
sidebar_position: 5
title: Headless Integration
description: Run chronos-code as an agent runtime for a daemon or orchestrator, with stream-json events, MCP config, exit codes and state files.
---

# Headless Integration

This page is for programs that launch `chronos-code run` and read its output, such as a task daemon. For everyday scripting see [Headless and Automation](headless.md).

Typical command:

```bash
echo "$PROMPT" | chronos-code run \
  --output-format stream-json \
  --dangerously-skip-permissions \
  --model anthropic/claude-sonnet-4-5 \
  --system-prompt-file system.md \
  --strict-mcp-config --mcp-config mcp.json \
  --max-turns 40 --ephemeral --prompt-stdin
```

Check what a binary supports before relying on a flag:

```bash
chronos-code version --json
# {"version":"v0.8.0","commit":"…","capabilities":["stream-json","mcp-http","mcp-config","prompt-stdin","system-prompt","max-turns","thinking","ephemeral"], …}
```

## Flags

| Flag | Meaning |
|---|---|
| `--output-format text\|json\|stream-json` | `stream-json` writes one JSON event per line on stdout. `json` writes one final envelope. |
| `--prompt-stdin` | Read the whole prompt from stdin (no argv size limit). |
| `--system-prompt <text>`, `--system-prompt-file <path>` | Appended to the primary agent's system prompt. The built-in security text is kept. |
| `--max-turns <n>` | Hard cap on model turns. Reaching it ends the run with error `max_turns`, exit 1. |
| `--model <id>` | A model id, or `<provider>/<model>` such as `azure/my-deployment` or `anthropic/claude-sonnet-4-5`. An explicit model turns the built-in router off. |
| `--provider <name>` | Provider, when the model id has no prefix. |
| `--thinking off\|low\|medium\|high` | Reasoning effort. Other values fail with `invalid_request`. |
| `--mcp-config <path>` | MCP servers for this run, repeatable. Servers listed here are trusted without approval. |
| `--strict-mcp-config` | Ignore `.mcp.json` and user-scope MCP files. Use it so a target repository's own MCP file is never loaded. |
| `--resume <session-id>` | Continue a session, even when the working directory changed (see below). |
| `--ephemeral` | Same as `CHRONOS_CODE_EPHEMERAL=1`: no cross-project memory, learning or telemetry writes. |
| `--dangerously-skip-permissions` | Run every tool call that would ask. Policy blocks and paths outside the workspace are still refused. Use only in a disposable environment. |

`run` never asks a question on stdin. Anything that would ask is refused, or the run ends with an error event.

An unknown `--flag` after `run` fails with `invalid_request` (exit 2). To send a message that starts with `--`, put it after `--` or use `--prompt-stdin`. The default input guardrail accepts prompts up to 1,000,000 characters.

## Event stream

Every line is a JSON object:

```json
{"schema_version":"chronos.execution.v1","sequence":2,"timestamp":"2026-10-05T18:10:14Z","type":"tool","task_id":"","payload":{ … }}
```

`sequence` starts at 1 and increases by one. The last line is always `completion` or `error`. Nothing follows it.

| `type` | `payload` |
|---|---|
| `session` | `session_id`, `model`, `provider`, `cwd`. Always first. |
| `content` | `content`, `delta`. Assistant text. |
| `thinking` | Reasoning text, when `--thinking` is on and the model streams it. |
| `tool` | `id`, `name`, `arguments` (JSON string), `input` (object). |
| `tool_result` | `id`, `name`, `output`, `is_error`. |
| `subagent` | A delegated agent started or finished. |
| `retry` | A provider call is being retried. |
| `usage` | `request_id`, `model`, `prompt_tokens`, `completion_tokens`, `cache_read_tokens`, `cache_creation_tokens`. |
| `verification` | Result of a verification step. |
| `completion` | Terminal. The full execution envelope: `status`, `stop_reason`, `content`, usage totals and `usage_by_model`. |
| `error` | Terminal. `code`, `category`, `retryable`, `message`, `status`, `stop_reason`, `session_id`. |

Example stream (trimmed):

```
{"type":"session","payload":{"session_id":"sess_ca5f66e795c08d11","model":"claude-sonnet-4-5","provider":"anthropic","cwd":"/work"}}
{"type":"tool","payload":{"id":"tu1","name":"mcp__fixture__echo","arguments":"{\"text\":\"hi\"}","input":{"text":"hi"}}}
{"type":"tool_result","payload":{"id":"tu1","name":"mcp__fixture__echo","output":"echo: hi","is_error":false}}
{"type":"content","payload":{"content":"DONE","delta":true}}
{"type":"usage","payload":{"request_id":"req-1","model":"claude-sonnet-4-5","prompt_tokens":20,"completion_tokens":7,"cache_read_tokens":0,"cache_creation_tokens":0}}
{"type":"completion","payload":{"status":"succeeded","stop_reason":"success", …}}
```

## Exit codes

| Code | Status | Meaning |
|---|---|---|
| 0 | `succeeded` | Done. |
| 1 | `failed` | Failed, including error `max_turns`. |
| 2 | `invalid_request` | Bad flag or input, such as an unknown `--thinking` value. |
| 3 | `approval_blocked` | A tool needed approval and none could be given. |
| 4 | `retryable_provider_error` | Provider error worth retrying. |
| 5 | `timed_out` | Timeout. |
| 6 | `budget_exhausted` | Spending cap reached. |
| 7 | `verification_failed` | Verification did not pass. |
| 8 | `session_not_found` | `--resume` named an unknown session. Retry without `--resume`. |
| 130 | `cancelled` | SIGINT or SIGTERM. |

On SIGINT or SIGTERM the run stops the provider stream and tools, kills child processes (shell commands and stdio MCP servers) as a process group, saves the session, writes an `error` event with code `cancelled`, and exits 130. It forces exit if shutdown takes longer than 10 seconds.

## MCP

`--mcp-config` takes the Claude Code shape:

```json
{
  "mcpServers": {
    "state-memory": {
      "type": "http",
      "url": "http://state-mcp.ns.svc.cluster.local/mcp",
      "headers": { "Authorization": "Bearer ${STATE_MCP_TOKEN}" }
    },
    "files": { "command": "npx", "args": ["-y", "some-mcp-server"] }
  }
}
```

- Transports: `stdio`, `sse` and `http` (streamable HTTP, also written `streamable-http`).
- `${VAR}` and `${VAR:-default}` in `url` and `headers` are read from the environment when the client starts. A missing variable with no default fails the connection and names only the variable. Header values are never printed.
- In `.mcp.json` and user-scope files, a header whose name looks like a credential (`Authorization`, `*-Token`, `*-Key`) must contain a `${VAR}` reference. In `--mcp-config` files the caller owns the file, so literal values are accepted. They are never printed.
- Plain `http://` is allowed only for loopback, `*.cluster.local`, and hosts listed in `mcp.allowed_insecure_hosts` in `security.yaml`. Everything else must use `https://`. An overlay may only narrow that list.
- A server name defined in two `--mcp-config` files is an error. So is a server with an invalid shape (for example a public `http://` URL) or a name in `mcp.denied_servers`: the run ends before it starts, with `invalid_request` and exit 2.
- Servers from `--mcp-config` connect without approval. Their tools still count as external tools, so pass `--dangerously-skip-permissions` for unattended runs.
- Tool names are `mcp__<server>__<tool>`, with any character other than letters, digits, `_` and `-` replaced by `_`. For example, tool `echo` on server `fixture` is `mcp__fixture__echo`.

## Resume and isolation

`--resume <id>` looks for the session in the current project's database, then in every other project under the same data home, and continues in the current working directory. If no database has it, the stream ends with `error` code `session_not_found` and exit 8. No new session is started.

`--ephemeral` skips the shared user memory database, learning suggestions and telemetry. Sessions are still saved so `--resume` works.

## Environment

| Variable | Use |
|---|---|
| `CHRONOS_CODE_DATA_HOME` | Root for all state. Use a different value per workspace or tenant. |
| `CHRONOS_CODE_EPHEMERAL=1` | Same as `--ephemeral`. |
| `CHRONOS_CODE_PROVIDER`, `CHRONOS_CODE_MODEL` | Default provider and model when the flags are absent. |
| `ANTHROPIC_API_KEY`, `AZURE_OPENAI_*`, `OPENAI_API_KEY` | Provider credentials. |

`chronos-code models --json` lists models per provider.

## State files

Under `$CHRONOS_CODE_DATA_HOME/projects/<name>-<hash>/` (default `~/.chronos-code/`):

| File | Written when |
|---|---|
| `sessions.db` | Always. Needed for `--resume`. |
| `plans.db`, `project.json`, `index/` | Always (plans, project marker, code index). |
| `telemetry.db` | Not with `--ephemeral`. |

`$CHRONOS_CODE_DATA_HOME/memory.db` holds user and organization memory and is not touched with `--ephemeral`.
