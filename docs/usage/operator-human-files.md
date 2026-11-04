# Operator notes: human file area (listing)

Operational notes for the read/list surface of the per-human file area
(`GET /api/v1/humans/{name}/files`). Design details:
[`docs/design/human-file-area.md`](../design/human-file-area.md).

## Recursive listing page contract

`?recursive=1` pages the **store**, not the API response:

- One backend listing call per page (S3 `ListObjectsV2` with
  `Prefix` + `StartAfter` + `MaxKeys`), so page cost is constant no
  matter how large the area is.
- `limit` is the page bound: default **1000**, capped at **10000**.
  Non-integer or `< 1` → `400`.
- Hidden objects consume page slots: deletion tombstones, the `.keep`
  marker, and anything under the reserved `.sync/`, `.control/`, and
  `.trash/` top levels are never returned to the client but still count
  against the store's page. A page may therefore return fewer than
  `limit` entries, possibly zero — clients must keep walking until
  `nextKey` is `null`, and must not infer "no more files" from a short
  page.
- `nextKey` is the full storage key of the page's last object, e.g.
  `humans/alice/files/docs/b.txt`. The controller only checks that a
  returned cursor starts with the caller's own area prefix
  (`humans/<username>/files/`) and passes it back to the store
  verbatim; cursors crossing areas → `400`.

## Tombstones are empty objects

A soft delete does **not** remove the object from storage: it writes an
**empty (zero-byte) object** at the original key with the marker content
type `application/vnd.agentteams.tombstone`. Consequences:

- The listing APIs do not return content types, so the handler cannot
  filter tombstones by header alone. It stats an entry only when the
  listing already reported its size as zero, and drops the entry if the
  stat shows the tombstone content type. Non-empty files are never
  stat'ed for this check.
- A stat failure keeps the entry (fail-open, matching the one-level
  listing).
- Because the tombstone body is always empty, its size is always 0 and
  it always takes the stat path above.
- Objects deleted out of band (outside this API) leave no marker and
  will appear as ordinary entries if non-empty.

## Reserved levels outside `files/`

- `humans/<user>/.keep` (and `.trash/.keep`) are created by
  `Deployer.EnsureHumanStorage` and hidden from listings.
- `humans/<user>/.control/quota.json` is the cross-replica quota
  ledger: a JSON byte counter that writers reserve against before PUT.
  It lives outside `files/` and `.trash/`, is excluded from live
  `usedBytes`, and is hidden from both listings. `.control` is also a
  reserved API path (writing there via the file API → `400`).
- `.sync/` is reserved for the file-sync phase and likewise hidden.

## Degraded listing (no sizes reported)

If the backend only offers the `mc ls --json` fallback (no S3 API),
the listing reports no sizes; the handler then stats every file entry,
which is the pre-recursive-listing behavior. Recursive listing still
works, but each page performs one stat per zero-size entry — expect
higher latency on large areas with that backend.
