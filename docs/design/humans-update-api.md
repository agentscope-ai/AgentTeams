# Human Update API

Status: implemented
API: `PUT /api/v1/humans/{name}`

## Problem

Human lifecycle had create / get / list / delete but no update. Changing a
human's permission level, team scope, or worker scope required deleting and
re-creating the Human CR — which re-provisions the Matrix account and hands
out a fresh one-time password, destroying the identity the user already
logged in with. There was also no way to narrow or widen an existing grant
as teams form and dissolve.

## Design

`PUT /api/v1/humans/{name}` — a merge-patch over the mutable part of
`spec`:

- **Pointer semantics** (the existing `containerManaged` / `state` pattern):
  absent field = unchanged; present field = replace; an explicitly empty
  list clears the list.
- **Mutable fields:** `displayName`, `email`, `permissionLevel`,
  `accessibleTeams`, `accessibleWorkers`, `capabilities`, `note`.
- **Immutable through this endpoint:** `name` and the Matrix identity
  (username / matrixUserID). Re-provisioning the account is a deliberate
  destroy-and-recreate operation, not an edit.
- **Validation, applied before the K8s write:**
  - `permissionLevel` must be 1, 2, or 3 → otherwise `400`.
  - `accessibleTeams` must reference existing Team CRs and
    `accessibleWorkers` existing Worker CRs → otherwise `400` naming the
    missing references. A dangling grant silently widens nothing but
    leaves the human unable to reach a resource they believe they can;
    rejecting it at write time keeps the permission model honest.
  - `capabilities` must be within the closed five-value set
    (`full_access`, `channel_secrets`, `external_sources`,
    `approval_policy`, `secret_reveal` — see
    [capability-foundation.md](capability-foundation.md)) → otherwise
    `400` naming the unknown value(s) and listing the valid set. Stored
    normalized (deduped + sorted); every grant/revoke is written to the
    dual-layer audit trail.
- **Authorization:** the route is `ActionUpdate` on the `human` kind. The
  existing matrix already allows that only for admin/manager; team leaders,
  team-scoped humans, and worker accounts fall through to the default deny.
  No authorizer change is required.
- **Reconcile integration:** the write updates the Human CR; the existing
  human reconciler (identity / infra / rooms phases) re-syncs Matrix
  invitations, room membership, and `groupAllowFrom` on its normal cycle.
  No new reconcile path.
- **Out of scope — room power levels:** this endpoint does not grant or
  revoke Matrix room administration. A `permissionLevel` change takes
  effect on API authorization immediately, but it does not by itself
  change the power level the human already holds in existing rooms;
  reconciling room power levels for human members is a separate concern
  that this endpoint and its reconciler integration do not handle.

## Contract

`PUT /api/v1/humans/{name}` →

| Result | Code |
|--------|------|
| updated | `200` + full human representation |
| human not found | `404` |
| invalid JSON / bad level / dangling reference / unknown capability | `400` |
| reference validation backend failure (K8s error) | `500` |
| K8s conflict after retries | `409` |

Request body (all fields optional):

```json
{
  "displayName": "Mai Zong",
  "email": "maizong@example.com",
  "permissionLevel": 2,
  "accessibleTeams": ["market-team"],
  "accessibleWorkers": [],
  "capabilities": ["approval_policy"],
  "note": "Marketing lead"
}
```

## Out of scope

- Matrix identity changes (account re-provisioning).
- Self-service updates by the human themselves (an L2 human updating their
  own grants would be a privilege escalation path; admin-only is the safe
  default).
- Bulk updates.

## Tests

- `internal/server/resource_handler_human_update_test.go` — level + teams
  update applied with untouched fields preserved; partial merge preserves
  the rest; explicit empty list clears; invalid levels (0/4/-1) → 400;
  missing team → 400 named; missing worker → 400 named; existing worker →
  200; unknown human → 404. Capabilities: grant applied (deduped + sorted)
  with other fields preserved; omitted = unchanged; `[]` clears; unknown
  value → 400 listing the valid set (and not persisted); grant + revoke
  write the expected dual-layer audit lines.
- `internal/auth/authorizer_test.go` — `TestAuthorizer_HumanUpdateAdminOnly`:
  admin/manager allowed; team leader, L2 human, and worker denied.
