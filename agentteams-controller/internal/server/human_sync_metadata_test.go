package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Metadata fast path (zero GetObject) -------------------------------------

// TestHumanSyncStatusMetadataFastPath pins the point of the feature: the
// status read answers from exactly one StatMeta and never downloads the
// manifest body, with values equal to the body-parsed summary.
func TestHumanSyncStatusMetadataFastPath(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)
	body := syncManifestBody([]string{"files/a.txt", "files/sub/b.md"}, []string{"files/gone.txt"})
	rec := hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed write: %d %s", rec.Code, rec.Body.String())
	}
	updatedAt, _ := hfSyncJSON(t, rec)["updatedAt"].(string)
	if updatedAt == "" {
		t.Fatalf("seed response missing updatedAt: %s", rec.Body.String())
	}

	// The accepted write stored the summary in the object's user metadata.
	key := humanSyncManifestKey("alice")
	meta, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if got := meta.UserMetadata[syncMetaEntriesKey]; got != "2" {
		t.Fatalf("stored metadata entries = %q, want 2: %v", got, meta.UserMetadata)
	}
	if got := meta.UserMetadata[syncMetaTombstonesKey]; got != "1" {
		t.Fatalf("stored metadata tombstones = %q, want 1: %v", got, meta.UserMetadata)
	}
	if got := meta.UserMetadata[syncMetaGenerationKey]; got != "1" {
		t.Fatalf("stored metadata generation = %q, want 1: %v", got, meta.UserMetadata)
	}
	if got := meta.UserMetadata[syncMetaUpdatedAtKey]; got != updatedAt {
		t.Fatalf("stored metadata updatedAt = %q, want %q", got, updatedAt)
	}
	if got := meta.UserMetadata[syncMetaUpdatedByKey]; got != "alice" {
		t.Fatalf("stored metadata updatedBy = %q, want alice: %v", got, meta.UserMetadata)
	}

	// The status read: one StatMeta, zero GetObject, body-equal values.
	store.StatMetaCalls.Store(0)
	store.GetObjectCalls.Store(0)
	rec = hfGetSyncStatus(t, hfh, "alice", alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	st := hfSyncJSON(t, rec)
	if st["entries"] != float64(2) || st["tombstones"] != float64(1) || st["generation"] != float64(1) {
		t.Fatalf("status = %v, want entries=2 tombstones=1 generation=1", st)
	}
	if st["updatedAt"] != updatedAt || st["updatedBy"] != "alice" {
		t.Fatalf("status updatedAt/updatedBy = %v/%v, want %q/alice", st["updatedAt"], st["updatedBy"], updatedAt)
	}
	if got := store.GetObjectCalls.Load(); got != 0 {
		t.Fatalf("GetObject calls = %d, want 0 (the fast path must not read the body)", got)
	}
	if got := store.StatMetaCalls.Load(); got != 1 {
		t.Fatalf("StatMeta calls = %d, want 1 (one HEAD, no body read)", got)
	}
}

// --- Body fallbacks ------------------------------------------------------------

// TestHumanSyncStatusBodyFallbackWithoutMetadata pins the pre-metadata
// object: no user metadata on the stored object, so the status read falls
// back to the manifest body (a read, never a write-back).
func TestHumanSyncStatusBodyFallbackWithoutMetadata(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)
	body := syncManifestBody([]string{"files/a.txt"}, []string{"files/gone.txt", "files/x.md"})
	// A pre-metadata object: a plain write stores the body without user
	// metadata (S3 plain-PUT semantics).
	key := humanSyncManifestKey("alice")
	if err := store.PutObject(context.Background(), key, []byte(body)); err != nil {
		t.Fatal(err)
	}

	store.StatMetaCalls.Store(0)
	store.GetObjectCalls.Store(0)
	rec := hfGetSyncStatus(t, hfh, "alice", alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	st := hfSyncJSON(t, rec)
	// The body's own values (the sentinel generation 99 proves the body,
	// not a zero summary, answered).
	if st["entries"] != float64(1) || st["tombstones"] != float64(2) || st["generation"] != float64(99) {
		t.Fatalf("status = %v, want the body counts (1/2/99)", st)
	}
	if got := store.GetObjectCalls.Load(); got < 1 {
		t.Fatalf("GetObject calls = %d, want >= 1 (the fallback must read the body)", got)
	}
	// The read must not mutate the object: the ETag (and thus the metadata
	// state) is unchanged — no write-back.
	meta, _ := store.StatMeta(context.Background(), key)
	if meta.UserMetadata != nil {
		t.Fatalf("metadata after a status read = %v, want none (no write-back)", meta.UserMetadata)
	}
}

