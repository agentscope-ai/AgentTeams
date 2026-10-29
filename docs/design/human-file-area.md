# Per-Human File Area, Recycle Bin, and Workspace Quota

Status: implemented (P1 of #1414)
APIs: `GET/PUT/DELETE /api/v1/humans/{name}/files/{path...}`, `GET /api/v1/humans/{name}/trash`, `POST /api/v1/humans/{name}/trash/{id}/restore`, `DELETE /api/v1/humans/{name}/trash/{id}`, `GET/PUT /api/v1/humans/{name}/quota`

## Scope

P1 gives every human a private, controller-proxied file area with a
recycle bin and an optional workspace quota:

- **In scope (P1):** per-human storage namespace, the proxied file API
  (read / list / write / soft-delete), the trash (list / restore / purge),
  path validation, a per-file size limit, and an optional workspace quota
  (`workspaceQuotaBytes`).
- **Out of scope (later phases):** team default authorization and
  `workspaceAccess` (P2), file sync / carriers / `file_sync` (P3), agent
  attribution and UI (P4). In P1 the area is reachable by the human
  themself, admins, and managers only.

## Storage layout

The area lives under the top-level `humans/` namespace in the shared
bucket, parallel to `agents/` and `teams/`:

```
humans/                              (created by the initializer)
└── <username>/
    ├── files/                       human's working files (this API)
    │   └── ...
    └── .trash/                      recycle bin (this API)
        ├── <id>/meta.json           original path, deletedAt, caller, role, size
        └── <id>/content             the deleted file's bytes
```

- `<username>` is the Human's effective username
  (`spec.username`, falling back to the metadata name).
- `Deployer.EnsureHumanStorage` creates `files/.keep` and
  `.trash/.keep` idempotently; the human reconciler calls it after the
  infra phase. A storage failure is **non-fatal** — it is logged and the
  reconcile requeues sooner, the same contract as team shared storage —
  so a transient storage hiccup never blocks Matrix user/room sync.
- `memory/` and `digest/` are deliberately not created in P1.
- Trash entries count toward the workspace quota.

Trash id format: `<UTC yyyymmddThhmmssZ>-<8 hex>` (e.g.
`20261024T123045Z-abcd1234`). The timestamp prefix keeps listings
sortable; the random suffix disambiguates same-second deletes.

## Authorization matrix

All routes are authenticated; authorization happens in the handler after
the Human CR is resolved, so the two denial shapes can coexist:

| Caller | read / list / write | trash restore | trash purge | quota GET | quota PUT |
|---|---|---|---|---|---|
| the human (username matches the area) | ✔ (own area) | ✔ (own area) | ✖ 403 | ✔ (own area) | ✖ 403 |
| manager | ✔ (any area) | ✔ | ✖ 403 | ✔ | ✖ 403 |
| admin | ✔ (any area) | ✔ (`?overwrite=1`) | ✔ | ✔ | ✔ |
| other human / team leader / worker | ✖ 404 | ✖ 404 | ✖ 404 | ✖ 404 | ✖ 404 |

Callers with no access at all receive **404 with the same body as a
non-existent Human** — the API does not confirm a human's existence or
file layout to strangers.

## File API

### `GET /api/v1/humans/{name}/files/{path...}`

- **File** (plain path): streams the bytes with `Content-Type` and an
  `ETag` header.
- **Directory** (path ends with `/`, `?list=1`, or the empty path):

```json
{"path":"files/docs/","entries":[
  {"name":"b.txt","type":"file","size":6,"updatedAt":"2026-10-24T12:00:00Z"},
  {"name":"nested","type":"directory","updatedAt":"2026-10-24T12:00:00Z"}
]}
```

  Directory entries are synthesized from nested keys; `.keep` markers are
  hidden; entries are sorted by name.

### `PUT /api/v1/humans/{name}/files/{path...}`

- The raw request body is the file content.
- Per-file size limit: **64 MiB** by default, overridable via the
  `HUMAN_FILE_MAX_BYTES` environment variable; over the limit → `413`
  with `{"code":"size_exceeded"}`.
- Optional `If-Match: <etag>` for optimistic concurrency; a stale etag or
  a not-yet-existing target → `412` with `{"code":"etag_mismatch"}`.
- The workspace quota is enforced before the write: if
  `usedBytes + size` exceeds `workspaceQuotaBytes` → `413` with
  `{"code":"quota_exceeded"}`.
- Success → `200` `{"path":"files/<rel>","size":N,"etag":"..."}`.

### `DELETE /api/v1/humans/{name}/files/{path...}`

Soft delete: the file is copied to `.trash/<id>/{meta.json,content}`
**before** the original is removed, then the original is deleted. Success
→ `200` `{"trashId":"<id>"}`. Directory deletion is not supported.

## Trash

| Endpoint | Access | Behavior |
|---|---|---|
| `GET .../trash` | self / manager / admin | `{"entries":[{id,path,size,deletedAt,caller,role}]}`, newest first |
| `POST .../trash/{id}/restore` | self / manager / admin | writes the bytes back to the original path and removes the entry; target already exists → `409` `{"code":"path_conflict"}`; `?overwrite=1` is accepted only from an admin |
| `DELETE .../trash/{id}` | **admin only** (others `403`) | permanent removal + `human_trash_purge` audit event |

## Quota

`Human.spec.workspaceQuotaBytes` (nullable `int64`) is the total byte
budget for the area — `files/**` plus `.trash/**` — and is unset
(unlimited) by default.

- **`GET /api/v1/humans/{name}/quota`** (self / manager / admin) →
  `{"usedBytes":N,"quotaBytes":M|null}`. `usedBytes` is computed live by
  listing both prefixes (nothing is persisted); `quotaBytes` is `null`
  when the quota is unset.
- **`PUT /api/v1/humans/{name}/quota`** (admin only) — body
  `{"quotaBytes":N}` sets the quota, `{"quotaBytes":null}` clears it; a
  negative value → `400`. The value is patched onto the Human CR and the
  change is audited (`human_quota_set`).
- **Enforcement:** writes are checked before the bytes land —
  `usedBytes + size > quota` → `413` `{"code":"quota_exceeded"}`.
- **Atomic reservation (cross-controller):** quota enforcement uses a
  per-human ledger at `humans/<user>/.control/quota.json`, not only a
  process-local check. The first writer creates it with
  `PutObjectIfAbsent` (`If-None-Match:*`); later writers reserve the
  `newSize-oldSize` delta with `PutObjectIfMatch` (`If-Match`) and retry on
  a precondition conflict. The reservation is released if the file write
  fails. The ledger is outside `files/` and `.trash/`, is excluded from
  live usage, and `.control/` is a reserved API path. Purge and an
  overwrite restore adjust the ledger when it exists. While quota is
  disabled, an existing ledger still advances on writes; enabling a quota
  rebuilds the ledger from live usage before enforcement, but never lowers an
  existing ledger value. The higher value may be an outstanding reservation
  whose object PUT has not committed yet; preferring a conservative over-count
  avoids admitting a second write over the quota.
- **Cross-replica test:** two independently constructed handlers sharing
  one OSS store must produce one 200 and one 413 under a 10-byte quota,
  with final usage ≤10.
  A trash restore is not quota-gated: the bytes are already counted in
  `usedBytes`, and moving them back to `files/` does not grow usage (an
  `?overwrite=1` restore replaces the target and reduces it). Data that
  already exceeds a newly lowered quota is not rejected; only further
  growth is.

## Path rules

Paths are relative to `files/`, NFC-normalized, and validated before any
storage access. Rejected with `400`: absolute paths, `..` / `.` segments,
empty segments, backslashes, control characters, non-UTF-8 input, total
length over 1024 bytes, any segment over 255 bytes, the reserved
top-level names `.trash` and `.sync`, and Windows reserved device names
(`CON`, `NUL`, `COM1-9`, `LPT1-9`, with or without an extension).

## Audit

Every mutating operation appends to the dual-layer audit trail:

| Action | Detail fields |
|---|---|
| `human_file_write` | path, size, etag |
| `human_file_delete` | path, size, trashId |
| `human_trash_restore` | path, size, trashId, overwrite |
| `human_trash_purge` | path, size, trashId |

Events carry `who` / `role` / `target` (the Human CR name) and are
written to `audit/<date>.jsonl` like all other controller audit events.

## Errors

| Condition | Code | Body |
|---|---|---|
| no access (or human missing) | `404` | `{"message":"human not found"}` |
| admin-only operation without admin | `403` | `{"message":"admin role required"}` |
| invalid path / bad id / writing a directory | `400` | message names the problem |
| per-file size limit | `413` | `{"code":"size_exceeded",...}` |
| workspace quota exceeded | `413` | `{"code":"quota_exceeded",...}` |
| stale `If-Match` | `412` | `{"code":"etag_mismatch",...}` |
| restore target exists | `409` | `{"code":"path_conflict",...}` |
