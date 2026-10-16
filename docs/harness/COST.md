# Cost-first context engineering

_Part of [HARNESS-SPEC.md](HARNESS-SPEC.md) (§3). Goal: complete a task at a **fraction** of
today's cost, and below Cursor or Claude Code, without capping how long the agent runs._

## 1. Where the money goes today (measured, verified)

The data covers every chronos-code model call on quant-trade from Sep 22 to Oct 15: 5,926 calls,
of which 5,829 are priced.
- **Prices:** each call is priced at its own model's list rate, checked against the official
  Anthropic pricing page on 2026-10-15 and models.dev for GPT.
- **Usage accounting:** `input_tokens` in traces is already the uncached share
  (`chronos/sdk/agent/agent.go:745`), so cache tokens are not double-counted.
- **Reproduce:** `analysis/cost_breakdown.py`.

| Component | Cost | Share |
|---|---|---|
| **Cache writes** | **$238.0** | **56.8%** |
| Output | $85.6 | 20.4% |
| Cache reads | $77.4 | 18.5% |
| Uncached input | $17.9 | 4.3% |
| **Total** | **$418.8** | |

| Model | Calls | Cost | of which cache writes |
|---|---|---|---|
| claude-opus-5-5 | 2,322 | $213.9 | $139.8 |
| claude-fable-5-1 | 531 | $76.3 | $47.2 |
| claude-sonnet-5-5 | 1,282 | $42.2 | $23.7 |
| claude-sonnet-4-6 | 674 | $42.0 | $25.4 |
| gpt-6-astra | 371 | $34.7 | $1.3 |
| gpt-6.1-sol | 603 | $9.7 | $0.6 |
| claude-haiku-5-5 | 46 | $0.1 | — |
| *unpriced: gpt-6-sol (81), claude-haiku-4-5 (16)* | 97 | — | |

The figure is accurate for API list prices. Two caveats:
- Bedrock, Vertex and Azure pricing can differ from list.
- The 97 unpriced calls are excluded.

Prompt size per call is 60k tokens at p50 and 100k at p90. The first call of a session is
already 12.4k tokens at p50.

### 1.0 What chronos-code itself reports is wrong
| Where | Bug | Effect |
|---|---|---|
| `telemetry.db` `sessions.cost_usd` | never written: the UPDATE at `internal/learning/telemetry.go:395` sets only turns and tokens | always `$0` |
| `telemetry.db` `input_tokens` | adds the uncached share only | under-reports prompt volume about 90× (no cache read or write tokens) |
| TUI and budget (`internal/budget`) | correct formula (`costWithCache`, per-model and per-tier rates); prices in `pricing.yaml` match the official page for every Anthropic model checked | correct, **but** `gpt-6-sol` and `claude-haiku-4-5` (with a provider prefix) fall to "unpriced" |
| Session status | never closed | the cost for each session can't be aggregated reliably |

Fix: write the `budget.IncurredCost` microdollars and all four token classes into telemetry for
every call, close sessions, add the missing price entries, and add a test asserting that telemetry
cost equals the sum of budget costs.

### 1.1 The single biggest leak: full-prompt cache rewrites near the trim limit
- In 394 calls the model rewrote more than 80% of a prompt that was otherwise cached. That is
  **36.5 M tokens, 75% of all cache-write tokens, $173 at real per-model rates (41% of all spend)**.
- 317 of those rewrites happened **mid-turn, between tool rounds**. Only 2 followed a compaction,
  and 65 came at the start of a user turn (`analysis/cache_rewrites_by_phase.py`). In 249 the cache
  was still warm (under 5 minutes since the last call), so the rewrites are not TTL expiry.
- The prompt size stays the same while the cached content changes underneath it:

| Prompt size | Calls | In-place full rewrites |
|---|---|---|
| < 100k | 4,664 | ≤1% |
| 100–125k | 593 | 9% |
| 125–150k | 411 | 11% |
| 150–175k | 246 | **43%** |
| 175–200k | 12 | **92%** |

- **Root cause:** compaction is configured against `max_tokens: 128000` at
  `summarize_threshold: 0.45`, while the context guard trims against the model's real window
  (`internal/orchestrator/context_guard.go:188-229`).
  - Once a conversation reaches about 150k tokens, every round re-trims the middle of the history.
  - `reuseTrim` only reuses a trim when the prefix hash matches and the result still fits the
    limit, so as the tail grows each round fails that check and a new trim is cut.
  - On Anthropic thinking models the trim also strips thinking blocks (`StripAnthropicThinking`).
  - The whole prompt then becomes a fresh cache write at 1.25× the input price, round after round.
- About half of all spend (49%) is on calls whose prompt exceeds 100k tokens.

