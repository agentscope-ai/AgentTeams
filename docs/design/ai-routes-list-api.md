# AI Routes List API

Status: implemented
API: `GET /api/v1/gateway/ai-routes`

## Problem

Deployments that serve models through the AI gateway have no read-only,
in-band way to see which AI routes are configured and which consumers are
authorized on them. Today, listing routes requires **gateway console access
(admin credentials)**, so API clients holding only a controller token
cannot audit route authorization.

## Route catalog, not model catalog

An AI route and a model are different things, and this endpoint reports the
former only:

- A **route** is the gateway's `/v1` entry point: it carries consumer
  authorization (`authConfig.allowedConsumers`) and upstream provider
  weights. The default deployment creates exactly one, `default-ai-route`.
- A **model** is the value of the `model` field in a chat-completion
  request (and in Worker/Manager `spec.model`). Model IDs are defined and
  served by the route's **upstream provider** — in the default deployment
  the SGLang instance, whose own model list (e.g. its `/v1/models`) is the
  source for valid IDs. One route can serve several models.

Therefore the route name is **not** a model alias: in the default
deployment a client would otherwise receive `default-ai-route` as if it
were a model choice. This endpoint is named and shaped accordingly
(`gateway/ai-routes`, response key `routes`) and makes no claim about
which model names are valid.

The gateway's own `GET /v1/models` is also **not** a full model list: the
ai-proxy plugin matches only `chat/completions` and `embeddings` traffic,
so that endpoint under-reports. Neither the console route list nor the
gateway's `/v1/models` is a model catalog; model discovery stays with the
provider.

## Design

`GET /api/v1/gateway/ai-routes` proxies the console AI route list through
the controller and returns one entry per route:

```json
{
  "routes": [
    {
      "name": "default-ai-route",
      "upstreams": [
        { "provider": "sglang-local", "weight": 100 }
      ],
      "allowedConsumers": ["manager", "worker-sysdev-lead"]
    }
  ],
  "total": 1
}
```

- **`name`** — the AI route name, verbatim (NOT a model ID).
- **`upstreams`** — the providers serving the route, with console weights.
  Omitted when the route has no upstreams.
- **`allowedConsumers`** — gateway consumers authorized on the route
  (from the route's `authConfig`). Omitted when empty.

Implementation notes:

- The console list endpoint returns route names only, so the client fetches
  each route individually for its upstreams and consumer allowlist — the
  same list-then-get pattern the consumer authorization code already uses.
- One unreadable route fails the whole call with a `502` rather than
  returning a silently incomplete catalog.
- Backends without a route-list API (the `ai-gateway` cloud provider) return
  `501`.

## Authorization

The route is registered as `ActionGet` on the `gateway` resource kind. The
existing authorizer matrix already grants the `gateway` kind to
admin/manager only; team leaders, team-scoped humans, and worker accounts
fall through to the default deny. No authorizer change is required.

## Contract

`GET /api/v1/gateway/ai-routes` →

| Result | Code |
|--------|------|
| OK | `200` + route catalog (possibly empty) |
| gateway console unreachable / error | `502` |
| backend without route-list support, or no gateway configured | `501` |
| caller below L1 | `403` |
