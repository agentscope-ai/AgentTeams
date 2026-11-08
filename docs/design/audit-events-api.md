# Design: GET /api/v1/audit — query endpoint for the durable audit store

## Context

The capability foundation (PR #1220 issue, branch `feat/capability-foundation`,
PR #1237) introduced the write side of the dual-layer audit store: the
`internal/audit` client appends JSONL events to `audit/<YYYY-MM-DD>.jsonl`
objects in MinIO — durable, append-only, outside the K8s/SQLite data plane.
Until now the store was write-only: events could not be read back, so the
workbench could not show "who changed what, and when".

This design adds the read side, a new controller endpoint:

```
GET /api/v1/audit
    ?team=<team-name>     team scope; L2 users may only query teams they
                          can access (same scope rules as the worker/team
                          read APIs). Absent: L1 (admin/manager) only.
    ?from=<RFC3339>       inclusive lower bound on event time
    ?to=<RFC3339>         inclusive upper bound on event time
    ?kind=<category>      capability | approval_level | channel | source
                          (derived from the action; unknown future actions
                          pass through as their own kind)
    ?cursor=<opaque>      keyset pagination cursor from a previous response
    ?limit=<n>            page size, default 50, max 200
```

Input validation (all 400, before any storage scan):

- `from`/`to` must parse as RFC3339; when both are present the **original
  instants** are compared — `from` strictly after `to` is rejected,
  including within a single day (the day truncation used to enumerate
  daily objects must not mask within-day ordering).
- The cursor must decode as base64url JSON whose `ts` parses as RFC3339
  and `date` as `YYYY-MM-DD`; a malformed field is rejected at decode
  time (400), never surfaced later as a server error during the scan.

Response:

```json
{
  "events": [
    {
      "ts": "2026-09-14T03:22:10.123456789Z",
      "kind": "capability",
      "actor": "admin",
      "target": "h1",
      "targetTeam": "alpha-team",
      "action": "capability_grant",
      "capability": "approval_policy",
      "before": ["approval_policy"],
      "after": ["approval_policy", "channel_secrets"],
      "detail": ""
    }
  ],
  "cursor": "<opaque>"
}
```

- Events are returned newest-first, keyset-paged on (timestamp, seq) where
  seq is the event's line number within its daily object (stable: objects
  are append-only).
- `before`/`after` appear only where defined (capability add/remove,
  approval_level change). They never contain secret values — the writer
  guarantees that by construction (field names only).
- `cursor` is present only when the page is full (more events may follow).
- `targetTeam` (additive over the issue's event shape) lets the L1 view
  attribute events to teams; team-scoped queries are already filtered on it.

## Scope rules (consistent with the existing read APIs)

- **admin / manager**: full scope. Without `?team=` they see every event,
  including events with no target team (e.g. capability changes on a human,
  which are global, not team-scoped). With `?team=T` only events whose
  `targetTeam` is T.
- **team leader / L2 human**: `?team=` is mandatory (400 with a
  self-explanatory message otherwise); a cross-team read is hidden as 404
  (W8 anti-probing — 403 would expose the team's existence).
- **worker**: denied (the route is `ActionGet` on resource kind `audit`;
  the authorizer's default deny applies).

The authorizer lets the read through for humans and team leaders at the
middleware level — the handler is the real scope boundary, exactly as the
worker/team read paths do ("handler filters by accessibleTeams").

## Degradation

- **Missing daily object** (no events recorded that day): normal — zero
  events from that day, not an error.
- **Storage read failure** (object get or prefix list): 502 with the
  standard error envelope.
- **Malformed object line**: 502 for the whole request — never a partial,
  silently truncated list.

Unbounded ranges enumerate the `audit/` prefix (non-daily objects ignored);
explicit ranges compute the day set directly and are capped at 366 days
per request (400 beyond), which bounds both the scan and the memory.

## Why keyset pagination

The store is append-only and unbounded over time; offset pagination would
re-scan and re-sort every page and would shift under concurrent appends.
Keyset on (timestamp, seq) is stable under append: seq only grows at the
tail of a day, so an event's position never changes after the page that
returned it.

## Files

| File | Change |
|------|--------|
| `agentteams-controller/internal/audit/query.go` | new: Query engine (day enumeration, filters, keyset paging, typed errors) |
| `agentteams-controller/internal/audit/query_test.go` | new: engine tests (pagination, filters, malformed, storage failure, tiebreak) |
| `agentteams-controller/internal/server/audit_handler.go` | new: HTTP handler (scope, validation, envelope) |
| `agentteams-controller/internal/server/audit_handler_test.go` | new: handler tests (role matrix, 400/404/502 mapping) |
| `agentteams-controller/internal/server/http.go` | +3: route registration |
| `agentteams-controller/internal/auth/authorizer.go` | +18: `audit` read case for human / team-leader (handler is the scope boundary) |
| `agentteams-controller/internal/auth/authorizer_test.go` | +22: role matrix test |

## Compatibility

- Read-only: no schema change, no writer change, no data migration.
- The route is additive; older workbench clients that do not call it are
  unaffected.
- Stacked on `feat/capability-foundation` (PR #1237) because the writer
  and this reader share the `internal/audit` package and the MinIO layout.
  Once #1237 merges, this PR rebases to main and becomes self-contained.

## Open questions

- v1 has no live tailing (polling only); SseStream is a follow-up if the
  workbench wants real-time.
- `secret_reveal` (reserved in #1220 §8) will appear as its own kind once
  wired on the write side; no reader change needed.
