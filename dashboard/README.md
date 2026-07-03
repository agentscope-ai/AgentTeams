# HiClaw Dashboard

Milestone 2, Step 3 — dashboard **v1 (read-only)** + **v1.1** wake/sleep/ensure-ready,
served behind a same-origin proxy. See
[`docs/implementation-milestone-2.md`](../docs/implementation-milestone-2.md)
("Step 3 — Dashboard v1...") for the full contract this implements.

Milestone 3, Step 1 adds **v1.5**: a message-injection UI (Message button on
Manager/Team cards) wired to the `POST .../message` controller routes that
have existed since M1, plus the matching scoped proxy-allowlist extension and
audit-log entries. See
[`docs/implementation-milestone-3.md`](../docs/implementation-milestone-3.md)
("Step 1 — Dashboard v1.5...") for the full contract.

## Layout

- `server/` — a small, near-zero-dependency Node proxy (`node:http` + a
  hand-rolled AWS SigV4 signer for MinIO — no framework, no AWS SDK). Serves
  the built SPA as static files AND proxies a strict allowlist of `/api/*`
  routes. Tests: `node --test`.
- `web/` — a plain Vite + vanilla-JS SPA (no framework). Talks only to
  same-origin `/api/*` paths; never sees the admin token or MinIO
  credentials.
- `Dockerfile` — multi-stage build: compiles the SPA, then ships only the
  proxy + built static assets (no dev dependencies, no Vite) at runtime.

## Why a proxy, and why it's mandatory

The controller's REST API sets **no CORS headers** and **requires a Bearer
admin token on every request** — the SPA cannot call it directly from the
browser without exposing that token client-side. The proxy:

1. Reads the admin token from a file (`HICLAW_AUTH_TOKEN_FILE`, minted by the
   embedded controller at startup — see `hiclaw-controller/internal/app/app.go`
   `bootstrapAdminCLIToken` and `hiclaw-controller/Dockerfile`) and injects it
   as `Authorization: Bearer <token>` on every upstream controller call. The
   browser never holds this token, and it is stripped from any response
   headers before being relayed back (see `server/src/controller-client.js`
   `sanitizeResponseHeaders`).
