> See [HARNESS-SPEC.md](HARNESS-SPEC.md) — authoritative v2 spec (no runtime caps; futility governance; all languages).

# Critique of current chronos-code prompts and tool descriptions

## chronos-code.yaml (1,432 words: about 1,250 in system_prompt plus 6 instructions)

**Length and redundancy**
- The code-index routing appears three times: the main prompt's "Investigate" section, context-engineering.yaml (which is appended to every agent), and each subagent prompt. That is roughly 350 words repeated per request.
- Autonomy is stated three times: "Work autonomously", "Before ending your turn, check your last paragraph", and "Context is compacted… Never stop". Verification is stated twice: the "Verify and finish" section and `instructions[0]`.
- The setup/configuration paragraph (`chronos-code init`, `config show` and so on) applies to under 1% of tasks. It belongs in a skill.

**Contradictions**
- "Work autonomously… never stop… end your turn only when complete" conflicts with "inspect the diff… ask whether you met the user's actual goal… do not leave routine verification for the user". Together they push the model into indefinite self-verification. The harness gate makes this worse.
- "Solve straightforward tasks directly; routing labels are advisory" conflicts with "For multi-step work, record each deliverable and its verification in update_plan" and the injected `Path: complexity=… suggested_plan=…` line. The model follows the more concrete instruction, which is the label.
- "For questions, reviews, or planning-only requests, deliver that outcome without implementing" conflicts with `instructions[0]`: "After your final edit, run the checks… then `git diff --stat`". The runtime's verification obligations, together with the instruction, lead to tests and git diff being run on read-only tasks.
- "Avoid rereading unchanged material" conflicts with "Summaries and compressed results are leads, not proof" and "confirm in source anything a change depends on". The second pair licenses re-reads, which is how one file got read 10 times.
- "Ask a focused question only when…" conflicts with "The user is not watching and cannot answer mid-task". The prompt is unclear about whether asking is ever allowed.

**Ceremony-inducing instructions**
- Mandatory `git diff --stat`, plus "Run each as its own shell command with no pipes, output redirection, or chained commands". This exposes a harness implementation detail, the evidence classifier, and forces extra turns.
- "any later edit or mutating command makes that evidence stale". Combined with the gate, this produces the re-run loop.
- The `ppdLoopDirective` (orchestrator.go:2635) forces planner, then update_plan, then a planner re-spawn. Each spawn costs a fresh context.
- The routing hint "spawn_subagent <specialist> if that specialist loop is needed" invites spawning.

**Missing guidance**
- Nothing on parallel or batched tool calls. The only related text is "batch independent lookups", buried mid-paragraph.
- Nothing on long-running commands or backgrounding, and the tool cannot do it.
- No OS userland guidance. The environment line gives `os=darwin` but nothing says "BSD sed".
- No final-answer format. "finish with the outcome, verification evidence, remaining limitations, and answers" invites a long recap.
- No quantitative criterion for spawning a subagent.
- No stop criterion for read-only tasks.

## context-engineering.yaml
- It duplicates the main prompt's investigation section nearly word for word.
- "Do not substitute shell grep, find, or whole-file reads" is good, but should live in tool descriptions.
- It is appended to every subagent as well, including tester, which does not navigate.

## Subagent prompts (490–600 words each)
- Each re-teaches the full code-index roster.
- Handoffs are prose labels (STATUS/EVIDENCE/…) and cannot be validated by the harness, so the parent re-reads files to check claims.
- There are no budgets, no time limits and no early exit. The debugger explicitly allows a re-spawn loop ("return to step 3").
- `coder` carries the same mandatory `git diff --stat` instruction and may itself spawn reviewer, planner, researcher and tester. That allows nested spawns, which are slow and can be futile.
- coder says: "If the user requests multiple agents, use at least two relevant specialists". This forces spawning.
- The architect/planner and explainer/researcher pairs overlap.

## Tool descriptions
- `file_read` (builtins): "Read the contents of a file at the given path." incctx appends the bounds. Nothing about outlines-by-default, read tracking, tails, or multi-file reads.
- `file_grep` (incctx): a long list of byte limits (8 MiB, 32 MiB, 10000 entries), which the model doesn't need. There are no files-only or count modes, no glob filter, and no context lines.
- `file_glob`: "Find files matching a glob pattern." It does not say that `**` is unsupported, so the model tries `**` and fails or retries.
- `shell`: "Execute a shell command in the workspace and return stdout/stderr." It says nothing about the fixed 300 s timeout, the lack of backgrounding, cwd not persisting, BSD userland, or interactive commands.
- `spawn_subagent`: "Delegate a self-contained sub-task… keeping your context small." This is pure encouragement, with no guidance on when not to delegate. It lists 9 specialists with "Default X agent for chronos-code" descriptions that carry no signal.
- Graph tools: 12 tools with overlapping jobs. resolve_symbol, find_definition and graph_query all resolve a name, and codebase_search versus codebase_context is a further overlap.

## Harness behaviours that override any prompt
1. The verification-repair gate (`orchestrator/repair.go`, `verification.Derive`). It must key strictly on observed mutation and accept prior passing evidence when nothing has changed since. `KindDiff` should not be an obligation.
2. `formatRoutingHint`: complexity and kind labels shown to the model. Move these to telemetry only.
3. `<turn_context>` nesting: rebuild it fresh each turn.
4. The 1,500-token compaction summary cap.
5. The shell implementation, which needs to be rebuilt (see tools.md section 6).
