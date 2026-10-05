---
sidebar_position: 4
title: Headless and Automation
description: Run Chronos Code from scripts and CI with chronos-code run, JSON output, plan mode and permission flags.
---

# Headless and Automation

`chronos-code run` performs one task without the interactive app and then exits. Use it in scripts, git hooks, and CI pipelines.

```bash
chronos-code run "add a CHANGELOG entry for the new retry option"
```

The reply streams to your terminal. Tool calls are shown as short readable lines, for example `> shell  go test ./...`. Token usage is printed at the end.

Start the message with `@agent` to use a specialist:

```bash
chronos-code run "@reviewer review the changes on this branch against main"
```

Start it with `/skill-name` to load a skill in full before the task, like the interactive app does (`chronos-code skills list` shows the names):

```bash
chronos-code run "/code-review the changes on this branch against main"
```

## MCP servers when nobody is watching

Servers found in `.mcp.json`, `~/.chronos-code/mcp.json` and the other discovery files wait for approval, and in the interactive app you approve them with `/mcp connect <name>`. Headless runs can't ask, so name the servers you trust with `--mcp-connect`:

```bash
chronos-code run --mcp-connect arxiv "find recent papers on speculative decoding"
```

The approval lasts for this run only. Calls to the server's tools still need approval, so add `--yolo` or `--dangerously-skip-permissions` if the run should use them unattended. Without `--mcp-connect`, unapproved servers stay disconnected and a warning names them.

## Permissions when nobody is watching

In headless mode there is nobody to press `y`. Anything that would normally ask for approval is **refused** by default. The agent is told and continues without it. Reading and searching code always works.

Choose how much to allow:

| Flag | What runs without asking | Use it for |
|---|---|---|
| *(none)* | Only reading, searching and commands the policy already auto-allows | Reviews, explanations, reports |
| `--yolo` | Also file edits and simple commands from the shell allowlist | Everyday automation in a trusted repository |
| `--dangerously-skip-permissions` | **Everything** that would ask: any shell command, edits, external (MCP) tools | Disposable environments only: containers, CI runners, sandboxes |

```bash
# Let it edit and run allowlisted tools
chronos-code run --yolo "fix the failing lint errors"

# Fully unattended inside a throwaway CI container
chronos-code run --dangerously-skip-permissions "upgrade the dependencies and make the tests pass"
```

Even with `--dangerously-skip-permissions`, some things are always refused: commands on the never-allow list (like `sudo` and `rm -rf /`), and access to protected paths or files outside your project. Chronos Code prints a warning when this flag is on.

:::danger
`--dangerously-skip-permissions` lets the agent run arbitrary commands with your user's permissions. Only use it where a mistake can't hurt you: an isolated container or a disposable CI checkout without production credentials.
:::

## Plan first, then implement

Add `--plan-mode` to make the agent plan before it changes anything. Headless runs approve the plan automatically. The plan is printed, and implementation starts right after it.

```bash
chronos-code run --plan-mode --yolo "split the payments module into smaller files"
```

This tends to give more careful results on larger tasks, at the cost of one extra model round.

## JSON output for pipelines

Add `--json` to get exactly one JSON document on stdout instead of streamed text:

```bash
chronos-code run --json "summarize the open TODOs in this repository" > result.json
```

The most useful fields:

| Field | Meaning |
|---|---|
| `status` | `succeeded`, `failed`, `approval_blocked`, `timed_out`, `budget_exhausted`, `verification_failed`, `retryable_provider_error`, `invalid_request`, `cancelled` |
| `content` | The agent's final reply |
| `changed_paths` | Files the task changed |
| `usage`, `cost_microdollars` | Tokens used and cost (1,000,000 microdollars = $1) |
| `error.message` | What went wrong, when `status` isn't `succeeded` |
| `session_id` | Pass to `--resume` to continue the same conversation |

With `--plan-mode`, the JSON describes the implementation run.

### Exit codes

With `--json`, the exit code tells your pipeline what happened without parsing the output:

| Code | Meaning | Typical reaction |
|---|---|---|
| `0` | Succeeded | Continue |
| `1` | Failed or cancelled | Fail the job |
| `2` | Invalid request | Fix the command |
| `3` | Blocked waiting for an approval | Grant permissions (see above) or narrow the task |
| `4` | Provider error that is worth retrying | Retry later |
| `5` | Timed out | Retry or split the task |
| `6` | Budget exhausted | Raise `--budget` or narrow the task |
| `7` | Verification failed | Inspect the result |

## Useful flags

| Flag | Purpose |
|---|---|
| `--model <id>` / `--provider <name>` | Use a specific model for this run |
| `--budget <usd>` | Stop before spending more than this many US dollars |
| `--resume <session-id>` | Continue an earlier session |
| `--no-stream` | Print the reply at the end instead of streaming it |
| `-c <file>` | Use an extra config file for this run |

## CI example

```yaml
# .github/workflows/ai-review.yml
jobs:
  review:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - run: curl -fsSL https://raw.githubusercontent.com/spawn08/chronos-code/main/scripts/install.sh | bash
      - run: |
          ~/.local/bin/chronos-code run --json --budget 2 \
            "@reviewer review the diff between origin/main and HEAD; list concrete problems only" \
            > review.json
        env:
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
      - uses: actions/upload-artifact@v4
        with: { name: review, path: review.json }
```

Tips for CI:

- Give each job a fresh checkout and a job timeout.
- Start read-only (no permission flags) and add `--yolo` or `--dangerously-skip-permissions` only for jobs that must change files.
- Always set `--budget`.
- Store API keys in your CI secret store, never in committed files.
