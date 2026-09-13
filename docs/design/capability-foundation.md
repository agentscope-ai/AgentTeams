# Capability Foundation

Status: implemented
API: `PUT /api/v1/humans/{name}` (new field) · CRD: `Human.spec.capabilities`

## Problem

The L2 permission model (#1220) needs a way to grant named sensitive-surface
privileges beyond the role baseline: writing channel credentials, managing
the remoteSkills registry, setting worker `approval_level=OFF`. The role
matrix (admin / manager / team-leader / worker / human) answers *who you
are*; capabilities answer *which sensitive surface you may touch*. Without
a foundation (field + validation + helper + audit), each consumer PR would
invent its own gating and its own audit trail.

This design is the foundation only. It gates **no** existing operation —
the consumers are the follow-on PRs in #1220 §12 (the `approval_policy`
OFF gate on the #1216 tool-approval endpoints, the mcpServers/remoteSkills
restore with `external_sources`, the secret contract).

## Value set

Five values, closed set, single source of truth
(`agentteams-controller/internal/auth/capability.go`):

| Value | Grants | Consumer |
|-------|--------|----------|
| `full_access` | meta value: implies all capabilities | (permissionLevel 1 already implies all at the role baseline) |
| `channel_secrets` | writing channel credentials | channel-credential endpoints |
| `external_sources` | remoteSkills registry + credential management | mcpServers/remoteSkills restore |
| `approval_policy` | setting worker `approval_level=OFF` | #1216 tool-approval endpoints |
| `secret_reveal` | (reserved, no v1 consumer — #1220 §6.4) | — |

Drift between this table and the code constants is pinned by
`TestValidCapabilitiesMatchesDocumentedValueSet`.

## Design

### CRD

`Human.spec.capabilities: []string` (omitempty). List-shaped so future
values are additive. Team leaders and other SA-based identities never hold
capabilities (#1220 §5) — the only writer is the human-update API, which is
admin/manager-only.

### human-update API

`PUT /api/v1/humans/{name}` gains `capabilities` with the same merge-patch
semantics as `accessibleTeams` (absent = unchanged / explicit list =
replaces / `[]` = clears). Unknown values → `400` listing the closed set.
Values are stored normalized (deduped + sorted). Only admin/manager may
update humans (pre-existing authorizer default-deny, pinned by
`TestAuthorizer_HumanUpdateAdminOnly`) — self-grant is structurally
impossible.

### HasCapability

`auth.HasCapability(caller *CallerIdentity, cap Capability) bool`:

- admin / manager → always true (role baseline);
- worker → always false;
- human / team-leader → set membership, with `full_access` implying every
  value.

A capability **never implies team scope**: consumers compose the full
#1220 §3 check order — role baseline AND `TeamMatches` AND `HasCapability`
— in that order. `CallerIdentity.Capabilities` is populated by the Matrix
authenticator, where the Human CR is already fetched (zero new I/O); SA
identities never carry the field.

### Dual-layer audit (#1220 §8)

`internal/audit`: `Client.Record(ctx, Event)` writes

1. an immediate structured log line (always, even without storage);
2. an append-only `audit/<YYYY-MM-DD>.jsonl` object (UTC date),
   read-modify-write with `PutObjectIfMatch` ETag-optimistic concurrency
   and 3 retries (100/200/400 ms + jitter). In-process concurrency is
   serialized by a client mutex; the durable layer assumes a single
   controller replica (embedded and k8s deployments today).

Secret hygiene: `Event` is a closed schema — `Before`/`After` carry
capability names only, `Detail` is a controlled summary, and there is no
free-text field a credential could hide in (pinned by
`TestEventJSONHasClosedSchema`).

v1 wires only the capability grant/revoke events (one event per changed
value). Later consumers (approval_level changes, channel credential
writes, external source adds/updates) call the same `Record`.

## Contract

| Surface | Contract |
|---------|----------|
| CRD | `spec.capabilities` omitted = none; unknown values rejected at admission (400); team leaders never hold capabilities |
| API | merge-patch: absent = unchanged / list = replaces / `[]` = clears / unknown value = 400 (error lists the valid set) |
| Helper | `HasCapability`: admin/manager true, worker false, human/leader set membership with `full_access` meta-implication; no team-scope semantics |
| Audit | every grant/revoke → log line + `audit/<date>.jsonl` append; never secret values |

## Known limitations

- Single controller replica for the durable audit layer; a queryable audit
  store (or per-event objects) is #1220 §13 Q3.
- `secret_reveal` has no v1 consumer (reserved value).
- Identity caching: a capability change becomes visible to the affected
  human when their Matrix-token identity cache expires — the same
  semantics `accessibleTeams` already has.

## Tests

- `internal/auth/capability_test.go` — role-baseline matrix (admin/manager
  always true, worker always false, leader structurally false, nil /
  unknown role false), per-value set membership, `full_access`
  meta-implication, value-set drift pin, normalize semantics.
- `internal/auth/matrix_authenticator_test.go` — L2 human carries granted
  capabilities; capability-less human carries an empty set and holds
  nothing.
- `internal/server/resource_handler_human_update_test.go` — grant applied
  (deduped + sorted) with other fields preserved; omitted = unchanged;
  `[]` clears; unknown value → 400 naming the value and listing the set
  (and not persisted); a grant + revoke writes the expected audit lines.
- `internal/audit/audit_test.go` — object creation with a parseable
  line, append preserves earlier lines, retry on ETag conflict, give-up
  after exhausted retries (all-or-nothing, no panic), 20-way concurrency
  with no lost lines, closed-schema secret-hygiene pin, nil-storage
  degradation.
