# Worker Channels API

Proxy endpoints for per-worker channel configuration (QQ / Matrix /
DingTalk / Feishu / WeChat / ...), forwarding a fixed surface of each
worker's qwenpaw app channel API to the Controller.

**Purpose.** L1 admins and L2 humans connect channels to agents in their
teams through a graphical frontend (workbench plugin / dashboard) instead
of SSH + container surgery on the worker's `agent.json`. The endpoints are
the Controller-side half of that frontend; the qwenpaw-side API
(`PUT /config/channels/{channel}`, `GET /config/channels/schemas`, ...) is
upstream and unchanged.

## Routes

All routes are `embedded` mode only. In `kube` mode every route returns
`503` (uniform, before any worker lookup, so worker existence cannot be
probed).

| Method & path | Upstream (worker qwenpaw app) | Notes |
|---|---|---|
| `GET /api/v1/workers/{name}/channels` | `GET /config/channels` | All channel configs for the worker's agent |
| `GET /api/v1/workers/{name}/channels/types` | `GET /config/channels/types` | Channel name list |
| `GET /api/v1/workers/{name}/channels/schemas` | `GET /config/channels/schemas` | Per-channel form schemas — the UI render driver (field names/types/labels/options), so the frontend needs no per-channel code |
| `GET /api/v1/workers/{name}/channels/{channel}` | `GET /config/channels/{channel}` | Single channel config |
| `PUT /api/v1/workers/{name}/channels/{channel}` | `PUT /config/channels/{channel}` | Body = the **full** channel config object; response + `X-AgentTeams-MinIO-Persisted` header |
| `GET /api/v1/workers/{name}/channels/{channel}/health` | `GET /config/channels/{channel}/health` | Channel health / connection state |
| `GET /api/v1/workers/{name}/channels/{channel}/qrcode` | `GET /config/channels/{channel}/qrcode` | QR-auth channels (wechat / dingtalk scan login) |
| `GET /api/v1/workers/{name}/channels/{channel}/qrcode/status` | `GET /config/channels/{channel}/qrcode/status?token=` | Poll scan status; strict query whitelist (`token` only) |
| `POST /api/v1/workers/{name}/channels/{channel}/restart` | `POST /config/channels/{channel}/restart` | Stop/start the channel without restarting the agent |
| `POST /api/v1/workers/{name}/channels/{channel}/conflict-check` | `POST /config/channels/{channel}/conflict-check` | Detect other agents holding the same credentials (e.g. a second agent on the same QQ AppID) |

`{channel}` must match `^[a-z0-9][a-z0-9_-]*$`; anything else is `400`
before the upstream dial (injection guard). The single-segment channel
position also hosts the reserved fixed resources `types` and `schemas`.

## Authorization

| Role | Read routes | Mutating routes (`PUT` / `restart`) |
|---|---|---|
| `admin` / `manager` (L1) | any worker | any worker |
| `human` (L2, Matrix token) | own accessibleTeams workers | own accessibleTeams workers — **requires the worker-scoped update policy** (authorizer `ActionUpdate` → same-team). Until that policy is merged the middleware denies L2 worker updates and only L1 reaches the handler |
| `team-leader` | own team workers | **denied — `403`** (team leaders have read-only access to channels; the middleware's same-team `ActionUpdate` would otherwise allow it, so the handler is the real boundary) |
| scoped caller, other team | `404` | `404` (W8: never `403`, so cross-team existence cannot be probed) |
| scoped caller, standalone worker (no team) | `404` | `404` |

Mutating calls are audit-logged (`worker`, `upstream`, `actor`,
`minio_persisted`).

## PUT semantics

1. **Validation boundary = upstream.** The body is forwarded verbatim;
   qwenpaw validates it with the channel's pydantic model. Upstream
   `400`/`422` (validation detail), `404` (unknown channel) and `409`
   responses are passed through verbatim. An **empty body is rejected by
   the Controller with `400`** — upstream would treat it as an empty
   config and wipe the saved channel.
2. **The write is the qwenpaw-authoritative path.** Upstream persists the
   config into the worker's `agent.json` and hot-reloads the channel —
   no worker restart. The response body is the persisted channel config,
   returned verbatim.
3. **Read-back validation.** After a `200`, the Controller reads the
   MinIO baseline (`agents/{name}/.qwenpaw/workspaces/default/agent.json`)
   up to three times (2s apart) to verify the worker's `push_loop`
   converged the new config to the storage source that `mirror_all` pulls
   on rebuild. The outcome is reported in the
   `X-AgentTeams-MinIO-Persisted` response header — the body stays
   verbatim:

   | Header value | Meaning |
   |---|---|
   | `true` | baseline verified (canonical-JSON comparison of `channels.{channel}` vs the persisted config) |
   | `false` | baseline not converged within the read-back budget (or missing) — frontend should warn and re-check; the config IS live on the worker |
   | `skipped` | no storage client configured |

   The Controller never writes to the baseline — `push_loop` remains the
   single writer (manual-edit persistence gaps, where the MinIO copy lagged
   the live container, are what this header surfaces).

## Status mapping

| Upstream | Controller |
|---|---|
| `200` | `200`, body verbatim (+ read-back header on `PUT`) |
| `400` / `404` / `409` / `422` | same status, body verbatim (`404` doubles as the version gate: a qwenpaw build without the channel router yields its own `404` detail) |
| anything else / dial failure | `502` with a truncated upstream body |

## Example

```bash
# Connect a QQ channel to daily-luo (L1 admin, cli token)
curl -s -X PUT http://127.0.0.1:8090/api/v1/workers/daily-luo/channels/qq \
  -H "Authorization: Bearer $AGENTTEAMS_TOKEN" -H "Content-Type: application/json" \
  -d '{"enabled":true,"app_id":"1904153419","client_secret":"***","markdown_enabled":true}'
# → 200 {"enabled":true,...}  X-AgentTeams-MinIO-Persisted: true

# L2 user's form: fetch schemas, render, save
curl -s http://127.0.0.1:8090/api/v1/workers/daily-luo/channels/schemas \
  -H "Authorization: Bearer $MATRIX_TOKEN"
```

## Notes

- **Unmasked credentials.** Configs round-trip unmasked by design: scoped
  callers can only reach agents in their own teams, and the form needs the
  saved values to round-trip unchanged. L1 sees all workers, consistent
  with its existing worker-management surface.
- **Single-agent workers.** Without an `X-Agent-Id` header the worker's
  qwenpaw app resolves the active agent from its config; in a
  single-profile worker container that is the worker's own agent. The
  global (non-agent-scoped) upstream path therefore targets the right
  agent without header plumbing.
- **Addressing.** Upstream is dialed as
  `http://{containerPrefix}{name}:{AGENTTEAMS_CONSOLE_PORT}` (default
  `8088`, system-wins env resolution — the same chain the container is
  created with), same as the worker checkpoint/approval proxies.
