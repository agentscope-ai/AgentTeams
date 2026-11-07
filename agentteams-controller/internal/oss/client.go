package oss

import (
	"context"
	"errors"
	"time"
)

// ObjectMeta describes an object in storage.
type ObjectMeta struct {
	Size    int64
	ModTime time.Time
	// ETag is the object's content hash (MD5 for single-part writes).
	// Empty when the backend cannot report one.
	ETag string
	// ContentType is the stored object's media type ("" when the backend
	// does not report one). The file API uses it to mark deleted files with
	// a tombstone object instead of an unconditional delete.
	ContentType string
}

// ErrPreconditionFailed is returned by PutObjectIfMatch when the object's
// current ETag does not match the expected one — the write was NOT applied.
var ErrPreconditionFailed = errors.New("oss: precondition failed (etag mismatch)")

// ErrConditionalUnsupported is returned by a strict conditional write
// (PutObjectIfMatchStrict) when the backend cannot honor If-Match at all
// (the provider answers NotImplemented/501). The write is NOT performed and
// the call never degrades to a compare-then-put: callers that need conflict
// safety must fail closed on this error.
var ErrConditionalUnsupported = errors.New("oss: backend cannot provide conditional writes")

// ErrPresignUnsupported is returned by PresignGet when the deployment has
// no caller-reachable storage origin (Config.PublicEndpoint is empty). No
// URL is minted: a URL signed for the internal endpoint is unusable from
// the caller because the Host participates in the S3 V4 signature. Callers
// map it to an explicit capability response (501 presign_unsupported) and
// fall back to the streamed GET.
var ErrPresignUnsupported = errors.New("oss: presign unsupported (storage endpoint is not caller-reachable)")

// ErrStorageBucketUnavailable reports that the configured bucket is gone:
// deleted out of band, renamed, or never created. It is deliberately NOT
// os.ErrNotExist — a storage/deployment failure must never reach callers
// (or the sync client) as a missing file, which would read as a deletion.
//
// The case that forces this distinction is HEAD: minio-go turns any 404 on
// a HEAD into NoSuchKey, including the one a deleted bucket answers, so the
// not-found classification alone cannot tell "no such key" from "no such
// bucket". Key-scoped calls therefore confirm the bucket still exists before
// normalizing to os.ErrNotExist.
var ErrStorageBucketUnavailable = errors.New("oss: bucket unavailable")

// StorageClient abstracts object storage operations.
// Implementations: MinIOClient (mc CLI), future S3Client (aws-sdk-go).
// ObjectInfo is a listing entry with the last-updated timestamp when the
// backend can provide it.
type ObjectInfo struct {
	Name string
	// UpdatedAt is RFC3339 UTC, or "" when the backend does not expose
	// (or the parser cannot decode) a timestamp.
	UpdatedAt string
	// Size is the object's byte count as reported by the listing; 0 when
	// the backend does not report it. A Size of 0 is ambiguous (a
	// genuinely empty object is also 0), so callers that must
	// distinguish use StatMeta.
	Size int64
	// ETag is the object's version identifier as reported by the
	// listing; "" when the backend does not report one.
	ETag string
}

type StorageClient interface {
	// ListObjectsDetailed is like ListObjects but also reports each entry's
	// last-updated timestamp when the backend exposes one (mc ls lines carry
	// a date). Unparseable dates degrade to UpdatedAt == "" — a listing must
	// never fail because of timestamp parsing.
	ListObjectsDetailed(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// ListObjectsRecursive lists every object under prefix at all levels
	// (no common-prefix folding), sorted lexicographically by full key.
	// startAfter restricts the page to keys strictly greater than the given
	// full key ("" for the first page) — pass the previous page's cursor
	// back to walk the prefix without gaps or duplicates. limit must be > 0
	// and bounds the page.
	//
	// It returns the page's entries (Name relative to prefix, the same
	// contract as ListObjectsDetailed) and the cursor for the next page —
	// the last key returned by the store, or "" when the listing is
	// exhausted.
	ListObjectsRecursive(ctx context.Context, prefix, startAfter string, limit int) ([]ObjectInfo, string, error)

	// PutObject writes data to the given key path.
	// Key is relative to the configured storage prefix.
	PutObject(ctx context.Context, key string, data []byte) error

	// PutObjectIfMatch writes data only when the object's current ETag
	// equals matchETag (conditional write). Returns ErrPreconditionFailed
	// (without writing) on mismatch. matchETag must be non-empty.
	PutObjectIfMatch(ctx context.Context, key string, data []byte, matchETag string) error

	// PutObjectIfMatchWithType is PutObjectIfMatch plus an explicit content
	// type. The file API writes deletion tombstones through it so a deleted
	// path stays recognizable without reading every object.
	PutObjectIfMatchWithType(ctx context.Context, key string, data []byte, contentType, matchETag string) error

	// PutObjectIfAbsent creates the object only when no object exists at
	// key. Returns ErrPreconditionFailed when the key already exists.
	// Used for the per-human quota ledger's atomic first initialization;
	// implementations must use a storage-level If-None-Match, not a
	// Stat-then-Put fallback.
	PutObjectIfAbsent(ctx context.Context, key string, data []byte) error

	// PutFile uploads a local file to the given key path.
	PutFile(ctx context.Context, localPath, key string) error

	// GetObject reads the object at the given key path.
	GetObject(ctx context.Context, key string) ([]byte, error)

	// Stat checks if an object exists. Returns os.ErrNotExist if not found.
	Stat(ctx context.Context, key string) error

	// StatMeta returns object metadata (size, modification time). Returns
	// os.ErrNotExist if the object does not exist. Used by W-PR-2 write
	// endpoints for mtime optimistic-locking (compare-before-write).
	StatMeta(ctx context.Context, key string) (ObjectMeta, error)

	// DeleteObject removes the object at key.
	DeleteObject(ctx context.Context, key string) error

	// Mirror synchronizes contents between src and dst paths.
	// Paths can be local directories or remote prefixes (with storage prefix).
	Mirror(ctx context.Context, src, dst string, opts MirrorOptions) error

	// ListObjects lists object names under a prefix.
	ListObjects(ctx context.Context, prefix string) ([]string, error)

	// DeletePrefix recursively removes all objects under the given prefix.
	DeletePrefix(ctx context.Context, prefix string) error
}

// PresignUploader is implemented by backends that can hand out a direct
// upload URL. Absence of the capability is a 500 fail-closed at the API.
type PresignUploader interface {
	PresignUpload(ctx context.Context, key string, maxBytes int64, contentType, sha256Hex string, ttl time.Duration) (method string, url string, fields map[string]string, err error)
}

// BucketManager is implemented by StorageClient backends that can create buckets.
// In incluster mode with external OSS, the bucket is pre-created, so this is a no-op.
type BucketManager interface {
	EnsureBucket(ctx context.Context) error
}

// StorageAdminClient handles user and policy management (embedded mode only).
// In incluster mode (cloud OSS), these operations are unnecessary — workers
// get scoped credentials via STS instead.
type StorageAdminClient interface {
	// EnsureUser creates a storage user or updates their password if they exist.
	EnsureUser(ctx context.Context, username, password string) error

	// EnsurePolicy creates a scoped access policy and attaches it to the user.
	// The policy restricts the user to their own agent prefix and shared data.
	EnsurePolicy(ctx context.Context, req PolicyRequest) error

	// DeleteUser removes a storage user and their associated policy.
	DeleteUser(ctx context.Context, username string) error
}
