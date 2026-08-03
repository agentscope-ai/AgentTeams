package oss

import "context"

// Config holds connection parameters for object storage.
type Config struct {
	MCBinary      string // mc binary path, default "mc"
	Alias         string // mc alias name, default "agentteams"
	Endpoint      string // MinIO endpoint URL, e.g. "http://minio:9000"
	AccessKey     string // MinIO root access key
	SecretKey     string // MinIO root secret key
	StoragePrefix string // full mc prefix, e.g. "agentteams/agentteams-storage"
	Bucket        string // bucket name for policy generation, e.g. "agentteams-storage"
}

// CredentialSource provides per-invocation credentials for object storage.
//
// When a MinIOClient is constructed with a non-nil CredentialSource, every
// mc invocation resolves the current credentials via Resolve and injects
// them into the process environment as MC_HOST_<alias>=<scheme>://<ak>:<sk>[:<token>]@<host>.
// The <host> component comes from MinIOClient.config.Endpoint, NOT from
// the CredentialSource — endpoint is a deployment-time static value and
// is kept orthogonal to the short-lived STS triple on purpose.
// This is the canonical way to drive mc against Alibaba Cloud OSS with
// refreshable STS tokens: the alias file on disk is never written and
// tokens are never cached beyond the TokenManager's own expiry.
type CredentialSource interface {
	Resolve(ctx context.Context) (Credentials, error)
}

// Credentials is the per-call credential bundle that CredentialSource returns.
// Endpoint is intentionally NOT carried here: the OSS endpoint is a static
// deployment config read from MinIOClient.config.Endpoint.
type Credentials struct {
	AccessKeyID     string
	AccessKeySecret string
	SecurityToken   string // optional
}

// MirrorOptions controls the behavior of Mirror operations.
type MirrorOptions struct {
	Overwrite bool     // overwrite existing files at destination
	Exclude   []string // file patterns to exclude (passed as --exclude flags to mc mirror)
}

// PolicyRequest describes a scoped access policy for a worker.
type PolicyRequest struct {
	WorkerName string // worker name (used as MinIO username and in path scoping)
	Bucket     string // bucket name, e.g. "agentteams-storage"
	TeamName   string // optional: grants additional access to teams/<teamName>/ prefix
	IsManager  bool   // when true, grants additional access to manager/ prefix for workspace sync
}
