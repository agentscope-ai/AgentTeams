# L2 Human Worker-Scoped Write

Status: implemented
API: `PUT /api/v1/workers/{name}` (existing endpoint, new caller class)

## Problem

L2 humans (`Human` CR with `permissionLevel: 2`, authenticated with their
Matrix token) can read the teams and workers in their `accessibleTeams` scope,
and (since the project write endpoints landed) they can create and drive
projects in that scope. Worker configuration, however, was admin/leader-only:
`authorizeHuman` denied every worker action except `get`/`list`.

A team-scoped human who owns a dedicated team therefore cannot adjust the
capabilities of the workers they coordinate — enabling a built-in skill, a
remote skill from the source registry, or an MCP server — without escalating
to the admin. Every such change round-trips through a human operator and a
`PUT /api/v1/workers/{name}` call with an admin token.

## Design

Extend the existing `PUT /api/v1/workers/{name}` endpoint with a
code-level boundary for `RoleHuman` callers. The same pattern is used by the
project write endpoints: the middleware cannot resolve `worker -> team`, so
`requireSameTeam` in the authorizer is a pass-through for the scoped
request, and the handler enforces the real boundary after resolving the
worker's team.

Two rules, enforced in `ResourceHandler.checkHumanWorkerUpdate`:

1. **Team scope.** The worker must be a member of one of the caller's
   `accessibleTeams` (resolved via the same team-membership lookup the list
   endpoints use). Standalone workers (no team membership) are hidden from
   L2 readers in `GET /api/v1/workers`; the update path hides them the same
   way and returns `404` so the endpoint stays probe-resistant. Cross-team
   updates return `404` for the same reason — a `403` would let a scoped
   human enumerate workers it cannot see and learn their owning team. Only
   a teamless human (no `accessibleTeams` at all) is rejected with `403` at
   the middleware, before any worker lookup.
2. **Field whitelist.** A default L2 update may only set `skills`
   (public-catalog assignment). `remoteSkills` (registry source URIs may
   embed credentials) and `mcpServers` (the gateway bearer key is injected
   into every entry verbatim — an L2-controlled URL would exfiltrate it) are
   closed to default L2 pending the elevated-capability design; any other
   field present in the body (`model`, `modelProvider`, `runtime`, `image`,
   `identity`, `soul`, `agents`, `package`, `expose`, `channelPolicy`,
   `resources`, `containerManaged`, `state`) is rejected with `400` naming
   the offending fields. Ownership, persona, image, network, and lifecycle
   remain the team owner's domain. A full-request-type probe test
   (`TestL2WorkerUpdateFieldPolicyCoversAllRequestFields`) pins the policy:
   every field of `UpdateWorkerRequest` must be explicitly decided, so no
   field can become L2-writable by omission (deny-by-default).

Semantics for allowed fields are unchanged: merge-patch, non-empty (or
non-nil) wins, conflict-retry loop as for all updates. The request type
gains `remoteSkills` (previously unreadable through the API even for admins,
although the CRD and the deployer already support it); admins may set it,
default L2 may not (see the whitelist above).

## Contract

| Caller | `PUT /api/v1/workers/{name}` |
|--------|------------------------------|
| admin / manager | full update, unchanged |
| team leader | all workers, all fields, unchanged (the leader path is not team-scoped in the current code) |
| L2 human (default) | in-team workers only; `skills` only (public-catalog assignment); `remoteSkills` / `mcpServers` 400 (elevated capability pending design); `404` standalone and cross-team (probe-resistant), `400` off-whitelist field |
| worker / other | denied by the authorizer (unchanged) |

The authorizer change is deliberately minimal: `ActionUpdate` on `worker`
for `RoleHuman` now returns `requireSameTeam` (pass-through when
`ResourceTeam` is empty) instead of `deny`. The handler is the single
enforcement point — the same layering the project endpoints use — so the
scope and whitelist cannot be bypassed by any caller that authenticates as
an L2 human.

## Out of scope

- `DELETE`/`POST` for workers (team membership changes stay admin/leader).
- Wake/sleep lifecycle for L2 humans (separate decision).
- Team-scoping the leader update path (the current code lets a team leader
  update any worker; pre-existing, out of scope here).
- Standalone-worker access via `accessibleWorkers` (read path does not
  expose standalone workers to L2 humans either; keep parity).
- Propagating the update to a running worker container — the existing
  reconcile machinery already applies `spec` changes.

## Tests

- `internal/auth/authorizer_test.go` — `TestAuthorizer_HumanScoped`:
  in-scope and empty-team `ActionUpdate` on `worker` allowed, cross-team
  denied, create/delete/wake/sleep still denied.
- `internal/server/resource_handler_l2_update_test.go` — handler boundary:
  in-scope skills update applies (200), credential-bearing surfaces
  (`remoteSkills` / `mcpServers`) 400 for default L2, cross-team 404,
  standalone 404, off-whitelist fields 400 (named), empty body no-op 200,
  admin full update unchanged, team leader update unchanged, teamless human
  hidden (404; the middleware rejects it first), full-request-type
  field-policy probe (every field probed, only `skills` may pass).
