package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// TestHumanFiles_S3Lifecycle exercises the tombstone contract against a real
// S3-compatible backend (MinIO or RustFS) instead of the in-memory store, so
// the `mc stat --json` metadata shape that carries the marker is covered:
// the in-memory store returns the field directly and cannot catch a parsing
// mismatch.
//
// Skipped unless HUMAN_FILES_S3_ENDPOINT is set. The CI job that runs it
// starts RustFS (our 1.2.6 default) and installs mc, which is the client this
// package shells out to.
func TestHumanFiles_S3Lifecycle(t *testing.T) {
	endpoint := os.Getenv("HUMAN_FILES_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set HUMAN_FILES_S3_ENDPOINT/HUMAN_FILES_S3_ACCESS_KEY/HUMAN_FILES_S3_SECRET_KEY/HUMAN_FILES_S3_BUCKET to run the native backend lifecycle")
	}
	access := os.Getenv("HUMAN_FILES_S3_ACCESS_KEY")
	secret := os.Getenv("HUMAN_FILES_S3_SECRET_KEY")
	bucket := os.Getenv("HUMAN_FILES_S3_BUCKET")
	if access == "" || secret == "" || bucket == "" {
		t.Fatal("HUMAN_FILES_S3_ACCESS_KEY, HUMAN_FILES_S3_SECRET_KEY and HUMAN_FILES_S3_BUCKET are required")
	}

	// StoragePrefix is "<mc alias>/<bucket>": fullPath() prefixes it, so the
	// first element must be the alias that ensureAlias registers.
	alias := "humanfilestest"
	client := oss.NewMinIOClient(oss.Config{
		Endpoint:      endpoint,
		AccessKey:     access,
		SecretKey:     secret,
		Alias:         alias,
		Bucket:        bucket,
		StoragePrefix: alias + "/" + bucket,
	})

	h, _, _ := newHumanFilesRig(t)
	h.oss = client
	ctx := context.Background()

	if rec := hfPutFile(t, h, "alice", "s3.txt", hfSelf(), "hello-s3"); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h, "alice", "other.txt", hfSelf(), "keep"); rec.Code != http.StatusOK {
		t.Fatalf("put other: %d %s", rec.Code, rec.Body.String())
	}

	// Soft delete: the live key becomes a tombstone with the marker stored as
	// an S3 Content-Type header; the read path must see it through mc.
	del := httptest.NewRecorder()
	h.deleteFile(del, hfReq(t, http.MethodDelete, "alice", "s3.txt", hfSelf(), nil))
	if del.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", del.Code, del.Body.String())
	}
	var deleted struct {
		TrashID string `json:"trashId"`
	}
	if err := json.Unmarshal(del.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("delete response: %v", err)
	}
	if deleted.TrashID == "" {
		t.Fatalf("delete response carries no trashId: %s", del.Body.String())
	}

	meta, err := client.StatMeta(ctx, humanFilesPrefix("alice")+"s3.txt")
	if err != nil {
		t.Fatalf("stat tombstone: %v", err)
	}
	if !isHumanFileTombstone(meta) {
		t.Fatalf("backend did not report the tombstone marker: %+v", meta)
	}

	get := httptest.NewRecorder()
	h.getFile(get, hfReq(t, http.MethodGet, "alice", "s3.txt", hfSelf(), nil))
	if get.Code != http.StatusNotFound {
		t.Fatalf("GET of a deleted path: got %d, want 404: %s", get.Code, get.Body.String())
	}

	// A legitimately empty file next to the tombstone must stay readable.
	if rec := hfPutFile(t, h, "alice", "empty.txt", hfSelf(), ""); rec.Code != http.StatusOK {
		t.Fatalf("put empty: %d %s", rec.Code, rec.Body.String())
	}
	emptyGet := httptest.NewRecorder()
	h.getFile(emptyGet, hfReq(t, http.MethodGet, "alice", "empty.txt", hfSelf(), nil))
	if emptyGet.Code != http.StatusOK || emptyGet.Body.Len() != 0 {
		t.Fatalf("GET empty file = %d %q, want 200 empty", emptyGet.Code, emptyGet.Body.String())
	}

	// Restore is an ordinary restore of the same trash entry; a tombstoned
	// target must be treated as absent, not as a path conflict.
	restore := httptest.NewRecorder()
	h.restoreTrash(restore, hfTrashReq(t, http.MethodPost, "alice", deleted.TrashID, hfSelf(), ""))
	if restore.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", restore.Code, restore.Body.String())
	}
	back := httptest.NewRecorder()
	h.getFile(back, hfReq(t, http.MethodGet, "alice", "s3.txt", hfSelf(), nil))
	if back.Code != http.StatusOK || back.Body.String() != "hello-s3" {
		t.Fatalf("GET after restore = %d %q", back.Code, back.Body.String())
	}
}
