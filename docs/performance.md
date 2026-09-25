# Tool latency and cost accounting

## Implemented optimizations

- **Concurrent bounded reads/searches.** `incctx.Wrap` and `WrapGrep` mark
  their implementations parallel-safe; `read_stored_result` is also safe.
  The SDK overlaps an all-safe batch with its existing concurrency limit
  (`MaxConcurrentSubAgents`, default 5), and returns results in request order.
  A mixed batch containing a write or another non-safe tool remains serial.
  Configured pre/post tool hooks disable this optimization because those
  commands can mutate shared state. Approval, security, evidence, and result
  compression still run for every call.
- **Allocation-light scanning.** Reads and searches borrow buffered byte
  slices. Skipped lines allocate nothing; retained matches and source output
  are copied. A reader pool avoids a new 64 KiB buffer per file. Cancellation,
  binary detection, traversal limits, scan budgets, and continuation semantics
  are preserved. This caches buffers, not filesystem contents.
- **Incremental token counting.** An agent's context guard caches counts by
  exact text and model identity. Session and delivery budget hooks reuse that
  counter against the current request. Unchanged history and tool schemas
  avoid repeated BPE work; edits, trimming, and model overrides are counted
  correctly. Each cache retains at most 8 MiB of key text, 2,048 entries, and
  one tokenizer; individual entries over 1 MiB bypass retention. Tool result
  compression has its own bounded cache. Tiny JSON results bypass tokenization
  when their byte length already proves they fit the token threshold.

These local token-count caches do not imply a provider prompt-cache hit or
change the provider-reported usage used to calculate actual spend.

### Measurements

Offline microbenchmarks on an Apple M1 Pro, three runs before and after the
changes; values below are medians. The filesystem was warm.

| Operation | Before | After | Allocation count before → after |
|---|---:|---:|---:|
| Literal no-match search, 100,000 lines / 4.9 MB | 10.01 ms | 8.10 ms | 100,008 → 7 |
| Read lines 90,000–90,010 of that file | 3.43 ms | 1.87 ms | 90,029 → 18 |
| Repeated context preflight, ~130 KB transcript | 24.07 ms | 0.00935 ms | 174,546 → 5 |

The last row measures unchanged-text reuse after cache warm-up, amortizing the
first fill. A cold or substantially changed transcript still incurs BPE work.
These are local component improvements, **not an end-to-end model speedup**.
Concurrency is verified by an SDK integration test requiring both operations
to start before either can finish, rather than a timing-sensitive assertion.

Reproduce:

```sh
go test ./internal/incctx ./internal/orchestrator -run '^$' -bench 'Benchmark(FileOperations|ContextPreflight)$' -benchmem -count=3
go test ./... -race -count=1
```

## Why an answer can take time to appear

There are distinct critical paths:

1. **Process startup:** `orchestrator.New` builds agents, initializes storage,
   indexes the graph (`workspace.index_on_start: true` by default), detects
   the workspace, and initializes integrations. MCP startup can wait for
   connections. Project-document rendering can call a summarizer when one is
   configured and the documents exceed its budget. The models.dev network
   refresh is already asynchronous.
2. **Before the first main-agent request:** deterministic routing can fall back
   to a separate T1 model classifier; prompt preparation loads selected context;
   session restoration/compaction and preflight also cost time.
3. **At the provider:** queueing, network time, processing the prompt, and
   generation/reasoning precede useful output. The bundled `strategy: cot`
   adds a reasoning instruction; it is not a separate local model-call loop.
   Bundled `native: false` disables explicit native-reasoning configuration,
   although individual model/provider behavior can differ.
4. **During execution:** every dependent tool round waits for a model response,
   tool execution, persistence/hooks, then another model call. Repairs and
   delegated work can add calls. Streaming is enabled by default, but cannot
   display provider text before it arrives.

A useful approximation is:

```
turn latency ≈ preparation + Σ(provider wait/generation + tool critical path + local hooks/persistence)
```

The benchmarks show removable local costs in milliseconds. They do not establish
the cause of a particular multi-second delay without a live trace. For that,
measure startup stages, classifier time, preflight, provider time-to-first-token,
tool queue/execution time, and total model rounds separately.

