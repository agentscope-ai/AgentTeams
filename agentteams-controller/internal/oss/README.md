# internal/oss

Object storage client for the controller. `StorageClient` (client.go) is the
interface the server layer sees; `MinIOClient` (minio.go + s3_client.go) is
the implementation. Object I/O goes through the S3 API via minio-go
(`PutObject` / `GetObject` / `StatObject` / `ListObjects` / `RemoveObject` /
`RemoveObjects` / `MakeBucket` / `PresignedGetObject` / `PresignedPostPolicy`
/ `PresignedPutObject`, SigV4). `ossfake/memory.go` is the in-memory fake for
unit tests.

## Backend compatibility

The object path depends on the **S3 protocol only**. MinIO is the default
deployment; RustFS (S3-compatible) is a supported drop-in target. MinIO's
community edition reached EOL, which is why the storage layer must not lean
on MinIO-specific behavior.

Allowed on the object hot path:

- S3 object API: put / get / head / list / delete / multi-delete / presigned
  URL (SigV4).
- `MakeBucket` for `EnsureBucket` (idempotent: `BucketAlreadyOwnedByYou` /
  `BucketAlreadyExists` / 409 count as success — the SDK form of
  `mc mb --ignore-existing`).

Forbidden on the object hot path:

- `mc` subprocesses of any kind (no `mc cp/rm/ls/stat/share/mb`).
- MinIO extension headers (`x-minio-*`).
- Assumptions about MinIO-specific error *codes* (see normalization below).

`mc` survives as an **optional management tool** in exactly two places, both
off the object path:

| Site | Why mc stays |
| --- | --- |
| `Mirror` (`mc mirror --remove`) | no minio-go equivalent of reconcile-with-delete; skill sync is a low-frequency management operation |
| `MinIOAdminClient` (minio_admin.go, `mc admin user/policy`) | user/policy admin is embedded-mode only; incluster mode gets scoped STS credentials instead |

The `mc alias set` registration (`ensureAlias`) runs only before those
management calls. No object I/O path resolves or registers an mc alias.

### Known MinIO ↔ RustFS differences

The client is written against the S3 baseline; observed per-backend
differences it has to tolerate:

| Area | MinIO | RustFS | Client behavior |
| --- | --- | --- | --- |
| 404 object-missing code | `NoSuchKey` | `object_missing` (sometimes `NoSuchKey`) | `isSDKNotFound` keys off the **404 status**, not the code → `os.ErrNotExist` |
| 404 bucket-missing code | `NoSuchBucket` | `NoSuchBucket` | same normalization (`StatusCode == 404` covers both) |
| Conditional PUT (If-Match / If-None-Match) | 412 on conflict | 412 on conflict | `ErrPreconditionFailed` on `PreconditionFailed` / 412; 501/`NotImplemented` → `ErrConditionalUnsupported` (strict paths fail closed) |
| Conditional DELETE | not supported | not supported | not used anywhere; never rely on it |
| List pagination | ListObjectsV2 + continuation token | ListObjectsV2 + continuation token | recursive walks drive the SDK channel with `StartAfter`/`MaxKeys`; the one-level view uses the delimiter answer (common prefixes become `dir/` entries) |
| Presigned URLs | SigV4, host bound in the signature | SigV4, host bound in the signature | URLs are signed against `PublicEndpoint` and **never host-rewritten** afterwards (that would break the signature); path-style `/bucket/key` |
| Single-part ETag | MD5 hex | MD5 hex | compared via `canonicalizeETag` (strips quotes, lowercases) |
| `mc admin` duplicate idempotency | errors on "already exists" (handled) | answers rc=0 silently (RustFS 1.0.3/1.0.4) | the admin client treats both as success; admin output is never parsed as a capability signal |

### MinIO file/directory name collisions

Keep file keys distinct from directory prefixes on MinIO. With both `z`
and `z/child` stored, MinIO RELEASE.2025-09-07T16-13-09Z can serve the child
by its exact key while omitting it from recursive S3 listings. Both the
legacy `mc` implementation and the SDK implementation then leave that
unlisted child behind during a directory sweep. This migration does not
repair that existing backend limitation. RustFS 1.0.4 supports this layout.

The backend matrix tests ordinary directory cleanup on both backends and
coexisting bare-marker preservation on RustFS. The unit tests additionally
verify that a trailing-slash sweep never deletes the bare key or exceeds
its prefix-scoped permissions.

### Error normalization

Every read/stat path maps a 404 (any code, including empty-body 404s some
backends answer) to `os.ErrNotExist`. Callers see only the sentinels from
client.go: `os.ErrNotExist`, `ErrPreconditionFailed`, `ErrConditionalUnsupported`,
`ErrPresignUnsupported`. No MinIO-specific error string or code may leak past
this package.

## Real-backend matrix

`backend_matrix_test.go` runs the same contract — put/get/stat, one-level and
paged recursive list, presigned GET (anonymous download) and presigned upload
(POST policy and PUT fallback), recursive delete (tombstones included,
sibling prefix survives, idempotent re-sweep), conditional put (absent /
match / stale / strict), and the error mapping above — against whichever
backend is configured. Env-gated; all tests `t.Skip` when unset, so CI is
unaffected.

```sh
export AGENTTEAMS_OSS_KIND=rustfs     # or: minio
export AGENTTEAMS_TEST_OSS_ENDPOINT=http://127.0.0.1:9000
export AGENTTEAMS_TEST_OSS_AK=minioadmin
export AGENTTEAMS_TEST_OSS_SK=minioadmin
export AGENTTEAMS_TEST_OSS_BUCKET=agentteams-matrix
go test ./internal/oss/ -run 'TestMatrix' -count=1 -v
```

Local backend (the CI recipe is `.github/workflows/test-human-files-s3.yml`):

```sh
docker run -d --name rustfs-matrix -p 9000:9000 \
  -e RUSTFS_ACCESS_KEY=minioadmin -e RUSTFS_SECRET_KEY=minioadmin \
  rustfs/rustfs:1.0.4
```

Each run writes under a unique `oss-matrix/<kind>-<nanotime>/` prefix in the
configured bucket and sweeps it through `DeletePrefix` again on cleanup.
