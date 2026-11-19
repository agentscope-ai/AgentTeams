# G1 gate — five-route dual-mode comparison

G1 (R1): the closed mode (service disabled, controller serves the
public workers API) and the enabled mode (service owns the three native
routes and proxies the two writes) must produce the same observable result
on all five routes.

| Route | Closed mode | Enabled mode | Expected difference |
|---|---|---|---|
| `POST /api/v1/workers` | controller | service native | new `{code,message}` on errors; 201 body byte-identical |
| `GET /api/v1/workers` | controller | service native | new envelope on errors; rows/order/ETag identical |
| `GET /api/v1/workers/{name}` | controller | service native | new envelope on errors; body identical |
| `PUT /api/v1/workers/{name}` | controller | service proxy | **none** — bytes/status forwarded unchanged |
| `DELETE /api/v1/workers/{name}` | controller | service proxy | **none** — bytes/status forwarded unchanged |

Known R1 gaps that the golden cases must exclude or annotate:

- `team` / `role` are empty in native responses (no role column, no team
  writer in R1).
- `exposedPorts` is derived from the create request in native mode, while
  the controller omits it until status is written back.
- `lastActiveAt` / `lastRunAt` / `lastFinishAt` are RFC3339Nano UTC
  strings in the service (the controller passes through the CR string).

## What is already locked in code

- The three projection views and the 43-field response are pinned by
  hand-written byte goldens in `internal/modules/resources/view_test.go`.
- PUT/DELETE byte passthrough, write-through sync, soft-delete rules and the
  dispatcher are covered by `internal/modules/resources/proxy_test.go` and
  the binary e2e (`test/e2e`, fake controller).
- The disabled mode is pinned by `cmd/agentteams-service/main_test.go`
  (no arg → exit 0 before any DB/controller access).

## Capturing real goldens (needs a live controller)

```sh
BASE_URL=http://controller:8090 TOKEN=<admin-bearer> ./capture.sh ./testdata/controller
```

Then run the same script against the service (enabled mode) and diff the
five files. This is the manual gate; it requires a live controller
deployment and the controller's /internal/v1 routes for the write path.
