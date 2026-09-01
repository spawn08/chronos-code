# chronos-code PRD v2 — Path to a World-Class Coding Agent

**Owner:** Amarsingh
**Status:** Draft for build execution
**Base:** `github.com/spawn08/chronos-code` on top of `github.com/spawn08/chronos` v0.5.2
**Target:** Single static Go binary ≤20 MB, feature-parity with Claude Code / Pi, differentiated on multi-agent routing + local self-learning.

---

## 0. Upfront honesty

I could not fetch the `chronos-code` repo directly (GitHub blocks the automated fetch). This PRD is written from:
- What I know about your Chronos v0.5.2 framework and the prior chronos-code PRD (MCP, YAML agents, subagents, self-learning, Postgres).
- Public research on the competitors you called out (Pi, Claude Code, Codex CLI, OpenCode, Aider, Sourcegraph amp) done for this document.

If any capability listed below as "missing" is actually implemented, tell me and I'll re-scope. The gap read in §2 is based on the ten capabilities you listed as absent or subpar. Assume it is directional, not line-by-line.

---

## 1. Product thesis

Coding agents in 2026 have converged on the same feature set (agent loop, tool calls, MCP, subagents, plan mode, permissions). What actually differentiates them is:

1. **How fast the loop is** (indexer speed, TUI render, token efficiency).
2. **How cheaply it runs** (routing to the right model per task complexity).
3. **How much it improves with use** (local episodic memory, skill promotion).
4. **Whether it works with the user's existing accounts** (Claude Max/Enterprise, ChatGPT Business/Enterprise) without a separate API bill.

chronos-code should compete on **(2) + (3) + (4)** as its wedge. Feature parity on the rest is table stakes.

**Non-goals:**
- Being a "minimal harness" like Pi. That market is taken and you don't have Mario's distribution.
- Building a hosted product. This is a single-binary CLI + TUI.
- Beating Claude Code on Anthropic-only workloads. You will lose. Win on multi-provider routing and enterprise auth flexibility.

---

## 2. Gap analysis vs. your stated targets

For each of your ten capabilities, the honest read:

| # | Capability | Likely current state | Gap severity | What "done" looks like |
|---|-----------|----------------------|--------------|------------------------|
| 1 | Skills integration | Partial — you probably have hooks | Medium | `~/.chronos-code/skills/` with SKILL.md loader, auto-discovery, per-repo overrides, deterministic selection algorithm |
| 2 | MCP server integration | Present per prior PRD | Low-Medium | stdio + SSE + HTTP transports, config in `mcp.json`, permission-scoped, health checks, hot reload |
| 3 | Enterprise Claude/Codex login | **Missing** — you called this out | **Critical** | OAuth PKCE flow, credential reuse from Claude Code / Codex CLI, refresh handling, gateway/proxy support |
| 4 | AGENTS.md / CLAUDE.md loading | Unknown | Low | Root-repo discovery, hierarchical merge (user > project > subdir), token-budgeted injection |
| 5 | Self-learning + local DB | Prior PRD mentions PostgreSQL | **Critical** | SQLite-first (no external DB dep), episodic memory, pattern extraction, skill promotion loop |
| 6 | Code indexer | Unknown | **Critical** | Tree-sitter + SCIP hybrid, Merkle-diff incremental, background daemon, code graph, sub-100ms queries |
| 7 | Multi-agent dynamic routing | Partial (YAML defs exist under `benchmarks/`) | High | Router agent, complexity classifier, cost-aware dispatch, agent registry, streaming handoff |
| 8 | Shell + permissions | Unknown | High | Tiered permission model, per-command allowlist/denylist, sandboxing (bwrap/sandbox-exec), timeout, output capture |
| 9 | World-class TUI | Weak (your own assessment) | **Critical** | Bubble Tea with differential rendering, Kitty keyboard, inline images, <16ms frames, no flicker |
| 10 | Task-complexity model switching | Missing | High | Deterministic classifier + escalation on failure, cost/latency budget aware |

**Verdict:** Four criticals (enterprise auth, self-learning, indexer, TUI), three high (routing, permissions, complexity switching). The rest is polish.

---

## 3. Competitive benchmark

Fresh reads on each competitor's actual behavior:

### Claude Code (Anthropic)
- OAuth 2.0 flow via `/login`. Credentials in `~/.claude/.credentials.json` (or Keychain on macOS). Token auto-refresh in the last 60 seconds before expiry. Supports API key, subscription OAuth, `CLAUDE_CODE_OAUTH_TOKEN` for CI, `ANTHROPIC_AUTH_TOKEN` for enterprise gateways, Bedrock, Vertex, Foundry. Precedence order documented.
- TUI is React-based (Ink). Suffers flicker on re-layout; Mario Zechner (Pi) publicly criticized this as a "game engine" problem.
- Skills discovered from `~/.claude/skills/` and project `.claude/skills/`. Slash commands, hooks, subagents, plan mode.
- MCP: stdio, SSE, HTTP. Server config in `~/.claude.json` and per-project `.mcp.json`.
- No public code indexer — relies on file reads + grep. Fast because the model is fast.

