# Worker Channels API

Proxy endpoints for per-worker channel configuration (QQ / Matrix /
DingTalk / Feishu / WeChat / ...), forwarding a fixed surface of each
worker's qwenpaw app channel API to the Controller.

**Purpose.** L1 admins and L2 humans connect channels to agents in their
teams through a graphical frontend (workbench plugin / dashboard) instead
of SSH + container surgery on the worker's `agent.json`. The endpoints are
the Controller-side half of that frontend; the qwenpaw-side API
(`PUT /api/config/channels/{channel}`, `GET /api/config/channels/schemas`, ...) is
upstream and unchanged.

## Routes

All routes are `embedded` mode only. In `kube` mode every route returns
`503` (uniform, before any worker lookup, so worker existence cannot be
probed).

| Method & path | Upstream (worker qwenpaw app) | Notes |
|---|---|---|
| `GET /api/v1/workers/{name}/channels` | `GET /api/config/channels` | All channel configs for the worker's agent |
| `GET /api/v1/workers/{name}/channels/types` | `GET /api/config/channels/types` | Channel name list |
| `GET /api/v1/workers/{name}/channels/schemas` | `GET /api/config/channels/schemas` | Per-channel form schemas — the UI render driver (field names/types/labels/options), so the frontend needs no per-channel code |
| `GET /api/v1/workers/{name}/channels/{channel}` | `GET /api/config/channels/{channel}` | Single channel config |
| `PUT /api/v1/workers/{name}/channels/{channel}` | `PUT /api/config/channels/{channel}` | Body = the **full** channel config object; response + `X-AgentTeams-MinIO-Persisted` header |
| `GET /api/v1/workers/{name}/channels/{channel}/health` | `GET /api/config/channels/{channel}/health` | Channel health / connection state |
| `GET /api/v1/workers/{name}/channels/{channel}/qrcode` | `GET /api/config/channels/{channel}/qrcode` | QR-auth channels (wechat / dingtalk scan login) |
| `GET /api/v1/workers/{name}/channels/{channel}/qrcode/status` | `GET /api/config/channels/{channel}/qrcode/status?token=` | Poll scan status; strict query whitelist (`token` only) |
| `POST /api/v1/workers/{name}/channels/{channel}/restart` | `POST /api/config/channels/{channel}/restart` | Stop/start the channel without restarting the agent |
| `POST /api/v1/workers/{name}/channels/{channel}/conflict-check` | `POST /api/config/channels/{channel}/conflict-check` | Detects other agents holding the same channel credentials (QQ double-AppID kick-out guard); non-mutating, run before a channel write. **Additive 2.2.x-only route** — a 2.0.x worker answers with its own `404`, passed through verbatim (version gate) |

`{channel}` must match `^[a-z0-9][a-z0-9_-]*$`; anything else is `400`
before the upstream dial (injection guard). The single-segment channel
position also hosts the reserved fixed resources `types` and `schemas`.

## Authorization

