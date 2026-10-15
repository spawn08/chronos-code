> Superseded where it conflicts by [HARNESS-SPEC.md](HARNESS-SPEC.md) (no runtime caps; language-agnostic).

# chronos-code tool suite proposal

Design rules applied to every tool (from Anthropic "Writing effective tools for agents", SWE-agent ACI, Codex prompting guide):

- One distinct job per tool, so no two tools overlap ("if a human can't say which tool to use, the agent can't").
- Results are high-signal, carry stable handles (paths, line numbers, shell ids), and have one budget: <= 10k tokens per result, truncated head+tail with an explicit "narrow with X" hint. Remove the "compressed, refetch with read_stored_result" round trip for anything under that budget.
- Errors are actionable: state what was wrong and give a correct example call.
- Empty output is reported explicitly ("exit 0, no output").
- Every read-only tool is `parallel_safe: true`. The harness executes a batch of parallel calls concurrently.
- Tool names are short verbs. The core set is loaded eagerly. Everything else is deferred behind `tool_search`.

Core (always loaded): `read`, `grep`, `glob`, `edit`, `write`, `shell`, `await_shell`, `kill_shell`, `code_search`, `todo`, `task`, `tool_search`.
Deferred (loaded on demand): `diagnostics`, `web_search`, `web_fetch`, the code-graph family (`code_context`, `callers`, `impact`, `code_map`), `memory_*`, `skill`.

---

## 1. `read` (replaces file_read, file_list; absorbs read_stored_result)

```json
{
  "name": "read",
  "description": "Read one or more files (or a directory listing). Batch every file you already know you need into ONE call via `files`. Lines are numbered `N|`. Default window is the first 400 lines; pass offset/limit for other ranges, negative offset reads the tail (offset:-50 = last 50 lines). Go/TS/Py files over 400 lines return an outline plus the first window unless you pass a range. The harness remembers what you read: re-reading an unchanged file region returns `unchanged since turn N` instead of content, so do not re-read to 'refresh'. A directory path returns its entries (non-recursive, with sizes and mtimes). Also reads stored large outputs via their `ref:` handle.",
  "input_schema": {
    "type": "object",
    "properties": {
      "files": {
        "type": "array", "minItems": 1, "maxItems": 20,
        "items": {
          "type": "object",
          "properties": {
            "path":   {"type": "string", "description": "Workspace-relative or absolute path, or a `ref:<id>` handle from a truncated result."},
            "offset": {"type": "integer", "description": "1-based start line; negative counts from end of file."},
            "limit":  {"type": "integer", "minimum": 1, "maximum": 2000, "description": "Max lines (default 400)."}
          },
          "required": ["path"]
        }
      },
      "force": {"type": "boolean", "description": "Return content even if unchanged since your last read (only after an external process may have changed it without the harness noticing)."}
    },
    "required": ["files"]
  }
}
```

The result for each file has `path`, `total_lines`, `range`, `mtime`, `content` and `truncated`, plus `next_offset` when truncated.

Rationale and failures fixed:
- **Redundant reads** (one file read 10 times): read-tracking returns `unchanged since turn N`. This mirrors Cursor/Claude Code "file state" tracking.
- **Log polling with file_read 14 times**: the negative-offset tail lets the agent read the last lines in one call. Polling itself moves to `await_shell` (section 6).
- **Serial reads**: the `files[]` batch and an explicit instruction to batch. The Codex guide says "Batch everything… never read files one-by-one unless logically unavoidable."
- **Compressed output, then a refetch**: `ref:` handles go through the same tool, so the separate `read_stored_result` tool is no longer needed.
- The SWE-agent ablation found a 100-line window beat both a 30-line window and the full file (18.0 vs 14.3 / 12.7). A 400-line default suits frontier models with large contexts, but the bounded window stays.

## 2. `grep` (replaces file_grep; ripgrep semantics)

