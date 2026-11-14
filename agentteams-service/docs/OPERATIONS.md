# agentteams-service R1 operations runbook

Status: R1 (workers only). This is the operator view of the service; the API
contract and verification evidence are in the service README and tests.

## Configuration

Environment variables use the AGENTTEAMS_SERVICE prefix (flags win over env,
env wins over defaults).

| Variable | Required | Default | Meaning |
|---|---|---|---|
| AGENTTEAMS_SERVICE_RES_DSN | yes | - | res schema DSN. The pool opens every transaction here; ops tables are referenced schema-qualified, so this user must also have ops privileges. |
| AGENTTEAMS_SERVICE_OPS_DSN | yes | - | ops schema DSN (migrations/health use it). |
| AGENTTEAMS_SERVICE_LISTEN_ADDR | no | :8080 | TCP listen address (binds all interfaces; put the service behind a gateway in real deployments). |
| AGENTTEAMS_SERVICE_UDS | no | empty | Optional Unix-socket health face (0600). |
| AGENTTEAMS_SERVICE_CONTROLLER_URL | required when enabled | - | Controller internal API base URL (Validate rejects enabled-without-URL). |
| AGENTTEAMS_SERVICE_INTERNAL_TOKEN | required when enabled | - | Shared bearer for /internal/v1 (token review, provision, deprovision, execution). |
| AGENTTEAMS_SERVICE_OUTBOX_INTERVAL | no | 5s | Dispatcher poll interval. |
| AGENTTEAMS_SERVICE_DEFAULT_WORKER_RUNTIME | no | openclaw | Runtime applied when create omits it. |
| AGENTTEAMS_SERVICE_DEFAULT_MODEL | no | empty | Model applied when create omits it. |
| AGENTTEAMS_SERVICE_SHUTDOWN_TIMEOUT | no | 15s | Graceful shutdown budget. |
| AGENTTEAMS_SERVICE_LOG_LEVEL | no | info | debug/info/warn/error (JSON slog). |

## Startup and migrations

1. Open res and ops pools, ping both.
2. Acquire the kernel:upgrade lease in ops.upgrade_leases (one migrator across
   replicas), then apply embedded migrations forward-only, one statement per
   file, per schema schema_migrations.
3. Start the outbox dispatcher only when CONTROLLER_URL and INTERNAL_TOKEN are
   both set; otherwise events accumulate for a later deployment.
4. Serve TCP (and optional UDS) health and API.

A crash between DDL and the migration record is safe: every file is
CREATE ... IF NOT EXISTS (or CREATE OR REPLACE VIEW), except the 0024
ALTER, which is covered by the runner's duplicate-name tolerance
(see migrate.go).

## Health

- GET /healthz: process alive. 200 even when the DB is down.
- GET /readyz: 200 {"status":"ok"} when all checks are ok; 200
  {"status":"degraded",...} when degraded; 503 when any check is down.
- UDS serves the same two routes only.

## Failure matrix (verified R1 drills)

| Condition | Observed behavior |
|---|---|
| MySQL stopped | healthz 200, readyz 503, list/create 500 internal; after restart all recover without restarting the service. |
| Controller token-review unreachable | create 502 upstream_unavailable, message identity_resolution_unavailable. |
| Controller token-review OK, provision 503 | create still 201; outbox row status failed with attempts=1 and next_attempt_at backoff; recovery publishes it. |
| Service SIGTERM | in-flight requests drain within the shutdown budget; exit code 0. |
| Restart with failed outbox rows | dispatcher resumes from next_attempt_at; rows publish after the controller recovers. |
| Retry exhaustion | after 5 failed attempts status=dead, attempts=5, last_error kept; no further attempts. |

Retry policy: base 5s, doubling per attempt, cap 5m, dead at 5 attempts
(kernel constants).

## Outbox statuses

- pending: never attempted or waiting for the first attempt.
- failed: an attempt failed; next_attempt_at is set (this is the retry queue).
- published: delivered (or deprovision 404, which is treated as done).
- dead: retries exhausted or the payload/event type is undeliverable.

Listing the backlog:

    SELECT status, COUNT(*) FROM ops.outbox GROUP BY status;
    SELECT id, worker_name, event_type, attempts, next_attempt_at, last_error
      FROM ops.outbox WHERE status = 'failed' ORDER BY next_attempt_at;

A forced replay is safe because delivery is idempotent on
(name, generation, event_type, incarnation): reset status to pending and
next_attempt_at to NULL, do not edit the payload.

## Idempotency

- Optional Idempotency-Key header, at most 128 characters, 24h TTL.
- A retry after the first response replays the stored status and body
  byte-for-byte, including across a service restart.
- Concurrent requests with the same key are not serialized: one wins, the rest
  see 409 until the first record exists; a later retry replays the winner.
- The record is written before the response; a failed record write is logged
  and the create still succeeds (fail-open for availability).

## Request limits

- Bodies: 1 MiB (MaxBytesReader); larger requests get 400 invalid_request.
- A value longer than its column (MySQL 1406) is 400 invalid_request, never 500.
- TCP: 5s read-header, 30s read, 60s write, 120s idle, 64 KiB headers.

## R1 known gaps

- team/role and exposedPorts are empty/omitted (no DB columns yet).
- The PUT/DELETE write-through is a transparent proxy; native write authority
  is R2.
- The dispatcher assumes one instance (no SKIP LOCKED lease); R2 may add it.
- Spec hashes from the controller and service are not comparable; execution
  refresh uses generation only.
