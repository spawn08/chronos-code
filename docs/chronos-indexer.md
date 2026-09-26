# Chronos indexer: design

Status: M5 complete (2026-09-25): agents' graph tools are served by the
chronos indexer, `codebase_context` and per-turn prefetch use graph
retrieval, and the store, reconcile and watcher are built for million-file
repositories. M4 (precise tier) is still open. M6 complete (2026-09-26):
parser runtime decided and wrapped (`extract/treesitter`, `extract/packs`);
facts v2 and segment format v4 done; query packs for all 17 languages are
indexed (M6.3); the generic resolver, type uses and binding hints are in
(M6.4); the graph tools answer from resolved edges and detect tests in
every language (M6.5); the SCIP baseline covers Go, Python, TypeScript,
Rust, Java, Kotlin, C, C++ and C#. M7 (language resolvers) is next.
Replaces the synchronous parts of `internal/graph` with an in-process
indexer.

## Goals

1. An edit becomes visible to queries in < 50 ms. Type-checking never blocks an
   edit or a query.
2. The index is usable < 1.5 s after a first build on this repo. Type-checked
   facts arrive later in the background.
3. Warm queries: exact symbol < 1 ms, callers at depth 3 < 5 ms, uncached
   `codebase_context` (1,500 tokens) < 15 ms.
4. No external binary, no MCP, no dependency on another repository. `CGO_ENABLED=0`
   release builds keep working.
5. Works without a Go toolchain on `PATH`, in syntactic-only mode.
6. Every result reports how reliable it is (`type_checked`, `import_resolved`,
   `name_matched`, `ambiguous`) and whether the index is fresh.

Non-goals for v1: languages other than Go (decided at M5), embeddings/vector
search, million-file scale, persistent Git history.

Update 2026-09-25: the plan after M2 widens this scope to every common
language, graph-based retrieval and million-file monorepos. Embeddings stay
out, for code and for documents. See "Plan after M2" below.

## Why the current graph is slow

References are to the current working tree.

| Cause | Where |
|---|---|
| `packages.Load` (runs `go list`, loads export data) on every edit | `internal/graph/indexer.go:710`, `:724` |
| First tool call after an edit type-checks inline | `internal/graph/request_scope.go:138-187` |
| Call edges resolved and stored by name, so a removed or changed declaration forces reverse importers to reload | `indexer.go:906-1036` |
| Work across the whole repo per update: full-table `PruneStaleEdges`, a second certification scan, reloading every package with types after a structural change | `store.go:532`, `indexer.go:714-836` |
| Synchronous `IndexAll` at startup | `internal/orchestrator/orchestrator.go` (`setupGraph`) |
| Released binaries index Go only (`CGO_ENABLED=0`, no `treesitter` tag) | `.github/workflows/release.yml` |

## Architecture

```
fsnotify ─► dirty set ─► worker pool ─────────► single writer ─► overlay segment ─► manifest publish
            (debounce,    (parse once,           (assemble,        (immutable)        (atomic rename)
             batch cap)    extract facts)         intern, sort)
                                                                          │
queries ◄── snapshot (mmap'd segments + in-memory overlay routing) ◄──────┘
    ▲
    └── precise tier (background): go/packages type-check ─► precise segment
```

Package layout (new):

```
internal/indexer/
  scan/      discovery, ignore rules, stat/hash cache, dirty-set management
  extract/   per-language extractors; extract/golang uses go/parser
  segment/   on-disk format: writer, reader, checksums, mmap
  store/     manifest, generations, overlays, compaction, writer lock
  query/     symbols, calls, search, map, impact; resolution at query time
  context/   evidence selection, budgeting, seen-subtraction, memoization
  precise/   background go/packages enrichment
  engine.go  lifecycle: Open, Start, Notify, Snapshot, Close, Status
```

`internal/graph` tools are moved onto `query/` in M2; the old store/indexer is
deleted in M6.

## Data model

Facts are extracted **once per file from one parse**.

| Record | Fields |
|---|---|
| File | path (root-relative), language, size, mtime_ns, xxh64 content hash, parse status |
| Symbol | name, kind, receiver, package path, file, start/end line, signature, doc summary, exported |
| Import | file, import path, local name (alias, `.`, `_`) |
| Call site | file, enclosing symbol, callee name, qualifier (package alias or receiver expression text), line, col |
| Reference | file, identifier, line (only for identifiers that aren't locals; for impact and rename) |
| Precise edge | call site (file, line, col) → target symbol key, produced by the precise tier |

**Call sites are stored unresolved; resolution happens at query time** against
the live symbol table of the current snapshot. Adding,
removing or changing a definition therefore never rewrites facts in other
files. This removes the reverse-importer reload completely.

Resolution order for a Go call site:

1. A precise edge exists and its file hash is current → `type_checked`.
2. `pkg.Func` where `pkg` is an import alias in the file → exact package lookup → `import_resolved`.
3. Unqualified `Func` → same package → `import_resolved`.
4. `x.Method` → methods named `Method`, ranked same file, same package, imported packages, then global → `name_matched`. Several candidates → `ambiguous`, all listed.

## On-disk format (custom segments)

Location: `~/.chronos-code/projects/<id>/index/v1/`, following the existing
per-project data layout (see `docs/harness-memory.md`). It never lives inside
the checkout.

### Manifest

`manifest.json`: format version, generation (monotonic), canonical root,
extractor versions, an ordered segment list (base segments first, then
overlays), and a tombstone list for each segment. It is published by writing
`manifest.json.tmp`, fsync, rename, then fsync of the directory. Readers keep
the snapshot they opened.

### Segment file

Immutable once written. Little-endian. Fixed-width records point into shared
string and blob sections. No Go struct is cast from bytes; every read is
bounds-checked.

```
Header (64 bytes)
  magic "CHXSEG\x00\x01", format version u16, kind u16 (base|overlay|precise),
  record counts, section table offset, xxh64 of the section table
Section table: [kind u16, offset u64, length u64, xxh64 u64] × N
Sections
  strings     interned UTF-8, (offset,len) addressed; names, paths, signatures
  files       fixed-width file records, sorted by path
  symbols     fixed-width, sorted by (name, package, file, line)
  sym_by_file symbol indexes grouped by file (outlines, impact)
  imports     fixed-width, sorted by (file, path)
  calls       fixed-width, sorted by (callee name, qualifier, file, line)  → callers
  calls_out   call indexes sorted by (enclosing symbol)                    → callees
  refs        fixed-width, sorted by (identifier, file, line)
  terms       sorted term dictionary → postings offsets (code-aware tokens: split
              camelCase/snake_case, paths, identifiers, doc words)
  trigrams    sorted trigram dictionary → postings (fuzzy names, substring search)
  postings    delta-varint file/symbol ids with term frequency
```

- Because every lookup table is a sorted fixed-width array, lookups are binary
  searches over mmap'd bytes, with no heap index to rebuild at open. An FST
  (e.g. `blevesearch/vellum`) can replace a sorted dictionary later, but only
  if profiling shows the dictionary size or lookup time matters.
- Source text is **not** stored. Excerpts are read from the working tree and
  checked against the indexed hash, as `evidence.go` does today. This keeps
  segments small and avoids a compression dependency.
- Checksums: each section's xxh64 is verified once, the first time a segment is
  opened in a process. After that the segment is trusted because it is
  immutable. A full scrub runs on `indexer verify` and on each full rebuild. Not
  re-verifying the whole index on every update is deliberate: synchronous
  whole-index verification would make each update cost grow with the index
  size, not with the size of the change.
- mmap: `syscall.Mmap` on unix and `x/sys/windows` file mapping on Windows,
  selected by build tags, with `ReadAt` into a heap buffer as the fallback.

### Overlays, routing, compaction

- An update writes one small overlay segment holding the changed files' facts,
  plus tombstones for deleted or replaced paths.
- Routing: at open, a path → (segment, record) map is built newest-first. It is
  in memory, sized by file count; it is persisted only if startup profiling
  requires it. Path routing avoids probing every segment for every file.
- Query-time merge: base segment results are filtered through the tombstones,
  then overlay results are added. Overlay count is bounded: when there are more
  than 16 overlays, or overlay bytes exceed 25% of base bytes, a background
  compaction rewrites the base from live records (facts are merged, files are
  not re-parsed) and publishes a new generation.
- Old segment files are deleted only when no open snapshot in this process
  references them. A lock file records other live processes; if another process
  holds a reader lock, deletion is deferred.

### Writer lock

One OS advisory lock per index directory (`flock` / `LockFileEx`). Readers never
take it. If a second chronos-code session finds the lock held, it opens
read-only and follows the manifest; it does not index.

## Pipeline

- **Scan:** reuse `scanCache` behaviour: skip unchanged size, mtime and mode,
  and hash only changed files. Honor `.gitignore`. `git ls-files` runs only on
  full reconciliation, never per query.
- **Workers:** `GOMAXPROCS` workers pull paths from a channel, parse with
  `go/parser` (`SkipObjectResolution`, `ParseComments`) and emit a bounded
  per-file fact record. There is no barrier per batch, so one slow file never
  stalls the other workers.
- **Writer:** one goroutine interns strings, sorts records and writes the
  segment. Backpressure is limited by bytes in flight.
- **Watcher:** 50 ms debounce, 2 s batching ceiling. The dirty set is capped
  at 4,096 paths; overflow, ignore-file changes or uncertain events trigger a
  full reconciliation. An adaptive idle reconciliation catches lost events.
- **Startup:** open the existing manifest immediately so the last generation can
  be queried while a reconciliation runs in the background. Results report
  `stale_files` until the reconciliation publishes.

## Precise tier

- After the dirty set has been quiet for 1 s, the existing loader logic
  (`loadGoPackagesWithTests`) is moved to `precise/` and run on the dirty
  packages.
- It writes a `precise` segment per load: call site → target symbol key, plus
  `implements` edges.
- Query-time resolution only uses a precise edge whose file hash matches the
  current file record; otherwise it falls back to the syntactic result.
- A missing Go toolchain, a load failure or a type error only disables the
  precise tier for the affected packages. Syntactic results still work.
- It runs at low priority, can be cancelled when new edits arrive, and at most
  one load runs at a time.

## Query engine

| Query | Plan |
|---|---|
| Exact symbol | binary search `symbols` in each live segment, merge, filter tombstones |
| Callers(name, depth) | `calls` by callee name → resolve each site → enclosing symbol; BFS by level with node, depth and work caps; truncation reported separately for each |
| Callees | `calls_out` by enclosing symbol → resolve |
| Search | BM25 over `terms` postings, exact-name boost, top-k by partial selection |
| Fuzzy / did-you-mean | trigram postings intersection, then edit-distance rerank |
| Map | package → files → exported symbols from `sym_by_file`, paginated |
| Impact | symbols overlapping a range → callers at depth 3 → tests (`_test.go` enclosing symbols) |

## Context engine

Rewritten in `context/`. Techniques:

- Seeds: exact identifiers from the task first, then BM25 matches. An
  identifier that looks like a name but matches nothing returns nothing instead
  of expanding into unrelated fragments.
- Evidence windows inside the enclosing declaration, not whole files. Several
  non-overlapping windows per file are allowed, each with its signature.
- Selection: the strongest seed first; later picks favour task terms and files
  not yet covered. Imports and tests are downweighted unless the task asks for
  them.
- Graph neighbours: expansion by push-based personalized PageRank from the
  seeds (see "Graph retrieval" under "Plan after M2"). For Go in M3 it uses
  only `type_checked` or `import_resolved` edges. This replaces the earlier
  rule of at most 3 seeds, one hop each way.
- Hard budget: cheap byte estimate while selecting, then an exact `o200k_base`
  count at the end. The tokenizer is built once per process.
- `seen` subtraction: ranges already delivered in this session are removed, and
  ranges whose file hash has changed are reported as invalidated.
- Memoization keyed by (normalized task, options) within one generation, capped
  at 1 MiB.

## Integration

- Tool names keep their current interfaces in M2 (`graph_query`,
  `codebase_search`, `codebase_context`, `codebase_map`, `find_callers`,
  `find_implementations`, `multi_resolution_view`, `resolve_symbol`,
  `impact_analysis`, `test_map`, `co_change`). Whether to merge overlapping
  tools is a separate decision (open question 2).
- M2 adds two backward-compatible changes to cut tool-call turns:
  - The symbol tools (`graph_query`, `find_callers`, `find_implementations`,
    `resolve_symbol`) accept an optional `names` list alongside `name`, and
    answer every name in one call, with a result section per name.
  - Empty results carry the same labels as non-empty ones: how the answer was
    checked (`type_checked`, `import_resolved`, `name_matched`) and how fresh
    the index is. Example: `"note": "No callers of \"Close\": no indexed
    call site calls a function or method with that name; … (index up to
    date; generation 12, 418 files)."`, next to `"resolution":
    "name_matched"` and an `"index"` object. An agent can then trust the
    absence instead of re-checking with grep. As built, the report carries
    no age in milliseconds, so identical questions give identical answers.
- `RequestScope` stops calling `IndexAll`. It takes the current snapshot and
  reports freshness.
- The orchestrator stops calling `IndexAll` synchronously at startup.
- M3 adds task-ranked context before each turn's first model call (a new
  `workspace.indexer.prefetch_tokens` setting and a `repository_context` source
  in the context report), backed by `context/`.
