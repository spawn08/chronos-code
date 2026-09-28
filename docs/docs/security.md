---
sidebar_position: 7
title: Permissions and Safety
description: Control what Chronos Code may do without asking, and how your project and credentials stay protected.
---

# Permissions and Safety

Chronos Code can read your code freely, but it asks before it changes things. This page explains what it asks about, how to reduce the prompts when you trust it, and which protections always stay on.

## What needs your approval

| Action | Default |
|---|---|
| Reading, listing and searching files in your project | Allowed |
| Plans, notes, loading skills | Allowed |
| Editing or creating files | Asks |
| Running shell commands | Asks, except a few safe ones such as `go test` |
| Tools from external (MCP) servers | Asks |
| Dangerous commands (`sudo`, `rm -rf /`, piping downloads into a shell) | Always refused |
| Secrets and sensitive files (`.env`, `*.pem`, `*.key`, `credentials*`, `secrets/`) | Always refused |
| Files outside your project | Refused |

## Fewer prompts when you trust the task

| Option | Scope | What stops asking |
|---|---|---|
| Press `a` at a prompt | Rest of the session | That one tool (for example all shell commands) |
| Press `A` at a prompt | Rest of the session | Everything |
| Approve a plan with `a` in [plan mode](./using-chronos-code#plan-mode) | Rest of the session | File edits |
| `--yolo` | This run | File edits and simple commands from the allowlist |
| `--dangerously-skip-permissions` | This run | Everything that would ask |

With `--yolo`, combined commands (with `;`, `|`, `&&` or redirects), commands that aren't on the allowlist, and commands on the always-confirm list (such as `git push`) still ask. `--dangerously-skip-permissions` approves those too. It is meant for disposable environments such as containers and CI runners. See [Headless and Automation](./headless#permissions-when-nobody-is-watching).

No option can override the "always refused" rows in the table above.

## Plan mode as a safety net

For risky changes, [plan mode](./using-chronos-code#plan-mode) is the simplest protection. Chronos Code can't edit anything or run commands until you've read and approved its plan.

## Undo

- `/rewind` undoes the last file edit Chronos Code made.
- `/diff` shows everything that changed in your working tree.
- For anything bigger, use git. Commit, or at least stash, before a large task so you can compare and roll back easily.

## Project safety policy (`security.yaml`)

The safety policy decides which paths and commands are allowed. A built-in policy always applies. You can add restrictions for a project in `.chronos-code/security.yaml`, or for yourself in `~/.chronos-code/security.yaml`:

```yaml
version: "v1"
filesystem:
  writable_paths: ["src", "tests"]        # only allow edits here
  denied_paths: ["migrations/**", "**/*.sql"]
shell:
  allowed_commands: [go, git, make]       # only these programs may run (after approval)
  confirm: ['^make\s+deploy']             # always ask for these, even with --yolo
  never_allow: ['^git\s+push\s+.*--force'] # always refuse these
  max_execution_time_sec: 120
```

| Setting | What it does |
|---|---|
| `filesystem.writable_paths` / `readable_paths` | Where Chronos Code may write / read |
| `filesystem.denied_paths` | Files and folders it must never touch |
| `shell.allowed_commands` | Programs it may run (matched on the first word, such as `go` or `npm`) |
| `shell.confirm` | Commands that always need approval |
| `shell.never_allow` / `denied_patterns` | Commands that are always refused |
| `shell.max_execution_time_sec` | Time limit per command |
| `secrets.patterns` | Extra secret formats to redact from command output |
| `mcp.denied_servers` | External tool servers that may never be used |

### What you can and can't change

Your `security.yaml` can only make the policy **stricter**. You can:

- narrow the readable and writable paths
- remove programs from `allowed_commands`
- add denied paths, confirm rules, never-allow rules and secret patterns
- lower time and connection limits

You **can't** add programs that aren't in the built-in allowlist, widen paths, or turn off secret scanning. If a file tries to, Chronos Code refuses to start and tells you which line is the problem. This way, a repository you clone can never quietly give the agent more power than the defaults.

To run a program that isn't on the allowlist, approve it when asked. Press `a` to stop being asked about shell commands for the rest of the session, or use `--dangerously-skip-permissions` in a sandbox.

The built-in allowlist is: `go`, `git`, `make`, `npm`, `node`, `python`, `python3`, `pip`, `pip3`, `pytest`, `ruff`, `mypy`, `cargo`, `rustc`, `ls`, `cat`, `grep`, `rg`, `find`, `wc`, `diff`, `jq`.

## Credentials and secrets

- Keep API keys in environment variables or the system keychain (`chronos-code login`). Never put them in committed files.
- When a config file needs to refer to a secret, use a reference such as `"${MY_TOKEN}"`. For external tool servers in `.mcp.json`, anything that looks like a credential **must** be written this way, or the server is refused.
- Command output is scanned for common secret formats (cloud keys, tokens, private keys) before it reaches the model.

## Hooks from cloned repositories

[Hooks](./configuration#hooks) defined in a project's `config.yaml` never run until you explicitly trust them in your personal `~/.chronos-code/security.yaml`. Opening an unfamiliar repository can't make Chronos Code run its commands behind your back.

## Recommended setups

| Situation | Suggested setup |
|---|---|
| Everyday interactive work | Defaults. Press `a` for tools you trust during a session. |
| Unfamiliar or untrusted repository | Defaults plus plan mode. Don't use `--yolo`. |
| Sensitive codebase | Add a `security.yaml` that narrows `writable_paths` and adds `denied_paths`, and set a `--budget` |
| CI review job | No permission flags (read-only) |
| CI job that changes code | Disposable checkout, `--dangerously-skip-permissions`, `--budget`, and no production credentials in the environment |
