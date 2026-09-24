# Delivery admission HTTP slice

`chronos-code serve` with API-key or OIDC authentication opens a durable SQLite
delivery store at the resolved project's `deliveries.db`. The HTTP API exposes
**admission and inspection** only. Records remain in `admitted` state and are
not runnable. An optional `serve --delivery-read-only-worker` can process
explicitly queued records using the common execution path with read-only tool
effects, then parks their results for acceptance verification; the API does not
queue new admissions. The existing
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
changing the content under that key returns HTTP 409. An admission does not
start work or imply future execution has already been scheduled.

`GET /v1/deliveries/{id}` requires `delivery.inspect` and returns the scoped
record or HTTP 404. Authentication is mandatory on both endpoints (`auth: none`
returns HTTP 403). The ID is stable across server restarts. The idempotency key
is never included in the HTTP response.

F05 must wire an executor and atomically move admitted records into the queue
before the API can describe them as scheduled or runnable.

The delivery aggregate, ordered events, worker attempts, and queue share the
same SQLite database. `QueueAdmitted` commits a promotion event, state, and
queue entry in one transaction. Plan generations use a separate database;
crossing that boundary will require an idempotent outbox consumer before any
plan-to-delivery handoff is advertised as atomic.

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