- `chronos-code indexer status` reports generation, segments, overlay count,
  file counts, precise-tier coverage and stage timings. `indexer verify` runs a
  full scrub.

M0 removed the earlier external sidecar integration completely: its build,
release and install steps, its client package, its tools, its config section
and its docs.

## Measurement

- The benchmark harness is added before any optimization (M0). It measures:
  - the current graph
  - the new indexer at every milestone
- Stage timers: scan, hash, parse, extract, write, fsync, publish, compaction,
  precise load.
- Work counters: files parsed, segments opened, overlays, bytes written,
  postings visited.
- Scenarios:
  - fresh build
  - unchanged reconciliation
  - edits (body only, signature change, declaration removed)
  - 1, 100 and 1,000 changed files
  - branch switch
  - delete/rename
  - 1,000 successive updates, to catch overlay degradation
- Corpora: this repo, `../chronos`, and one larger public Go repo (candidate:
  `kubernetes/kubernetes` subset).
- Report medians and p95 over at least 5 runs; measure warm and cold OS cache
  separately.
- Correctness: the caller/callee sets from the new indexer, restricted to
  `type_checked`, must match the old graph on this repo. The old graph's
  `PruneStaleEdges` regression test is carried over.

### Baseline (M0, current graph)

`make bench-index` on Apple M1 Pro, macOS, warm OS cache; median of 5 runs
(indexing: 5 iterations each). Corpus: a private copy of this repo (418 Go
files). The edit probe lives in `internal/config`, which about 40 packages
import. Harness: `internal/indexbench` plus `internal/graph/index_bench_test.go`.

| Scenario | Current graph | Target |
|---|---:|---:|
| Fresh build (`BenchmarkIndexFresh`) | 3.07 s | < 1.5 s |
| No-op reconcile, same process (`IndexNoop`) | 2.2 ms | — |
| No-op reconcile, new process (`IndexReopen`) | 79 ms | < 20 ms |
| Body-only edit (`IndexEditBody`) | 513 ms | < 50 ms |
| Declaration added (`IndexEditAddDecl`) | 407 ms | < 50 ms |
| Signature changed (`IndexEditSignature`) | 2.02 s | < 50 ms |
| Declaration removed (`IndexEditRemoveDecl`) | 1.94 s | < 50 ms |
| First tool call after an edit (`IndexQueryAfterEdit`) | 504 ms | < 50 ms |
| Per-call freshness scan (`ScanOwnRepo`) | 1.6 ms | — |
| `graph_query` hit / miss (fuzzy) | 28 µs / 126 µs | < 1 ms |
| `graph_query` via `RequestScope` | 1.56 ms | < 1 ms |
| `find_callers` depth 3 | 1.58 ms | < 5 ms |
| `impact_analysis` (700 lines) | 7.5 ms | — |
| `codebase_search` | 302 µs | — |
| `codebase_context` (4,096 tokens) | 58 ms | < 15 ms |

Queries are already fast. The problem is indexing: every edit costs 0.4–2 s
because `packages.Load` sits on the edit path and on the first query after it.

### M1 results (chronos indexer, not yet wired in)

Same machine, corpus and probe as the baseline; `internal/indexer/index_bench_test.go`,
median of 3 runs of 20 iterations.

| Scenario | Current graph | Chronos indexer | Target |
|---|---:|---:|---:|
| Fresh build | 3.07 s | 118 ms | < 1.5 s |
| Body-only edit | 513 ms | 3.1 ms | < 50 ms |
| Declaration added | 407 ms | 1.4 ms | < 50 ms |
| Signature changed | 2.02 s | 3.2 ms | < 50 ms |
| Declaration removed | 1.94 s | 5.1 ms | < 50 ms |
| Edit, then visible in a new snapshot | 504 ms | 3.3 ms | < 50 ms |
| Restart, then first symbol lookup (`IndexOpenQuery`) | 79 ms (reopen reconcile) | 0.9 ms | < 20 ms |
| Full no-op reconcile (`IndexNoop` / `IndexReopen`) | 2.2 ms / 79 ms | 43 ms / 43 ms | — |

A full reconcile re-lists the workspace every time. Here the corpus copy has
no `.git`, so listing falls back to a directory walk; in a real repo it runs
`git ls-files`, which takes about 66 ms on this repo. It is not on the edit or
query path: the watcher sends changed paths, and a restart serves the stored
snapshot at once. A listing cache keyed by directory mtimes is a follow-up if
periodic reconciles turn out to matter.

As built, M1 differs from the design above in these ways:

- The `refs`, `terms` and `trigrams` sections are deferred to M2 with the
  query layer. The M1 format has strings, files, symbols, symbols-by-file,
  imports, imports-by-path, calls, calls-by-caller and calls-by-file.
- The header carries its own checksum. In the byte-flip test, every flip in
  the checksummed header, the section table and the section payloads is
  rejected. Only reserved header bytes and alignment padding are unchecked.
- `Package` is the import path from the nearest `go.mod`. A module path
  change republishes a base; after a restart, stored package paths are
  compared with the current layout instead of forcing a rebuild.
- Compaction runs in the background after publish (more than 16 overlays, or
  overlay bytes above a quarter of the base once there are 4 or more
  overlays). It merges stored facts; no file is re-read.
- Fsync uses plain `fsync` (not `F_FULLFSYNC` on darwin). The index is a
  checksummed, rebuildable cache.
- Windows uses a read-whole-file fallback instead of mmap, and an
  exclusive-create lock file instead of `flock`.

### M2 results (graph tools on the chronos indexer)

Same machine as above; `internal/graph/scope_bench_test.go`
(`BenchmarkScope*`, this repository, warm). The M0 column is the SQLite
graph; rows marked * call its store directly rather than through
`RequestScope`, which added about 1.5 ms of freshness scan to every call.

| Scenario | M0 graph | M2 index | Target |
|---|---:|---:|---:|
| First tool call after an edit | 504 ms | 5.2 ms | < 50 ms |
| `graph_query` (via the request scope) | 1.56 ms | 26 µs | < 1 ms |
| `graph_query` miss with did-you-mean | 126 µs* | 43 µs | < 1 ms |
| `graph_query`, 4 names in one call | — | 57 µs | — |
| `find_callers` depth 3 | 1.58 ms* | 0.42 ms | < 5 ms |
| `find_implementations` | — | 0.36 ms | — |
| `impact_analysis` (700 lines) | 7.5 ms* | 1.6 ms | — |
| `codebase_search` | 302 µs* | 318 µs | — |
| `codebase_context` (4,096 tokens) | 58 ms* | 45 ms | < 15 ms (M3) |

Startup no longer indexes synchronously: `NewIndexScope` opens the stored
manifest and returns; the reconcile and the watcher start in the background.
Tool calls answer from the stored index meanwhile and wait only when no index
exists yet. `TestIndexScopeNeverRunsGo` puts a fake `go` first on `PATH`,
calls all 11 tools before and after a signature change, and asserts it was
never run. `TestIndexScopeMatchesStoreTools` checks callers (depth 3),
implementations, `test_map`, `impact_analysis`, `codebase_context` roles,
the first `codebase_search` hit and L1 package symbols against the
type-checked SQLite graph on the same workspace.