```json
{
  "name": "grep",
  "description": "Search file contents with ripgrep semantics (Rust regex; use `fixed:true` for literals). Respects .gitignore. Use for literal text: strings, config keys, log messages, error text. For 'where is X defined / what calls X' prefer code_search. Default output_mode is `files_with_matches`; use `content` with context lines only after you know which files matter. Results over head_limit are cut, and the response tells you how many were omitted. Narrow with `path`/`glob`/`type` instead of paging.",
  "input_schema": {
    "type": "object",
    "properties": {
      "pattern":     {"type": "string"},
      "path":        {"type": "string", "description": "File or directory; default workspace root."},
      "glob":        {"type": "string", "description": "Filter files, e.g. `*.go`, `!**/*_test.go`, `internal/**/*.yaml`."},
      "type":        {"type": "string", "description": "ripgrep file type, e.g. go, ts, py, yaml."},
      "output_mode": {"type": "string", "enum": ["files_with_matches", "content", "count"], "default": "files_with_matches"},
      "context":     {"type": "integer", "minimum": 0, "maximum": 10, "description": "Lines before and after each match (content mode)."},
      "case_insensitive": {"type": "boolean", "default": false},
      "fixed":       {"type": "boolean", "default": false, "description": "Treat pattern as a literal string."},
      "multiline":   {"type": "boolean", "default": false},
      "head_limit":  {"type": "integer", "default": 100, "maximum": 500}
    },
    "required": ["pattern"]
  }
}
```

Implementation: shell out to `rg --json` when it is on PATH. Otherwise fall back to the existing pure-Go walker with identical output.

Failures fixed: the current file_grep returns up to 500 raw matches with no file-only mode, which floods context. SWE-agent found that summarized search beat iterative search by 6 points, and iterative search was worse than no search. The `files_with_matches` default plus a cut-with-count implements that "summarize, then narrow" behaviour.

## 3. `glob` (replaces file_glob)

```json
{
  "name": "glob",
  "description": "Find files by path pattern with full `**` support (e.g. `**/*_test.go`, `internal/{api,cli}/**/*.go`). Respects .gitignore. Results are sorted by modification time, newest first, so recently edited files come first. Max 200 results; the response says how many were omitted.",
  "input_schema": {
    "type": "object",
    "properties": {
      "pattern": {"type": "string"},
      "path":    {"type": "string", "description": "Root directory; default workspace root."},
      "sort":    {"type": "string", "enum": ["mtime", "path"], "default": "mtime"}
    },
    "required": ["pattern"]
  }
}
```

Failures fixed: there was no `**` support, which forced chains of file_list calls or a GNU-flavoured `find` (which then broke on macOS). Sorting by mtime surfaces the files the user is working on.

## 4. `edit` (new; splits editing out of file_write)

```json
{
  "name": "edit",
  "description": "Apply exact string replacements to existing files. Batch every replacement for a change into ONE call; edits to the same file apply in order, atomically (all succeed or none are written). Each old_string must match exactly (including indentation) and be unique in the file unless replace_all is true. Copy old_string from the numbered `read` output without the `N|` prefix. You must have read the file (or the relevant region) in this session; editing a file that changed since your last read is rejected so you don't clobber external changes. Returns a short unified diff of each change and any new diagnostics for the edited files.",
  "input_schema": {
    "type": "object",
    "properties": {
      "edits": {
        "type": "array", "minItems": 1, "maxItems": 50,
        "items": {
          "type": "object",
          "properties": {
            "path":        {"type": "string"},
            "old_string":  {"type": "string"},
            "new_string":  {"type": "string"},
            "replace_all": {"type": "boolean", "default": false}
          },
          "required": ["path", "old_string", "new_string"]
        }
      }
    },
    "required": ["edits"]
  }
}
```

Alternative or addition: an `apply_patch` tool that takes the Codex V4A format (`*** Begin Patch / *** Update File / @@ / *** End Patch`). Offer it only when the routed model is an OpenAI model trained on it; the Codex guide says to use the exact implementation. Claude models do best with str_replace semantics.

Errors are actionable. For example: `old_string matched 3 times in foo.go (lines 12, 40, 88); add surrounding lines to make it unique or set replace_all`. Or: `no match; closest region lines 40-46 shown below`.

Failures fixed:
- **One edit per round trip**: multi-edit lets a coherent change land in one call.
- **Clobbering or drift**: read-before-edit and mtime checks, as in Claude Code and Cursor StrReplace.
- **Blind edits**: inline diagnostics after each edit. SWE-agent found a linter-gated edit worth +3 points, and showing "what the edit would produce" stopped repeated failed edits.
- Diagnostics should surface errors, not block the edit. Blocking forces an edit order: you cannot remove an argument before its references.

## 5. `write` (create or overwrite whole files only)

```json
{
  "name": "write",
  "description": "Create a new file or fully replace a file you have read. For changes to existing files use `edit`. Parent directories are created. Keep a single call under ~800 lines; split larger outputs into modules.",
  "input_schema": {
    "type": "object",
    "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
    "required": ["path", "content"]
  }
}
```