| Role | Read routes | Write-gated routes (`PUT` / `restart` / `conflict-check`) |
|---|---|---|
| `admin` / `manager` (L1) | any worker | any worker |
| `human` (L2, Matrix token) | own accessibleTeams workers | own accessibleTeams workers via the worker-scoped update policy (authorizer `ActionUpdate` → same-team); **replacing a credential value or explicitly clearing it (empty string) additionally requires the `channel_secrets` capability** (403 names the offending fields); unchanged credential values are ordinary fields and omitted ones are preserved (see PUT semantics) |
| `human` (L3, `permissionLevel: 3`, Matrix token) | **assigned workers only** (`accessibleWorkers`, standalone or team members; see [l3-worker-scoped-read.md](l3-worker-scoped-read.md)) — **channel-config reads sanitized server-side: credential fields omitted, normal fields preserved** | **denied** — `403` (middleware `requireSameTeam`: L3 carries no teams) and `404` at the handler scope check (strict team predicate on mutations); read-only by contract (Q2) |
| `team-leader` | own team workers | **denied — `403`** (team leaders have read-only access to channels; the middleware's same-team `ActionUpdate` would otherwise allow it, so the handler is the real boundary) |
| scoped caller, other team | `404` | `404` (W8: never `403`, so cross-team existence cannot be probed) |
| scoped caller, standalone worker (no team) | `404` | `404` (except an L3 caller with that worker in `accessibleWorkers` — read only) |

Mutating calls are audit-logged (`worker`, `upstream`, `actor`,
`minio_persisted`).

## PUT semantics

1. **Validation boundary = upstream.** The body is forwarded (after the
   credential back-fill below, item 5); qwenpaw validates it with the
   channel's pydantic model. Upstream `400`/`422` (validation detail),
   `404` (unknown channel) and `409` responses are passed through
   verbatim. An **empty body is rejected by the Controller with `400`** —
   upstream would treat it as an empty config and wipe the saved channel.
   Unparseable JSON passes through untouched (no diff, no back-fill) —
   upstream's validation rejects it.
2. **The write is the qwenpaw-authoritative path.** Upstream persists the
   config into the worker's `agent.json` and hot-reloads the channel —
   no worker restart. The response body is the persisted channel config,
   returned verbatim.
3. **Read-back validation (async).** After a `200`, the Controller answers
   immediately with `X-AgentTeams-MinIO-Persisted: pending` and schedules
   a **single background re-check at a conservative bound (120s) far
   beyond the worker push_loop sync interval** (`check_interval=5s` in
   the current qwenpaw worker). The re-check reads the MinIO baseline
   (`agents/{name}/.qwenpaw/workspaces/default/agent.json`) and records
   the outcome in the durable audit log (action `channel_readback`,
   `converged=true|false`, attributed to the PUT's actor) — the
   convergence result is an audit signal, not an in-request one. The old
   bounded in-request polling (3x2s) was systematically false-negative
   against the push_loop sync interval in production, so every healthy
   PUT looked unpersisted. **Superseded writes:** each PUT claims the
   next readback generation for its (worker, channel); if a newer PUT
   lands before an older re-check runs, the older re-check is skipped —
   a newer successful write is never reported as a persistence failure
   of the older (superseded) one.

   | Header value | Meaning |
   |---|---|
   | `pending` | background re-check scheduled; the convergence result lands in the audit log (`channel_readback`) |
   | `skipped` | no storage client configured |

   The Controller never writes to the baseline — `push_loop` remains the
   single writer (manual-edit persistence gaps, where the MinIO copy lagged
   the live container, are what `converged=false` surfaces). The `200`
   body is authoritative either way: the config IS live on the worker.
4. **Credential gate (diff against the saved config).** Before the write,
   the Controller fetches the saved channel config from the worker (a
   read-only dial on the same upstream; a `PUT` whose baseline cannot be
   read fails with `502` rather than proceeding) and diffs the request
   against it, credential field by credential field (any nesting depth).
   A credential field is one whose leaf name is in the
   `channelCredentialKeys` denylist: `access_token`, `bot_token`,
   `token`, `app_secret`, `app_token`, `client_secret`, `secret`,
   `encrypt_key`, `verification_token`, `password`, `sip_password`,
   `api_key`, `dashscope_api_key`, `livekit_api_key`,
   `livekit_api_secret`, `twilio_auth_token`. The semantics per field:

   | Field in the body | Meaning | Gate (L2) | Audit |
   |---|---|---|---|
   | absent | **preserved** — the saved value is back-filled into the forwarded body, because upstream replaces the whole channel (`config_class(**body)`) and an omitted secret would be erased by the model default | — | — |
   | present, non-empty, equal to the saved value | unchanged round-trip → an ordinary edit | — | — |
   | present, non-empty, different from the saved value | a credential replacement | `channel_secrets` required (403 names the fields) | `channel_credential_write` |
   | present as `""` | an explicit clear (an empty string is a clear, not a placeholder) | `channel_secrets` required | `channel_credential_write` |

   L1 (admin/manager) writes are exempt from the gate but audit-logged
   (action `channel_credential_write`, `who`/`role`/target
   `worker/channel`, capability, field list). A `PUT` whose baseline
   fetch returns the worker's own `404` (unknown channel) still proceeds
   — the write then passes through upstream's own `404` verbatim — but
   with no back-fill and every present credential field gated as a
   write.
5. **Baseline fetch cost.** The diff costs one extra read-only upstream
   call per `PUT` (5s dial bound, same as the write). It runs after the
   empty-body and team-leader rejections (those answer without a dial).

## Status mapping

| Upstream | Controller |
|---|---|
| `200` | `200`, body verbatim (+ read-back header on `PUT`) |
| `400` / `404` / `409` / `422` | same status, body verbatim (`404` doubles as the version gate: a qwenpaw build without the channel router yields its own `404` detail) |
| anything else / dial failure | `502` with a truncated upstream body |

## Example

```bash
# Connect a QQ channel to daily-carol (L1 admin, cli token)
curl -s -X PUT http://127.0.0.1:8090/api/v1/workers/daily-carol/channels/qq \
  -H "Authorization: Bearer $AGENTTEAMS_TOKEN" -H "Content-Type: application/json" \
  -d '{"enabled":true,"app_id":"1904153419","client_secret":"***","markdown_enabled":true}'
# → 200 {"enabled":true,...}  X-AgentTeams-MinIO-Persisted: pending

# L2 user's form: fetch schemas, render, save
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/channels/schemas \
  -H "Authorization: Bearer $MATRIX_TOKEN"
```

## Notes

- **Unmasked credentials (L1/L2).** Configs round-trip unmasked by design:
  scoped callers can only reach agents in their own teams, and the form
  needs the saved values to round-trip unchanged. L1 sees all workers,
  consistent with its existing worker-management surface. Read-surface
  contract decided per #1220 §13 Q5 (2026-09-16): round-trip is retained
  for L1/L2 — a masked read would be a separate change (mask helper +
  reveal capability), not a config flag. L3 readers are the exception
  (next bullet).
- **L3 reads are sanitized.** L3 (worker-scoped) humans may read normal
  config/status of assigned workers but not plaintext credentials
  (maintainer decision, #1277 review): the channel-config read routes
  (`GET /channels`, `GET /channels/{channel}`) strip the
  credential-bearing fields (the qwenpaw channel-model secret fields,
  any nesting depth) **server-side** for worker-scoped callers — fields
  omitted, normal fields preserved, `types`/`schemas` untouched. The
  stripping is server-side because the raw response is the contract;
  a non-JSON 200 body fails closed to `{}` for L3 readers. L1/L2
  responses are never touched.
- **Single-agent workers.** Without an `X-Agent-Id` header the worker's
  qwenpaw app resolves the active agent from its config; in a
  single-profile worker container that is the worker's own agent. The
  global (non-agent-scoped) upstream path therefore targets the right
  agent without header plumbing.
- **Addressing.** Upstream is dialed as
  `http://{containerPrefix}{name}:{AGENTTEAMS_CONSOLE_PORT}` (default
  `8088`, system-wins env resolution — the same chain the container is
  created with), same as the worker checkpoint/approval proxies.

## QwenPaw version contract

This proxy is **version-agnostic**: it forwards to a fixed, prefixed path
(`/api/config/channels/...`) and contains no version logic, so no specific
QwenPaw pin is required to merge or run it. That path is the contract the
worker's own client (`qwenpaw_worker/api.py`) and the integration coverage
use, and `TestChannelsUpstreamPathsMatchWorkerContract` pins every
forwarded path against it, so any future worker API move fails the test
instead of silently 404-ing at runtime.

The **9-route minimum contract** has been verified directly against the
official PyPI release wheels (hash-checked), all exposing the identical
paths under the `/api` mount:

| QwenPaw release | 9 forwarded routes | `conflict-check` |
|---|---|---|
| 2.0.1 (2026-07-24) | all present, identical paths | absent (zero references in the package) |
| 2.2.0 (2026-09-03) | all present, identical paths | present (`config.py:379`) |
| 2.2.1 (2026-09-11) | all present, identical paths | present (`config.py:379`; router byte-identical to 2.2.0) |

So the API works unchanged on any QwenPaw across the 2.0.1 → 2.2.1 range:

- **`conflict-check` is an additive 2.2.x-only route.** 2.2.x workers
  expose it; 2.0.x workers do not. The proxy now forwards it: on a 2.2.x
  worker the check runs, and on an older build the upstream's own `404`
  detail is returned verbatim, so clients can distinguish "no conflict
  check available on this worker build" from a real failure and hide the
  entry accordingly (the version gate by pass-through below).
- **Version gate by pass-through.** On a QwenPaw build without the
  channel router at all, the upstream's own `404` detail is returned
  verbatim, so callers see a distinguishable, upstream-sourced failure
  instead of a silent proxy error.
- **MinIO read-back timing.** `X-AgentTeams-MinIO-Persisted: pending`
  means the baseline convergence check runs in the background (2x the
  push_loop interval) and its result is an audit-log entry
  (`channel_readback`, `converged=true|false`), not a header value.
  `converged=false` means "not converged at check time", not "write
  failed" (the 200 body is authoritative). Clients that need a
  persistence signal query the audit object instead of the header.
