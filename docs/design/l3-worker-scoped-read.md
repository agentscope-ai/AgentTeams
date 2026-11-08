# L3 Worker-Scoped Read Access

Status: implemented
Scope: `internal/auth` (identity), `internal/server` (worker / channels /
approval read routes)

## Problem

`Human` CRs with `permissionLevel: 3` (worker-scoped,
`accessibleWorkers`) could not authenticate against the controller API at
all: `resolveHuman` hard-rejected every level other than 2, and even the
`accessibleWorkers` field had no consumer in the identity chain — the
field was mutable and validated, but carried no permissions. Q2 of the L2
permission model (#1220 §2) settled the intended contract: **L3 humans are
read-only viewers of exactly the workers they are assigned to** (channel
configuration and tool-approval configuration reads), no writes.

## Design

The L2/L3 distinction is carried by **data, not by a new role value**:
`RoleHuman` is unchanged, and an L3 identity is a `RoleHuman` caller with a
non-empty `AccessibleWorkers` set. Every existing team-scope check keys on
`RoleTeamLeader`/`RoleHuman` plus `TeamMatches`, so nothing changes for
admins, managers, leaders, L2 humans, or worker SAs.

### Identity (`internal/auth`)

- `CallerIdentity` gains `AccessibleWorkers []string` — populated **only**
  for permissionLevel=3 humans (from `spec.accessibleWorkers`). L2 humans
  keep `Teams` + `Capabilities` and always carry an empty
  `AccessibleWorkers`; SA-based identities never carry it (same invariant
  as `Capabilities`). The `permissionLevel` is the discriminator — an L3
  CR that also lists `accessibleTeams` or `capabilities` gets neither in
  its identity (level-strict isolation, no silent scope widening).
- `resolveHuman` now resolves both supported levels:
  - `2` → `RoleHuman` + `Teams` + `Capabilities` (unchanged)
  - `3` → `RoleHuman` + `AccessibleWorkers` (new)
  - any other level → rejected (level 1 uses the admin SA)
- New read predicate `WorkerReadable(team, workerName)`: the worker leg
  (`workerName ∈ AccessibleWorkers`, standalone or team members alike)
  unioned with the existing team scope. For every caller without
  `AccessibleWorkers` it reduces to `TeamMatches` exactly — a no-op.

### Read routes (W8: 404, never 403)

The scoped-read checks on the four surfaces below now use
`WorkerReadable` instead of `TeamMatches`:

| Route | Scope |
|---|---|
| `GET /api/v1/workers/{name}` | assigned workers (200); anything else 404 |
| `GET /api/v1/workers` (list) | filters to assigned workers |
| `GET /api/v1/workers/{name}/channels...` | assigned workers (200); anything else 404 |
| `GET /api/v1/workers/{name}/approval` | assigned workers (200); anything else 404 |

Out-of-scope workers are hidden as 404 (existence cannot be probed),
consistent with the L2 team-scope behavior.

### Write routes (read-only is structural)

No write route is touched. L3 is read-only by two independent layers:

1. **Authorizer/middleware** — `ActionUpdate` on `worker` runs
   `requireSameTeam`, and an L3 identity carries no teams, so every
   `PUT /api/v1/workers/{name}` and `PUT .../channels/...` is denied with
   403 before reaching a handler. `create`/`delete`/`wake`/`sleep`/
   credential refresh are default-denied for humans.
2. **Handler fallback** — the channels and approval scope checks
   (`channelsScope` / `approvalScope`) are shared by GET and PUT, so the
   union predicate is applied **only on `GET`**; mutations keep the strict
   `TeamMatches` predicate, which L3 identities (no teams) fail. A handler
   therefore hides an L3 mutation as 404 even if the middleware layer
   ever changes. (The `PUT /api/v1/workers/{name}` handler keeps its own
   `TeamMatches` check as before — same effect.)

## Contract

| Caller | `GET` worker | `GET` channels / approval | `PUT` worker / channels / approval |
|---|---|---|---|
| admin / manager (L1) | any worker | any worker | any worker (unchanged) |
| L2 human (`permissionLevel: 2`) | own accessibleTeams (unchanged) | own accessibleTeams (unchanged) | existing scoped-write policy (unchanged) |
| **L3 human (`permissionLevel: 3`)** | **assigned workers (team + standalone); others 404 — MCP endpoint URLs scrubbed (userinfo + credential query values)** | **assigned workers; others 404 — channel-config reads sanitized (credentials stripped)** | **denied — 403 (worker/channels, middleware) or 404 (approval, handler); no upstream mutation** |
| team leader / worker SA | unchanged | unchanged | unchanged |

`accessibleWorkers` on an L2 (level 2) CR is inert — the worker leg
activates solely at level 3.

### Read sanitization (credentials never reach L3)

Maintainer decision (#1277 review): an L3 reader may read **normal
config/status** of its assigned workers, but **no plaintext credentials**.
The L3-readable response surfaces carrying credential VALUES are two (both
audited surface by surface: the approval GET returns a single
`approval_level` string; checkpoints/workspace-files hide as 404 for L3;
the runtime-status endpoint is authorizer-denied for humans; channel
health is `channel/status/detail`):

1. The channel-config read pair (`GET .../channels`,
   `GET .../channels/{name}`) — stripped by the `channelCredentialKeys`
   denylist (below).
2. The `mcpServers` URLs of `WorkerResponse` (worker detail
   `GET /workers/{name}` + worker list `GET /workers`) — the struct is
   name/url/transport, but the URL VALUE may embed credentials: an API
   key in the query (`?api_key=...`, `?apiKey=...`, `?key=...` — arbitrary
   vendor naming) or a user:password pair in the userinfo component
   (`https://user:pass@host`). `sanitizeMCPURLForL3` **reduces** each URL
   to `scheme://host[:port]/path`: the userinfo component, the **entire
   query string**, and any fragment are dropped wholesale. MCP endpoints
   are arbitrary external URLs, so their query namespaces are
   unclassified input — a denylist of known credential field names (e.g.
   the channel-config denylist) cannot be a complete credential contract
   for that namespace (review, 0918 round 4: `apiKey`/`key` survived the
   key-by-key filter), so no query value, classified or not, is exposed
   to L3. This is the same secret contract as the MCP catalog surface
   (`redactMCPURL`: host/path only); the worker response additionally
   keeps the scheme as a transport-security signal. A URL with none of
   those components is returned byte-identical; a URL that cannot be
   parsed, or that is not absolute, or has no host, fails closed to empty
   (an unprovable URL is not served). Known residual: a credential
   encoded *in the path* (rare by convention) would survive — reducing
   to `scheme://host[:port]` is a one-line tightening. The scrub applies
   to `IsWorkerScoped()` callers only; L1/L2/SA responses carry the
   URLs verbatim (team controllers legitimately manage these).

Both channel-config read routes therefore strip
credential-bearing fields **server-side** for worker-scoped callers:
the `channelCredentialKeys` denylist (the qwenpaw 2.2.x channel model
secret fields, case-insensitive leaf names, any nesting depth) is removed
from the response before it is written. Fields are **omitted**, not
replaced by a sentinel (presence visible, value never); normal fields
are preserved; `types`/`schemas` pass through untouched (their
documents carry credential field NAMES as schema property keys —
stripping by key name would break form rendering). The strip is
server-side by design: frontend-only masking is not a boundary, because
the raw response is the contract. A 200 body that is not valid JSON
fails closed to `{}` for L3 readers (an unparseable upstream response
cannot be proven credential-free). L1/L2 responses never pass through
the strip (the round-trip read contract, #1220 §13 Q5).

## Out of scope

- L3 writes of any kind (Q2: read-only; a future write grant is a new
  design, not a flag flip).
- L3 access to the other read surfaces (runtime config, workspace files,
  checkpoints, skills, projects, teams, MCP catalog, audit events) — those
  stay team-scoped, so an L3 human sees nothing on them (their teams set
  is empty). Extending any of them is a follow-up.
- Room power levels / Matrix roster (room-management plane, separate
  concern per the humans-update contract).

## Tests

- `internal/auth/matrix_authenticator_test.go` — L3 resolution
  (`ResolvesL3Human`), level-strict isolation both directions
  (`L3StrictPerLevel`, `L2IgnoresAccessibleWorkers`), unsupported-level
  and unknown-user rejection.
- `internal/auth/authenticator_test.go` — `WorkerReadable` L3 leg and
  no-op contract for non-L3 callers.
- `internal/auth/authorizer_test.go` — `HumanL3WriteDenied`: L3 reads pass
  the authorizer (handler filters), every L3 worker write is denied
  (update via the uniform no-team rejection).
- `internal/server/resource_handler_test.go` — `GetWorker_L3Scoped`,
  `ListWorkers_L3Scoped`, `UpdateWorker_L3Denied` (handler-level probe:
  updating an *assigned* worker still 404s),
  `GetWorker_L3MCPCredentialsSanitized` (query api_key + userinfo
  sentinels absent from the raw L3 detail response; host/path/non-
  credential query values retained), `ListWorkers_L3MCPCredentialsSanitized`
  (same contract on the list surface), `GetWorker_L2MCPCredentialsVerbatim`
  (the scrub does not over-apply: L2 reads the URLs verbatim).
- `internal/server/worker_channels_test.go` — `ChannelsL3AssignedReadAllowed`
  (team + standalone), `ChannelsL3UnassignedHidden` (404, no dial),
  `ChannelsL3MutationDenied` (handler-level probe: PUT of an *assigned*
  worker still 404s, no dial), `ChannelsL3ReadsSanitizeCredentials`
  (both read routes, non-empty sentinel credentials across qq/matrix/
  feishu/voice/dingtalk: raw L3 responses contain none of them, normal
  fields preserved, credential keys absent; L2 + admin verbatim
  round-trip; schemas untouched), `ChannelsL3UnparseableUpstreamFailsClosed`
  (non-JSON 200 body → `{}` for L3 readers).
- `internal/server/worker_approval_test.go` — `ApprovalGet_L3AssignedAllowed`,
  `ApprovalGet_L3UnassignedHidden`, `ApprovalPut_L3Denied` (no upstream
  PUT).
