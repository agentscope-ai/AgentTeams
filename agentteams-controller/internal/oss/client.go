package oss

import "context"

// StorageClient abstracts object storage operations.
// Implementations: MinIOClient (mc CLI), future S3Client (aws-sdk-go).
type StorageClient interface {
	// PutObject writes data to the given key path.
	// Key is relative to the configured storage prefix.
	PutObject(ctx context.Context, key string, data []byte) error

	// PutFile uploads a local file to the given key path.
	PutFile(ctx context.Context, localPath, key string) error

	// GetObject reads the object at the given key path.
	GetObject(ctx context.Context, key string) ([]byte, error)

	// Stat checks if an object exists. Returns os.ErrNotExist if not found.
	Stat(ctx context.Context, key string) error

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