// TestHumanSyncStatusCorruptMetadataFallsBack pins that a missing, partial,
// or unparseable metadata set degrades to the body — never a 500.
func TestHumanSyncStatusCorruptMetadataFallsBack(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)
	body := syncManifestBody([]string{"files/a.txt"}, nil)
	key := humanSyncManifestKey("alice")
	full := map[string]string{
		syncMetaEntriesKey:    "1",
		syncMetaTombstonesKey: "0",
		syncMetaGenerationKey: "1",
		syncMetaUpdatedAtKey:  "2026-11-07T00:00:00Z",
		syncMetaUpdatedByKey:  "alice",
	}
	cases := []struct {
		name string
		meta map[string]string
	}{
		{"non-numeric counter", mutateMeta(full, syncMetaEntriesKey, "not-a-number")},
		{"negative counter", mutateMeta(full, syncMetaGenerationKey, "-1")},
		{"missing key", omitMeta(full, syncMetaUpdatedByKey)},
		{"empty value", mutateMeta(full, syncMetaTombstonesKey, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh object per case: the body is valid, the metadata is
			// the corrupt shape under test.
			if err := store.DeleteObject(context.Background(), key); err != nil {
				t.Fatal(err)
			}
			if err := store.PutObjectIfAbsentWithMetadata(context.Background(), key, []byte(body), "application/json", tc.meta); err != nil {
				t.Fatal(err)
			}
			rec := hfGetSyncStatus(t, hfh, "alice", alice)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200 (never a 500): %s", rec.Code, rec.Body.String())
			}
			st := hfSyncJSON(t, rec)
			if st["entries"] != float64(1) || st["tombstones"] != float64(0) || st["generation"] != float64(99) {
				t.Fatalf("status = %v, want the body counts (1/0/99)", st)
			}
		})
	}
}

// --- Write-side fallbacks -------------------------------------------------------

// TestHumanSyncMetadataWriteFailFallsBackToPlainWrite pins the write
// degradation: a metadata write the store rejects (non-conflict) still
// lands the manifest through the plain conditional write, and the status
// read falls back to the body.
func TestHumanSyncMetadataWriteFailFallsBackToPlainWrite(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)
	store.MetadataWritesFail.Store(true)
	defer store.MetadataWritesFail.Store(false)

	rec := hfPutSync(t, hfh, "alice", alice, syncManifestBody([]string{"files/a.txt"}, nil), map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("write with a failing metadata write: %d %s", rec.Code, rec.Body.String())
	}
	// The object landed through the plain write: body present, no metadata.
	key := humanSyncManifestKey("alice")
	meta, err := store.StatMeta(context.Background(), key)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if meta.UserMetadata != nil {
		t.Fatalf("metadata = %v, want none (plain write fallback)", meta.UserMetadata)
	}
	rec = hfGetSyncStatus(t, hfh, "alice", alice)
	st := hfSyncJSON(t, rec)
	if rec.Code != http.StatusOK || st["entries"] != float64(1) || st["generation"] != float64(1) {
		t.Fatalf("status = %d %v, want 200 with the body summary", rec.Code, st)
	}
}

// TestHumanSyncStatusUnsupportedMetadataCapability pins the capability
// degradation: a backend without ConditionalMetadataWriter (the NoPresign
// wrapper strips it on purpose) writes without metadata and answers the
// status from the body.
func TestHumanSyncStatusUnsupportedMetadataCapability(t *testing.T) {
	_, store, k8s := newHumanFilesRig(t)
	hfh := NewHumanFilesHandler(k8s, "default", ossfake.NewNoPresign(store), audit.NewClient(store))
	alice := hfSyncCaller(authpkg.CapabilityFileSync)

	rec := hfPutSync(t, hfh, "alice", alice, syncManifestBody([]string{"files/a.txt", "files/b.txt"}, nil), map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("first write: %d %s", rec.Code, rec.Body.String())
	}
	meta, err := store.StatMeta(context.Background(), humanSyncManifestKey("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.UserMetadata != nil {
		t.Fatalf("metadata = %v, want none (the backend does not advertise ConditionalMetadataWriter)", meta.UserMetadata)
	}
	rec = hfGetSyncStatus(t, hfh, "alice", alice)
	st := hfSyncJSON(t, rec)
	if rec.Code != http.StatusOK || st["entries"] != float64(2) || st["generation"] != float64(1) {
		t.Fatalf("status = %d %v, want the body fallback", rec.Code, st)
	}
}

// TestHumanSyncMetadataOverCapWritesPlain pins the 1 KiB cap: a summary
// that does not fit (here the updated-by value, a very long username)
// skips the metadata entirely — the write still succeeds and the status
// read falls back to the body. Never a 500.
func TestHumanSyncMetadataOverCapWritesPlain(t *testing.T) {
	hfh, store, k8s := newHumanFilesRig(t)
	ctx := context.Background()
	longUsername := "dave-" + strings.Repeat("x", 1500)
	dave := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: "Dave", Username: longUsername},
	}
	if err := k8s.Create(ctx, dave); err != nil {
		t.Fatal(err)
	}
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: longUsername, Capabilities: []string{string(authpkg.CapabilityFileSync)}}

	rec := hfPutSync(t, hfh, "dave", caller, syncManifestBody([]string{"files/a.txt"}, nil), map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("write with an over-cap summary: %d %s", rec.Code, rec.Body.String())
	}
	// Over-cap summary: stored without metadata, the write still landed.
	key := humanSyncManifestKey(longUsername)
	meta, err := store.StatMeta(ctx, key)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if meta.UserMetadata != nil {
		t.Fatalf("metadata = %v, want none (the summary exceeds the cap)", meta.UserMetadata)
	}
	rec = hfGetSyncStatus(t, hfh, "dave", caller)
	st := hfSyncJSON(t, rec)
	if rec.Code != http.StatusOK || st["entries"] != float64(1) || st["updatedBy"] != longUsername {
		t.Fatalf("status = %d %v, want the body fallback", rec.Code, st)
	}
}

// mutateMeta returns m with key set to value.
func mutateMeta(m map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}

// omitMeta returns m without key.
func omitMeta(m map[string]string, key string) map[string]string {
	out := make(map[string]string, len(m)-1)
	for k, v := range m {
		if k == key {
			continue
		}
		out[k] = v
	}
	return out
}