Separating write from edit removes the dual-mode ambiguity in file_write (old_content/new_content versus full content). That ambiguity sometimes produced accidental full-file rewrites.

## 6. `shell` + `await_shell` + `kill_shell` (replaces shell)

```json
{
  "name": "shell",
  "description": "Run a command with /bin/sh -c in a persistent session: cwd and exported env vars carry over between calls (`cd` persists). The command waits up to block_until_ms (default 30000, max 600000). If it is still running then, it keeps running in the background and you get its shell_id and the output so far; use await_shell to wait for it or read new output, never sleep-and-read loops. Set block_until_ms:0 to start servers, watchers or long builds directly in the background. Output over 10k tokens keeps the head and tail and stores the full log at the returned `log_path`. The OS userland is reported in the Environment banner (on darwin: BSD sed/find/xargs/date; use `sed -i ''`, no `grep -P`, no `timeout`). Prefer grep/glob/read tools over rg/find/cat. Do not run interactive programs (editors, pagers, `git rebase -i`); set `GIT_PAGER=cat`/`--no-pager`.",
  "input_schema": {
    "type": "object",
    "properties": {
      "command":          {"type": "string"},
      "description":      {"type": "string", "description": "5-10 words, shown to the user (e.g. 'Run router package tests')."},
      "working_directory":{"type": "string", "description": "Run in this directory for this call only (does not change the session cwd)."},
      "block_until_ms":   {"type": "integer", "minimum": 0, "maximum": 600000, "default": 30000},
      "env":              {"type": "object", "additionalProperties": {"type": "string"}}
    },
    "required": ["command", "description"]
  }
}
```

Result: `{status: "exited"|"running", exit_code?, shell_id?, stdout, stderr, elapsed_ms, log_path?, cwd}`.

```json
{
  "name": "await_shell",
  "description": "Wait for a background shell to exit or for new output, without polling. Returns as soon as the process exits, OR `pattern` appears in new output, OR timeout_ms elapses, whichever comes first. Returns only output produced since your last read of this shell_id (or the tail when `tail_lines` is set).",
  "input_schema": {
    "type": "object",
    "properties": {
      "shell_id":   {"type": "string"},
      "timeout_ms": {"type": "integer", "minimum": 1000, "maximum": 600000, "default": 120000},
      "pattern":    {"type": "string", "description": "Optional regex; return as soon as it appears (e.g. 'Listening on|FAIL|panic')."},
      "tail_lines": {"type": "integer", "minimum": 1, "maximum": 500}
    },
    "required": ["shell_id"]
  }
}
```

```json
{
  "name": "kill_shell",
  "description": "Terminate a background shell's whole process group. Use when a command hangs, or when a server/watcher you started is no longer needed. Returns the final output tail.",
  "input_schema": {"type": "object", "properties": {"shell_id": {"type": "string"}}, "required": ["shell_id"]}
}
```

Harness behaviour:
- No wall-clock cap and never killed by the harness (HARNESS-SPEC.md §2.2). The idle-output/CPU watchdog emits a `no output for 120s` note on the next await.
- Kill all background processes at session end.
- A per-session list of running shells is injected into the turn context (`Background: sh-2 'go test ./...' running 3m12s`).

Failures fixed:
- **95-minute hung-shell stall**: a hung command no longer blocks the agent loop. Control returns at block_until_ms, and the agent can await, kill or move on.
- **Polling logs with file_read 14 times**: `await_shell` blocks on exit or pattern and returns only the delta.
- **No cwd persistence**: this caused repeated `cd x && ...` and wrong-directory runs.
- **GNU flags on macOS**: the userland note in the description and banner.
- Codex's unified exec (`exec_command` with `yield_time_ms`, plus `write_stdin` for polling) and Cursor's `block_until_ms` with AwaitShell are both this design.

## 7. `diagnostics` (deferred; surfaced automatically after `edit`)

```json
{
  "name": "diagnostics",
  "description": "Get compiler/type-checker/linter errors for files or packages from the language server (gopls, tsserver, pyright...) without running the full build. Fast (<2s typical). Use after a series of edits before running tests, or to check a file you didn't edit. Returns at most 50 issues, errors first.",
  "input_schema": {
    "type": "object",
    "properties": {
      "paths":    {"type": "array", "items": {"type": "string"}, "description": "Files or directories; default = files edited this turn."},
      "severity": {"type": "string", "enum": ["error", "warning", "all"], "default": "error"}
    }
  }
}
```