## Next design: revision-scoped evidence packets

The largest further gain is likely to come from **eliminating model round trips**.
Build on the existing request-scoped graph and `codebase_context` tooling:

1. **Fuse evidence acquisition.** One structured lookup asks for a symbol,
   bounded implementation ranges, relevant callers, and associated tests.
   Resolve and collect these locally, concurrently where independent, then
   return one bounded packet with source coordinates and coverage metadata.
   This replaces model-mediated `search → inspect → callers → tests` chains.
2. **Bind packets to revisions and workspace identity.** Include canonical root,
   content hashes, ranges, and graph revision. Invalidate only affected packets
   after edits. Keep retained data bounded. Do not omit requested source merely
   because it was read earlier: compaction may have removed it from model context.
3. **Schedule read groups around mutation barriers.** Extend the SDK's current
   all-or-nothing scheduler to run consecutive safe read groups in parallel,
   with writes/shell/unknown tools as barriers. Only a later path-conflict-aware
   scheduler should overlap proven-disjoint writes; preserve journal and undo
   ordering, cancellation, and output order.
4. **Keep useful evidence in the first result.** Current compression can evict a
   deliberately requested range, prompting `read_stored_result` and another
   model round. Allocate the available context budget to the requested ranges
   first, with handles for optional evidence. Compare latency and token spend
   together: a larger useful first response can be cheaper than multiple rounds.

For illustration, removing three dependent model round trips averaging 1.5 s
would save about 4.5 s, far more than shaving a few milliseconds from a scan.
This is a sizing example, not a measured result. Validate the design with
fixed tasks, unchanged correctness/evidence requirements, cold/warm p50/p95
latency, model-round counts, and provider-reported cost.

Startup can separately become progressive: render the interface first, load
an existing graph snapshot, and refresh in the background. Graph tools must
still await verified freshness or explicitly fall back to source. This needs
lifecycle/readiness handling; merely putting startup in a goroutine is unsafe.

## Is the TUI cost cache-aware?

**Yes, for calls recorded by the session budget hook.**

- OpenAI Chat Completions/Responses and Gemini mark cached reads as included
  in prompt tokens; `Usage.UncachedPromptTokens` subtracts those hits before
  applying full input pricing. Azure's OpenAI path uses the same convention.
- Anthropic reports uncached input, cache reads, and cache creation separately.
  Five-minute and one-hour cache writes have separate rates; one-hour writes
  are a subset of total cache-creation tokens, not an additional token count.
- `budget.ModelPrice.costWithCache` applies per-model rates and long-context
  tiers to the full prompt window. Actual cost is rounded to microdollars;
  the TUI renders four decimal places.
- Pre-call reservations conservatively assume full-price input. After the
  response, reconciliation replaces the reservation with cache-aware spend.
  The TUI displays reconciled spend, not the outstanding reservation.

At the bundled Sonnet 4.6 rates, an illustrative call with 400 uncached input,
12,000 cache reads, 80 five-minute cache writes, and 20 output tokens costs:

```
(400×3 + 12,000×0.3 + 80×3.75 + 20×15) / 1,000,000 = $0.0054
```

The price table resolves bundled rates, then models.dev catalog rates, then
user/project `pricing.yaml` overlays. Missing cache rates default to full input
price. A missing model price is reported as `unpriced` or `≥$…`, not free.
Use `/usage` for the cache read/write breakdown.

### Accuracy boundaries found in the audit

- T1 classification (`internal/router/t1.go`) and project-document summarization
  (`projectDocsSummarizer`) call providers directly, outside the session budget
  hook. Their costs are absent, including from the unpriced-call count.
- The session cost tracker is in memory; starting a new process does not rebuild
  historical spend from a resumed session's ledger.
- Missing provider usage, failed/interrupted calls, provider-internal retries,
  custom deployment pricing, and negotiated/non-token charges can differ from
  the locally recorded total. The current hook releases reservations on errors.

Thus the display is a **cache-aware estimate of recorded calls**, not a complete
provider invoice. Full accounting requires a shared provider-level usage recorder
for main-agent and helper calls, with persisted call IDs, retry/error usage, and
session restoration. The performance changes above do not alter billing math.
