# Higress Gateway API Reference

AgentTeams embeds [Higress](https://github.com/alibaba/higress) as its AI gateway and
API gateway. This document describes the interfaces Higress exposes to the outside
world (the **data plane**) and the console APIs AgentTeams itself uses to manage the
gateway (the **control plane**).

- **Data plane** — endpoints that Workers, Managers, and external clients call to reach
  LLM providers, MCP servers, exposed Worker ports, and bundled services.
- **Control plane** — the Higress Console REST API that the `agentteams-controller`
  and legacy Manager scripts use to configure routes, consumers, and MCP servers.

## Default domains and ports

| Resource | Default domain | In-container port | Host port (installer) |
|----------|----------------|-------------------|------------------------|
| AI Gateway (LLM + MCP) | `aigw-local.agentteams.io` | `8080` | `18080` (`AGENTTEAMS_PORT_GATEWAY`) |
| Higress Console API | (controller-internal) | `8001` | `18001` (`AGENTTEAMS_PORT_CONSOLE`) |
| Matrix homeserver | `matrix-local.agentteams.io` | `8080` | `18080` |
| Element Web | `matrix-client-local.agentteams.io` | `8080` (via gateway) / `8088` (direct) | `18080` (via gateway) / `18088` (direct, `AGENTTEAMS_PORT_ELEMENT_WEB`) |
| MinIO file system | `fs-local.agentteams.io` | `9000` (MinIO S3 API, **not** gateway `8080`; controller rewrites `:8080` → `:9000`) | no direct host mapping |
| OpenClaw Console | `console-local.agentteams.io` | `8080` (via gateway) / `18888` (direct) | `18080` (via gateway) / `18888` (direct) |

> **Port note**: inside the `agentteams-net` Docker network (i.e. from a Worker or
> Manager container) the gateway listens on **`:8080`**. The installer publishes it to
> the host as **`:18080`**. Prefer the in-container form when writing Worker
> configuration (`AGENTTEAMS_AI_GATEWAY_URL=http://aigw-local.agentteams.io:8080`).

## Data plane — externally callable endpoints

### 1. LLM OpenAI-compatible API

The AI route `default-ai-route` (path prefix `/v1`, upstream selected by
`AGENTTEAMS_LLM_PROVIDER`) exposes OpenAI-compatible LLM endpoints through Higress's
`ai-proxy` plugin. Requests must carry the caller's consumer key.

```
POST /v1/chat/completions   # chat completions (streaming supported)
POST /v1/embeddings         # embeddings (used by memorySearch when configured)
```

`GET /v1/models` is not a full OpenAI models-list endpoint in Higress; the
`ai-proxy` plugin matches `/v1/chat/completions` and `/v1/embeddings` paths only.
A `curl /v1/models` probe is still useful from a Worker as an **auth/connectivity
check** — a `401`/`403` proves the consumer key or `allowedConsumers` is wrong,
while a `404` means the path simply isn't an ai-proxy route (see the Worker Guide
troubleshooting section).

`/v1/chat/completions` is also the readiness probe the controller uses to verify a
Manager/Worker consumer is authorized on the AI route before onboarding
(`IsManagerLLMAuthReady` in `agentteams-controller/internal/service/provisioner.go`).

