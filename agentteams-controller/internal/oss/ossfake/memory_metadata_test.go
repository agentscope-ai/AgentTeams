package ossfake

import (
	"context"
	"errors"
	"maps"
	"os"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

const testManifestKey = "humans/alice/.sync/manifest.json"

// testSyncMetadata is the shape the sync-status fast path relies on: five
// summary keys, mixed input casing and prefixing on the way in.
func testSyncMetadata() map[string]string {
	return map[string]string{
		"x-amz-meta-AGENTTEAMS-SYNC-ENTRIES":   "2",
		"AgentTeams-Sync-Tombstones":           "1",
		"agentteams-sync-generation":           "3",
		"agentteams-sync-updated-at":           "2026-11-07T00:00:00Z",
		"X-Amz-Meta-Agentteams-Sync-Updated-By": "alice",
	}
}

func wantTestSyncMetadata() map[string]string {
	return map[string]string{
		"agentteams-sync-entries":    "2",
		"agentteams-sync-tombstones": "1",
		"agentteams-sync-generation": "3",
		"agentteams-sync-updated-at": "2026-11-07T00:00:00Z",
		"agentteams-sync-updated-by": "alice",
	}
}

// TestMemoryPutWithMetadataRoundTrip pins the metadata round trip: a
// conditional put with metadata stores it under normalized bare lowercase
// keys, and StatMeta reads it back verbatim.
func TestMemoryPutWithMetadataRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	if err := m.PutObjectIfAbsentWithMetadata(ctx, testManifestKey, []byte(`{"v":1}`), "application/json", testSyncMetadata()); err != nil {
		t.Fatalf("create with metadata: %v", err)
	}
	meta, err := m.StatMeta(ctx, testManifestKey)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !maps.Equal(meta.UserMetadata, wantTestSyncMetadata()) {
		t.Fatalf("UserMetadata = %v, want %v", meta.UserMetadata, wantTestSyncMetadata())
	}
	if meta.ContentType != "application/json" {
		t.Fatalf("ContentType = %q, want application/json", meta.ContentType)
	}

	// A bump write through the If-Match path replaces the metadata too.
	etag := meta.ETag
	if err := m.PutObjectIfMatchWithMetadata(ctx, testManifestKey, []byte(`{"v":2}`), "application/json", etag,
		map[string]string{"agentteams-sync-generation": "4"}); err != nil {
		t.Fatalf("bump with metadata: %v", err)
	}
	meta, err = m.StatMeta(ctx, testManifestKey)
	if err != nil {
		t.Fatalf("stat after bump: %v", err)
	}
	if !maps.Equal(meta.UserMetadata, map[string]string{"agentteams-sync-generation": "4"}) {
		t.Fatalf("UserMetadata after bump = %v, want only the new keys", meta.UserMetadata)
	}
	if meta.ETag == etag {
		t.Fatal("ETag did not change after a content-changing write")
	}
}