As built, M2 differs from the design or from the old graph in these ways:

- **Search indexes are in memory, per segment.** BM25 postings over name,
  receiver, signature, doc, package and path (with camelCase/snake_case
  subwords), plus trigrams over distinct names, are built lazily once per
  immutable segment and dropped when the segment leaves the snapshot. An
  edit therefore builds only its small overlay's index. Persisting them as
  segment sections (`terms`, `trigrams`) moves to M5, where heap per file
  count matters.
- **Search ranks by any-term BM25 with a coverage factor** (hits matching
  more query words rank higher; an exact name or `Recv.Name` match ranks
  first). The SQLite store required every term to match.
- **Did-you-mean is fuzzy.** Substring matches first; when none exist, the
  names sharing most trigrams are ranked by edit distance (`NewRepp` →
  `NewRepo`). The store only did substrings.
- **Segment format v2 and extractor `go-syntax-2`.** Symbols gained a
  container. Interface method specs are recorded as methods whose receiver
  is the interface, and embedded types in interfaces and structs as `embed`
  records. Older indexes are discarded and rebuilt on open.
  `find_implementations` matches method names (including methods promoted
  from embedded fields and a table of common standard-library interfaces)
  and is labelled `name_matched`.
- **Interface method specs are declarations.** `graph_query("Save")` also
  returns `Repo.Save` (receiver `Repo`). The type-checked graph did not
  list them.
- **Paths are root-relative** in every result (the SQLite Go tier stored
  absolute paths). Path arguments accept either form.
- **Signatures come from syntax** (`func (Card) Pay(amount int) error`),
  not `types.ObjectString`.
- **Package dependencies are filled for Go.** `multi_resolution_view` L1
  `depends_on`/`dependents` came only from tree-sitter import edges before,
  so they were empty for Go packages.
- **Freshness per call.** Each tool call first applies the changed paths
  the watcher has already seen (`Engine.Sync`), then answers from one
  snapshot. A write whose filesystem event has not arrived yet can still
  be missed; the `index` report then shows `pending_changes` or
  `up_to_date: false`.
- **Second session on one repository.** When the writer lock is held, the
  session uses a private index under `index/sessions/`, removed on close,
  instead of following the other session's manifest (open question 4).
- **Background work does not print.** Indexing messages from background
  goroutines would corrupt the TUI, so they are dropped; the last error is
  reported in each result's `index.error`.
- **The tree-sitter tier stays for non-Go files** in local cgo builds: the
  SQLite store is refreshed with `Indexer.RefreshNonGo` (no Go loading;
  stale Go rows removed) at most once a second and merged with the index.
  Default and release builds do not open `graph.db`.
- `RequestScope`, the old `Indexer`/`Watcher` and `IndexAll` remain for the
  tree-sitter tier and their tests until M11. `activation` now takes a
  `GraphReader` interface, and `graph.Backend` is the seam every tool uses.

### M3 results (graph retrieval, prefetch)

`internal/indexer/retrieve` implements "Graph retrieval" below for Go:
seeds, push-PPR over call edges resolved at query time, file
diversification, packing at three zoom levels, reasons per item, and
session `seen` subtraction. `codebase_context` on the index and the new
per-turn prefetch use it; the SQLite path is unchanged.

| Scenario | M0 | M2 | M3 | Target |
|---|---:|---:|---:|---:|
| `codebase_context` (4,096 tokens, uncached) | 58 ms | 45 ms | 12.4 ms | < 15 ms |
| Prefetch (1,500 tokens, `BenchmarkScopePrefetch`) | — | — | 8.1 ms | < 15 ms |

Where the time went and what changed: the M2 path read an excerpt for every
candidate and then re-tokenized the whole JSON after each shrink step (BPE
was 60% of the time, file reads 35%). M3 packs by estimated tokens first,
reads only the excerpts it keeps, and counts once; `fitEvidence` still
certifies the budget with the exact tokenizer.

Retrieval eval baseline (`BenchmarkRetrievalEval`, 10 tasks on this
repository phrased as an agent receives them, 22 gold declarations, one
`codebase_context` call at 4,096 tokens):

| Path | Recall | Excerpt recall | Tokens per task |
|---|---:|---:|---:|
| SQLite graph (`evidence.go`, FTS) | 0% | 0% | 68 |
| Chronos index, M3 | 36% | 36% | 3,034 |

The SQLite path finds nothing for natural-language tasks because its FTS
query requires every word to match. 36% is a baseline, not a result to
defend: misses are mostly declarations reachable only by type (`load`,
`reset` behind `Parse`) or named differently from the task. The turn-count
eval with a real agent was not run in M3; it needs model calls and is
still open.

Acceptance: budget never exceeded (`TestIndexedEvidenceBudgetNeverExceeded`,
exact count, 5 budgets × 4 queries on this repository; prefetch at 64, 200
and 1,500 tokens in `TestPrefetch`); `seen` and invalidation
(`TestIndexedEvidenceSeenAndInvalidation`, `TestRetrieveSubtractsSeenAndReportsChanges`);
prefetch injected and disabled by config (`TestExecutePrefetchesRepositoryContext`).

As built, M3 differs from the design in these ways:

- **No persisted resolved-graph segments yet.** Edges are resolved lazily
  while pushing (`query.View.Outgoing`/`Resolve`/`IncomingCalls`), and
  push work is bounded (at most 600 pushes and 80 node expansions), so cost
  depends on the neighbourhood, not the repository. CSR graph segments and
  the global PageRank prior move to M5 with the other scale work.