chronos-code already has `internal/lsp` and pins up to 5 diagnostics. This exposes it as a tool and attaches new diagnostics to `edit` results.

Failure fixed: full `go test ./...` runs used as a type check, which made each iteration minutes long. ReadLints in Cursor and the SWE-agent lint gate both do this job.

## 8. `code_search` (semantic/hybrid; consolidates codebase_search, resolve_symbol, graph_query, find_definition)

```json
{
  "name": "code_search",
  "description": "Find code by meaning or symbol. `query` can be a symbol name (`NewRouter`, `Store.Migrate`) or a natural-language question ('where are verification obligations derived'). Hybrid ranking: exact symbol match, then BM25 over identifiers/docs, then embedding similarity when available. Returns up to `limit` hits as file:line, kind, signature, a 1-line doc and an excerpt. Set `include` to pull callers/tests/implementations for the top hit in the same call. Results carry index freshness; on `stale` or empty, fall back to grep.",
  "input_schema": {
    "type": "object",
    "properties": {
      "query":   {"type": "string"},
      "path":    {"type": "string", "description": "Restrict to a directory."},
      "kind":    {"type": "string", "enum": ["any", "func", "type", "method", "field", "file"], "default": "any"},
      "include": {"type": "array", "items": {"type": "string", "enum": ["callers", "callees", "tests", "implementations", "source"]}},
      "limit":   {"type": "integer", "default": 10, "maximum": 30}
    },
    "required": ["query"]
  }
}
```

Keep `impact` (blast radius plus tests to run) and `code_map` (package structure) as deferred tools. Fold `find_callers`, `find_implementations`, `codebase_context` and `multi_resolution_view` into `include` or `code_map`. The result is 12 graph tools reduced to 1 core and 2 deferred, which removes overlap and selection errors. Cursor reports +12.5% QA accuracy from semantic search, used together with grep rather than instead of it.

## 9. `tool_search` (deferred loading)

```json
{
  "name": "tool_search",
  "description": "Find and load additional tools by capability. Core tools are already loaded; call this when you need something else (web access, diagnostics, code graph/impact, memory, MCP server tools, skills). Returns matching tool names with one-line descriptions; the matched tools become callable on your next turn.",
  "input_schema": {
    "type": "object",
    "properties": {
      "query": {"type": "string", "description": "Capability keywords, e.g. 'web fetch', 'callers impact', 'postgres'."},
      "limit": {"type": "integer", "default": 5}
    },
    "required": ["query"]
  }
}
```

For Anthropic models use the native `defer_loading: true` plus the tool-search tool, which keeps the cache-stable prefix intact. For other providers, implement the same behaviour as a harness tool that adds definitions at the next turn boundary and appends them so the existing prefix is not invalidated.

Failures fixed: about 30 eagerly loaded tools inflate every request and cause mis-selection. Anthropic measured an 85% definition-token reduction and Opus MCP-eval accuracy rising from 49% to 74% and from 79.5% to 88.1%.

## 10. `web_search` / `web_fetch` (deferred)

```json
{"name": "web_search", "description": "Search the web for current docs, release notes, error messages and API changes. Use the current year from the Environment banner, do not invent one. Returns title, URL and snippet for up to 8 results.",
 "input_schema": {"type": "object", "properties": {"query": {"type": "string"}, "allowed_domains": {"type": "array", "items": {"type": "string"}}}, "required": ["query"]}}
```
```json
{"name": "web_fetch", "description": "Fetch a URL and return it as markdown, optionally focused by `prompt` (a cheap model extracts only what the prompt asks for). Max 10k tokens; larger pages return a `ref:` handle readable with `read`.",
 "input_schema": {"type": "object", "properties": {"url": {"type": "string", "format": "uri"}, "prompt": {"type": "string"}}, "required": ["url"]}}
```

Failure fixed: guessing at unfamiliar or changed APIs. The main prompt already says "verify unfamiliar APIs" but gives no tool to do it with.

## 11. `todo` (replaces update_plan; same semantics, cheaper)

```json
{
  "name": "todo",
  "description": "Track a multi-step task (3+ distinct steps). Skip for simple tasks. Send the full list each time; exactly one item in_progress. Mark an item completed immediately when done; do not batch. The current list is shown to you each turn, so never restate it in prose.",
  "input_schema": {
    "type": "object",
    "properties": {
      "items": {"type": "array", "items": {"type": "object", "properties": {
        "id": {"type": "string"}, "content": {"type": "string"},
        "status": {"type": "string", "enum": ["pending", "in_progress", "completed", "cancelled"]}},
        "required": ["id", "content", "status"]}}
    },
    "required": ["items"]
  }
}
```