Example — verify a consumer is authorized on the AI route (run inside a Worker container,
using the same probe shape as the controller's `IsManagerLLMAuthReady`):

```bash
# 200 = authorized; 401 = bad key; 403 = not on allowedConsumers; 404 = wrong path
curl -s -o /dev/null -w '%{http_code}\n' http://aigw-local.agentteams.io:8080/v1/chat/completions \
  -H "Authorization: Bearer ${AGENTTEAMS_WORKER_GATEWAY_KEY}" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"<model>\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with only one word: ok\"}]}"
```

Authentication is per-identity **key-auth** (Bearer). Each Manager/Worker consumer is
registered in Higress with its own `GatewayKey`, and is only allowed on AI routes that
list it in `authConfig.allowedConsumers`. This is managed by the controller through
`AuthorizeAIRoutes` / `DeauthorizeAIRoutes` (see
`agentteams-controller/internal/gateway/higress.go`).

### 2. MCP server endpoints

Each MCP server registered in Higress is exposed under `/mcp-servers/{name}/mcp` on the
AI Gateway domain. The name is the MCP server name — for the bundled GitHub MCP server
this is `mcp-github`. `transport: http` (Streamable HTTP) maps to this URL; mcporter
uses it by default.

```
POST /mcp-servers/{name}/mcp
```

Example (run inside a Worker container):

```bash
mcporter --transport http \
  --server-url "http://aigw-local.agentteams.io:8080/mcp-servers/mcp-github/mcp" \
  --header "Authorization=Bearer ${AGENTTEAMS_WORKER_GATEWAY_KEY}" \
  call list_repos '{"owner": "test"}'
```

MCP access is also governed by per-consumer authorization (`consumerAuthInfo` on the
MCP server). Registration is handled by the controller (embedded stacks) or by the
legacy `setup-higress.sh` / `setup-mcp-server.sh` scripts (≤v1.0.9 Manager images);
see `manager/agent/skills/mcp-server-management/`.

### 3. Exposed Worker ports (service publishing)

A Worker whose `spec.expose` lists ports gets a gateway route with an auto-generated
domain, so its HTTP services become reachable from outside the container.

Auto-generated domain pattern:

```
worker-{name}-{port}-local.agentteams.io
```

Example: worker `alice` exposing port `8080` becomes reachable at
`http://worker-alice-8080-local.agentteams.io:8080` from inside the
`agentteams-net` network (or `:18080` on the host, matching the gateway publish
port). The domain is bound on the gateway, so the port is the gateway port, not
the worker's internal port.

Exposed routes have **no authentication** (public access by design); the controller
creates the Higress domain, service source, and route during reconciliation
(`ReconcileExpose` in `agentteams-controller/internal/service/provisioner_expose.go`).
See `manager/agent/skills/service-publishing/SKILL.md` for usage.

### 4. Bundled service routes

The installer also registers routes for the services bundled with the embedded stack:

| Route | Domain | Path | Backend |
|-------|--------|------|---------|
| Matrix homeserver | any (`domains: []`) | `/_matrix` | Tuwunel (`tuwunel.static:6167`) |
| Element Web | `matrix-client-local.agentteams.io` | `/` | `element-web.static:8088` |
| HTTP file system | `fs-local.agentteams.io` | `/` | MinIO S3 (`minio.static:9000`) |
| OpenClaw Console | `console-local.agentteams.io` | `/` | `openclaw-console.static:18888` (basic-auth) |

These are created once on first boot by `setup-higress.sh` (non-idempotent, marker
protected) or by the controller initializer on embedded stacks.

## Authentication summary

| Interface | Mechanism | Credential |
|-----------|-----------|------------|
| LLM AI route (`/v1/*`) | key-auth WASM (Bearer) | Consumer `GatewayKey` (`Authorization: Bearer <key>`) |
| MCP endpoints (`/mcp-servers/*`) | key-auth (Bearer) via `consumerAuthInfo` | Consumer `GatewayKey` |
| Exposed Worker ports | none (public) | — |
| OpenClaw Console route | basic-auth | `AGENTTEAMS_ADMIN_USER` / `AGENTTEAMS_ADMIN_PASSWORD` |
| Higress Console API | session cookie | `POST /session/login` |

Consumer keys are generated per Manager/Worker by the controller and injected as
`AGENTTEAMS_MANAGER_GATEWAY_KEY` / `AGENTTEAMS_WORKER_GATEWAY_KEY`. Authorization on
AI routes is scoped per consumer through `authConfig.allowedConsumers`.

## Control plane — Higress Console API

The controller and legacy scripts manage the gateway through the Higress Console REST
API (in-container `http://127.0.0.1:8001`). Session-cookie auth: `POST /system/init`
bootstraps the admin account, `POST /session/login` obtains the cookie.

| Endpoint | Method(s) | Purpose |
|----------|-----------|---------|
| `/system/init` | POST | Initialize admin account (first boot) |
| `/session/login` | POST | Login, obtain session cookie |
| `/user/changePassword` | POST | Rotate admin password |
| `/v1/consumers` | GET, POST | List / create key-auth consumers |
| `/v1/consumers/{name}` | DELETE | Remove a consumer |
| `/v1/ai/routes` | GET, POST | List / create AI routes |
| `/v1/ai/routes/{name}` | GET, PUT | Read / update an AI route (incl. `authConfig.allowedConsumers`) |
| `/v1/ai/providers` | GET, POST | List / create LLM providers |
| `/v1/ai/providers/{name}` | GET, PUT | Read / update a provider |
| `/v1/domains` | POST | Create a domain |
| `/v1/domains/{name}` | DELETE | Remove a domain |
| `/v1/service-sources` | GET, POST | List / create service sources |
| `/v1/service-sources/{name}` | PUT, DELETE | Update / remove a service source |
| `/v1/routes` | GET, POST | List / create classic routes |
| `/v1/routes/{name}` | PUT, DELETE | Update / remove a classic route |
| `/v1/routes/{name}/plugin-instances/{plugin}` | PUT | Enable / configure a route plugin (e.g. `basic-auth` on the OpenClaw Console route) |
| `/v1/mcpServer` | GET, PUT | List / upsert MCP servers |
| `/v1/mcpServer/consumers` | GET, PUT | Query / authorize consumers on an MCP server |
| `/system/higress-config` | GET, PUT | Read / patch gateway config (e.g. stream `idleTimeout`) |

Consumer authorization on AI routes is the responsibility of the reconcilers — the
initializer never writes `authConfig.allowedConsumers` (see `EnsureAIRoute` in
`agentteams-controller/internal/gateway/higress.go`).

## Related

- [Architecture overview](architecture.md) — role of Higress in the system.
- [Worker guide](worker-guide.md) — troubleshooting LLM / MCP connectivity from a Worker.
- [Kubernetes-native orchestration](k8s-native-agent-orch.md) — LLM/MCP security model.
- [Development](development.md) — Higress configuration guidance for contributors.
