package oss

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// tombstoneContentType mirrors the file API's marker. It is duplicated here
// because internal/server imports this package.
const tombstoneContentType = "application/vnd.agentteams.tombstone"

// TestOSSFallback_KeepsContentType guards the 501 fallback contract: the
// provider path that cannot do If-Match must still write the requested media
// type, otherwise a tombstone becomes an ordinary empty object.
func TestOSSFallback_KeepsContentType(t *testing.T) {
	var gotContentType string
	var gotData []byte
	err := ossFallbackWrite(
		t.Context(), "humans/u/files/a.txt", nil, "etag-read", tombstoneContentType,
		func(context.Context, string) (ObjectMeta, error) { return ObjectMeta{ETag: "etag-read"}, nil },
		func(_ context.Context, _ string, data []byte, contentType string) error {
			gotData, gotContentType = data, contentType
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotContentType != tombstoneContentType {
		t.Fatalf("fallback wrote contentType=%q, want %q", gotContentType, tombstoneContentType)
	}
	if len(gotData) != 0 {
		t.Fatalf("fallback data = %q, want empty tombstone body", gotData)
	}
}

// TestS3FallbackLifecycle forces the documented NotImplemented fallback
// against a real S3-compatible backend (MinIO or RustFS) and the real mc
// client: the conditional write must be retried as a plain typed write, and
// the marker must survive in the backend's metadata. It also checks that a
// stale ETag is still rejected instead of overwriting.
//
// Skipped unless HUMAN_FILES_S3_ENDPOINT is set.
func TestS3FallbackLifecycle(t *testing.T) {
	endpoint := os.Getenv("HUMAN_FILES_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set HUMAN_FILES_S3_* to run the native fallback lifecycle")
	}
	access, secret, bucket := os.Getenv("HUMAN_FILES_S3_ACCESS_KEY"), os.Getenv("HUMAN_FILES_S3_SECRET_KEY"), os.Getenv("HUMAN_FILES_S3_BUCKET")
	if access == "" || secret == "" || bucket == "" {
		t.Fatal("HUMAN_FILES_S3_ACCESS_KEY, HUMAN_FILES_S3_SECRET_KEY and HUMAN_FILES_S3_BUCKET are required")
	}
	alias := "fallbacktest"
	c := NewMinIOClient(Config{
		Endpoint: endpoint, AccessKey: access, SecretKey: secret,
		Alias: alias, Bucket: bucket, StoragePrefix: alias + "/" + bucket,
	})
	ctx := context.Background()
	key := "humans/alice/files/fallback.txt"

	if err := c.PutObjectWithType(ctx, key, []byte("A"), "text/plain"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	meta, err := c.StatMeta(ctx, key)
	if err != nil || meta.ETag == "" {
		t.Fatalf("stat seed: meta=%+v err=%v", meta, err)
	}

	// Only conditional PUTs fail, as on a provider without If-Match.
	c.sdkPutIfMatch = func(context.Context, string, []byte, string) error {
		return minio.ErrorResponse{Code: "NotImplemented", StatusCode: 501}
	}

	if err := c.PutObjectIfMatchWithType(ctx, key, nil, tombstoneContentType, meta.ETag); err != nil {
		t.Fatalf("conditional write through the fallback: %v", err)
	}
	got, err := c.StatMeta(ctx, key)
	if err != nil {
		t.Fatalf("stat tombstone: %v", err)
	}
	if !strings.EqualFold(got.ContentType, tombstoneContentType) {
		t.Fatalf("backend content type = %q, want %q (the fallback dropped the marker)", got.ContentType, tombstoneContentType)
	}

	// A stale ETag must still be rejected by the fallback's re-check.
	stale := "00000000000000000000000000000000"
	if err := c.PutObjectIfMatchWithType(ctx, key, nil, tombstoneContentType, stale); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale etag through the fallback: err=%v, want ErrPreconditionFailed", err)
	}
}