Failure fixed: the current prompt requires update_plan "for multi-step work" and attaches verification to every item. That produces plans for trivial tasks. The Codex guide says to skip the plan for "roughly the easiest 25%" of tasks and never write a one-step plan.

## 12. `task` (replaces spawn_subagent; structured result)

```json
{
  "name": "task",
  "description": "Run an independent sub-task in a fresh context and get back a structured result. Worth it only when (a) the work needs roughly 10+ tool calls whose raw output you will not need, or (b) several such pieces are independent and can run in parallel (send them in one batch). Do NOT use for a lookup you can do with 1-3 grep/read/code_search calls, for single-file edits, or for reviewing your own diff unless the user asked for a review. The subagent sees none of your conversation: put the goal, the known file paths and facts, the constraints, and the exact deliverable in `prompt`.",
  "input_schema": {
    "type": "object",
    "properties": {
      "agent":       {"type": "string", "enum": ["explore", "implement", "verify", "debug", "review", "plan"], "description": "explore: read-only search/answer. implement: scoped edits to listed files. verify: run checks, no edits. debug: reproduce + root-cause, no edits. review: diff review. plan: next-steps plan."},
      "description": {"type": "string", "description": "3-6 words shown to the user."},
      "prompt":      {"type": "string", "description": "Self-contained brief: GOAL, CONTEXT (paths, facts already established), SCOPE (files allowed to change / out of bounds), DELIVERABLE, DONE WHEN."},
      "background":  {"type": "boolean", "default": true, "description": "Run without blocking; poll with await_task."},
      "model":       {"type": "string", "enum": ["inherit", "fast"], "default": "inherit"}
    },
    "required": ["agent", "description", "prompt"]
  }
}
```

The result is always this JSON, validated by the harness. On a validation failure, the harness asks the child once more for the structure; if that fails it returns `status:"failed"`.

```json
{
  "status": "complete|partial|blocked|failed",
  "summary": "<= 120 words answering the brief",
  "findings": [{"file": "path:line", "fact": "..."}],
  "changed_files": ["..."],
  "checks": [{"command": "...", "exit_code": 0, "result": "pass|fail", "excerpt": "..."}],
  "unresolved": ["..."],
  "next_action": "concrete step for the parent or 'none'",
  "usage": {"tool_calls": 0, "elapsed_s": 0, "futility_rung": 0}
}
```

Harness behaviour:
- No time or call caps (see HARNESS-SPEC.md §1). The child runs under the liveness supervisor and futility ladder; at rung 3/4 it returns `partial`/`blocked` with its findings.
- Stream child progress, a one-line status, to the UI.
- Make the parent's `checks` evidence count for the verification ledger.

Failures fixed:
- **Slow, futile subagents**: budgets, early exit and gating of when to use them. Anthropic's research system found agents spawned 50 subagents for trivial queries until explicit scaling rules were added. It also noted that coding has few truly parallel tasks, and that multi-agent setups cost about 15 times the tokens.
- **Prose handoffs**: the parent previously re-read files to check the STATUS text. The structured result now carries `findings` with locations and `checks` with exit codes.

---

## Harness changes that go with the tools (not tools, but required)

| Change | Failure it fixes |
|---|---|
| The verification gate keys on **observed mutation only**, and an obligation is satisfied by any passing check run after the last mutation. Read-only tasks have no obligations, and nothing is re-run when nothing has changed. Remove `git diff --stat` as an obligation; the harness computes the diff itself and attaches it to the final turn. | Forced test re-runs and `git diff --stat` on read-only tasks. |
| Stop injecting `Path: complexity=... kind=...` into the model context; keep it as a telemetry/router input only. | A mislabelled complexity anchors effort. |
| The `<turn_context>` is rebuilt from current state each turn and appended rather than nested. Include only the environment, todo list, running background shells, and new diagnostics. | A recursive, stale turn context. |
| The compaction summary budget scales with the window (for example 4-8k tokens or 3% of the window). It uses a structured template: goal, decisions, files changed, open todos, last check results, next step. | Summaries truncated at 1,500 tokens. |
| The environment banner adds `userland=bsd|gnu`, `shell=sh`, and which of `rg`, `gtimeout` and `gsed` are on PATH. | GNU flags used on macOS. |
| The tool-result budget is 10k tokens, head+tail with a `ref:` handle, instead of compression followed by a refetch. | Extra refetch turns. |
