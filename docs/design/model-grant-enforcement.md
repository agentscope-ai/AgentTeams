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

`effective = explicit rows ∪ implicit annotation ∪ current value`

- **Explicit rows** — `Human.spec.modelAccess` scopes, or the referenced
  `ModelAccessTable` (`GET /model-access` resolves them for the caller).
- **Implicit annotation** — `agentteams.io/model-grants` on the Worker. L1
  appends the chosen pair on create and on every later L1 model write
  (deduped, ≤ 32 rows). L2 never writes this annotation.
- **Current value** — what the worker runs right now, so an unchanged value
  is never locked out by a grant change.

The implicit annotation is capped at 32 rows; explicit rows (validated up
to 200) are never truncated. An over-cap annotation is a `409`
`model_grant_overflow` with the count, never a silently truncated view.

## Revocation contract

Revoking a scope removes its explicit rows immediately. It does **not**
rewrite the implicit annotation: those rows record models L1 previously
granted, and the current value keeps running. Consequently, after a
revocation:

- the current value remains writable (unchanged value);
- a value present in the L1-issued annotation remains selectable;
- any other value is denied (`model_not_allowed`), and an unknown provider
  answers `provider_not_allowed`.

Removing a historical value therefore requires an L1 edit that no longer
selects it (the annotation is append-only history, not a cache of the
Human's current scopes).

## Non-goals (still open, tracked as follow-ups)

- Native runtime-config policy hook (`native_config_*` 403,
  `model_pair_unresolvable`) — depends on the runtime-config work.
- Materialization of grants into runtime files — depends on the restart/
  hot-apply path.
- Catalog validation / `available` / staleness (A2) and runner default
  models (B2).
