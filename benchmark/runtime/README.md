# Configured runtime evaluation

This fixture exercises `TaskRunner -> ChronosCodeAdapter -> LiveExecutor ->
Orchestrator.Execute -> SDK tool loop -> real file/shell tools`. Its
OpenAI-compatible provider is a pinned script served on loopback. It performs no
billable model invocation and establishes **harness wiring and deterministic
correctness**, not live-model quality, speed, cost savings or Cursor parity.

## Repeat

From the repository root:

```sh
make eval-tasks-validate
make eval-runtime-test
make eval-fixture EVAL_OUTPUT=bin/eval/replay-001.jsonl
```

`eval-fixture` builds with the normal Makefile tags, runs two independent fixture
copies, uses a temporary external data home and removes that data home afterward.
The original fixture is never edited. Output files must not already exist; use a
new output name rather than overwriting evidence. `EVAL_REPETITIONS` controls the
repeat count. The accompanying `baseline-fixture.jsonl` records the offline run;
its source fingerprints describe the observed dirty trees at execution time.

A genuinely live run is opt-in and requires a real provider configuration:

```sh
make eval-live EVAL_LIVE_CONFIG=/path/to/provider.yaml \
  EVAL_MANIFEST=benchmark/runtime/manifest.yaml \
  EVAL_REPETITIONS=2 EVAL_OUTPUT=bin/eval/live-001.jsonl
```

That command was **not executed** for this baseline. Live configuration follows
the normal config/model-selection path, including applicable project/user config.
Fixture configuration is loaded directly from its explicit YAML, without those
ambient overlays. The adapter rejects task environment overrides and worktree
ablations rather than silently pretending they are applied.

## Objective oracle

The script reads `answer.txt`, replaces `original` with `resolved`, executes a
real shell check, and returns a final answer. A separate grader, run only after
execution, requires the exact nine-byte result and a diff touching only
`answer.txt`. The grader specification is not supplied to the agent. Tests also
serve a confident final answer without performing the edit and require the
external oracle to fail. The visible prompt/file are trivial; this is not an
unseen reasoning benchmark or a tamper-resistant execution sandbox.

## Recorded evidence

Each JSONL object uses `chronos.eval.run.v1` and contains:

- Executed binary SHA-256, Go/platform/build settings, code and SDK Git revisions,
  dirty status and source-byte fingerprints, and the resolved SDK module/replacement.
- Configured and effective redacted configuration plus SHA-256. Credentials,
  header/environment maps and DSNs are excluded. Effective workspace/loopback
  paths vary by run, so effective config hashes need not match repetitions.
- Manifest, materialized fixture, provider script, task prompt and oracle hashes;
  permissions, ablations and declared environment overrides.
- Runtime request observations from the real orchestrator: prompt/tool-schema
  hashes and available block hashes, budgets and mutable-tail observations.
- Start/end timestamps, terminal status, objective grader result/evidence, patch,
  observed model rounds/tool calls, distinct uncached-input/output/cache-read/
  cache-write totals, and explicit blocked/unpriced evidence.

Source fingerprints cover tracked/unignored `.go`, YAML, module/checksum files
and the Makefile, including local modifications. Fixture hashes cover relative
paths, permissions and all file bytes, excluding `.git`. Prompt and script hashes
pin their separate inputs. These are **observed source fingerprints**, not a
saved immutable compiler-input snapshot; concurrent tree edits can change them
while the executed binary hash remains constant. Retain the binary and source
work if exact dirty-build replay is required. Host environment/toolchains are not
hermetic. Runtime prompt hashes can differ due to workspace/session state.

Fixture usage is explicitly synthetic zero usage; it must never enter live-model
cost comparisons. Live model calls remain unpriced when per-call pricing evidence
is unavailable, even when a terminal cost is reported. Cache totals are not added
to the two distinct cache classes again. No one-hour cache-write subtotal is
observed by this adapter. A terminal-usage fallback is not counted as an observed
model round.

## Recorded baseline, 2026-10-16

The two recorded runs both reached `succeeded` and passed `deterministic-v1`.
Each observed four model requests, three executed tools and four runtime
prompt/schema observations. Patches and configured-input hashes were identical;
the original fixture remained `original\n`. Both used binary SHA-256
`b15d394002127f5741b6beb9b1e007f625bbe9cd207ebd75bca8ecb531949c88`,
code revision `8be54822f039fb039ca38eade253fb9f713e901a` on the recorded dirty
source tree and SDK revision `6a4be05546bf829be608235ade04fbc82f34484f`.
All usage classes are synthetic zero, with no live-model pricing observation.

Executed successfully: `make eval-fixture` with two repetitions,
`make eval-runtime-test` with normal build tags and `-race`, manifest validation,
`go test -race ./internal/eval ./internal/cli -count=1`, scoped `git diff --check`,
and JSONL assertions for metadata, objective outcomes, trace counts and replay
identity. These checks do not establish live-model performance.

## Remaining integration

`orchestrator.go` was not edited by this evaluation work. The current runtime
request hook supplies the hashes exercised by the fixture. Missing request
observations are recorded as blocked rather than synthesized. Before live
performance promotion, integrate/verify durable session-terminal events,
complete partial trace preservation on failures, correlated emitted/executed tool
calls, retry/repair counts, per-call usage-known/pricing metadata and affected
metrics (duplicate reads, artifact-only rounds, stalls). Preserve execution
failure classification when a subsequent oracle also fails. These integration
points are not established by successful fixture grading.

The baseline does not cover paired same-model comparisons, unseen holdouts,
crash/restart, interrupted side effects, mixed-language contracts or browser/UI
acceptance. Do not infer those outcomes from this fixture.