### Pi coding agent (Mario Zechner)
- ~600-line TUI (differential rendering, flicker-free even over SSH). Kitty keyboard, inline images.
- Deliberately minimal: no built-in subagents, plan mode, or MCP. Everything is an **extension** (TypeScript modules with tool/keybind/event/TUI access).
- Ranked #2 on TerminalBench with Opus 4.5 despite the missing features. Argues features aren't what wins.
- Unified LLM API (`pi-ai`) with Anthropic, OpenAI, Google, xAI, Groq, Cerebras, OpenRouter, Ollama.
- **Weakness for your use case:** no enterprise auth flow reuse from Claude Code / Codex — user has to bring API keys.

### Codex CLI (OpenAI)
- Sign-in with ChatGPT (browser OAuth), API key, or `codex login --with-access-token` for enterprise. `~/.codex/auth.json`.
- Enterprise access tokens (ChatGPT Business/Enterprise) usable without browser via `CODEX_ACCESS_TOKEN`.
- Device code auth (`--device-auth`) for headless.
- Configurable providers with `requires_openai_auth = true` for custom endpoints.
- Managed config can force login method (`forced_login_method`, `forced_chatgpt_workspace_id`).

### OpenCode
- Ships plugins to reuse Claude Code and Codex OAuth credentials (`opencode-claude-auth`, `opencode-openai-codex-auth`). Confirms the pattern is reusable — you should copy it.

### Sourcegraph amp / Cody, Cursor, Cline
- Cursor's Structural Codebase paper: three parallel indices (AST symbols, embeddings, graph), Merkle-diff incremental over the working copy. Exposes `codebase_search` and `codebase_graph` tools to the agent. Ablation shows measurable win when SC-ON vs SC-OFF.
- Sourcegraph: SCIP indexes (compiler-grade, per language).
- Cline / Aider: rely on repomap (tree-sitter tags + PageRank) rather than embeddings.

### Reference indexers to steal from
- `intuit/infigraph`: 62 languages, Kùzu graph DB, BM25 + Model2Vec hybrid, SCIP integration, watch mode. Zero LLM dependency.
- Tree-Sitter Knowledge Graph paper: SQLite single file, XXH3 content hashes, adaptive polling file watcher.

**Bottom line:** The technology to beat Claude Code on indexing exists in the open. Nobody has combined it with cheap-provider routing + credential reuse + a small Go binary. That's the hole to fill.

---

## 4. Target architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    chronos-code (single binary)                 │
│                                                                 │
│  ┌───────────┐  ┌────────────┐  ┌──────────────┐  ┌──────────┐│
│  │    TUI    │  │  Headless  │  │  RPC / SDK   │  │   Web    ││
│  │ (BubbleTea)│  │  (-p mode) │  │ (stdin/JSON) │  │ (future) ││
│  └─────┬─────┘  └──────┬─────┘  └──────┬───────┘  └────┬─────┘│
│        └───────────────┴────────────────┴──────────────┘      │
│                          │                                     │
│                  ┌───────▼────────┐                            │
│                  │  Agent Runtime │  ← Chronos core            │
│                  │  (loop, tools, │                            │
│                  │   checkpoints) │                            │
│                  └───┬────────┬───┘                            │
│           ┌──────────┘        └──────────┐                     │
│           ▼                              ▼                     │
│  ┌────────────────┐              ┌───────────────┐             │
│  │  Router Agent  │              │   Tool Bus    │             │
│  │  (complexity   │              │  read/write/  │             │
│  │   classifier)  │              │  bash/mcp/... │             │
│  └────────┬───────┘              └───────┬───────┘             │
│           │                              │                     │
│    ┌──────▼──────┐                       │                     │
│    │ Specialist  │                       │                     │
│    │ Agents      │                       │                     │
│    │ (YAML defs) │                       │                     │
│    └──────┬──────┘                       │                     │
│           │                              │                     │
│  ┌────────▼──────────────────────────────▼──────────────────┐  │
│  │              Provider Abstraction                        │  │
│  │  Anthropic OAuth │ Codex OAuth │ API keys │ Bedrock │…  │  │
│  └──────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              │
        ┌─────────────────────┼─────────────────────┐
        ▼                     ▼                     ▼