- **Name matching is import-scoped.** A call `x.M()` only links to methods
  `M` declared in the caller's package or a package it imports; this
  removed most false edges (for example `b.cancel()` linking to an
  unrelated package's `cancel`). Calls through types obtained indirectly
  are missed as a result.
- **Seeding rules.** Code-shaped words (camelCase, `snake_case`,
  `Recv.Name`) and capitalized words are exact seeds; a lower-case word of
  four or more letters that names a declaration is a weaker seed unless it
  is a common task word; `pkg/file.go` paths seed the file's declarations.
  BM25 fills in (at most 4 hits when exact seeds exist), and a hit must
  match at least two query words unless it is an exact name. Search now
  drops English stopwords, stems crudely (`compaction` → `compact`,
  `watcher` → `watch`), weights coverage by idf mass, and boosts a symbol
  whose name is a task word.
- **Scoring.** Exact seeds get a fixed bonus; items two hops away through
  a non-seed (a callee of a caller) are scaled by 0.4; each further item
  from the same file by 0.6.
- **Result fields.** Items gain `why` and `zoom`; the result gains
  `misses`, `invalidated` and `index`. All are omitted by the SQLite path,
  so its output is byte-identical. Retrieved roles now include `callee`.
- **Excerpt freshness.** A partial read cannot hash the file, so excerpts
  of files not read to the end are `unverified` (mtime matches the index);
  retrieval and prefetch deliver those as well as `verified` ones, and both
  count as seen.
- **Seen state** is per session (`storage.SessionFromContext`) and root, at
  most 64 records, cleared wholesale beyond that. Source delivered by
  prefetch counts as seen for later `codebase_context` calls.
- **No result memoization.** Uncached latency already meets the target;
  memoization would complicate `seen` handling.
- **Prefetch** is on by default (`workspace.indexer.prefetch_tokens`,
  default 1,500; 0 disables), replaces the older `[Pre-loaded context]`
  prediction when the index is available, is bounded by a 250 ms timeout,
  returns nothing for tasks that anchor on no declaration, and is reported
  as the `repository_context` context source (droppable under pressure).

### M5 results (scale foundations)

The six M1 problems in "Scale: million-file monorepos" are fixed:

| # | Problem | As built |
|---|---|---|
| 1, 2 | Publish and open copied/built an O(files) routing map | Layered routing (`store/snapshot.go`): an overlay map plus binary search in the one shard whose path range holds the path; shadowed records and tombstones in sparse per-segment dead sets, copied on write. Deriving a snapshot costs O(overlay files); opening costs O(shards + overlay files). |
| 3 | A fresh build held every file's facts in memory | `store.BaseWriter` streams files in path order and writes a shard whenever its estimated size reaches 8 MiB; the engine extracts 1,024 files at a time. |
| 4 | One base; compaction rewrote the repository | Sharded base. `Compact` rewrites only shards whose range holds an overlay record, splits shards that outgrow the target and drops emptied ones. |
| 5 | fsnotify needs a descriptor per file on macOS | Watch backends (`watch.go`): fsnotify up to 20,000 files on macOS/BSD (200,000 on Linux), then watchman when installed, then git polling. |
| 6 | A reconcile listed and statted every file | Git reconcile: the manifest records the commit, the module map and `Touched` paths; a restart indexes `git diff <commit> HEAD` ∪ staged ∪ `git ls-files -m -d -o` ∪ `Touched`. The workspace is listed only for a first build, module or `.gitignore` changes, more than 50,000 changes, more than 4,096 touched paths, an unknown commit, or no git. |

Also built:

- **Persisted search.** Segment format v3 adds BM25 postings, document
  lengths, distinct declaration names with a trigram index, and a
  package-ordered file list. The query layer reads them from the mapping;
  the in-memory search index and per-generation package index of M2/M3 are
  gone. Search skips terms found in more than 50,000 documents (and more
  than 1 in 20) when the query has rarer terms, and visits at most 200,000
  postings per term.
- **Reuse without parsing.** A candidate whose size or mtime changed but
  whose content hash matches is re-recorded from its stored facts (`touch`,
  checkouts that rewrite unchanged files, imported indexes).
- **Progressive first build.** Above 20,000 files, the working set (the
  launch directory's subtree, files in the last 50 commits, working-tree
  changes; at most 5,000 files) is published first as an overlay with
  `Complete: false`; tool results report `index.coverage: "partial"` until
  the streamed base replaces it.
- **Prebuilt base.** `Engine.Import(dir)` copies another checkout's index
  (paths are root-relative) and rewrites its root; the next reconcile runs
  from the imported commit and indexes only the difference
  (`TestImportPrebuiltIndex`: one file parsed).
- **Parallel open.** Segments are validated (checksums and every record) in
  parallel on open.

Synthetic scale benchmark (`query/scale_bench_test.go`,
`CHRONOS_SCALE_FILES=1000000`): facts streamed straight into the store (no
source files, so parse cost is excluded), 4 declarations and 6 calls per
file, 136 shards, 1.3 GB of segments; Apple M1 Pro, warm cache.

| Operation | 100k files | 1M files | Target (1M) |
|---|---:|---:|---:|
| Build base (streaming, no parse) | 3.0 s | 32.9 s | — |
| Restart, open, first symbol lookup | 12.7 ms | 102 ms | < 100 ms |
| Heap after open | 1.1 MiB | 1.4 MiB | < 1 GB |
| Exact symbol lookup | 4 µs | 22 µs | < 2 ms |
| Callers of a name | 13 µs | 0.16 ms | < 10 ms (depth 3) |
| Search (identifier + common words) | 20 µs | 0.14 ms | — |
| Publish a one-file edit | 1.2 ms | 1.9 ms | < 50 ms (with parse) |
| Routing entries touched by that edit | 20 | 20 | independent of size |
| Compact 20 scattered edits | 1.9 s | 4.3 s | background |

`TestEditPathWorkIndependentOfBaseSize` asserts the same routing work for a
one-file edit over 500 and 5,000 files. Restart-to-first-lookup misses its
target by 2%: opening validates every record of every shard. The fix, if it
matters, is to validate section checksums in the background and only
string references synchronously.

On this repository (M3 → M5): fresh build 118 → 229 ms (search sections
are now written), edit then query 5.2 → 8.7 ms, `codebase_context` 12.4 ms,
prefetch 7.5 ms. The corpus copy has no `.git`, so its no-op reconcile
lists (79 ms); in a repository it reconciles from git.

As built, M5 differs from the design in these ways:

- **Not measured with real files at a million.** The scale benchmark
  streams synthetic facts; the "working set queryable in < 5 s without a
  prebuilt base" target is untested at that size.
- **No persisted graph segments or global PageRank prior.** Call edges stay
  resolved lazily and bounded per retrieval (M3); nothing on the query path
  needs a precomputed graph at this scale yet.
- **Git polling does not poll on every tool call** (a poll stats the whole
  git index without `core.fsmonitor`); changes appear within its 2 s
  interval and the index report shows the lag. Watchman is polled before
  each tool call (`Engine.Sync`).
- **Watchman is driven through its CLI** (`watchman -j` per poll, every
  200 ms), not a persistent socket subscription.
- **Segments are portable, not byte-identical across machines:** file
  records keep mtimes. An imported index relies on git reconcile (which
  never stats unchanged files) and on hash-verified reuse.
- **No `chronos-code indexer import/export/verify` command yet**; import is
  an engine API only.
- **Sparse and virtual checkouts** are handled only in the sense that git
  reconcile never reads files git does not report; a first build still
  lists and reads every listed file.

### M6 parser runtime spike

Measured 2026-09-26: pure-Go `github.com/odvcencio/gotreesitter` v0.55.0
against the cgo `github.com/smacker/go-tree-sitter` already in `go.mod`.
The corpus is about 2 MB of real, hand-written files per language (local
checkouts and module caches, plus shallow clones of typelevel/cats,
Newtonsoft.Json, laravel/framework and dart-lang/http), deduplicated and
without minified files; files are 1–200 KB. One language per process, one
parse thread, Apple M1 Pro. The cgo package has no Objective-C or Dart
grammar.

| Language | Pure Go MB/s | cgo MB/s | Ratio | Worst file | Files > 50 ms | Grammar load |
|---|---:|---:|---:|---:|---:|---:|
| Ruby | 5.8 | 11.7 | 2.0× | 18 ms | 0% | 13 ms |
| TSX | 5.0 | 9.8 | 2.0× | 16 ms | 0% | 10 ms |
| Bash | 5.3 | 12.7 | 2.4× | 15 ms | 0% | 11 ms |
| Python | 4.4 | 11.8 | 2.7× | 27 ms | 0% | 5 ms |
| Java | 4.4 | 16.4 | 3.8× | 32 ms | 0% | 4 ms |
| C++ | 3.4 | 8.2 | 2.4× | 53 ms | 0.6% | 47 ms |
| Kotlin | 2.6 | 8.1 | 3.1× | 156 ms | 0.3% | 47 ms |
| JavaScript | 2.7 | 9.2 | 3.4× | 74 ms | 0.4% | 3 ms |
| PHP | 2.6 | 12.2 | 4.7× | 139 ms | 0.9% | 8 ms |
| TypeScript | 2.5 | 15.7 | 6.2× | 501 ms | 0.3% | 10 ms |
| Rust | 2.0 | 11.4 | 5.8× | 604 ms | 2.9% | 12 ms |
| C | 1.7 | 8.6 | 5.1× | 439 ms | 0.9% | 7 ms |
| Scala | 0.7 | 3.4 | 4.6× | 659 ms | 1.4% | 38 ms |
| Dart | 0.6 | — | — | 534 ms | 2.6% | 8 ms |
| Objective-C | 0.2 | — | — | 324 ms | 11.4% | 32 ms |
| C# | 0.08 | 7.0 | 87× | 1.8 s | 22.1% | 31 ms |
| Swift | 0.08 | 7.75 | 97× | 3.4 s | 15.2% | 119 ms |

Findings:

- The median ratio is about 3.5× and matches the candidate's own claim, but
  C# and Swift hit GLR-ambiguity cliffs on ordinary 12–25 KB files. The
  library's pooled path and `GOMAXPROCS=1` give the same numbers.
- Allocation is 0.2–20 bytes per input byte for most grammars, but 590–900
  for Scala, Swift and C#; peak RSS reached 755 MiB on the Scala corpus.
  Decoded grammar tables total about 100 MB for all 17 (Swift alone 29 MB).
- The runtime sometimes stops when error recovery fails (`no_stacks_alive`),
  which the C runtime never does: 17 of 154 C++ files, 5 of 259 JavaScript
  (Flow annotations, template placeholders, unexpanded macros). The partial
  tree covers 5–100% of the file and often holds most declarations.
- A deadline stop returns an empty tree.
- Embedding all ~200 registry grammars adds about 18 MiB to the binary;
  the 17 pack grammars add about 5 MiB with the `grammar_subset` build tags.

Decision:

- **Pure-Go runtime in every build, with a per-parse deadline (1 s) and
  memory budget (256 MiB).** A file stopped by either is recorded without
  symbols. A `no_stacks_alive` tree is kept as a partial parse and the
  file is flagged. At 1 s, the corpus loses 6 of 280 C# files and none in
  other languages.
- **No cgo backend for the new extractor.** Query packs are written and
  tested against one grammar set, the pinned pure-Go registry; the older
  cgo grammars use different node names in places. The old cgo tier stays
  until M11.
- **Grammar subset build tags.** `GRAMMAR_TAGS` in the Makefile lists the
  pack grammars; `make build*`, `make test` and the release workflow pass
  it. A plain `go build` still works and embeds every grammar.
- C#, Swift and Objective-C are documented as slow: some edits of those
  files miss the 50 ms target (queries keep answering from the previous
  facts while the parse runs), and fresh builds of large Swift or C#
  repositories are slow. A WebAssembly build of the C runtime (wazero) is
  the unmeasured alternative if this matters.

Reproduce with `BenchmarkParse` in `internal/indexer/extract/treesitter`
(`CHRONOS_TS_CORPUS=<dir>`, one subdirectory per grammar name). With the
1 s deadline it reports dropped and partial files per language.

### M6.2 results (facts v2, segment format v4)

`facts` is language-neutral and segments are format v4
(`ExtractorVersion` `go-syntax-4`, so older indexes are rebuilt):

- Symbols carry `Visibility` (public, protected, internal, private,
  package) and a `Modifiers` bitset (static, abstract, async, override,
  deprecated, test, decl). Twelve kinds were appended to the on-disk kind
  list (class, constructor, trait, protocol, enum, enum member, field,
  property, type alias, module, namespace, macro).
- Calls became references: `facts.Ref` with a kind (call, type use,
  extends, implements, instantiate, decorator). The kind uses the spare
  byte of the old call record, so the record stays 32 bytes.
- Files carry `Generated`, `Vendored` and `Test` flags; imports carry a
  kind (module, wildcard, re-export, include) and their listed names with
  aliases.
- New per-file sections: exports and binding hints. `Parse` validates every
  new field, including that a reference's enclosing symbol and a hint's
  scope belong to the same file.

As built, this differs from the design above in these ways:

- **No separate `unit` field.** `File.Package` is the unit: the Go import
  path, and the directory for other languages until M7 assigns modules,
  crates or build targets.
- **`Receiver` is the owning type** of any member (a Go receiver or the
  enclosing class), so `Qualified()` stays `Owner.name` for every language.
- **Exports and hints are stored per file only**; no by-name index yet.
  The M7 resolvers add one if they need it.
- **The query layer keeps its call API** (`CallSites`, `Callers`,
  `Callees`, `Outgoing`) and filters to call references; `Stats.Calls`
  became `Stats.Refs` (every kind), which the graph tools report as edges.
- **The Go extractor fills** visibility, the test and generated flags, and
  the test, decl and deprecated modifiers, and records dot imports as
  wildcard imports. It emits no type-use references and no binding hints
  yet; those come with the generic resolver (M6.4). Nothing sets `Vendored`
  yet, because scan skips `vendor/`.

Edit path and fresh build are unchanged, measured against M6.1
(benchstat, n=10, this repository): `IndexFresh` 241 → 242 ms,
`IndexEditBody` 7.0 → 6.8 ms, `ScopeQueryAfterEdit` 9.2 → 9.3 ms (none
significant). A first cut used stable sorts in `Encode` for deterministic
bytes, which cost 8% on query-after-edit; unstable sorts with a total order
(ties broken by record index) are deterministic too
(`TestEncodeDeterministic`).

### M6.3 results (query packs)

Every pack language is discovered and extracted in all builds
(`ExtractorVersion` `syntax-5`, so older indexes are rebuilt):

- **One extractor, data per language.** `extract/generic` runs a pack's
  `tags.scm` and turns the captures into facts. The capture names are
  documented in `extract/packs/packs.go`: `@def.<kind>` (a `.decl` suffix
  marks bodiless declarations), `@ref.<kind>` with `@ref.qualifier`,
  `@import` / `@include` with path, alias, listed names and wildcard,
  `@export`, `@scope` (Rust `impl`, Swift `extension`, Objective-C
  `@implementation`) and `@package`. Everything else is derived the same
  way for every language: nesting and receivers from source ranges, doc
  comments from the comments right before a definition, signatures from
  the text before the body, and visibility and modifiers from the words
  before the name plus the pack's rules in `pack.yaml` (keywords, member
  defaults per container kind, a private name prefix, an export wrapper,
  access labels such as C++ `public:` or a Ruby `private` line).
