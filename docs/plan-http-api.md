# Plan HTTP control API (first async-job slice)

The server now exposes **existing durable plan generations** through its authenticated HTTP API. This is inspection and control, not task submission or a background worker: plans must already have been created by the closed-loop plan runtime. The plan identity is the tuple `(task_id, plan_id, generation_id)` within an authenticated tenant and a repository; it is not yet a single job ID.

Every call needs a tenant-bearing API-key or OIDC identity and exactly one `repository_id` query parameter. `auth: none` cannot use these endpoints. The tenant is taken from the validated credential, never from the query or request body. Results are scoped by tenant and repository; wrong-scope references return 404.

| Method | Path | Result |
| --- | --- | --- |
| GET | `/v1/plans?repository_id=REPO` | Plan summaries, including state, stop reason and optimistic-lock version |
| GET | `/v1/plans/TASK/PLAN/GENERATION?repository_id=REPO` | Plan metadata: nodes, verification instructions, attempts, evidence IDs, and event IDs (with redacted idempotency keys) |
| GET | `/v1/plans/TASK/PLAN/GENERATION/stream?repository_id=REPO` | SSE `plan` snapshots, each with `id` equal to the persisted generation version |
| POST | `/v1/plans/TASK/PLAN/GENERATION/pause?repository_id=REPO` | Pause an active generation |
| POST | `/v1/plans/TASK/PLAN/GENERATION/resume?repository_id=REPO` | Resume a paused generation |
| POST | `/v1/plans/TASK/PLAN/GENERATION/cancel?repository_id=REPO` | Cancel a non-terminal generation |

Control requests require JSON `{"expected_version": N}` from the last inspection or stream snapshot; stale versions and invalid transitions return HTTP 409. Controls update durable state; pausing/canceling does **not** interrupt an already executing node. Resuming changes state but does **not** start a worker; no background worker exists yet.

The SSE stream sends a full plan snapshot when its version advances. Reconnect with `Last-Event-ID: N` to skip snapshots already seen; the current snapshot is replayed if newer. A cursor ahead of the current version is rejected. Streams are bounded by the server's request timeout (five minutes by default); clients should reconnect. This is versioned snapshot streaming, not a full replay of every intermediate transition. The plan store currently exposes verification instructions and evidence IDs, but not detailed verification results or a per-job budget.

**Still needed to complete point 1:** accept a task and return a single job ID immediately; persist admission and submission; run it independently of the HTTP request; expose detailed attempt verification and budget information. That requires a durable job/admission model and worker lifecycle rather than a detached goroutine tied to this server process.