// TestMemoryPutWithMetadataConditionalSemantics pins the S3 semantics: a
// failed precondition leaves the object AND its metadata untouched; an
// existing key fails the create-if-absent.
func TestMemoryPutWithMetadataConditionalSemantics(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	body := []byte(`{"v":1}`)
	if err := m.PutObjectIfAbsentWithMetadata(ctx, testManifestKey, body, "application/json", testSyncMetadata()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Stale If-Match: no write, metadata intact.
	if err := m.PutObjectIfMatchWithMetadata(ctx, testManifestKey, []byte(`{"v":9}`), "application/json", "stale-etag",
		map[string]string{"agentteams-sync-entries": "99"}); !errorsPrecondition(err) {
		t.Fatalf("stale If-Match err = %v, want ErrPreconditionFailed", err)
	}
	data, err := m.GetObject(ctx, testManifestKey)
	if err != nil || string(data) != string(body) {
		t.Fatalf("object after failed If-Match = %q (%v), want unchanged", data, err)
	}
	meta, _ := m.StatMeta(ctx, testManifestKey)
	if !maps.Equal(meta.UserMetadata, wantTestSyncMetadata()) {
		t.Fatalf("metadata after failed If-Match = %v, want unchanged", meta.UserMetadata)
	}

	// If-Match against a missing key also fails.
	if err := m.PutObjectIfMatchWithMetadata(ctx, "missing/key.json", body, "application/json", "etag",
		map[string]string{"agentteams-sync-entries": "1"}); !errorsPrecondition(err) {
		t.Fatalf("If-Match on missing key err = %v, want ErrPreconditionFailed", err)
	}

	// If-None-Match against an existing key fails, metadata intact.
	if err := m.PutObjectIfAbsentWithMetadata(ctx, testManifestKey, body, "application/json",
		map[string]string{"agentteams-sync-entries": "99"}); !errorsPrecondition(err) {
		t.Fatalf("If-None-Match on existing key err = %v, want ErrPreconditionFailed", err)
	}
	meta, _ = m.StatMeta(ctx, testManifestKey)
	if !maps.Equal(meta.UserMetadata, wantTestSyncMetadata()) {
		t.Fatalf("metadata after failed If-None-Match = %v, want unchanged", meta.UserMetadata)
	}
}

// TestMemoryPlainWriteClearsMetadata pins the S3 plain-PUT semantics the
// fake mirrors: a conditional write without metadata headers replaces the
// object and clears its user metadata (the self-heal contract: the next
// in-protocol write re-stores it).
func TestMemoryPlainWriteClearsMetadata(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.PutObjectIfAbsentWithMetadata(ctx, testManifestKey, []byte(`{"v":1}`), "application/json", testSyncMetadata()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	meta, _ := m.StatMeta(ctx, testManifestKey)
	if err := m.PutObjectIfMatch(ctx, testManifestKey, []byte(`{"v":2}`), meta.ETag); err != nil {
		t.Fatalf("plain If-Match: %v", err)
	}
	meta, _ = m.StatMeta(ctx, testManifestKey)
	if meta.UserMetadata != nil {
		t.Fatalf("UserMetadata = %v, want none after a plain write", meta.UserMetadata)
	}
}

// TestMemoryMetadataWritesFailModelsRejectedMetadata pins the fault
// injection: the WithMetadata writes fail with a non-conflict error and
// the object is left untouched.
func TestMemoryMetadataWritesFailModelsRejectedMetadata(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.PutObjectIfAbsentWithMetadata(ctx, testManifestKey, []byte(`{"v":1}`), "application/json", testSyncMetadata()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m.MetadataWritesFail.Store(true)
	defer m.MetadataWritesFail.Store(false)

	meta, _ := m.StatMeta(ctx, testManifestKey)
	err := m.PutObjectIfMatchWithMetadata(ctx, testManifestKey, []byte(`{"v":2}`), "application/json", meta.ETag,
		map[string]string{"agentteams-sync-generation": "4"})
	if err == nil || errorsPrecondition(err) {
		t.Fatalf("err = %v, want a non-nil non-conflict error", err)
	}
	meta, _ = m.StatMeta(ctx, testManifestKey)
	if !maps.Equal(meta.UserMetadata, wantTestSyncMetadata()) {
		t.Fatalf("metadata = %v, want unchanged after the rejected write", meta.UserMetadata)
	}
}

// TestMemoryMetadataEdgeCases covers the empty/nil and missing-key edges.
func TestMemoryMetadataEdgeCases(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	// A create with no metadata stores nothing to read back.
	if err := m.PutObjectIfAbsentWithMetadata(ctx, testManifestKey, []byte(`{}`), "application/json", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	meta, err := m.StatMeta(ctx, testManifestKey)
	if err != nil {
		t.Fatal(err)
	}
	if meta.UserMetadata != nil {
		t.Fatalf("UserMetadata = %v, want nil for a metadata-less object", meta.UserMetadata)
	}

	// A metadata-only map (empty values) is dropped, not stored.
	etag := meta.ETag
	if err := m.PutObjectIfMatchWithMetadata(ctx, testManifestKey, []byte(`{}`), "application/json", etag,
		map[string]string{"agentteams-sync-entries": ""}); err != nil {
		t.Fatalf("bump: %v", err)
	}
	meta, _ = m.StatMeta(ctx, testManifestKey)
	if meta.UserMetadata != nil {
		t.Fatalf("UserMetadata = %v, want nil after a fully-dropped map", meta.UserMetadata)
	}

	// A missing key has no metadata to read.
	if _, err := m.StatMeta(ctx, "missing/key.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("StatMeta missing key err = %v, want not-exist", err)
	}
}

// errorsPrecondition reports the S3 precondition failure sentinel.
func errorsPrecondition(err error) bool {
	return errors.Is(err, oss.ErrPreconditionFailed)
}