- **Queries are written against this runtime's grammars**, which differ
  from upstream in places (Python has no `expression_statement` around
  assignments; Kotlin has almost no field names). Each pack has a sample
  under `extract/generic/testdata/<pack>/` and a reviewed golden of its
  facts (`TestPackGoldens`, `-update` to regenerate). `TestQueryCaptures`
  rejects unknown capture names, so a typo cannot silently drop facts. `tsx`
  reuses the typescript query and adds JSX patterns (`query_from`).
- **Discovery.** `scan.Indexable` accepts Go and every pack extension. A
  non-Go file's `Package` is its directory. Pack files above 1 MiB, and
  minified files (at least 32 KiB with an average line over 1,000 bytes),
  are recorded without symbols; minified files are also flagged
  `Generated`.
- **The old cgo tree-sitter tier no longer answers.** `IndexScope` stopped
  merging it (`mergedBackend`, `RefreshNonGo` and the `GraphDB` option are
  gone), because the indexer now holds the same files and cgo builds would
  list them twice. The old graph was then deleted (see "M11 results").

As built, this differs from the design above in these ways:

- **No type-use references or binding hints yet.** As for Go (M6.2), they
  come with the generic resolver (M6.4). References are calls,
  instantiations, extends, implements and decorators or annotations.
- **`QualPackage` means "the qualifier is a name an import binds"**, and
  `Qualifier` is that import's spec. That can be a module (`os`, `path`)
  or an imported type (Java `Helper.check()`, Rust `HashMap::new()`). The
  current resolver matches the spec against `Package` exactly, so these
  edges resolve only for Go until M6.4; `find_callers` still finds them by
  name.
- **Some syntax is ambiguous without types.** C# bases named `I` plus a
  capital letter count as implemented interfaces, other bases as extended.
  Swift and Kotlin superclasses and protocols are not distinguishable
  (Swift records all of them as extends). A Swift `async` after the
  parameters is not detected. Trait methods in a Rust `impl Trait for T`
  get the default (private) visibility.
- **Graph tools still detect tests the Go way** (`_test.go`, `IsTest`). The
  facts carry `File.Test` and `ModTest` for every language; switching the
  tools to them is part of the graph switch-over (done in M6.5). Path-shaped arguments
  (`l3Snippet`, `test_map`, retrieval tokens) now accept any pack
  extension.

Binary size (linux/amd64, stripped, release flags): 40.9 MB before, 52.9 MB
after. The tree-sitter parser core with the grammars' scanners is about
7.5 MB, the 17 grammar blobs with the runtime wrapper about 4.8 MB and the query engine 0.5 MB. The
size gates are now 56 MiB (release and core); the cgo full-build gate went
with M11.
Two cuts in chronos are planned so the gates can come down again: loading
only tiktoken's o200k vocabulary (about -5.0 MB; the other three are linked
through `tokenizer.ForModel`) and keeping `storage/adapters/postgres` out
of `sdk/agent`'s imports (about -2.0 MB of pgx).

The edit path and fresh build are unchanged on this repository, whose only
non-Go sources are two shell scripts and two small JavaScript files
(benchstat, n=10, old and new runs interleaved): `IndexFresh` 289 → 265 ms
(-8%, p=0.01), `IndexEditBody` 8.1 → 7.1 ms (not significant). The machine
was loaded (load average about 45 on 10 cores), so these numbers resolve to
about ±10%. Grammars load once per process (3–13 ms each); query compilation
takes 0.1–4 ms per pack.

### M6.4 results (generic resolver, type uses, binding hints)

`ExtractorVersion` is `syntax-6`. Every language now records type uses and
binding hints, and retrieval's graph expansion resolves references with
the ladder from "Resolution":

- **Type uses.** Go records every named type in signatures, fields,
  `var` declarations, assertions and type arguments (not predeclared types
  or type parameters); `T{}` is an instantiation and embeds are extends.
  Packs capture `@ref.type`; a type pattern matching a declaration's own
  name is skipped, and primitive types are dropped.
- **Binding hints.** Go: receivers, typed parameters, struct fields
  (scoped to the struct), `var x T`, `x := T{}` / `&T{}` / `new(T)` /
  `v.(T)`, and `x := f()` recorded as `f()`. Packs capture `@hint` with a
  type or a call (`x = Foo()` is `Foo()`); a hint is scoped to its function
  or class, never to the field it names. `T::new()` and `T.new` construct
  `T`.
