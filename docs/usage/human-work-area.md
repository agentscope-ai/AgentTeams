English | [中文](../zh-cn/usage/human-work-area.md)

# Human work area: listing files

Every human has a private file area — the **work area** — reached through
the controller API under `/api/v1/humans/{name}/files`. Only the human
themself, managers, and admins can read it; every other caller (other
humans, team leaders, workers) gets a `404` that is indistinguishable
from a non-existent human — the API never confirms a human's existence
or file layout to strangers.

## Root listing

The installation administrator also has a Human work area. Startup and upgrades
idempotently register its existing Matrix identity as a Human marked with
`agentteams.io/system-admin=true`. Its password and existing room memberships
remain managed by the installation; Human reconciliation only initializes its
file area. Human API responses expose `systemAdmin: true` so user-management
views can hide this built-in record while personal files remain available.

`GET /api/v1/humans/{name}/files` lists the top level of the area:

```json
{"path":"files/","entries":[
  {"name":"b.txt","type":"file","size":6,"updatedAt":"2026-10-24T12:00:00Z"},
  {"name":"nested","type":"directory","updatedAt":"2026-10-24T12:00:00Z"}
]}
```

Directories are synthesized from nested keys and listed without a
trailing slash; `.keep` placeholder objects are hidden; entries are
sorted by name. File sizes come straight from the storage listing — only
zero-byte entries are stat'ed again, because a zero-byte entry may be a
deletion tombstone (dropped from the listing).

## Recursive flat listing

Append `?recursive=1` to list **every file in the area at all depths** in
one flat, key-ordered response:

```json
{"entries":[
  {"path":"files/a.txt","size":5,"etag":"d41d...","updatedAt":"2026-11-01T02:33:00Z"},
  {"path":"files/docs/nested/c.txt","size":1,"etag":"9e10...","updatedAt":"2026-11-01T02:33:00Z"}
],
"nextKey":"humans/alice/files/docs/nested/c.txt"}
```

- `path` always carries the `files/` prefix — it is the API path, not
  the raw storage key.
- `limit` bounds the **store page**, not the entry count: default
  **1000**, capped at **10000**; any non-integer or value `< 1` is a
  `400`. Objects hidden from the response (deletion tombstones, `.keep`,
  the reserved `.sync`/`.control`/`.trash` levels) still consume page
  slots, so a page can return fewer than `limit` entries — even zero.
- `nextKey` is the full storage key of the page's last object, or
  `null` when the listing is exhausted. To walk the whole area, pass it
  back verbatim as `?cursor=<key>`; the cursor must live inside this
  area's own prefix (a cursor from another area is rejected with `400`).
- Deleted files never appear: a soft delete leaves a zero-byte
  tombstone object, and only zero-byte entries are stat'ed to check for
  the tombstone marker.
- The field is deliberately named `nextKey` (not `cursor` /
  `nextCursor` in the response) so it can be told apart from the
  incremental-delta cursors (`v1:<seq>`) of the later sync API.

A complete walk:

```
GET /api/v1/humans/alice/files?recursive=1&limit=1000
→ append entries; while nextKey != null:
GET /api/v1/humans/alice/files?recursive=1&limit=1000&cursor=<nextKey>
```
