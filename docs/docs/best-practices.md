---
sidebar_position: 10
title: Best Practices
description: Practical habits for getting better results from Chronos Code, with less cost and less risk.
---

# Best Practices

## Set up your project once

**Write an `AGENTS.md`.** This is the most valuable ten minutes you can spend. Tell Chronos Code how to build, how to run the tests (including a single test), where things live, and what to avoid. Keep it short and factual. It's read on every task, so every line should earn its place. See [Project instructions](./agents-and-skills#project-instructions).

**Make checking easy.** Chronos Code verifies its work by running your build and tests. If your project has a single `make test` (or `npm test`) command that runs quickly, it will use it and catch its own mistakes. Mention it in `AGENTS.md`.

**Commit the shared setup.** Commit `.chronos-code/` (agents, skills, safety policy) and `AGENTS.md` so the whole team gets the same behavior. Keep personal preferences in `~/.chronos-code/`.

## Ask well

**Describe the outcome and how to check it.** "Fix the flaky checkout test and run it 10 times to confirm" works much better than "look at the tests".

**Give it the context you already have.** Paste the error message, mention the file with `@path/to/file`, or say which approach you prefer. Anything you know saves the agent a search.

**One goal per request.** Big, multi-part requests work, but a sequence of focused requests is easier to review and cheaper to correct.

**Say what's off-limits.** "Don't change the public API" or "only touch the `billing` package" prevents well-meaning but unwanted changes.

## Use plan mode for anything risky

Turn on `/plan on` before refactors, migrations, changes across many files, and anything you'd want a colleague to review first. Read the plan properly. It's much cheaper to fix a wrong step in a plan than in code. Press `n` and write what to change, then approve when it's right.

In automation, use `--plan-mode` for larger tasks. The extra planning round usually pays for itself.

## Stay in control without micromanaging

- Press `a` at a prompt to stop being asked about a tool you trust for the rest of the session, instead of approving each call.
- Keep `--yolo` and `--dangerously-skip-permissions` for trusted repositories and sandboxes.
- **Commit before big tasks.** A clean working tree makes it trivial to review everything with `git diff`, or to throw it away.
- Check what happened: `Ctrl+O` shows the full output of every command. `/inspect changes` lists every edit.
- Changed your mind? `/rewind` undoes the last edit.

## Pick the right helper

| You want to… | Use |
|---|---|
| Get something built end to end | Just ask. The main agent plans, delegates and checks. |
| Review a change for bugs and security | `@reviewer` |
| Find the cause of a failure | `@debugger`, with the error or failing test |
| Understand unfamiliar code | `@explainer` (fast and inexpensive) |
| Find where something is used or defined | `@researcher` (read-only and fast) |
| Decide on a design before coding | `@architect` or `@planner` |

Remember that a specialist stays active after you mention it. Switch back with `@chronos-code`.

## Keep long sessions healthy

- **Start a new session for a new topic** (`/clear`). Old, unrelated history costs tokens and can distract the model.
- **Compact when a session gets long** (`/compact`). It keeps the important facts and drops the noise.
- **Watch the cost** with `/usage`. Set a cap with `--budget` whenever you run unattended.
- **Save lasting knowledge** with `remember project: …`. Good memories are facts that will still be true next month, such as "we use X for Y because Z". Session chatter makes bad memories.
- **Resume instead of re-explaining.** `/resume` brings back the previous conversation, including what was already tried.

## Grow your setup gradually

1. Start with the defaults and an `AGENTS.md`.
2. When you explain the same procedure a second time, turn it into a [skill](./agents-and-skills#skills).
3. When a project needs tighter rules, add a [`security.yaml`](./security#project-safety-policy-securityyaml).
4. Automate formatting or checks with [hooks](./configuration#hooks).
5. Every so often, run `/learn` to review the improvements Chronos Code suggests from your sessions. Accept the useful ones and reject the rest.

## Automation checklist

- [ ] Fresh checkout per job, with a job timeout
- [ ] `--json` output, and decisions based on the exit code
- [ ] `--budget` set
- [ ] The narrowest permission flag that works: none, then `--yolo`, then `--dangerously-skip-permissions` in a sandbox
- [ ] API keys from the CI secret store, and no production credentials in the job
- [ ] `mcp.discovery_enabled: false` unless the job needs external tools