2. Enforces a **scoped allowlist** (`server/src/allowlist.js`) — this is the
   entire attack surface, deliberately kept in one pure, exhaustively-tested
   module:
   - `GET /api/managers|teams|workers|manager-tasks|projects[/...]` →
     controller `GET /api/v1/...` passthrough.
   - `GET /api/tasks/<...>` → MinIO, rooted at `shared/tasks/<...>`.
   - `GET /api/files/<shared|agents>/<...>` → MinIO, rooted at `shared/` or
     `agents/` only (path-traversal guarded — see the allowlist tests).
   - `POST /api/workers/{name}/wake|sleep|ensure-ready` → controller
     `POST /api/v1/workers/{name}/...` — logged server-side
     (`server/src/request-log.js`) as a JSON line with timestamp, action,
     worker name, and resulting status code (the #17 audit trail).
   - `POST /api/managers/{name}/message` and `POST /api/teams/{name}/message`
     (added in M3 Step 1) → controller `POST /api/v1/managers|teams/{name}/message`
     — injects an operator-authored message into the manager's admin DM room
     or the team's leader room (409 if that room isn't provisioned yet).
     Request bodies are capped at **64 KB** (413 over the cap, no upstream
     call made). Logged as a JSON line with timestamp, `action:"message"`,
     `kind` (`"managers"`|`"teams"`), `target` (the manager/team name),
     status code, and `bodyLen` + a **`bodyPreview` truncated to ≤120
     chars** — the full message body is never written to the log, only
     this preview.
   - Everything else is rejected: unknown path shapes → `404`; a disallowed
     method on a known path shape → `405`.
   - `/docker/` (the controller's embedded-mode Docker socket passthrough)
     has **no route here at all** — it is never proxied, under any
     conditions.
3. Sets no CORS headers itself either — the SPA and proxy are same-origin by
   construction (the proxy serves the built SPA's static files), so none are
   needed.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8090` | Port the proxy listens on. |
| `HICLAW_CONTROLLER_URL` | `http://127.0.0.1:8080` | Base URL of the hiclaw-controller REST API. |
| `HICLAW_AUTH_TOKEN_FILE` | `/var/run/hiclaw/cli-token` | Path to the admin Bearer token (read fresh on every request; never cached to survive rotation). |
| `MINIO_ENDPOINT` | `http://127.0.0.1:9000` | MinIO S3 API endpoint (**not** the Higress port 8080). |
| `MINIO_ACCESS_KEY` (or `MINIO_ACCESS`) | _(none)_ | MinIO access key. |
| `MINIO_SECRET_KEY` (or `MINIO_SECRET`) | _(none)_ | MinIO secret key. |
| `HICLAW_FS_BUCKET` | `hiclaw-storage` | Bucket holding `shared/` and `agents/` (same convention as the controller — see `hiclaw-controller/internal/config/config.go`). Never hard-coded. |

## Build & test locally

```bash
cd dashboard/server && npm install && npm test    # node:test, no network required
cd dashboard/web    && npm install && npm run build
```

```powershell
# PowerShell only -- Docker on this box mangles paths under Git Bash.
docker build -t hiclaw-dashboard dashboard/
```

## Deploy notes (Traefik + real controller/MinIO) — deferred-to-deploy

This dashboard is designed to sit behind Traefik as a same-origin service.
Example labels (adjust router/service names and the host rule to your
environment):

```yaml
labels:
  - "traefik.enable=true"
  - "traefik.http.routers.hiclaw-dashboard.rule=Host(`dashboard.example.com`)"
  - "traefik.http.routers.hiclaw-dashboard.entrypoints=websecure"
  - "traefik.http.routers.hiclaw-dashboard.tls.certresolver=letsencrypt"
  - "traefik.http.services.hiclaw-dashboard.loadbalancer.server.port=8090"
```

Run the container with:

```bash
docker run -d \
  --name hiclaw-dashboard \
  -e HICLAW_CONTROLLER_URL=http://hiclaw-controller:8080 \
  -e HICLAW_AUTH_TOKEN_FILE=/var/run/hiclaw/cli-token \
  -e MINIO_ENDPOINT=http://minio:9000 \
  -e MINIO_ACCESS_KEY=... \
  -e MINIO_SECRET_KEY=... \
  -e HICLAW_FS_BUCKET=hiclaw-storage \
  -v /var/run/hiclaw:/var/run/hiclaw:ro \
  hiclaw-dashboard
```

The token file must be readable from inside this container (bind-mount the
same path the controller writes to, read-only). If the controller and
dashboard run in the same embedded-mode host, this is the same directory the
controller's own Dockerfile writes `HICLAW_AUTH_TOKEN_FILE` to.

**Explicitly deferred to the first live checkpoint** (per
`docs/implementation-milestone-2.md`'s Step 3 acceptance criteria): live
serving behind a real Traefik instance, a real controller, and real MinIO
credentials — none of that is exercised by this repo's static tests, which
cover only the proxy's routing/security logic against fakes/stubs, plus the
production build.

## What's v1 / v1.1 vs. deferred

**Lands in this step:**
- Managers / Teams / Workers cards (poll every 15s; Workers every 30s,
  since `GET /api/v1/workers` triggers a live backend `Status()` call per
  team member).
- Manager task table, sourced from `/api/manager-tasks` and joined by task
  id with MinIO `shared/tasks/{id}/meta.json` where available.
- Project browser: one card per Project CRD (`GET /api/projects`), joined
  by id with the chat-flow layer (`shared/projects/{id}/meta.json` +
  `plan.md`) — federated, never schema-merged (decision #16). Progress is
  shown as `[ ]`/`[~]`/`[x]`/`[!]` counts parsed from `plan.md`; no
  kanban/DAG (that's v2).
- File browser under `shared/` and `agents/` (list + read; no upload/delete).
- Wake / Sleep / Ensure Ready buttons on worker cards, each behind a
  confirm dialog, calling the three allow-listed write routes.
- **(M3 Step 1 / v1.5)** Message buttons on Manager and Team cards, opening a
  textarea dialog and posting to the two message-injection routes above.
  Success toasts the destination room id; a `409` (room not provisioned yet)
  is surfaced as a distinct, friendlier error rather than a generic failure.

**Deferred to M3 later steps** (already possible against existing controller
endpoints, but out of scope for this step): kanban/DAG + task-detail (v2),
cross-instance fan-out, ETag conditional-GET, observability kit, Gitea API
context panels.