- **Resolver** (`query/resolve.go`), one label per step:
  `f()` resolves in the same file, the caller's class (implicit `this`
  languages), the file's imported names and wildcard or whole-module
  imports, then the same unit (languages whose units share scope):
  `import_resolved`; then any declaration of the name (`name_matched`, or
  `ambiguous`; not Go). `pkg.f()` resolves in the import's workspace units
  (static members of an imported class first). `x.m()` infers `x`'s type
  from `self`/`this`, hints in the caller, its class and sibling members
  (a constructor parameter stored as a field), field chains (`s.repo`),
  static calls on a type (`Helper.check()`) and call results (the result
  type is read from the callee's signature), then looks up `m` on that type
  and its supertypes: `type_hinted`. An import or type that maps to no
  workspace unit is external and resolves to nothing, so `os.path.join()`
  never name-matches a workspace `join`.
- **Import specs to units:** Go import paths exactly; relative specs
  (JS/TS, Python dots, C includes, Ruby, Dart, shell) against the file's
  directory; dotted and path specs by directory or file-stem suffix, with
  the spec's parent tried for imports naming a class or module; C#/PHP
  namespaces fall back to dropping leading segments. File stems come from
  a per-segment index built once per immutable segment; results are
  memoized per generation.

As built, this differs from the design above in these ways:

- **Build-target scoping is not implemented** (no build graphs before M7).
- **`Outgoing` now includes instantiations**, so graph expansion follows
  `new T()` to the class. Call-site queries (`CallSites`, `Callers`,
  `Callees`) still count calls only, and the graph tools still match
  callers by name until the switch-over (done in M6.5).
- **Type inference is shallow on purpose:** three levels of field chains
  and call results, no flow analysis, and the first matching hint in scope
  wins.

Measured on this repository against M11 (benchstat, n=6, runs
interleaved): `IndexEditBody` 1.45 → 1.28 ms (not significant),
`ScopeQueryAfterEdit` 8.9 → 9.5 ms (not significant), `IndexFresh` 233 →
257 ms (+10%, the extra references and hints), `ScopePrefetch` (1,500
tokens) 8.3 → 9.7 ms (+17%), `ScopeCodebaseContext` (4,096 tokens) 14.2 →
16.7 ms (+17%). The retrieval cost is resolving every candidate caller
instead of matching by name; per-view memos (declarations by name,
resolved references, lazily decoded file context) brought it down from
+25%. Retrieval eval recall is unchanged on this Go repository (0.41,
excerpt recall 0.32); the resolver's effect on other languages is what the
SCIP baseline measures.

### M6.5 results (graph switch-over, 2026-09-26)

The graph tools answer from resolved edges instead of callee names, and
detect tests in every language:

- **Callers and callees are resolved.** `query.CallerEdges(name)` resolves
  every call site of the name and keeps those whose targets include one of
  the declarations `Symbols(name)` finds; `IncomingEdges(targets)` does the
  same for known declarations (the next hop of a traversal), and
  `OutgoingEdges` / `Callees` return the workspace declarations a function
  calls. Each edge is one (caller, callee) pair with its first call site
  and the strongest label among its sites. Instantiations count only for
  constructors (`new T()` reaches `T`'s constructor); calls into external
  code are not edges. A name with no indexed declaration (`Println`) falls
  back to its call sites, labelled `unresolved`.
- **`find_callers`** groups callers by callee and label, each as
  `"Caller (file:line)"`, so an agent gets the call site without a second
  lookup. Levels follow declarations, not short names, and ambiguous
  callers are listed but not followed to the next depth. With the call
  sites included, the depth-3 output on this repository is the same size
  as before (40.7 KB for `Execute`); the old answer for `Reconcile`
  included callers of unrelated `Reconcile` methods.
- **`impact_analysis`, `test_map` and L2 counts** use the resolved callers
  of the exact declaration. A breaking change is an exported declaration
  (`Symbol.Exported`, any language) with a caller in another unit.
- **Tests in every language.** `Symbol.Test` (from `facts.ModTest`: Go
  `Test*`/`Benchmark*`/`Example*`/`Fuzz*` in `_test.go`, and each pack's
  test names and markers such as JUnit `@Test`, pytest `test_*`, Rust
  `#[test]`) replaces the `_test.go` + `Test` prefix check in `test_map`,
  `impact_analysis` and retrieval roles; `Symbol.TestFile` replaces the
  `_test.go` suffix in retrieval's test penalty.
- **`find_implementations`** also lists, for languages other than Go, the
  types whose extends or implements clauses resolve to the interface or
  class (`query.Subtypes`), transitively through sub-interfaces. Go keeps
  method-set matching.
- `IndexReport.Relations` now describes implementations only; call edges
  carry their own labels, so `find_callers` has no top-level
  `resolution` field.

Measured on this repository against the M6.4 commit (benchstat, n=6, runs
interleaved, load average about 10): `ScopeFindCallersDepth3` 0.40 →
2.2 ms (inside the 5 ms goal; it now resolves every candidate call site
instead of reading names), `ScopeImpact` (every symbol of `engine.go`)
6.1 → 8.5 ms (a per-call memo of incoming edges keeps overlapping test
searches from repeating), `ScopeFindImplementations` +5%;
`ScopeCodebaseContext`, `ScopePrefetch` and `ScopeGraphQuery` unchanged.

### SCIP edge baseline (Tier 1 and Tier 2, 2026-09-26)

`make eval-edges` (`benchmark/edges/run.sh`) clones pinned repositories
(`benchmark/edges/repos.tsv`), indexes each with a SCIP indexer, and scores
the indexer's resolved references against SCIP's definitions
(`benchmark/edges`, decoding SCIP with `protowire`; nothing is added to the
binary). Results are merged into `benchmark/edges/baseline.json`. It needs
network access and is not part of CI. Nothing is installed globally:

- scip-typescript and scip-python run through `npx` (npm cache), scip-go
  through `go run` (module cache), rust-analyzer from rustup.
- scip-java 0.12.3 and scip-clang 0.4.0 are downloaded into
  `~/.cache/chronos-edges/tools`, checked against pinned SHA-256 sums.
  scip-java runs with JDK 17 (`$JAVA17_HOME`, default SDKMAN's
  17.0.18-tem; 0.13 needs a newer bash than macOS ships). Maven projects
  need `mvn`; Gradle projects use their wrapper (`~/.gradle`).
- C and C++ projects are configured with `cmake` into
  `~/.cache/chronos-edges/<name>-build` for `compile_commands.json`.
- C# uses a .NET SDK in `~/.cache/chronos-edges/dotnet`
  (`dotnet-install.sh --channel 10.0 --install-dir …`), scip-dotnet 0.2.14
  installed there with `--tool-path`, and CLI and NuGet state in
  `~/.cache/chronos-edges/dotnet-home`.
- A repository whose toolchain is missing is skipped with a message.

A reference is scored where SCIP has a reference (not an import) to an
in-repository declaration at the same line and name. Top1 means the first
target is SCIP's definition; "any" that some target is. Calls are calls
and instantiations; types are type uses, extends and implements. The
harness drops occurrences SCIP records that are not references in source
text: scip-clang's macro expansions, duplicate occurrences and forward
declarations, scip-java's imports and Kotlin property accessors (counts in
`-v` output).

| Repository | Calls: coverage | Calls: recall | Types: coverage | Types: recall |
|---|---:|---:|---:|---:|
| cobra v1.8.1 (Go, scip-go 0.2.7) | 0.755 | 0.999 | 0.347 | 1.000 |
| click 8.1.7 (Python, scip-python 0.6.6) | 0.507 | 0.966 | 0.265 | 1.000 |
| ky v1.7.2 (TypeScript, scip-typescript 0.4.0) | 0.900 | 0.889 | 0.661 | 1.000 |
| walkdir 2.5.0 (Rust, rust-analyzer 1.93.0) | 0.571 | 0.961 | 0.446 | 0.944 |
| jsoup 1.18.1 (Java, scip-java 0.12.3) | 0.992 | 0.811 | 0.605 | 0.990 |
| mockito-kotlin 6.0.0 (Kotlin, scip-java 0.12.3) | 0.981 | 0.755 | 1.000 | 1.000 |
| cJSON 1.7.18 (C, scip-clang 0.4.0) | 0.982 | 0.998 | 0.947 | 0.915 |
| tinyxml2 10.0.0 (C++, scip-clang 0.4.0) | 0.856 | 0.564 | 0.701 | 0.987 |
| MediatR 12.4.1 (C#, scip-dotnet 0.2.14) | 0.965 | 0.857 | 0.719 | 0.995 |

Precision by label, calls (top1 / references, any in parentheses when it
differs):

| Label | cobra | click | ky | walkdir | jsoup | mockito-kotlin | cJSON | tinyxml2 | MediatR |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `import_resolved` | 1419 / 1419 | 422 / 431 (428) | 9 / 9 | 147 / 150 (150) | 2458 / 3599 (3565) | 111 / 157 (144) | 825 / 825 | 322 / 715 (711) | 274 / 298 (292) |
| `type_hinted` | 910 / 910 | 233 / 233 | 7 / 7 | 266 / 266 | 5151 / 5251 | 53 / 53 | – | 509 / 529 | 79 / 79 |
| `name_matched` | 164 / 164 | 14 / 15 | – | 81 / 81 | 427 / 432 | 40 / 40 | 302 / 302 | 65 / 65 | 76 / 76 |
| `ambiguous` | – | 31 / 35 (35) | – | 5 / 15 (15) | 3192 / 4524 (4376) | 30 / 31 (31) | 0 / 2 (2) | 173 / 492 (484) | 118 / 185 (169) |
| unresolved | 2 | 11 | 2 | 7 | 33 | 29 | 0 | 96 | 0 |

Observations:

- **Labels mean what they say where the language has no overloading.**
  `import_resolved` and `type_hinted` are 97–100% top1-precise for Go,
  Python, TypeScript, Rust and C, and `type_hinted` is 96–100% everywhere.
- **Overloads are the remaining precision gap** (Java, Kotlin, C++, C#).
  A reference records no argument count, so every overload of the right
  member in the right type is a candidate: "any" is 98–99% for
  `import_resolved` in jsoup and tinyxml2 while top1 is 45–68%
  (`Jsoup.parse` has 8 overloads, tinyxml2's `XMLTest` 4). Selecting
  overloads by arity needs the argument count in the ref facts (M7). In
  languages with overloading, a subtype's overload no longer hides the
  supertype's others (`Document.text(String)` vs `Element.text()`); such a
  call is `ambiguous`, not a wrong `type_hinted`.
- **Coverage** is limited by SCIP references that are not calls
  (functions passed as values, decorators, method references), by
  attribute calls on values the hints do not type (Python, Kotlin DSLs),
  and for types by instantiations being scored as calls.
- **tinyxml2 exposes runtime gaps** (risk 6). The pure-Go runtime
  mis-parses `if (a && T::f(b->c(), d()))` as an rvalue-reference
  declaration, and its error recovery then wraps most of `tinyxml2.cpp`
  and `tinyxml2.h` in error nodes. Queries still find most definitions
  inside them; hints and scopes inside the broken regions are lost.

Fixes the Tier 2 runs found, all in before the numbers above (calls recall
before → after):

| Language | Fix | Recall |
|---|---|---|
| Rust | `use crate::X` resolves from the crate root (the directory of the nearest `lib.rs`/`main.rs`; `crate::`/`self::`/`super::` specs are memoized per directory); builder chains `T::new(a).m(b).n()` follow each call's declared result, `Self` included, in receivers and in `let` hints (call hints keep the chain, arguments and type arguments dropped); a call prefers a method over a field of the same name | 0.776 → 0.961 |
| Java | an import of one item binds its last segment, so single-type imports shadow the package and `import static a.b.C.m` resolves `m()`; casts `((T) x).m()` give the receiver's type | 0.738 → 0.811 |
| Kotlin | a same-file or same-unit type wins over one reached by a wildcard import; a function whose result is its own type parameter (`mock<T>(): T`) gives an unknown type, not an external one; calls inside lambdas with receivers fall back to methods by name (Kotlin, Swift, Scala, Ruby) | 0.561 → 0.755 |
| C / C++ | a bodiless declaration is dropped when a definition of the same member is a candidate (header vs out-of-line definition); before parsing, null directives (`#  // …`), conditional groups opened inside an expression and export macros (`class FOO_API Bar`) are blanked, keeping offsets | C 0.991 → 0.998, C++ 0.351 → 0.564 (types 0.163 → 0.987) |
| C# | the enclosing namespace wins over `using` directives (explicit imports, then the unit, then wildcard and whole-module imports, for every language); extension methods resolve on their `this` parameter's type; `class_declaration` is matched positionally because the runtime drops its fields after a generic base list | 0.760 → 0.857 |

Swift, Objective-C, Dart, Bash, Ruby and PHP have no reliable SCIP
indexer and are covered by the per-pack goldens only.

### M11 results (old graph removed, 2026-09-26)

M11 moved ahead of M7–M10: after M6.3 nothing used the old graph at
runtime, yet `make build` still linked its cgo grammars.

- **Deleted from `internal/graph`:** the SQLite `Store`, the type-checking
  `Indexer` (`go/packages`), `RequestScope`, the fsnotify `Watcher`, Merkle
  hashing, the cgo tree-sitter tier (`treesitter.go`, `smacker/go-tree-sitter`)
  and their tests (about 7,700 lines). The shared types (`Symbol`,
  `SearchResult`, `Stats`, …) moved to `types.go`. `codebase_context` now
  requires the index; its SQLite/FTS fallback is gone.
- **Tests ported, not dropped.** The `codebase_context` tests (validation,
  budgets, bounded reads, symlink escape, staleness) run against a real
  `IndexScope`. The code map, tool plumbing and activation tests run against
  in-memory fakes of `Backend`, because they need exact control of the data
  (duplicate symbols, unsafe paths, insertion order, edges added mid-test).
  The parity tests against the old store are gone with it. The M6 exit
  criterion "parity with the old tree-sitter tier on its tests" is covered
  by the per-pack goldens instead.
- **Config and data:** `workspace.graph_db` is removed. The index lives in
  the project data directory (`<data>/index`), as before for the default
  configuration; an explicit `graph_db` in existing YAML is ignored. A
  leftover `graph.db` is no longer copied from `.chronos-code/`.
- **Build:** no package uses cgo, so `CGO_ENABLED` defaults to 0 and there
  is one build profile; `size-check-full` and its 80 MiB gate are gone.
  `make bench-index` now runs the indexer benchmarks and `BenchmarkScope*`
  (which query `Reconcile` instead of the deleted `IndexAll`).
- **Size:** release (linux/amd64) 52.9 → 51.7 MB; local `make build`
  (darwin/arm64) 77.4 → 52.2 MB.

## Plan after M2

Status: agreed direction (2026-09-25). The sections above still describe M1–M4
for Go; this section extends them.

### Decisions

| Topic | Decision |
|---|---|
| Consumer | Agents, through tool calls. The indexer never calls an LLM, neither while indexing nor while answering. |
| Goal | Fewer tool-call turns and fewer wasted tokens before the agent has enough context to act. |
| Retrieval | Exact names, BM25 and graph traversal. No embeddings for code or documents. |
| Parser runtime | Go keeps `go/parser`. Other languages use the pure-Go tree-sitter runtime `github.com/odvcencio/gotreesitter` (pinned) in every build, bounded by a per-parse deadline and memory budget. No cgo backend. Decided by the M6 spike ("M6 parser runtime spike"). |
| Scale | Million-file monorepos. |
| Languages | All languages in the table below get syntactic support at once (M6). Resolution quality is then improved language by language (M7). |

Why this runtime: release builds cross-compile six targets with
`CGO_ENABLED=0`, so today they index Go only. The old graph's tree-sitter tier
(`internal/graph/treesitter.go`) exists only in local cgo builds. The pure-Go
candidate ships 206 grammars. Its own benchmark page (BENCH.md, v0.55.0)
reports full parses at about 3–5× the C runtime's time (median about 3×
across languages), and it is pre-1.0. The M6 spike confirmed that median
but found 87–97× cliffs for C# and Swift, so single-file edit parses stay
inside 50 ms for most languages but not all of them (see "M6 parser runtime
spike"). The cost shows up mostly in fresh builds, which is why fresh builds
are progressive and can be seeded from a prebuilt base (see "Scale").

### Languages

Every language produces the same facts (next section). "Resolver inputs" are
the files the language resolver reads to turn names into edges. "Precise
source" is the optional type-checked tier (M10). Whether each grammar exists
in the pure-Go registry and each SCIP indexer works in practice is checked
during M6 and M10.

| Language | Extensions | Resolver inputs | Precise source |
|---|---|---|---|
| Go | `.go` | `go.mod`, `go.work` | `go/packages` (M4) |
| TypeScript / JavaScript | `.ts .tsx .mts .cts .js .jsx .mjs .cjs` | `package.json` (incl. `exports`), `tsconfig.json` (`paths`, `baseUrl`), npm/pnpm/yarn workspaces | scip-typescript |
| Python | `.py .pyi` | `pyproject.toml`, `setup.cfg`, `__init__.py`, src layout | scip-python |
| Java | `.java` | `pom.xml`, `build.gradle(.kts)`, package directories | scip-java |
| Kotlin | `.kt .kts` | Gradle | scip-java |
| Scala | `.scala .sc` | sbt, Mill | scip-java |
| C / C++ | `.c .h .cc .cpp .cxx .hpp .hh .hxx` | `compile_commands.json`, `CMakeLists.txt` | scip-clang |
| Objective-C | `.m .mm` (+ `.h`) | Xcode project, `compile_commands.json` | none known, so syntactic only |
| C# | `.cs` | `.csproj`, `.sln` | scip-dotnet |
| Rust | `.rs` | `Cargo.toml`, module tree from `lib.rs`/`main.rs` | rust-analyzer (SCIP output) |
| Swift | `.swift` | `Package.swift`, Xcode project | none in SCIP, so syntactic only |
| Ruby | `.rb` | `Gemfile`, Rails conventions | scip-ruby |
| PHP | `.php` | `composer.json` (PSR-4 autoload) | scip-php (community) |
| Dart | `.dart` | `pubspec.yaml` | community indexer |
| Shell | `.sh .bash` | none | none |

Non-language sources that become graph nodes (M7–M8):

- **Contracts:** Protobuf/gRPC, Thrift, GraphQL, OpenAPI/Swagger, SQL DDL.
- **Build graphs:** Bazel/Buck `BUILD` files (Starlark), Gradle/Maven modules, Cargo and npm workspaces.
- **Deploy config:** Terraform (HCL), Kubernetes/Helm YAML, Dockerfile.
- **Documents:** Markdown and plain text.

### Facts v2

`facts.go` is Go-shaped: methods are modelled by `Receiver`, `Package` is an
import path, and there are seven kinds. v2 is language-neutral. It keeps the
M1 invariant that facts come from one parse of one file and never depend on
other files.

| Record | Fields |
|---|---|
| File | as v1, plus `unit` (module, package, crate or build target id), `generated`/`vendored`/`test` flags |
| Symbol | name, kind, `container` (index of the enclosing symbol, or none), signature, doc summary, line range, visibility (public, protected, internal, private, package), modifiers bitset (static, abstract, async, override, deprecated, test), decl-or-def (C/C++ headers) |
| Import | spec (module path, header or package), alias, imported names with their aliases (`from x import a as b`, `import {a as b}`), kind (module, wildcard, re-export, include), line |
| Export | exported name, source spec, source name (JS/TS `export … from`, Python `__all__`, Rust `pub use`) |
| Ref | kind (call, type use, extends/implements, instantiate, decorator/annotation), enclosing symbol, name, qualifier text, qualifier kind, line, col |
| Binding hint | enclosing symbol, local or field name, declared or constructed type name (`x := &Engine{}`, `Foo x = new Foo()`, `self.repo: Repo`) |

Symbol kinds form an open, extensible list: func, method, constructor, class,
interface, trait, protocol, struct, enum, enum member, field, property, var,
const, type alias, module, namespace, macro. Contract kinds (route, rpc,
message, topic, table) are added in M8.

Binding hints give a cheap middle tier between import resolution and name
matching. `x.Save()` resolves to `Repo.Save` when a hint says `x` is a
`Repo`. The confidence ladder becomes:

`type_checked` > `import_resolved` > `type_hinted` > `name_matched` > `ambiguous`

Extraction is YAML-first, following repository convention. Each language is a
pack under `internal/indexer/extract/packs/<lang>/`, embedded with `go:embed`.
A pack has a YAML file (extensions, kinds, visibility rules, test-file
patterns) and tree-sitter query files (`.scm`) that capture definitions,
imports, exports, refs and binding hints. The upstream grammars' `tags.scm`
queries are the starting point. Go code is written only for resolvers that
need real logic. Packs stay inside the indexer's import boundary.

### Resolution

Resolution stays a pure function of (ref facts, the file's imports, the
symbol table) and runs against a snapshot, as in M1.

- **Generic resolver (M6), all languages.** The order is: same file, then the
  same container, then explicitly imported names, then the same unit, then
  units named in build-target dependencies when known, then a global name
  match. Each step sets the confidence label.
- **Language resolvers (M7).** Module and package semantics: Java, Kotlin, C#
  and Scala packages; TS/JS module resolution with `paths`, `exports` and
  barrel re-exports; Python relative imports and `__init__` re-exports; the
  Rust module tree; C/C++ include paths from `compile_commands.json` and
  declaration↔definition linking across headers; PHP PSR-4; best effort for
  Ruby, Swift, Objective-C and Dart.
- **Build graph.** In monorepos, Bazel/Buck target dependencies limit the
  candidate set for a name to the units the caller's target depends on. This
  is the biggest accuracy gain for `name_matched` at scale.

### Resolved graph

Graph traversal cannot re-resolve every hop at query time once the repository
reaches a million files. Resolved edges become a derived index. Facts stay the
source of truth.

- **Nodes:** symbol, file, unit, build target, contract, document section.
- **Edges:** contains, calls, type use, extends/implements, imports
  (unit→unit), tests (test→symbol), documents (section→symbol or file),
  produces/consumes (symbol→contract), depends (target→target), co-change
  (file↔file, from git). Each edge carries a type and a confidence byte.
- **Storage:** CSR adjacency (forward and reverse) in graph segments, mmap'd
  and sharded like the fact segments. Nothing proportional to repository size
  is held on the heap.
- **Incremental maintenance.** When a publish changes files F, the edges to
  re-resolve are the outgoing refs of F plus the refs anywhere whose name is
  a symbol added to, removed from or changed in F. The `calls`-by-callee-name
  table finds the second set. The new edges go into a small patch layer that
  readers consult before the base. Compaction folds patches into the shard.
- **Very common names** (`get`, `Close`, `String`) can hit thousands of sites.
  Above a cap they stay unresolved in the patch and are resolved lazily at
  query time, labelled accordingly. This keeps edit cost bounded.
- **Global importance** (PageRank over the resolved graph) is recomputed in
  the background per generation. It serves as a tie-breaker and for map
  queries.

### Graph retrieval

Used by `codebase_context`, by per-turn prefetch, and by the intent-level
tools that come after M3.

1. **Seeds.** Handles the agent passes in, then exact identifiers and paths
   found in the task text (with code-aware splitting: camelCase, snake_case,
   paths), then BM25 over symbol names, doc comments, paths and document
   sections. A name-like token that matches nothing is reported as a miss.
   It is not expanded into unrelated results.
2. **Expansion: local personalized PageRank with the push method**
   (Andersen–Chung–Lang) from the weighted seeds. Its cost depends on the
   push threshold, not on graph size, so it stays fast at a million files.
   Edge weights combine edge type and confidence. `ambiguous` edges are split
   across their candidates, and `name_matched` edges are down-weighted.
   Generated and vendored nodes are penalised; test nodes are penalised
   unless the task asks for tests.
3. **Scoring.** The expansion score is combined with BM25 relevance and the
   global prior.
4. **Diversification.** Coverage of files and units, so the budget isn't
   spent on one file.
5. **Packing at several zoom levels.** Greedy by score per token within the
   token budget:
   - top items get evidence windows inside their declaration
   - middle items get signatures only
   - the tail gets one line with a handle the agent can expand in one call
6. **Explanations.** Every item says why it is included ("caller of
   `Engine.Update` via `Watcher.flush`"). The push step keeps the best
   predecessor for each node, so this comes almost free.

Proposed latency budget at a million files (warm): seeds < 2 ms, expansion
< 5 ms, packing and excerpt reads < 10 ms, final token count < 5 ms. That
totals under 25 ms for a 4,096-token context.

### Scale: million-file monorepos

These M1 behaviours are fine at 418 files and break at a million:

| # | Problem | Where | Fix (M5) |
|---|---|---|---|
| 1 | Every publish copies the whole path-routing map and clones the base segment's live bitmap, so each edit costs O(files) | `store/snapshot.go:84-101` | Layered routing: binary search over each shard's sorted file table, plus a small overlay map and overlay tombstones. The routes are rebuilt only at compaction. |
| 2 | Opening a snapshot builds the full routing map | `store/snapshot.go:58-75` | Same fix. Opening does work proportional to overlays, not files. |
| 3 | A fresh build holds every file's facts in memory before writing | `store.Publish(files []*facts.File, …)` | A streaming writer that flushes one shard at a time. |
| 4 | There is one base segment, so compaction rewrites the whole repository | `store.Compact` | A sharded base, split by path prefix and sized by bytes. Compaction and graph rebuilds run per shard. |
| 5 | The watcher registers every directory with fsnotify. On macOS fsnotify uses kqueue, which needs a file descriptor for every watched file. | `watch.go:146-163` | Watcher backends picked by repository size: fsnotify for small repos, Watchman when installed, git's fsmonitor (`core.fsmonitor`) through `git status`, and polling of dirty directories as the fallback. |
| 6 | A full reconcile lists and stats every file | `scan.go:65-73` | Store the indexed commit in the manifest. Changed files are then `git diff --name-only <indexed>..HEAD` plus `git status`. A full listing happens only on explicit `indexer verify`. |

Other scale requirements:

- **Progressive fresh build.** The working set is indexed first: the current
  directory's subtree, files changed recently in git, and files the agent has
  touched. Results report `coverage` until the build finishes.
- **Prebuilt base.** Segments are portable: root-relative paths, deterministic
  bytes, keyed by commit. CI can publish shards for commit X. A client
  downloads them and indexes only `X..HEAD` plus the working tree. Immutable
  segments make this straightforward.
- **Sparse and virtual checkouts** (sparse checkout, partial clone, virtual
  filesystems). Never read a file git reports as not present, because reading
  it can trigger hydration. The prebuilt base covers the rest.
- **Memory ceiling.** Heap stays bounded by caches. Segment and graph data are
  mmap'd. Target: heap < 1 GB at a million files.
- **Grammar loading.** Grammars load lazily on the first file of each
  language and are then kept. Some grammar blobs are large (the candidate
  runtime documents Swift at about 16 MiB decompressed).
- **Size limits.** Minified or generated files above a size limit, and paths
  marked `linguist-generated` in `.gitattributes`, are recorded as files
  without symbols.

Proposed targets at a million files (warm; measured on a synthetic corpus plus
one large real repository per major language):

| Operation | Target |
|---|---:|
| Edit visible to queries | < 50 ms |
| Restart, then first query | < 100 ms |
| Exact symbol lookup | < 2 ms |
| Callers at depth 3 | < 10 ms |
| Graph retrieval, 4,096 tokens | < 25 ms |
| Working set (about 5k files) queryable after a fresh start without a prebuilt base | < 5 s |
| Heap | < 1 GB |

### Cross-repo and documents

- **Federation (M9).** Each repository keeps its own index. A workspace is a
  set of indexes. Queries fan out across snapshots.
  - Imports of another indexed repository's module (`go.mod`,
    `package.json`, Maven coordinates, Cargo) resolve into that
    repository's index.
  - Contract nodes join across repositories by global key: proto full name
    `pkg.Service/Method`, normalised route `GET /v1/users/{id}`, topic name,
    table name.
- **Contracts (M8).** Framework recognisers are YAML query packs, like the
  language packs. For example: Spring `@GetMapping`, Express `app.get`,
  FastAPI `@app.get`, `net/http` handlers, gRPC server and client stubs,
  Kafka producers and consumers. They emit contract refs, so the graph can go
  from a client call site to the route to the handler.
- **Documents (M8).** Markdown and plain text are split into sections by
  heading and indexed in BM25. Links to code are deterministic, with
  confidence from strongest to weakest:
  - backticked identifiers that match the symbol table
  - file paths
  - routes and URLs
  - ticket ids
  - bare identifiers

  Other formats (PDF, Word) are converted to text before indexing. How that
  conversion happens is decided in M8.

### Evaluation

This runs from M3 onward. It is the counterpart to the M0 speed harness.

- **Edge accuracy for each language.** Resolved edges, per confidence label,
  are compared against SCIP output on a small open-source corpus for each
  language. Report precision and recall. Each label must mean what it claims.
- **Retrieval.** Tasks with gold files and symbols, drawn from this repository
  and `../chronos` and later one corpus per language. Report recall at the
  token budget, and tokens delivered compared with tokens used.
- **Agent runs.** For the same tasks: tool-call turns before the first correct
  edit, and how often the agent falls back to grep or `read_file` to check a
  result, with and without the indexer.

### Package layout additions

```
internal/indexer/
  extract/treesitter/   pure-Go runtime wrapper: lazy grammars, deadline, memory budget
  extract/packs/<lang>/ YAML + .scm query packs, go:embed
  extract/generic/      query-driven extractor shared by every pack
  resolve/              generic resolver; resolve/<lang>/ for language semantics
  graph/                resolved-graph segments (CSR), patches, global prior
  retrieve/             seeds, push-PPR expansion, scoring, packing (backs context/)
  contracts/            framework recognisers → contract nodes
  docs/                 document sections, mention linking
  federation/           workspace of several indexes
```

The import-boundary rule still applies: nothing under `internal/indexer/`
imports other chronos-code packages. It moves to a public package path, with
an MCP adapter, in M9.

## Milestones

Each milestone ends with `make test` (race), `go vet`, the benchmarks, and its
acceptance criteria met.

| # | Scope | Acceptance |
|---|---|---|
| M0 | Remove the sidecar from build/release/install; benchmark harness; baseline numbers | Release workflow builds no external indexer; baseline table committed to this doc |
| M1 | `scan`, `extract/golang`, `segment`, `store` (manifest, overlays, lock), worker/writer pipeline, watcher | Fresh build < 1.5 s and single edit < 50 ms on this repo; crash-before-publish test; corruption detection test |
| M2 | `query/`; graph tools moved onto it; `RequestScope` and startup no longer block; `names` batching; labelled empty results | Tool contract tests pass unchanged; no `packages.Load` on any query path (fake-`go` test); a batched call returns the same per-name results as separate calls; empty results report confidence and freshness |
| M3 (done) | `context/` backed by `graph/` (resolved-graph segments) and `retrieve/` (push-PPR expansion, packing at several zoom levels); per-turn prefetch; retrieval and turn-count eval for Go | `codebase_context` and prefetch < 15 ms uncached; budget never exceeded; `seen` and invalidation tests; eval baseline recorded in this doc |
| M4 | `precise/` | Type-checked caller parity with the old graph; edits stay < 50 ms while precise loads run |
| M5 (done) | Scale foundations: layered routing, streaming sharded base, compaction per shard, watcher backends, git-based reconcile, progressive build, portable segments | On a synthetic million-file corpus, every target in "Scale" is met; a test counts work on the edit path and shows none proportional to repository size; macOS watching uses no descriptor per file |
| M6 (done) | Parser runtime spike (pure Go vs cgo: MB/s per language, memory, grammar load time); facts v2 with format version bump; query packs for every language in the table; generic resolver; binding hints | Runtime decision recorded with measurements; every listed language produces symbols, outlines and refs in release builds; parity with the old tree-sitter tier on its tests; baseline edge precision for each language against SCIP |
| M7 | Language resolvers and build graphs (Bazel/Buck, Gradle/Maven, workspaces, `compile_commands.json`) | `import_resolved` and `type_hinted` precision for each language meets the target set from the M6 baseline; no edit-latency regression |
| M8 | Contracts (Protobuf/gRPC, Thrift, GraphQL, OpenAPI, SQL DDL, framework recognisers) and documents (Markdown and text sections, mention links) | Client call → route → handler paths are found in fixtures for each recogniser; document↔code links are tested; retrieval eval includes document tasks |
| M9 | Federation across repositories; public package path; MCP adapter | Cross-repo import and contract joins are tested; an external agent gets the same results over MCP as the in-process tools |
| M10 | Precise tier for other languages through SCIP import, when the toolchain is present | A precise edge is used only when its file hash matches; indexing and edits never block on an external indexer |
| M11 (done) | Delete old `internal/graph` store and indexer, including its tree-sitter tier | No dead code; docs updated |

Order: M2 → M3 → M5 → M6 → M7 → M8 → M9. M4 and M10 are independent and can
move. M5 comes before languages because M6 multiplies the file count that the
store and watcher must handle.

M2 must keep the old tree-sitter tier (local cgo builds only) answering for
non-Go files until M6 lands, so that local builds don't lose non-Go results
in between. Release builds are unaffected, because they never had that
tier. (Resolved: M6.3 stopped merging the tier and M11 deleted it.)

## Open questions and risks

1. **Custom format cost.** It's more work than SQLite, and the payoff at this
   repo's scale is unproven. Mitigation: the format is kept minimal (sorted
   fixed-width tables, no compression, no FST) until benchmarks justify more.
2. **Tool overlap.** 11 graph tools inflate schema tokens. Merge them after M3
   once usage is measured.
3. **Windows mmap and locking.** These need CI coverage on `windows-latest`.
4. **Multiple sessions on one repo.** Readers following another process's
   manifest need a reload trigger (poll the manifest mtime, or fsnotify on the
   index directory).
5. **Method-call accuracy before the precise tier finishes.** Results labelled
   `name_matched`/`ambiguous` may mislead agents. Tool output must show the
   label, and context selection excludes them from graph expansion. From M6
   on, graph retrieval down-weights these edges instead of excluding them. In
   Python, Ruby or JavaScript most edges are `name_matched`, so excluding them
   would leave the graph nearly empty.
6. **Pure-Go parser runtime maturity.** The candidate is pre-1.0, changes
   quickly and has a large API. Mitigation: it sits behind
   `extract/treesitter`'s interface and its version is pinned; every parse
   has a deadline and a memory budget. Output is checked against the
   per-pack goldens and against SCIP gold. Known gaps at v0.55.0: C#/Swift parse
   cliffs and `no_stacks_alive` stops on invalid input (see "M6 parser
   runtime spike"); `TestFailedRecoveryKeepsPartialTree` pins the latter.
7. **Fresh-build time at a million files.** At 3–5× the C runtime's parse
   time, a fresh build can take minutes. Mitigations: progressive working-set
   indexing, coverage reporting and prebuilt bases from CI.
8. **Syntactic accuracy in dynamic languages** (Python, Ruby, JavaScript,
   PHP). Binding hints and build-graph scoping help. The eval shows what the
   labels actually deliver, and the precise tier (M10) covers the rest where
   a toolchain exists.
9. **Grammar coverage.** Grammar availability in the pure-Go registry is
   confirmed for each language during M6. A language without a working
   grammar falls back to file-level indexing (paths and BM25 only) until
   one exists.
