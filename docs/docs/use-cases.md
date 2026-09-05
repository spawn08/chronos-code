---
sidebar_position: 7
title: Use Cases
description: Concrete scenarios where Chronos Code makes a real difference
---

# Use Cases

## Getting oriented in a large, unfamiliar codebase

You've just joined a project with hundreds of thousands of lines of code. You need to understand how a particular feature works, where the boundaries are between components, and what touches what — without reading every file.

With Chronos Code, you describe what you're trying to understand and it uses a lightweight index of your codebase — function signatures, type relationships, call chains — to answer structural questions without opening files unnecessarily. You can ask "what calls this function?" or "where does this type get constructed?" and get a meaningful answer in seconds. When you do need to read code, it reads only the relevant sections, not entire files.

The result: you can build a mental map of a large system in a single session instead of days of spelunking.

---

## Making a risky change safely

You need to refactor something that touches multiple parts of the system — a shared interface, a config format, a core utility. Doing it blind risks breaking callers you didn't know about.

With Chronos Code, you start in plan mode. It maps out what's affected before any code changes. You review the plan, approve it, and then a coder agent implements it step by step — checking for broken callers as it goes. When the implementation is done, you call in a reviewer by name to check the changes for correctness and security issues before anything ships.

The result: a multi-file refactor with a clear plan, an implementation step, and a review step — all in one session, all before a single line lands in your main branch.

---

## Keeping API costs predictable on long sessions

You're using an AI coding tool for several hours across a big feature. Costs spiral because every question — even simple lookups — goes to the same expensive model.

Chronos Code routes by task type. Structural questions ("where is this defined?") are answered by the code graph at effectively zero cost. Research and explanation tasks go to a cheap model. Only implementation and review — tasks that genuinely need reasoning and judgment — use a frontier model. You can also set a hard spending cap; if the session would exceed it, calls stop rather than silently running up a bill.

The result: long sessions cost a fraction of what they would if every call hit a frontier model, and the budget cap means there are no surprise invoices.

---

## Sharing team conventions as versioned files

You have a set of house rules for your codebase: how errors should be handled, what patterns are approved, what the review checklist looks like. Right now they live in a wiki page or someone's head, and they don't reliably influence how code gets written or reviewed.

With Chronos Code, those conventions live in plain YAML files in your repository — as memory entries, skill definitions, or custom guardrail rules. Commit them to version control and every teammate who runs Chronos Code gets the same conventions applied automatically. Want to update the rules? Open a PR. Want to see what rules are active? Read the file.

The result: your team's standards travel with the code, not with individual people's setups.

---

## Turning a one-off fix into a reusable shortcut

You spent an hour figuring out the right way to handle a tricky pattern in your codebase. Next time this comes up, you don't want to repeat that work — but you also don't want the tool to silently learn habits without you knowing.

Chronos Code can suggest a new habit based on what it observed in a session — a shortcut, a preferred pattern, a project-specific convention. That suggestion goes into a review queue. You read it, decide whether it's right, and accept or reject it. Accepted suggestions become part of the project's configuration, available to you and your team in future sessions.

The result: institutional knowledge builds up over time, controlled by human judgment, with a clear record of what was learned and when.
