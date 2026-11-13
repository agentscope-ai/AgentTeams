# agentteams-service (R1)

The AgentTeams data-plane service kernel plus the R1 `resources` slice
(workers create / list / get, transparent PUT/DELETE proxy, status
writeback). Independent Go module, binary and release channel inside the
AgentTeams repository.

The API surface and boot contract are documented in this directory; the
runbook is [docs/OPERATIONS.md](docs/OPERATIONS.md).

## Layout

```
cmd/agentteams-service/     process entrypoint (boot order, exit codes, graceful shutdown)
internal/kernel/
  config/     flag > env > default; dual DSN; no credentials in code
  log/        log/slog JSON
  db/         res + ops pools; forward-only migration runner with ops.upgrade_leases
  module/     kernel module lifecycle (Init → Start, reverse Stop, worst-of health)
  httpapi/    /healthz, /readyz, kernel middleware, route mount, internal-token auth
  uds/        unix-socket health face (0600, stale-socket cleanup)
  identity/   CallerIdentity + token-review cache (60s) + auth middleware
  adapter/    service → controller internal client (58 §5)
  outbox/     ops.outbox enqueue + dispatcher (retry/backoff/dead-letter)
  audit/      ops.audit_events writer (same tx as the business row)
  idem/       ops.idempotency_keys (24h, byte-exact replay)
  httputil/   {code,message} envelope + spec-view ETag
internal/modules/resources/ R1 slice: types, store, spec_hash, handlers, views, authz, validate, proxy
migrations/res, migrations/ops   forward-only DDL (0001-0004 frozen; 0024 idempotency scope correction)
operations/workers.json          operation manifest (CLI/Skill generator input)
deploy/higress/routes.yaml       five-route split (never public: /internal/v1)
test/integration                 real-DB tests (build tag integration)
test/e2e                         real binary + fake controller (build tag e2e)
```

## Configuration

Precedence: command-line flag > environment variable > default. There is no
YAML file (54 §3). No DSN default and no sample credential exists in code.

| Env (`AGENTTEAMS_SERVICE_` prefix) | Default | Notes |
|---|---|---|
| `ENABLED` | `false` | G1 dual mode: false = boot nothing, exit 0 (58 §6) |
| `LISTEN_ADDR` | `:8080` | public HTTP face |
| `UDS` | empty | unix-socket health face, 0600 |
| `RES_DSN` / `OPS_DSN` | required when enabled | MySQL 8 / OceanBase / seekdb 5.7 |
| `CONTROLLER_URL` | required when enabled | controller internal API root |
| `INTERNAL_TOKEN` | required when enabled | shared service↔controller bearer |
| `SERVICE_ID` | `agentteams-service` | upgrade-lease holder / audit correlation |
| `DEFAULT_WORKER_RUNTIME` | `openclaw` | mirrors the controller default |
| `DEFAULT_MODEL` | empty | no service-side default |
| `OUTBOX_INTERVAL` | `5s` | dispatcher poll |
| `LOG_LEVEL` | `info` | debug / info / warn / error |
| `SHUTDOWN_TIMEOUT` | `15s` | bounds HTTP drain + module stop |

> **Deployment prerequisite**: the store runs `workers` + `ops.outbox` +
> `ops.audit_events` in **one** transaction on the res pool (a `*sql.Tx` is
> bound to one connection). The ResDSN account therefore needs
> INSERT/SELECT on the `ops` schema as well (58 §11 I3).

## Build / run / test

```sh
make build
make test            # unit
make race            # -race
make test-integration   # needs AGENTTEAMS_TEST_{RES,OPS}_DSN
make test-e2e           # same DSNs; runs the binary against a fake controller
make image IMAGE=agentteams-service:dev   # multi-stage, non-root, static binary
```

Integration tests run against MySQL 8 **and** standalone seekdb
(5.7.25-OceanBase); both are green as of 2026-11-13. Without the DSNs the
tests skip, so `go test ./...` stays green on a bare machine.

## Idempotency-Key is scoped to the authenticated principal and bound to the
canonical create request; reusing a key with a different body returns 409
`idempotency_conflict` (migration 0024).

## R1 scope and known gaps

- Native: `POST /api/v1/workers`, `GET /api/v1/workers`,
  `GET /api/v1/workers/{name}`. Proxy (D110): `PUT`/`DELETE` forward to
  the controller byte-for-byte; the service refreshes its row from the PUT
  response (write-through) and soft-deletes on 204/404.
- Status writeback: `POST /internal/v1/workers/status`
  (shared bearer + `X-AT-Timestamp` ±5 min + `X-AT-Request-Id`).
- `team`/`role`/`exposedPorts`: R1 returns `team=team_slug` (no writer yet),
  `role=""` and omits `exposedPorts` — the DB has no role or observed-port
  column (58 §11 I7/I14).
- get-by-name refreshes observed state and soft-deletes on controller 404
  (58 §1.3); spec freshness beyond that still depends on the controller
  writeback because the two spec-hash algorithms differ.
- The transparent PUT write-through emits a generation-only sync event, not
  a full-spec update, so fields the service cannot read back are never
  reverted (58 §11 I16).
- R1-2e adds the controller `/internal/v1` receiver + status pusher (a
  separate PR, branch `feat/agentteams-controller-internal-r1`). With both
  branches the cross-process e2e (controller test tag `service_e2e`) and
  the G1 dual-mode parity test run green on MySQL 8 and seekdb (58 §11
  I8/I13).
- The outbox dispatcher assumes a single service instance (no
  `SKIP LOCKED`, which seekdb 5.7 lacks); multi-instance claiming lands in
  R2.

Operations runbook (config, failure matrix, outbox statuses, runbook SQL):
[docs/OPERATIONS.md](docs/OPERATIONS.md).

## Request limits

- Request bodies are capped at 1 MiB (MaxBytesReader). Larger bodies get the
  normal 400 invalid_request envelope; a value that exceeds a column width
  (MySQL 1406) is also 400, never a 500.
- TCP: 5s read-header, 30s read, 60s write, 120s idle, 64 KiB headers.
  The UDS health listener uses the same header/idle bounds.

## Security / toolchain

- Requires Go 1.26.11 (go.mod). That patch fixes GO-2026-6859, the net/http
  MaxBytesReader header race that this service's request cap uses.
- vuln scan (govulncheck) is clean; staticcheck is clean.
