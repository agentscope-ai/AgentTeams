# Model Grant Enforcement

Status: implemented (PR-B1, follow-up to #1513)
APIs: `GET /api/v1/model-access?worker=`, `PUT /api/v1/workers/{name}`,
`PUT /api/v1/workers/{name}/coding-cli/{cli}/settings`

## What is enforced

`#1513` defines per-human model grants and the read contract. This change
makes the grant binding on the two write surfaces that host models:

| Surface | Before | After |
|:--|:--|:--|
| `PUT /workers/{name}` (`model`, `modelProvider`, `subagentModel`) | hard-forbidden for L2 humans | grant-gated: the `(provider, model)` pair must be in the caller's effective set; `subagentModel: ""` needs the spawn inherit grant |
| `PUT /workers/{name}/coding-cli/{cli}/settings` (`model`) | unrestricted | grant-gated against the CLI's set; provider-defining keys (`security`, `provider(s)`, `baseUrl`, `env`) stay L1-only |
| Team leaders | read-only | unchanged (read-only) |
| L1 (admin/manager) | unrestricted | unrestricted; additionally records implicit grants |

Only value changes are checked: writing the value that is already there is
always allowed, even with no grant at all. A change to either half of the
pair is judged as the resulting pair, so swapping the provider behind the
same model name is caught; sending only `modelProvider` answers
`model_pair_required`.

## Effective set

`effective = explicit rows ∪ current value`

- **Explicit rows** — `Human.spec.modelAccess` scopes, or the referenced
  `ModelAccessTable` (`GET /model-access` resolves them for the caller).
- **Implicit annotation** — `agentteams.io/model-grants` on the Worker. L1
  appends the chosen pair on create and on every later L1 model write
  (deduped, ≤ 32 rows). This is history, not a selectable grant. L2 never writes it.
- **Current value** — what the worker runs right now, so an unchanged value
  is never locked out by a grant change.

The implicit annotation is capped at 32 rows; explicit rows (validated up
to 200) are never truncated. An over-cap annotation is a `409`
`model_grant_overflow` with the count, never a silently truncated view.

## Revocation contract

Revoking a scope or deleting its table rows removes authorization for future
selections immediately. Historical annotation rows do not restore that authority.
The currently selected value may keep running and may be written back unchanged;
switching away and then back requires a current explicit grant.

## Native configuration writes and rollback

The production member-config handler installs model authorization on both
`PUT /api/members/{name}/config` and snapshot rollback. Checks run inside the
per-worker mutation lock against complete, unredacted current and target native
documents, before snapshots, desired state or worker files are written.

OpenClaw main/fallback selections use main grants; subagent selections use spawn
grants. Hermes explicit main/delegation pairs use the corresponding grants.
Provider definitions, credentials, selection policy and auxiliary/opaque route
overrides remain L1-owned. Non-model settings and unchanged values remain editable.
Unresolvable bare/auto selections and Hermes `custom` route aliases return
`403 model_pair_unresolvable`; a runtime alias is not a verified gateway-provider
binding. L1 access and Team Leader read-only access are preserved.

Regressions cover distinct Human callers, revoked history, denied writes leaving
files/snapshots unchanged, and Hermes real temporary-file commands. They do not
represent a deployed Matrix login or a running model's hot-application.

## Non-goals (still open, tracked as follow-ups)

- Verified runtime-alias/provider bindings for otherwise unresolvable selections.
- Materialization of grants into runtime files — depends on the restart/
  hot-apply path.
- Catalog validation / `available` / staleness (A2) and runner default
  models (B2).
