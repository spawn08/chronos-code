---
sidebar_position: 8
title: Best Practices
description: Practical tips for getting the most out of Chronos Code
---

# Best Practices

## Keep custom instructions short and focused

It's tempting to dump every project convention into a single instructions file. Resist that. Long, unfocused instructions dilute what matters — the tool has to read them all, and the parts that are relevant to any given task get lost in the noise.

Instead, write short, specific instructions for specific concerns: one for error-handling conventions, one for the review checklist, one for testing patterns. When you call in a reviewer, it reads the review-relevant instructions; when you call in a coder, it reads the coding-relevant ones. Focused instructions produce better results than a wall of text.

---

## Call a specialist by name when you know what you need

The default agent is a generalist — good for orientation, routing, and multi-step tasks you haven't fully defined yet. But when you know exactly what kind of help you want, name the specialist:

- `@reviewer` when you want a bug and security review of a diff
- `@debugger` when you have an error trace and need to find the root cause
- `@planner` when you need a multi-step task broken down before anything changes
- `@researcher` when you want read-only exploration without any writes
- `@explainer` when you need a clear explanation of how something works

Named specialists are focused on their job and use the right model for it. A `@researcher` or `@explainer` will use a cheap model and won't try to make changes. A `@reviewer` won't try to implement the fix while reviewing — it'll give you the findings and let you decide.

---

## Match your safety-scanning strictness to the project

The default guardrails are a sensible baseline, but different projects have different sensitivity levels. A personal side project and a production service handling user data are not the same.

For sensitive projects: tighten the path allowlist so the tool can only read and write in the directories that make sense; lower the budget cap to catch runaway sessions early; require explicit approval for MCP tool calls.

For internal tools or experimental projects: you might relax approval prompts for faster iteration.

Your `security.yaml` and `config.yaml` files are the right place for this — not flags you retype every time. Put project-appropriate settings in the `.chronos-code/` directory and commit them so they're always in effect.

---

## Save durable facts about your project, not session chatter

Memory works best when it stores things that will still be true next week: architectural decisions, approved patterns, the reason a non-obvious choice was made. It works poorly as a dump of every question you asked during an exploration session.

When you discover something worth remembering — "we use X library for Y because of Z" or "don't use pattern A in this codebase, use B instead" — explicitly ask the tool to remember it as a project fact. That surfaces it in future sessions when it's relevant. General session history isn't durable and doesn't carry forward.

---

## Check what a long session is spending its budget on

If a session is running longer than expected or costing more than anticipated, ask for a context summary. You'll see which sources are contributing to the context, how many tokens each is consuming, and whether the session has accumulated a lot of exploration that's no longer useful.

Use `/compact` to summarize and compress the session history when it's grown large. This keeps later turns in the session cheaper without losing the key facts. Set a hard USD cap in `config.yaml` so that no session can silently exceed a limit you're comfortable with — the tool fails closed rather than running up a bill.
