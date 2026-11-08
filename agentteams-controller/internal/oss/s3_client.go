package oss

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Client is the lazy, concurrency-safe cache of the minio-go client for
// the internal endpoint. The hot path must never pay per request for a
// client (the mc era paid a process spawn, a temp file and extra copies per
// call); the client is built once and reused. It is rebuilt only when the
// resolved credential triple actually changes, which is how a dynamic STS
// token rotation takes effect: every request resolves the current triple,
// and a changed triple means a new signing identity.
type s3Client struct {
	mu     sync.Mutex
	client *minio.Client
	// The credential triple the cached client was built with.
	accessKey, secretKey, sessionToken string
}

// get returns the cached client, building it on first use or after a
// credential change. A build failure is returned without poisoning the
// cache, so the next call retries.
func (s *s3Client) get(endpoint, accessKey, secretKey, sessionToken string) (*minio.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil && s.accessKey == accessKey && s.secretKey == secretKey && s.sessionToken == sessionToken {
		return s.client, nil
	}
	client, err := buildS3Client(endpoint, accessKey, secretKey, sessionToken)
	if err != nil {
		return nil, err
	}
	s.client = client
	s.accessKey, s.secretKey, s.sessionToken = accessKey, secretKey, sessionToken
	return client, nil
}

// buildS3Client creates a minio-go client for endpoint with the given
// credential triple. The bucket region is not pinned: the SDK resolves it
// once per client instance with a GET /<bucket>?location= (cached in the
// client), the same one-time cost the conditional-write path already paid.
func buildS3Client(endpoint, accessKey, secretKey, sessionToken string) (*minio.Client, error) {
	host, secure := splitEndpoint(endpoint)
	return minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, sessionToken),
		Secure: secure,
	})
}

// splitEndpoint keeps the endpoint scheme only for TLS selection, exactly
// like the pre-cache sdkClient did: https:// → secure, http:// or a bare
// hostname → plain. The host (with optional port) is what the SDK dials.
func splitEndpoint(endpoint string) (host string, secure bool) {
	secure = strings.HasPrefix(endpoint, "https://")
	host = strings.TrimPrefix(endpoint, "https://")
	host = strings.TrimPrefix(host, "http://")
	return host, secure
}

// resolveCredentials returns the credential triple for one request: a
// dynamic source wins over the static config pair (external OSS /
// refreshable STS tokens). A resolution failure is returned, never silently
// replaced by the static pair (which would be the wrong identity for
// STS-based deployments) — the same contract the mc path and the old
// per-request sdkClient had.
func (c *MinIOClient) resolveCredentials(ctx context.Context) (Credentials, error) {
	if c.credSource != nil {
		creds, err := c.credSource.Resolve(ctx)
		if err != nil {
			return Credentials{}, fmt.Errorf("resolve dynamic oss credentials: %w", err)
		}
		return creds, nil
	}
	return Credentials{
		AccessKeyID:     c.config.AccessKey,
		AccessKeySecret: c.config.SecretKey,
	}, nil
}

// sdkClient returns the cached minio-go client for the internal endpoint.
// The current credential triple is resolved on every call so an STS token
// rotation is observed immediately; the client itself is only rebuilt when
// that triple changes (s3Client).
func (c *MinIOClient) sdkClient() (*minio.Client, error) {
	creds, err := c.resolveCredentials(context.Background())
	if err != nil {
		return nil, err
	}
	return c.sdk.get(c.config.Endpoint, creds.AccessKeyID, creds.AccessKeySecret, creds.SecurityToken)
}

// newPublicSDKClient builds a one-shot minio-go client for the caller-
// reachable public endpoint with the credential triple resolved at that
// moment. PresignUpload must sign each issuance with the identity it
// resolves at issuance (an STS-rotated client must not keep signing as a
// previous temporary identity), and a cached public client would also leak
// the one-time signed location lookup of its first issuer. Presign is a
// low-frequency management operation, so a fresh client per issuance is
// fine — the hot path never goes through it.
func (c *MinIOClient) newPublicSDKClient() (*minio.Client, error) {
	creds, err := c.resolveCredentials(context.Background())
	if err != nil {
		return nil, err
	}
	return buildS3Client(c.config.PublicEndpoint, creds.AccessKeyID, creds.AccessKeySecret, creds.SecurityToken)
}
