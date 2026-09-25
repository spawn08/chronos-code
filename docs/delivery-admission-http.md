# Delivery admission HTTP slice

`chronos-code serve` with API-key or OIDC authentication opens a durable SQLite
delivery store at the resolved project's `deliveries.db`. The HTTP API exposes
**admission and inspection**. Default records remain in `admitted` state.
An optional `serve --delivery-read-only-worker` queues explicit read-only
admissions, processes them using the common execution path, and parks their
results for acceptance verification. The existing
`/v1/chat` endpoints retain their bounded synchronous behavior.

`POST /v1/deliveries` requires an authenticated principal, the
`delivery.admit` repository action, and an `Idempotency-Key` header of 1–256
bytes. The body is `{"goal":"...","requirements":[{"statement":"...",
"checks":["..."]}]}`; requirements are optional. If omitted, one accepted
requirement is derived from the goal, without invented acceptance checks.
Tenant and repository identity come from authentication and server configuration,
not from the body. Unknown fields are rejected. A durable admission returns
HTTP 201 with its ID, `admitted` state, version, goal, requirements and a
`Location` header. Repeating the same key and content returns the same record;
changing the content under that key returns HTTP 409. A default admission does
not start work or imply future execution has already been scheduled. With an
explicit `"run_read_only": true` and an opted-in read-only worker, admission,
the queue entry and their events commit atomically and return HTTP 202 in
`queued` state. Without a configured worker, that request returns HTTP 503
before admission. Changing execution mode under the same key returns HTTP 409.
The read-only worker parks its result for verification; it cannot write to the
repository or declare the delivery complete.
An authenticated admission may specify `max_cost_microdollars` as immutable
delivery authority, displayed with cumulative usage on inspection. A capped
read-only execution request requires a worker with durable model-call
admission; otherwise it fails HTTP 503 before admission. A parked admission
can still retain the cap. Unknown pricing/usage is never reported as
zero spend, and actual usage is retained even when it exceeds a reservation.

`GET /v1/deliveries/{id}` requires `delivery.inspect` and returns the scoped
record or HTTP 404. Authentication is mandatory on both endpoints (`auth: none`
returns HTTP 403). The ID is stable across server restarts. The idempotency key
is never included in the HTTP response.

An authenticated request may include `plan_generation` (strict strategist JSON
with `source_request_ref`, `classifier_ref`, and bounded `nodes`). Only an
explicitly installed plan-capable worker accepts it; the default read-only
worker returns HTTP 503 without writing an admission. The host derives the
tenant, repository, task, plan, and generation (`1`) from the admission identity.
The plan proposal is persisted first, then delivery admission and queue state
commit together; retrying the same key/proposal repairs a crash between the two
databases. A changed proposal is rejected with HTTP 409. Plan results park for
acceptance verification; the packaged server does not enable write-capable plan
workers while the F10 acceptance gate is pending.

The delivery aggregate, ordered events, worker attempts, and queue share the
same SQLite database. `QueueAdmitted` commits a promotion event, state, and
queue entry in one transaction. Plan generations use a separate database;
their persistence and queue promotion are recoverable by idempotent retry,
not an atomic cross-database commit.
Legacy plan generations have separate timed node leases: the controller renews
an active lease, and a resumed scheduler parks expired or pre-migration leases
as blocked/ambiguous rather than replaying an unobserved effect. A stale owner
cannot finalize after expiration. Plan generations still require explicit
reconciliation/replanning before that work can safely continue.

A host-authorized `plan.reconcile` operation can accept an applied plan patch
after a crash before its plan receipt, but only while the delivery is parked.
The receipt must bind the original attempt and passed verification, and the
retained patch, parent revision, and selected file postimages must still match.
The operator transition fences the queue and commits the accepted artifact with
the plan node; it never reapplies the patch or assumes unknown provider/API
effects succeeded. Conflicting edits or an unverified receipt remain parked.

`server.delivery_http.request_url` and `observation_url` may configure a single
destination-specific `http_observed_mutation` tool. The destination must honor
the host-issued `Idempotency-Key` and return the original HTTP response as
`{"status_code":...,"headers":{...},"body":"..."}` from
`GET <observation_url>/<effect-key>` (404 means unknown, not safe to replay).
Both URLs must share an HTTPS origin (loopback HTTP is allowed); generic HTTP,
SQL, and shell mutations have no such observer and park on ambiguous outcomes.

Offline snapshots: `chronos-code delivery backup <new-path>` takes a validated,
consistent SQLite snapshot including WAL changes. `chronos-code delivery
restore <source> <new-path>` checks schema, checksums, integrity and foreign
keys before writing a **new** database. Stop the server/workers and configure
the restored path before serving it; restore never replaces an open database.
Keep a pre-migration backup to roll back with an older binary. A binary that
sees a newer schema refuses to start rather than silently downgrading records.
Backup destinations must be in private directories (0700), and the new database
files are set to 0600. Restores do not overwrite existing paths.

The operation schema persists prepared/running/observed/reconciled effects with
lease fencing, stable effect keys, input/output fingerprints and ordered events.
An ambiguous running operation requires explicit host reconciliation; it is
not retried automatically. SDK model/tool after-hook failures now stop the
turn instead of being silently discarded. Binding every production tool call
and provider continuation to this journal remains a separate F06 integration
gate. An unattended shell requires a host-supplied, locally available image
pinned by `@sha256:<64 hex characters>` in `server.delivery_sandbox.image`.
The optional `socket_path` must be an absolute Unix socket; network is denied
unless `allow_network: true` is explicitly set. The container sees only the
admitted workspace as a writable host mount, uses a read-only image filesystem,
and receives a fixed environment. Missing Docker, an unpinned/unavailable image,
or an unsupported daemon API fails before shell execution. The legacy bounded
execution-v1 shell still uses its documented approval path.

The real Docker isolation test is opt-in and runs with
`CHRONOS_CODE_SANDBOX_TEST_IMAGE=<locally-cached-pinned-image> go test ./internal/security -run TestMandatoryContainerSandbox -count=1`.