┌───────────────┐    ┌────────────────┐    ┌────────────────┐
│  Indexer      │    │  Memory Store  │    │  Skills / MCP  │
│  Daemon       │    │  (SQLite +     │    │  Registry      │
│  (chronos-    │    │   sqlite-vec)  │    │                │
│   indexerd)   │    │                │    │                │
│  Unix socket  │    │  sessions,     │    │  ~/.chronos-   │
│  tree-sitter  │    │  tool_calls,   │    │  code/skills/  │
│  SCIP fallback│    │  patterns,     │    │  mcp.json      │
│  Merkle diff  │    │  outcomes      │    │                │
└───────────────┘    └────────────────┘    └────────────────┘
```

**Two-binary shipping model:**
- `chronos-code` — the interactive CLI + TUI + agent runtime.
- `chronos-code-indexerd` — long-running per-repo indexer daemon. Started on first `cd` into a repo, persists across sessions, socket at `.chronos-code/indexer.sock`.

**Why two binaries:**
- Keeps the interactive binary <15 MB.
- Indexer can hold parsers, embeddings, hashes in memory — no cold start per session.
- Multiple TUIs (or subagents) share one index.
- The daemon can run under systemd/launchd if the user wants.

---

## 5. Capability specifications

### 5.1 Skills integration

**Resolution order (highest wins):**
1. Repo-local: `<repo>/.chronos-code/skills/*/SKILL.md`
2. User global: `~/.chronos-code/skills/*/SKILL.md`
3. Plugin-installed: `~/.chronos-code/plugins/*/skills/*/SKILL.md`
4. Bundled: skills embedded in the binary (small core set only)

**SKILL.md schema (front-matter):**
```yaml
---
name: python-testing
description: When to use pytest, uv, ruff for Python projects.
version: 1.0.0
triggers: [pytest, python test, uv, ruff]
model_hint: sonnet    # optional routing hint
tools_required: [read, write, bash]
---
# Body: markdown instructions the agent reads
```

**Selection:**
- On each turn, run a lightweight classifier (BM25 over triggers + description) against the current user message + last N tool calls.
- Top-K (default 3) skills are injected into the system prompt as `<skill name="…">…</skill>` blocks.
- Token budget: skills capped at 8k tokens total per turn; oldest drops first.
- **Do not** load all skills into context. That's Claude Code's biggest complaint from power users.

### 5.2 MCP server integration

**Transports:** stdio, SSE, streamable HTTP (spec-compliant).
**Config:** `~/.chronos-code/mcp.json` (user), `<repo>/.mcp.json` (project). Project overrides user.
**Lifecycle:** lazy-start on first tool call to that server. Health checks every 30s. Hot reload on config change (fsnotify).
**Permissions:** MCP tools inherit the calling agent's permission scope. A `require_confirmation: true` flag per tool for destructive operations.
**Discovery:** `/mcp` slash command lists connected servers with tool counts and health.

### 5.3 Enterprise Claude / Codex authentication

**This is the single most important feature for adoption.** Copy the OpenCode pattern.

**Claude auth chain (precedence high → low):**
1. `ANTHROPIC_AUTH_TOKEN` (gateway/proxy bearer)
2. `ANTHROPIC_API_KEY`
3. `CLAUDE_CODE_OAUTH_TOKEN` (long-lived, `claude setup-token`)
4. `~/.chronos-code/credentials.json` OAuth (chronos-code's own login)
5. `~/.claude/.credentials.json` reuse (if Claude Code installed)
6. Keychain (macOS) / libsecret (Linux) / DPAPI (Windows) lookup

**Codex auth chain:**
1. `CODEX_ACCESS_TOKEN`
2. `OPENAI_API_KEY`
3. `~/.chronos-code/codex-auth.json` OAuth
4. `~/.codex/auth.json` reuse
5. Device code fallback (`--device-auth`)

**OAuth PKCE flow (implement from scratch, small):**
- `chronos-code login anthropic` opens browser to Claude Console OAuth endpoint with PKCE challenge.
- Callback on `http://localhost:0` (random port).
- Token exchange, refresh token stored encrypted at rest (age or NaCl secretbox with OS-keyring master key).
- Refresh only in the 60-second pre-expiry window (matches Claude Code behavior; avoids double-rotation invalidating the other CLI's refresh token).

**Enterprise gateway support:**
- Config: `providers.anthropic.base_url = "https://internal-gateway.corp.com/claude"`
- Custom headers: `providers.anthropic.headers = {X-Team: platform}`
- Cert pinning optional.

**AWS Bedrock, GCP Vertex, Azure Foundry:** SDK-based (small footprint — use only the SDK's client, not full library). Behind a build tag if you need to trim further.

**Test matrix:** must work with Pro, Max, Team, Enterprise plan tokens for both providers.

### 5.4 AGENTS.md / CLAUDE.md loading

- On session start, walk from CWD up to repo root (git root), collect any `AGENTS.md`, `CLAUDE.md`, `AGENT.md`, `.cursorrules`, `.github/copilot-instructions.md`.
- Merge order: root → subdirs (subdirs override).
- Inject as `<project_instructions source="…">…</project_instructions>` at top of system prompt.
- Watch for changes via fsnotify; recompute on modify.
- Token budget: 16k. If over, summarize with a fast model (Haiku/gpt-5-nano) and cache the summary keyed by content hash.

### 5.5 Self-learning loop with local database

**Storage:** SQLite via `modernc.org/sqlite` (pure Go, no CGO). Path: `~/.chronos-code/memory.db`.

**Schema (core tables):**
```sql
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  repo_path TEXT NOT NULL,
  started_at INTEGER,
  ended_at INTEGER,
  model TEXT,
  turns INTEGER,
  input_tokens INTEGER,
  output_tokens INTEGER,
  cost_usd REAL
);

CREATE TABLE turns (
  id INTEGER PRIMARY KEY,
  session_id TEXT REFERENCES sessions(id),
  role TEXT,               -- user, assistant, tool
  content TEXT,
  tool_name TEXT,
  tool_input TEXT,
  tool_output TEXT,
  duration_ms INTEGER,
  ts INTEGER
);

CREATE TABLE outcomes (
  turn_id INTEGER REFERENCES turns(id),
  kind TEXT,               -- accepted, rejected, edited, reverted
  user_edit TEXT,          -- diff if edited
  ts INTEGER
);

CREATE TABLE patterns (
  id INTEGER PRIMARY KEY,
  repo_path TEXT,
  trigger_hash TEXT,       -- xxhash of normalized user query
  solution_summary TEXT,
  tool_sequence TEXT,      -- JSON array of tool names
  success_count INTEGER,
  fail_count INTEGER,
  last_used_at INTEGER,
  embedding BLOB           -- via sqlite-vec
);

CREATE VIRTUAL TABLE patterns_vec USING vec0(embedding float[384]);
```

**Learning signals:**
- **Accepted** — user did not edit or revert the edit within the session.
- **Rejected** — user reverted, undid, or explicitly said "that's wrong."
- **Edited** — user modified the diff; store the delta as the correction.

**Pattern extraction (offline job, runs on session end):**
1. Segment session into (user_turn, tool_calls, final_diff, outcome).
2. Compute trigger embedding (BGE-small or Model2Vec, <100 MB, run via ONNX in the indexer daemon or shell out to a tiny Rust sidecar).
3. Cluster similar triggers across sessions. On third+ occurrence with success rate >70%, promote to a **candidate skill**.
4. Prompt the user once: "You've handled X three times. Save as a skill?" (opt-in, never automatic write).

**Retrieval at query time:**
- Vector search over `patterns_vec` with top-K=5.
- Inject matched patterns as `<past_pattern success_rate="0.83">…</past_pattern>` in context.

**Why not Postgres:** ships as external dep, users hate it, kills the "single binary" story. If you need multi-user later, add Postgres behind an interface. Not now.

### 5.6 Code indexer

**This is where you can genuinely leapfrog Claude Code.** Claude Code has no indexer.

**Architecture: `chronos-code-indexerd`**

- **Parser layer:** `smacker/go-tree-sitter` or `alexaandru/go-sitter-forest` (both pure Go bindings; forest has more grammars).
- **Grammar shipping:** embed ~15 core grammars (Go, Rust, Python, TS/JS, Java, C/C++, C#, Ruby, PHP, Kotlin, Swift, Bash, SQL, YAML, HTML/CSS). Others lazy-downloaded to `~/.chronos-code/grammars/` on first encounter — keeps binary size down.
- **SCIP fallback:** for languages where a proper SCIP indexer exists (TypeScript, Python, Java, Go, Rust, C#, Ruby, Scala), auto-download the vendor binary to `~/.chronos-code/scip-tools/` and merge SCIP output with tree-sitter output. Compiler-grade cross-file resolution when possible; tree-sitter otherwise.

**Storage:** single SQLite file per repo at `.chronos-code/index.db`.

**Schema:**
```sql
CREATE TABLE files (
  path TEXT PRIMARY KEY,
  hash TEXT,             -- XXH3
  language TEXT,
  size INTEGER,
  parsed_at INTEGER
);

CREATE TABLE symbols (
  id INTEGER PRIMARY KEY,
  file TEXT REFERENCES files(path),
  name TEXT,
  kind TEXT,             -- function, class, method, var, type, interface
  signature TEXT,
  start_line INTEGER,
  end_line INTEGER,
  parent_id INTEGER,
  visibility TEXT,       -- public, private, internal
  doc TEXT
);
CREATE INDEX idx_symbols_name ON symbols(name);
CREATE INDEX idx_symbols_file ON symbols(file);

CREATE TABLE edges (
  src_id INTEGER REFERENCES symbols(id),
  dst_id INTEGER REFERENCES symbols(id),
  kind TEXT              -- calls, imports, implements, extends, references, defined_in
);
CREATE INDEX idx_edges_src ON edges(src_id, kind);
CREATE INDEX idx_edges_dst ON edges(dst_id, kind);

CREATE VIRTUAL TABLE symbols_fts USING fts5(name, doc, signature, content='symbols');
CREATE VIRTUAL TABLE chunks_vec USING vec0(embedding float[384]);
```

**Incremental indexing:**
- Build a Merkle tree over source files (XXH3 hash per file, per directory).
- On file change (fsnotify), re-hash affected files, walk up to update dir hashes, re-parse only changed files.
- For a 100k-line change of one file: <50ms to full re-index.
- Full re-index a 1M-line repo: target <30s on M-series Mac (parallelized parser workers).

**Tools exposed to the agent:**
```
codebase_search(query, top_k=10, mode=hybrid)     # BM25 + vector
codebase_symbol(name, kind?)                       # exact/fuzzy symbol lookup
codebase_callers(symbol)                           # who calls this
codebase_callees(symbol)                           # what this calls
codebase_impls(interface_name)                     # implementations
codebase_neighbors(symbol, depth=1)                # 1-hop graph
codebase_diff_summary(base, head)                  # semantic diff
```

**Communication:** Unix domain socket at `.chronos-code/indexer.sock`. Protocol: length-prefixed JSON or Cap'n Proto (smaller wire).

**Startup:**
- `chronos-code` launches the daemon on first repo open if not already running.
- Daemon idles at ~50 MB RAM for a mid-size repo, scales linearly.
- `chronos-code indexer status` / `stop` / `restart` for control.

### 5.7 Multi-agent capability with dynamic model assignment

**Agent definition (YAML — extend what's in `benchmarks/`):**
```yaml
name: refactorer
description: Multi-file refactoring specialist
model:
  primary: claude-opus-4.7
  fallback: claude-sonnet-4.6
  routing_hint:
    complexity: [high, medium]
    task_kinds: [refactor, rename, extract]
system_prompt: |
  You specialize in multi-file refactors. Always read all affected files first...
tools:
  allow: [read, write, edit, codebase_symbol, codebase_callers, codebase_impls, bash]
  deny: [git_push, npm_publish]
permissions:
  bash: confirm
  write: auto_in_repo
budget:
  max_turns: 20
  max_cost_usd: 5.0
```

**Router agent (built-in, not user-defined):**
- Input: user message + repo context summary + recent turn history.
- Classifier: small local model or a cheap remote call (Haiku, gpt-5-nano) that outputs `{task_kind, complexity, suggested_agent}`.
- Dispatches to the best-matching registered agent.
- Falls back to a default `general` agent if no match.
- User can override: `/agent refactorer` forces a specific agent.

**Handoff:**
- Agents share the working file set and indexer connection.
- Subagents get an isolated context window (Pi's pattern) — fresh conversation, parent passes only the task spec.
- Streaming: parent sees subagent tool calls in a collapsed section.

**Registry:**
- `~/.chronos-code/agents/*.yaml` (user)
- `<repo>/.chronos-code/agents/*.yaml` (project, overrides user)
- `chronos-code agents list`, `chronos-code agents test <name>`

### 5.8 Shell + permission model

**Three permission levels, per tool class:**

| Class | Default | Override |
|-------|---------|----------|
| read (file read, ls, grep, codebase_*) | auto | never blocked |
| write (edit, write, mv in repo) | auto-in-repo, confirm-out-of-repo | `--yolo` for auto everywhere |
| bash | confirm per command | `--auto-bash` with allowlist |
| network (fetch, curl) | confirm | allowlist per host |
| destructive (rm -rf, git push, npm publish) | always confirm | never auto |

**Per-command policies (in `.chronos-code/config.yaml`):**
```yaml
bash:
  auto_allow:
    - "^ls "
    - "^cat "
    - "^git status"
    - "^git diff"
    - "^go build"
    - "^go test"
    - "^npm test"
    - "^pytest"
  never_allow:
    - "rm -rf /"
    - "sudo "
    - "chmod 777"
    - "curl .* | (bash|sh)"
  confirm:
    - "git push"
    - "npm publish"
    - "docker "
```

**Sandboxing (optional, opt-in flag `--sandbox`):**
- Linux: `bubblewrap` — bind-mount repo read-write, everything else read-only, no network unless allowed.
- macOS: `sandbox-exec` with a generated profile.
- Windows: AppContainer or job objects (v1.1 — not launch).

**Timeout + output capture:**
- Default 2min per bash call, override per command.
- Stdout/stderr streamed to TUI, tail 500 lines kept in context.
- Long output → summarized before context injection (protects token budget).

### 5.9 World-class TUI

**Framework:** `charmbracelet/bubbletea` + `lipgloss` for styling.

**Non-negotiable properties (from Pi's critique of Claude Code):**
- Differential rendering — only redraw changed cells. Never full re-layout on state change.
- Frame budget <16ms. Measure it.
- Kitty keyboard protocol support (multi-modifier shortcuts, key release events).
- Inline images (Kitty and iTerm2 graphics protocols) for diagrams, screenshots.
- Wide-char (CJK, emoji) correct measurement.
- Works over SSH without flicker.

**Layout:**
```
┌────────────────────────────────────────────────────────────┐
│ chronos-code · claude-sonnet-4.6 · repo · agent: refactorer│
├────────────────────────────────────────────────────────────┤
│                                                            │
│  Conversation area (scrollback, markdown-rendered)         │
│  - collapsed tool calls with expand-on-click               │
│  - streaming assistant output                              │
│                                                            │
├────────────────────────────────────────────────────────────┤
│ ▸ Reading src/auth/oauth.go (line 1-120)                   │
│ ▸ Editing src/auth/oauth.go [+34/-12]                      │
├────────────────────────────────────────────────────────────┤
│ > _                                                        │
│   [Ctrl+A agent] [Ctrl+M model] [Ctrl+/ commands] [? help] │
└────────────────────────────────────────────────────────────┘
```

**Input handling:**
- `Enter` steers current run (interrupts remaining tool calls, delivers message).
- `Alt+Enter` queues follow-up (delivered after current turn completes).
- Multiline via `Shift+Enter`.
- Slash commands with fuzzy autocomplete: `/agent`, `/model`, `/skill`, `/mcp`, `/cost`, `/reset`, `/compact`, `/plan`, `/permissions`, `/session`.
- `@` for file/symbol references with fuzzy autocomplete from indexer.

**Modes:**
- Interactive TUI (default)
- Print mode: `chronos-code -p "fix the auth bug"` — headless, prints result, exits.
- JSON mode: `chronos-code -p --json` — event stream on stdout for scripting.
- RPC mode: `chronos-code --rpc` — JSON protocol on stdin/stdout for embedding.
- SDK: Go package `github.com/spawn08/chronos-code/sdk`.

### 5.10 Task-complexity model switching

**Classifier:**
- Input: user message + last-turn summary + repo language stats.
- Output: `{complexity: low|medium|high, kind: edit|refactor|debug|architect|explain}`.
- Implementation options (pick one, benchmark both):
  1. **Rule-based** — regex/keyword + turn history heuristics. Zero cost. Ship as baseline.
  2. **Cheap LLM** — Haiku 4.5 or gpt-5-nano with a 200-token prompt. ~$0.0001 per classification.

**Routing table (default, user-overridable):**

| Complexity × Kind | Model |
|-------------------|-------|
| low × edit | Haiku 4.5 / gpt-5-nano |
| low × explain | Haiku 4.5 |
| medium × any | Sonnet 4.6 / gpt-5 |
| high × refactor | Sonnet 4.6, escalate to Opus on failure |
| high × debug | Opus 4.7 / gpt-5 high |
| high × architect | Opus 4.7 with extended thinking |

**Escalation:** on tool failure or user "that's wrong", auto-escalate to next-tier model and retry with the failure context. Cap at 2 escalations per turn.

**Budget:** `chronos-code --budget 5.00` — hard USD cap per session, blocks calls that would exceed. Displays running cost in TUI.

---

## 6. Binary size strategy — hitting <20 MB

Reference: a stripped Go binary with tree-sitter and SQLite typically lands 30-50 MB. Getting to <20 MB requires discipline.

**Build flags (baseline):**
```bash
CGO_ENABLED=0 go build \
  -trimpath \
  -ldflags="-s -w -X main.version=$(git describe)" \
  -o chronos-code ./cmd/chronos-code
```

**Rules:**

1. **Pure Go SQLite.** `modernc.org/sqlite` (transpiled from C, ~4 MB) instead of `mattn/go-sqlite3` (needs CGO, hurts static build).
2. **Ship 15 tree-sitter grammars, lazy-download the rest.** Each grammar is 200-800 KB. Bundling all of them = 40+ MB dead weight.
3. **No embedded ML model in the main binary.** Embeddings live in the indexer daemon, which can pull an ONNX model on first index. Alternative: hashed-BM25 only (no embeddings) as a build flag for the ultra-small variant.
4. **Cloud SDKs behind build tags.** `go build -tags "aws vertex azure"` for enterprise builds; default build has none.
5. **No UPX.** Startup delay (24ms → 300ms per one benchmark) is unacceptable for a REPL-style tool. Users notice.
6. **No `github.com/aws/aws-sdk-go` (v1).** It's 15+ MB alone. Use `aws-sdk-go-v2` with only the specific service clients you need (bedrockruntime).
7. **Vendor only what you use.** Regularly run `go tool nm -size chronos-code | sort -n | tail -50` to find bloat.
8. **Provider clients:** hand-written HTTP for Anthropic and OpenAI. Don't pull the OpenAI Go SDK for one endpoint.
9. **JSON:** stick with encoding/json. `sonic` is faster but larger.
10. **Logging:** `log/slog` (stdlib), not `zerolog`+`zap`+etc.

**Realistic budget:**

| Component | Size |
|-----------|------|
| Go runtime | 2.0 MB |
| stdlib pieces used | 3.5 MB |
| Chronos core | 1.5 MB |
| Agent runtime + tools | 1.0 MB |
| TUI (Bubble Tea + Lipgloss) | 1.2 MB |
| Providers (Anthropic + OpenAI HTTP) | 0.3 MB |
| SQLite (modernc) | 4.0 MB |
| Tree-sitter core + 15 grammars | 3.5 MB |
| MCP client (JSON-RPC) | 0.4 MB |
| OAuth + keyring | 0.6 MB |
| Misc (yaml, fsnotify, xxhash) | 0.5 MB |
| **Target** | **~18.5 MB** |

Indexer daemon can be another 15-20 MB (it holds parsers, embeddings model, etc.), shipped separately.

**Enforcement:** CI job fails PR if `chronos-code` binary exceeds 20 MB on Linux/amd64 stripped.

---

## 7. Non-functional requirements

| NFR | Target |
|-----|--------|
| Binary size (chronos-code) | ≤20 MB stripped, Linux amd64 CGO_ENABLED=0 |
| Binary size (indexer daemon) | ≤25 MB |
| Cold TUI startup | <150ms to first paint |
| First-token latency (Sonnet) | <1.5s p50 |
| Indexer full-index 1M LOC repo | <30s on M-series Mac |
| Indexer incremental (1 file change), `chronos-code-indexerd` daemon (§5.6, not yet built) | <100ms |
| Indexer incremental (1 file change), current in-process indexer (built in plan_04, go/packages-based, no daemon) | ~0ms if the file's content hash is unchanged since the last pass; otherwise bounded by `go/packages.Load`'s own fixed process/module-resolution cost per changed package (~230ms+ measured on Apple M1 Pro), independent of file or repo size. Hitting the daemon's <100ms figure for an actually-changed file requires the warm in-process cache only a persistent daemon (or equivalent) can provide — see plan_04's Re-plan Note in `.ppd/chronos-code-v1/plans/plan_04.md`. |
| `codebase_search` query | <100ms p95 |
| Memory (TUI idle) | <80 MB |
| Memory (indexer, 1M LOC) | <500 MB |
| Crash recovery | Session resumable from checkpoint (Chronos already has this) |
| Offline capability | Local indexer works fully offline; only LLM calls need net |
| Supported OS | Linux (amd64, arm64), macOS (arm64, amd64), Windows (amd64) |
| Terminal compat | xterm-256color, kitty, wezterm, iTerm2, Windows Terminal, tmux |

---

## 8. Milestones

**M0 — Foundation reset (2 weeks)**
- Fork current chronos-code into a clean structure: `cmd/`, `internal/agent/`, `internal/tui/`, `internal/indexer/`, `internal/auth/`, `internal/mcp/`, `internal/skills/`, `internal/memory/`.
- CI with binary-size gate.
- Decide: keep or drop current TUI. If current TUI is Bubble Tea already, refactor; if not, replace.

**M1 — Auth + provider parity (3 weeks)**
- Anthropic OAuth PKCE + credential reuse from `~/.claude/`.
- Codex OAuth + reuse from `~/.codex/`.
- API key paths, enterprise gateway headers, Bedrock/Vertex (build-tag optional).
- `login`, `logout`, `whoami`, `providers` commands.
- **Ship as v0.6 alpha here — enterprise auth alone gets people trying it.**

**M2 — Indexer daemon (4 weeks)**
- Tree-sitter integration, 15 core grammars embedded.
- SQLite schema, Merkle-diff incremental, fsnotify watcher.
- Unix socket protocol, tool bindings (`codebase_search`, `codebase_symbol`, `codebase_callers`, `codebase_callees`).
- Benchmark against Sourcegraph on the top 20 Go/TS/Python repos on GitHub.

**M3 — TUI overhaul (3 weeks)**
- Bubble Tea rebuild with differential rendering.
- Kitty keyboard, inline images, ANSI markdown renderer.
- Slash commands, `@` mentions with autocomplete from indexer.
- Steering (`Enter`) and queued follow-up (`Alt+Enter`).

**M4 — Multi-agent + routing (3 weeks)**
- Router agent + complexity classifier.
- YAML agent definitions + registry.
- Subagent spawning with isolated context.
- `/agent`, `/model` commands.

**M5 — Skills + MCP (2 weeks)**
- Skill discovery, selection, token budgeting.
- MCP stdio/SSE/HTTP with hot reload.
- AGENTS.md / CLAUDE.md loader with fsnotify.

**M6 — Self-learning loop (3 weeks)**
- Session/turn/outcome logging.
- Pattern extraction offline job.
- Skill promotion flow (opt-in prompt).
- `chronos-code memory stats` command.

**M7 — Permissions + sandbox (2 weeks)**
- Tiered permission model.
- Per-command allowlists.
- Optional bwrap/sandbox-exec integration.

**M8 — Polish + benchmarks (2 weeks)**
- Run TerminalBench, SWE-bench Verified, publish scores.
- Docs site (start with just a README + man pages; don't build a docs product yet).
- 1.0 release.

**Total: ~24 weeks (6 months) to 1.0 with one engineer.** Parallelize M2 and M3 if you can afford two.

---

## 9. Success metrics

**Adoption (leading):**
- GitHub stars: 1k in 3 months post-1.0 (bar for a credible OSS coding agent).
- Weekly active users (opt-in telemetry): 500 in 6 months.
- Enterprise credential reuse: >60% of sessions use OAuth (proves the auth wedge works).

**Quality (leading):**
- TerminalBench score with Sonnet 4.6: top 5 open-source harnesses.
- SWE-bench Verified: >30% resolution rate (competitive with mid-tier harnesses).
- Cost per resolved task: <$0.20 on average across the benchmark (proves the routing wedge works).

**Product (lagging):**
- User-reported skill promotions per week (proves learning loop works).
- Median session cost 40% below flat-Sonnet baseline (proves routing works).
- Indexer P95 query latency <100ms across the top-20-repo benchmark.

---

## 10. Explicit rejects (do not build in v1)

- Web UI. Terminal only.
- Hosted mode / cloud sync. Local only.
- Fine-tuning. Retrieval only.
- Voice mode.
- Plan mode as a first-class feature (let it emerge from the router agent).
- Multi-user collaboration.
- IDE extensions (leave to the community; expose the SDK and let them build).
- Windows sandbox. macOS/Linux only for v1.

---

## 11. Risks and open questions

1. **Anthropic ToS.** Reusing Claude Code OAuth credentials in a third-party tool is a gray area. OpenCode's plugins do it; Anthropic hasn't shut them down. But if you go big, this could break. Mitigation: primary flow is chronos-code's own OAuth against the Claude Console (fully sanctioned); credential-reuse is a convenience for existing Claude Code users, clearly labeled.
2. **Codex OAuth similarly.** OpenAI has been quieter about this; same mitigation applies.
3. **Tree-sitter grammar quality varies.** Some grammars (Kotlin, Swift) are less mature than others. Where SCIP indexers exist, prefer them.
4. **Chronos framework readiness.** If Chronos itself has gaps (checkpointing edge cases, provider abstraction limits), those become chronos-code's problems. Recommend a companion Chronos v0.6 that fixes anything blocking.
5. **Single-engineer velocity.** 24-week plan assumes focus. Realistic if you're not doing this weekends-only.

---

## 12. Decision points for you

Before starting, decide:

1. **TUI framework:** Bubble Tea (recommended) vs. hand-rolled? Bubble Tea unless you have a specific reason. - Go with Bubble Tea as long as it produces a world class TUI
2. **Indexer language:** Pure Go (recommended for shipping story) vs. a Rust sidecar (faster but adds build complexity)? - Go with Go only but must support most of the languages
3. **Embeddings:** Ship them (adds 100-400 MB to daemon), or BM25-only for v1? 
Retrieval: BM25 (FTS5) as the primary retriever. Dense (opt-in hybrid): minishlab/potion-code-16M-v2 (static, ~16 MB, pure Go inference) ships in the daemon. Hybrid merge via Reciprocal Rank Fusion (k=60). Auto-enabled for repos dominated by Python/Java/JS/TS/Go/PHP/Ruby; auto-disabled otherwise with a prompt to configure a Tier 3 provider (Ollama nomic-embed-code, Voyage code-3, or Jina code v2).
4. **Postgres vs. SQLite for memory:** Recommend SQLite. Kill the Postgres plan unless there's a specific reason. - Approved for SQLite only
5. **License:** MIT/Apache-2.0 for reach, AGPL if you plan to commercialize a hosted version later.