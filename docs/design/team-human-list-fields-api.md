# Team and Human List Fields API

Status: implemented
API: `GET /api/v1/teams`, `GET /api/v1/humans`

## Problem

Every `GET /api/v1/teams` row carried the full `TeamResponse` — the
member and port references (`admin`, `humanMembers`, `workerMembers`,
`workerExposedPorts`), the room IDs, `heartbeatEvery` and the team-wide
LLM tuning — and every `GET /api/v1/humans` row carried the full
`HumanResponse`, including `initialPassword`, the one-shot provisioning
credential (issue #1475). Both list endpoints gained an opt-in `?fields=`
projection, and the human list stopped carrying `initialPassword` in
**any** view.

## Design

`?fields=` selects the row view; both endpoints share the worker list's
parser and fail-closed stance (see
[worker-list-fields-api.md](worker-list-fields-api.md)):

- `summary` — the stable list-card fields:
  - Team: `name`, `teamName`, `phase`, `description`, `leaderName`,
    `leaderReady`, `readyWorkers`, `totalWorkers`, `message`,
    `workerNames`. Excluded: the member and port references, the room
    IDs, `heartbeatEvery` and the team-wide LLM defaults — those stay on
    the detail endpoint.
  - Human: `name`, `phase`, `displayName`, `email`, `permissionLevel`,
    `matrixUserID`, `message`. Excluded: the scope lists
    (`accessibleTeams`/`accessibleWorkers`), `capabilities`, `note`,
    `rooms` and `initialPassword` — those stay on the detail endpoint.
- `all` (or an absent/empty value) — the full response, byte-identical
  to the pre-projection implementation.

Unknown tokens, or `all` combined with a subset, return `400` before any
cluster access. Token order, trimming and duplicates are tolerated.
Projection happens after visibility filtering, so scoping semantics are
untouched. The identity contract matches the worker list: every view
carries `name`; list rows are keyed by `name`, never by position.

## List-only contract

The `summary` view is **list-only**: it exists on the list endpoints
only. The detail endpoints `GET /api/v1/teams/{name}` and
`GET /api/v1/humans/{name}` have no projection and always return the
full resource. Consequently:

- A consumer that renders a list must **not** issue a per-row detail
  fetch (N+1): list rows render from the list payload alone; the detail
  fetch happens once, when one item is opened.
- A consumer that displays detail or initializes an edit form must fetch
  the full resource via the detail endpoint, **never** from a list row:
  a summary row lacks the member references / scope lists an edit form
  needs, and writing from it would drop existing values.
- For humans the rule is stricter: `initialPassword` is **never** a list
  field in any view (the one-shot provisioning credential must not ride
  along on every enumeration); only the detail endpoint returns it.

## Consumers

The `agt` CLI is the known consumer of both endpoints:

- `agt get teams` and `agt get humans` gained `--fields`
  (`all|summary`, default `all`, which sends `fields=all`); the default
  stays the full response, so existing readers of the list keep working
  unchanged.
- `-o json` is a **verbatim pass-through** of the selected view (the same
  contract as the worker list, issue #1477): the CLI re-emits the
  server's bytes without decoding them into a response type, so no field
  of the chosen view is dropped and none is invented.
- The named forms `agt get teams <name>` / `agt get humans <name>`
  always return the full detail, also passed through verbatim for
  `-o json`.

As of this release the in-repo CLI requests `fields=all` by default and
**no in-repo caller selects `fields=summary`** yet; the default stays the
full response until the consumers that need the full payload request
`fields=all` explicitly (the same gate as the worker list).