### 1.2 Other leaks
| Leak | Evidence | Cost driver |
|---|---|---|
| Recompute re-fetches | 290 rounds that only called `read_stored_result` | each round re-reads the whole prefix, plus output, plus about 5 s |
| Duplicate reads | one file read 10× in a session; 36% of subagent reads duplicate the parent's | tokens stay in context for every later call |
| Verification-repair turns | 4 of 7 sessions; repeated `cargo test` and `npm test` | extra rounds over a 100k+ prefix |
| Subagents | 17% of spend happens inside subagent windows | each child pays its own uncached first write and re-reads files |
| Nested, stale `<turn_context>` | up to 14 levels deep | prefix bloat that is carried forever |
| Tool schemas | about 25 tools pinned in every request | about 6–10k tokens in every prefix |
| Model mixing | opus (2.3k calls), sonnet, fable and gpt in the same sessions | each switch rewrites the cache from zero |

## 2. Cost model the harness optimises

```
cost/task ≈ Σ_calls [ prefix_tokens·0.1  +  new_tokens·1.25  +  output·5 ] × input_price
```
- `prefix_tokens` is the number of cache-read tokens, at 0.1×.
- `new_tokens` is the number of cache-write tokens, at 1.25×.
- Output tokens cost 5× input.

That gives three levers, in order of size:
1. **Never rewrite what was cached.** The history must be append-only and the prefix
   byte-identical. At the simplified 1.25× write / 0.1× read rates above, rewriting 150k
   tokens costs 187.5k input-price units. A cached round that reads the same 150k prefix
   and writes 150 new tokens costs about 15,187.5 units **before output**, so the rewrite
   costs about 12 such rounds, not 1,250. Output cost reduces that equivalence further;
   one-hour cache writes use a different rate. This is illustrative arithmetic, not a
   measured per-task saving.
2. **Keep the context small.** Read cost grows with context size × number of calls, so a 40k
   working set is 3–4× cheaper per call than 150k.
3. **Use fewer calls and fewer output tokens.** Batch tool calls, read each file once, avoid
   ceremony, and keep final answers concise.

## 3. Design

### 3.1 Append-only context with deterministic, block-aligned compaction
- **Make the window decision once and correctly.** Compaction targets the model's served window
  (200k, or 1M where served), not a 128k default. The context guard is only a safety net and must
  never fire during normal operation.
- **Clear tool results at fixed breakpoints instead of trimming every round.** When the context
  reaches about 60% of the window, replace old tool results with stubs in one batch:
  `[result of read research/recommend.py:120-180 @a3f9, round 14 — re-read if needed]`.
  - Clear the oldest results first, and clear enough to drop to about 35%. The cache is rewritten
    **once**, and the next ~40% of growth is pure appends.
  - This mirrors Anthropic's server-side context editing (`clear_tool_uses`, keeping the last N
    results) and should use that API where it is available.
- **Compact rarely, at about 80%,** into a structured state block (template in HARNESS-SPEC §3.3).
  The block is placed directly after the static prefix, so the static prefix stays cached.
- **Hysteresis.** Never trim or clear again until context grows by at least a set amount (for
  example 25% of the window). This replaces today's re-trim on every round above 150k.
- **Thinking blocks.** Clearing tool results is designed so that it does not require stripping
  thinking. Use the API's `clear_thinking` and keep-recent options rather than editing earlier
  turns ourselves.

### 3.2 Three-zone, byte-stable prompt layout
```
Zone A  STATIC  (1h TTL, shared across sessions/users of the same version)
        core tool schemas (9) · system prompt · (no dates, no model names, no paths)
Zone B  SESSION (5m/1h TTL, changes ≤ once per session)
        project instructions · skill catalog · environment banner (date, OS, langs, git branch)
Zone C  CONVERSATION (append-only; rolling breakpoints on the last 2 blocks)
        user turns · assistant turns · tool results (stubs once cleared)
Tail    UNCACHED (rebuilt per call, after the last breakpoint, ≤500 tokens)
        todo state · background shells/tasks · futility steer · turn-scoped recall
```
- **Zone A must be byte-identical across sessions.** Today the banner puts the date and model name
  into the system blocks (`orchestrator.go:997-1020`), which makes them per-session. Move both to
  Zone B.
- **Determinism tests.** Golden tests assert the same bytes for the same inputs: sorted tool
  lists, sorted JSON keys, sorted skill catalog, no map-iteration order and no timestamps in
  Zones A and B.
- **Cache-break detector.** On every call, compare the cache-write tokens against the expected new
  tokens. If writes exceed new tokens by more than 2k, log a `cache_break` event naming the first
  zone or block whose hash changed. Cache misses become visible regressions instead of silent cost.

