---
sidebar_position: 5
title: Why Chronos Code
description: The origin story and philosophy behind Chronos Code — why it exists and what makes it different
---

# Why Chronos Code

## The problem with most AI coding agents

When you use most AI coding tools, a few things happen behind the scenes that you never get to see or control:

**Routing is a black box.** The tool decides which model to use, when to call it, and how many tokens to spend — and you have no say. A simple "what does this function do?" question costs the same as a deep architectural analysis, because it all goes to the same model the same way.

**Configuration lives in the app, not in your project.** Your preferences, custom instructions, and safety rules are stored in an account, an extension's settings panel, or a cloud workspace. You can't put them in git, you can't share them with your team as code, and you can't see exactly what rules are actually running.

**There's one generalist doing everything.** Planning a complex change, writing the code, reviewing it for bugs, and explaining it to a new team member are very different jobs — but most tools route all of them to a single general-purpose model. That's fine for simple tasks, but it means you're paying frontier-model prices for work a cheaper, focused model could do just as well.

## How Chronos Code was built differently

Chronos Code started from a simple premise: your AI coding setup should be as readable and version-controllable as your `Makefile`.

**Everything is plain YAML.** Which agents exist, what skills they have, what guardrails are active, how requests are routed, what the spending cap is — all of it lives in files in your project directory. You can read them, edit them, commit them, and share them. No account lock-in, no settings buried in a UI.

**It looks at your code structure before burning API calls.** Chronos Code builds a lightweight index of your codebase — functions, types, call relationships — and consults that first before ever making an API call. The answer to "where is this function called?" should cost almost nothing. Only when the graph can't answer does it escalate to reading files, and only then to calling a frontier model.

**Different jobs go to specialists.** Planning what needs to change, implementing the change, reviewing it for correctness, debugging a failure, and explaining a concept are handled by separate specialist agents you can call on explicitly. Each one is tuned for its job and priced accordingly — a researcher or explainer runs on a cheap model; a coder or architect uses a frontier model only when the task genuinely needs it.

**Self-improvement is opt-in and human-reviewed.** When Chronos Code notices a recurring pattern in how it works with your project, it can suggest a new habit or shortcut. But that suggestion sits in a review queue — you approve or reject it before it ever changes how the tool behaves. Nothing sneaks in automatically.

The result is an AI coding tool that behaves predictably, costs less over long sessions, and whose entire configuration you can hand to a new teammate as a folder of files.