### 3.3 Small working set, retrieved just in time
- **Lazy tool loading.** Pin 9 core tools and load the rest (graph, memory, web, MCP, LSP) through
  `tool_search`. On Anthropic models use `defer_loading` (−85% schema tokens in Anthropic's data).
- **Bounded results.**
  - Every tool result has a 10k-token head+tail budget with a narrowing hint, and nothing below
    that budget is compressed.
  - Reads default to about 250 lines, or a symbol-scoped slice via `code_search`
    (`include: source`), not whole files. Large files return an outline in every language.
  - Grep defaults to `files_with_matches`.
- **Read cache with dedup.** A re-read of an unchanged range returns the stub `unchanged since
  round N`, which costs nothing.
- **Prefer index answers to raw text.** A `find_callers` result is about 300 tokens, while reading
  three files to find callers is about 9k.
- **No prefetch by default.** The `[Repository context]` block is fetched only for navigation-heavy
  queries, and its size is set by the same budget.

### 3.4 Model routing for cost (no quality loss)
- **Stay on one model per conversation.** A switch is a full cache rewrite. Route at the task
  boundary, never mid-conversation.
- **Use cheap models for side work** that does not touch the main conversation's cache:
  explore/search subagents, compaction summaries, routing classification, commit messages and web
  summarisation go to a Haiku-class model (about 1/3–1/5 the price).
- **Adaptive effort.** Read-only answers and mechanical edits run at low effort; debugging and
  design at high effort. Output tokens are 5× input, so this is a major lever.
- **Main model.** Default to Sonnet-class and escalate to Opus-class only after futility rung 2
  (HARNESS-SPEC §1.5) or when the user asks.

### 3.5 Subagents that are cheap by construction
- A subagent is a new conversation, so it pays a fresh first write. Spawn one only when its raw
  exploration would otherwise sit in the parent's context for many later calls; that is when
  isolation saves money.
- **Share Zone A** across parent and children: identical tool and system bytes per agent type, so
  the static prefix is a cache hit.
- **Handoff packet** instead of re-reading files (HARNESS-SPEC §4), and the child returns a
  structured result of at most about 1–2k tokens, not prose.
- **Default `explore` to the cheap model.**

### 3.6 Fewer calls and fewer output tokens
- Batch independent calls (one round instead of N) and use multi-edit (one call per file).
- Verification proportional to the change, with no re-runs on an unchanged tree. Diagnostics after
  an edit replace a full test run as the inner loop.
- Concise output: progress text is one line before a tool batch, and the final answer leads with
  the outcome with no recap. Never produce a duplicate summary after a repair.
- Futility ladder (HARNESS-SPEC §1.4–1.5): duplicate tool calls are answered from cache and spins
  are stopped early. Runs stay unbounded in time, while futile spend falls towards zero.

## 4. Targets (gate in eval, §7 of the spec)

| Metric | Today (quant-trade) | Target |
|---|---|---|
| Cache-write share of spend | 57% | **≤15%** |
| In-place full-prompt rewrites | 394 (6.6% of calls; 43–92% above 150k) | **≈0** (only at planned clear/compact events) |
| Cache hit ratio (read ÷ (read+write+input)) | 87% | **≥97%** |
| Median prompt per call | 60k | **≤35k** |
| First-call prompt | 12.4k | **≤6k** (lazy tools, lean prompt) |
| `read_stored_result`-only rounds | 290 | **0** |
| Cost per resolved task vs today | 1.0× | **≤0.35×** |
| Cost per resolved task vs Claude Code / Cursor on the same tasks | — | **≤0.7×** at equal resolve rate |

Estimated effect of each lever on today's $419:

| Lever | Saving | How |
|---|---|---|
| Rewrite fix (§3.1) | **≈$150–165** | removes most of the 36.5M rewrite tokens |
| Smaller working set (§3.3) | ≈30–40% of reads and new writes | |
| Cheap side models and effort (§3.4) | ≈15–25% of output | |
| Fewer calls (§3.6) | ≈20–30% of rounds | |
| **Combined** | **≈60–70%** | |

## 5. Implementation order (cost track)
1. **Cache-break detector and cost telemetry.** Fill `cost_usd` from `budget.IncurredCost` (§1.0), record the
   cache_break event, and add a per-session cost line to the TUI. *Measure first.*
2. **Fix compaction thresholds** to use the served window, and add tool-result clearing with
   hysteresis. Ship this behind a flag and A/B it on replayed sessions. This is the biggest saving.
3. **Zone A/B/C layout:** move the date and model name out of Zone A and add determinism golden
   tests.
4. **Bounded results:** no compression under 10k tokens, read dedup stubs, grep files-only default.
5. **Lazy tools** with `tool_search` / `defer_loading`.
6. **Cost routing:** one model per conversation, Haiku for side work, adaptive effort.
7. **Subagent cost:** shared Zone A, handoff packet, structured small returns.
