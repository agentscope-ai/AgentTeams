package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
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

// TestHumanFiles_S3SpaceNamesQuota reproduces the maintainer's quota bypass
// against a real S3-compatible backend: the human-readable `mc ls` parse
// truncated object names at the first space, so the per-key stat the quota
// scan then targeted a key that does not exist and silently counted the
// file as zero bytes. The full name must survive the listing round-trip.
//
// Skipped unless HUMAN_FILES_S3_ENDPOINT is set (the same gate as
// TestHumanFiles_S3Lifecycle; the CI job selects it with -run
// 'TestHumanFiles_S3').
func TestHumanFiles_S3SpaceNamesQuota(t *testing.T) {
	endpoint := os.Getenv("HUMAN_FILES_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set HUMAN_FILES_S3_ENDPOINT/HUMAN_FILES_S3_ACCESS_KEY/HUMAN_FILES_S3_SECRET_KEY/HUMAN_FILES_S3_BUCKET to run the native backend space-name quota regression")
	}
	access := os.Getenv("HUMAN_FILES_S3_ACCESS_KEY")
	secret := os.Getenv("HUMAN_FILES_S3_SECRET_KEY")
	bucket := os.Getenv("HUMAN_FILES_S3_BUCKET")
	if access == "" || secret == "" || bucket == "" {
		t.Fatal("HUMAN_FILES_S3_ACCESS_KEY, HUMAN_FILES_S3_SECRET_KEY and HUMAN_FILES_S3_BUCKET are required")
	}

	alias := "humanfilestest"
	client := oss.NewMinIOClient(oss.Config{
		Endpoint:      endpoint,
		AccessKey:     access,
		SecretKey:     secret,
		Alias:         alias,
		Bucket:        bucket,
		StoragePrefix: alias + "/" + bucket,
	})
	ctx := context.Background()

	// The other S3 regressions share this bucket and human; start from a
	// clean area so the quota gate only sees this test's bytes, and leave
	// it clean.
	_ = client.DeletePrefix(ctx, humanFilesPrefix("alice"))
	_ = client.DeletePrefix(ctx, humanTrashPrefix("alice"))
	_ = client.DeleteObject(ctx, humanQuotaLedgerKey("alice"))
	defer func() {
		_ = client.DeletePrefix(ctx, humanFilesPrefix("alice"))
		_ = client.DeletePrefix(ctx, humanTrashPrefix("alice"))
		_ = client.DeleteObject(ctx, humanQuotaLedgerKey("alice"))
	}()

	h, _, _ := newHumanFilesRig(t)
	h.oss = client

	listRoot := func() humanFileListResponse {
		rec := httptest.NewRecorder()
		h.getFile(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("list root: %d %s", rec.Code, rec.Body.String())
		}
		var resp humanFileListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
	findEntry := func(resp humanFileListResponse, name string) *humanFileEntry {
		for i := range resp.Entries {
			if resp.Entries[i].Name == name {
				return &resp.Entries[i]
			}
		}
		return nil
	}
	usedNow := func() int64 {
		rec := hfGetQuota(t, h, "alice", hfSelf())
		if rec.Code != http.StatusOK {
			t.Fatalf("get quota: %d %s", rec.Code, rec.Body.String())
		}
		var q humanQuotaResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
			t.Fatal(err)
		}
		return q.UsedBytes
	}

	// Quota 10 bytes: the 6-byte space-bearing upload fits...
	if rec := hfPutQuota(t, h, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h, "alice", "meeting notes.txt", hfSelf(), "012345"); rec.Code != http.StatusOK {
		t.Fatalf("put space name: %d %s", rec.Code, rec.Body.String())
	}
	// ...and must come back under its full name with its real size (the
	// truncated parser reported "notes.txt" with size 0).
	entries := listRoot()
	if e := findEntry(entries, "meeting notes.txt"); e == nil || e.Type != "file" || e.Size != 6 {
		t.Fatalf("listing lacks the full space-bearing file: %+v", entries.Entries)
	}
	if e := findEntry(entries, "notes.txt"); e != nil {
		t.Fatalf("listing carries the truncated name: %+v", entries.Entries)
	}
	// ...and count toward the live usage.
	if used := usedNow(); used != 6 {
		t.Fatalf("live usage = %d, want 6", used)
	}
	// ...so the 10-byte upload no longer fits.
	if rec := hfPutFile(t, h, "alice", "big.txt", hfSelf(), "0123456789"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload against a full quota: got %d, want 413: %s", rec.Code, rec.Body.String())
	}

	// Control: a plain name runs the identical flow. The quota grows by
	// exactly the 6 bytes the control file needs, so both files coexist.
	if rec := hfPutQuota(t, h, "alice", hfAdmin(), `{"quotaBytes":12}`); rec.Code != http.StatusOK {
		t.Fatalf("raise quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h, "alice", "plain.txt", hfSelf(), "012345"); rec.Code != http.StatusOK {
		t.Fatalf("put plain name: %d %s", rec.Code, rec.Body.String())
	}
	if e := findEntry(listRoot(), "plain.txt"); e == nil || e.Size != 6 {
		t.Fatalf("listing lacks the plain file: %+v", listRoot().Entries)
	}
	if used := usedNow(); used != 12 {
		t.Fatalf("live usage after the control = %d, want 12", used)
	}
	if rec := hfPutFile(t, h, "alice", "big2.txt", hfSelf(), "0123456789"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("plain-name control: got %d, want 413: %s", rec.Code, rec.Body.String())
	}

	// A space-bearing directory name: the entry must keep its full name and
	// its bytes must be walked. The quota gets headroom so this test's
	// bytes are the only variable.
	if rec := hfPutQuota(t, h, "alice", hfAdmin(), `{"quotaBytes":100}`); rec.Code != http.StatusOK {
		t.Fatalf("raise quota for dir: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h, "alice", "my dir/inner.txt", hfSelf(), "01"); rec.Code != http.StatusOK {
		t.Fatalf("put nested file: %d %s", rec.Code, rec.Body.String())
	}
	entries = listRoot()
	if e := findEntry(entries, "my dir"); e == nil || e.Type != "directory" {
		t.Fatalf("listing lacks the space-bearing directory: %+v", entries.Entries)
	}
	if used := usedNow(); used != 14 {
		t.Fatalf("live usage = %d, want 14 (6 + 6 + 2)", used)
	}
}

// TestHumanFiles_S3TrashRestorePurgeQuotaRace runs the trash restore/purge
// quota race against a real S3-compatible backend: the same sequence as
// TestHumanFiles_TrashRestorePurgeQuotaRace, with the stall pass-through
// wrapping the real client so the lease protocol runs on MinIO/RustFS
// conditional writes instead of the in-memory store.
//
// Skipped unless HUMAN_FILES_S3_ENDPOINT is set (the same gate as
// TestHumanFiles_S3Lifecycle; the CI job selects both with -run
// 'TestHumanFiles_S3').
func TestHumanFiles_S3TrashRestorePurgeQuotaRace(t *testing.T) {
	endpoint := os.Getenv("HUMAN_FILES_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set HUMAN_FILES_S3_ENDPOINT/HUMAN_FILES_S3_ACCESS_KEY/HUMAN_FILES_S3_SECRET_KEY/HUMAN_FILES_S3_BUCKET to run the native backend restore/purge quota race")
	}
	access := os.Getenv("HUMAN_FILES_S3_ACCESS_KEY")
	secret := os.Getenv("HUMAN_FILES_S3_SECRET_KEY")
	bucket := os.Getenv("HUMAN_FILES_S3_BUCKET")
	if access == "" || secret == "" || bucket == "" {
		t.Fatal("HUMAN_FILES_S3_ACCESS_KEY, HUMAN_FILES_S3_SECRET_KEY and HUMAN_FILES_S3_BUCKET are required")
	}

	alias := "humanfilestest"
	client := oss.NewMinIOClient(oss.Config{
		Endpoint:      endpoint,
		AccessKey:     access,
		SecretKey:     secret,
		Alias:         alias,
		Bucket:        bucket,
		StoragePrefix: alias + "/" + bucket,
	})
	ctx := context.Background()

	// The lifecycle regression shares this bucket and human and runs first in
	// the same `go test` invocation; it leaves files/ and .trash/ objects
	// behind. Start from a clean area so the quota gate only sees this test's
	// bytes (the in-memory variant starts empty).
	_ = client.DeletePrefix(ctx, humanFilesPrefix("alice"))
	_ = client.DeletePrefix(ctx, humanTrashPrefix("alice"))
	_ = client.DeleteObject(ctx, humanQuotaLedgerKey("alice"))

	stalled := &stallRestoreTargetOSS{
		inner:   client,
		key:     humanPathLockKey("alice", "f.txt"),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h, _, _ := newHumanFilesRig(t)
	h.oss = stalled

	// Quota 10 bytes; the 6-byte seed file leaves 4 bytes of headroom.
	if rec := hfPutQuota(t, h, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h, "alice", "f.txt", hfSelf(), "012345"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	del := httptest.NewRecorder()
	h.deleteFile(del, hfReq(t, http.MethodDelete, "alice", "f.txt", hfSelf(), nil))
	if del.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", del.Code, del.Body.String())
	}
	var delResp struct {
		TrashID string `json:"trashId"`
	}
	if err := json.Unmarshal(del.Body.Bytes(), &delResp); err != nil {
		t.Fatal(err)
	}

	// Start the restore: it stops once it has the entry lease and the
	// entry's bytes, at the moment it requests the target path lease.
	stalled.armed = true
	type restoreResult struct {
		code int
		body string
	}
	restore := make(chan restoreResult, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", delResp.TrashID, hfSelf(), ""))
		restore <- restoreResult{rec.Code, rec.Body.String()}
	}()
	select {
	case <-stalled.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("restore never requested the target path lease")
	}

	// Purge in the window: the entry is being restored, so it must be
	// rejected instead of freeing the bytes the restore will land.
	purge := httptest.NewRecorder()
	h.purgeTrash(purge, hfTrashReq(t, http.MethodDelete, "alice", delResp.TrashID, hfAdmin(), ""))
	if purge.Code != http.StatusConflict {
		t.Fatalf("purge during restore: got %d, want 409: %s", purge.Code, purge.Body.String())
	}
	var coded humanFileCodedError
	if err := json.Unmarshal(purge.Body.Bytes(), &coded); err != nil {
		t.Fatal(err)
	}
	if coded.Code != "file_busy" {
		t.Fatalf("purge code = %q, want file_busy", coded.Code)
	}

	// A 10-byte upload fits only if the purge freed the trashed 6 bytes;
	// while the entry is counted it must be rejected.
	if rec := hfPutFile(t, h, "alice", "g.txt", hfSelf(), "0123456789"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload in the window: got %d, want 413: %s", rec.Code, rec.Body.String())
	}

	close(stalled.release)
	select {
	case res := <-restore:
		if res.code != http.StatusOK {
			t.Fatalf("restore: got %d, want 200: %s", res.code, res.body)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("restore did not finish after release")
	}

	// Terminal state: the restored 6 bytes are the entire usage, the ledger
	// agrees, and the entry is gone — the pre-fix terminal state was 16
	// used against a 10-byte quota with all three requests green.
	get := httptest.NewRecorder()
	h.getFile(get, hfReq(t, http.MethodGet, "alice", "f.txt", hfSelf(), nil))
	if get.Code != http.StatusOK || get.Body.String() != "012345" {
		t.Fatalf("restored file: %d %q", get.Code, get.Body.String())
	}
	rec := hfGetQuota(t, h, "alice", hfSelf())
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 6 {
		t.Fatalf("live usage = %d, want 6", q.UsedBytes)
	}
	ledger, _, err := h.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ledger.UsedBytes != 6 {
		t.Fatalf("ledger usage = %d, want 6", ledger.UsedBytes)
	}
	list := httptest.NewRecorder()
	h.listTrash(list, hfTrashReq(t, http.MethodGet, "alice", "", hfSelf(), ""))
	if list.Code != http.StatusOK {
		t.Fatalf("list trash: %d %s", list.Code, list.Body.String())
	}
	var trashResp struct {
		Entries []trashEntry `json:"entries"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &trashResp); err != nil {
		t.Fatal(err)
	}
	if len(trashResp.Entries) != 0 {
		t.Fatalf("trash entry survived restore: %+v", trashResp.Entries)
	}
}

// TestHumanFiles_S3PurgeAfterLeaseExpiryQuotaRace runs the stale-purge
// quota bypass after lease expiry against a real S3-compatible backend:
// the same sequence as TestHumanFiles_TrashPurgeAfterLeaseExpiryQuotaRace,
// with the stall pass-through wrapping the real client so the lease
// protocol, the unconditional DeletePrefix and the conservative ledger
// accounting all run on MinIO/RustFS instead of the in-memory store.
//
// Skipped unless HUMAN_FILES_S3_ENDPOINT is set (the same gate as the
// other S3 tests; the CI job selects it with -run 'TestHumanFiles_S3').
func TestHumanFiles_S3PurgeAfterLeaseExpiryQuotaRace(t *testing.T) {
	endpoint := os.Getenv("HUMAN_FILES_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set HUMAN_FILES_S3_ENDPOINT/HUMAN_FILES_S3_ACCESS_KEY/HUMAN_FILES_S3_SECRET_KEY/HUMAN_FILES_S3_BUCKET to run the native backend stale-purge race")
	}
	access := os.Getenv("HUMAN_FILES_S3_ACCESS_KEY")
	secret := os.Getenv("HUMAN_FILES_S3_SECRET_KEY")
	bucket := os.Getenv("HUMAN_FILES_S3_BUCKET")
	if access == "" || secret == "" || bucket == "" {
		t.Fatal("HUMAN_FILES_S3_ACCESS_KEY, HUMAN_FILES_S3_SECRET_KEY and HUMAN_FILES_S3_BUCKET are required")
	}

	alias := "humanfilestest"
	client := oss.NewMinIOClient(oss.Config{
		Endpoint:      endpoint,
		AccessKey:     access,
		SecretKey:     secret,
		Alias:         alias,
		Bucket:        bucket,
		StoragePrefix: alias + "/" + bucket,
	})
	ctx := context.Background()

	// The lifecycle regression and the round-8 race share this bucket and
	// human and leave objects behind; start from a clean area so the quota
	// gate only sees this test's bytes (the in-memory variant starts empty).
	_ = client.DeletePrefix(ctx, humanFilesPrefix("alice"))
	_ = client.DeletePrefix(ctx, humanTrashPrefix("alice"))
	_ = client.DeleteObject(ctx, humanQuotaLedgerKey("alice"))

	stalled := &stallPurgeContentStatOSS{
		inner:   client,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	_, _, k8s := newHumanFilesRig(t)
	h1 := NewHumanFilesHandler(k8s, "default", stalled, audit.NewClient(client))
	h1.pathLockTTL = 2 * time.Second // real, short TTL for the test
	h2 := NewHumanFilesHandler(k8s, "default", client, audit.NewClient(client))

	// Quota 10 bytes; the 6-byte seed file leaves 4 bytes of headroom.
	if rec := hfPutQuota(t, h1, "alice", hfAdmin(), `{"quotaBytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("put quota: %d %s", rec.Code, rec.Body.String())
	}
	if rec := hfPutFile(t, h1, "alice", "f.txt", hfSelf(), "012345"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	del := httptest.NewRecorder()
	h1.deleteFile(del, hfReq(t, http.MethodDelete, "alice", "f.txt", hfSelf(), nil))
	if del.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", del.Code, del.Body.String())
	}
	var delResp struct {
		TrashID string `json:"trashId"`
	}
	if err := json.Unmarshal(del.Body.Bytes(), &delResp); err != nil {
		t.Fatal(err)
	}

	// Start the purge: it stops on the second stat of the entry's content,
	// at its commit boundary — after the meta read (size captured), before
	// the DeletePrefix (unfixed) / before the fence (fixed).
	stalled.key = humanTrashPrefix("alice") + delResp.TrashID + "/content"
	stalled.armed = true
	type purgeResult struct {
		code int
		body string
	}
	purge := make(chan purgeResult, 1)
	go func() {
		rec := httptest.NewRecorder()
		h1.purgeTrash(rec, hfTrashReq(t, http.MethodDelete, "alice", delResp.TrashID, hfAdmin(), ""))
		purge <- purgeResult{rec.Code, rec.Body.String()}
	}()
	select {
	case <-stalled.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("purge never reached its commit boundary")
	}

	// Let the purge's entry lease expire for real, then restore the entry
	// from a second handler: it takes over the expired lease, lands the
	// bytes in files/ and removes the entry.
	time.Sleep(2200 * time.Millisecond)
	restore := httptest.NewRecorder()
	h2.restoreTrash(restore, hfTrashReq(t, http.MethodPost, "alice", delResp.TrashID, hfSelf(), ""))
	if restore.Code != http.StatusOK {
		t.Fatalf("restore after lease expiry: got %d, want 200: %s", restore.Code, restore.Body.String())
	}

	// Release the stale purge: it must abort at the fence without
	// deleting or deducting.
	close(stalled.release)
	var res purgeResult
	select {
	case res = <-purge:
	case <-time.After(60 * time.Second):
		t.Fatal("stale purge did not finish after release")
	}
	if res.code != http.StatusConflict {
		t.Fatalf("stale purge resume: got %d, want 409 lease_lost: %s", res.code, res.body)
	}
	var coded humanFileCodedError
	if err := json.Unmarshal([]byte(res.body), &coded); err != nil {
		t.Fatal(err)
	}
	if coded.Code != "lease_lost" {
		t.Fatalf("stale purge code = %q, want lease_lost", coded.Code)
	}

	// Terminal state: the restored 6 bytes are the entire usage, the
	// ledger agrees, and a 10-byte upload must not fit — the unfixed
	// terminal state is 16 used against a 10-byte quota with all three
	// requests green.
	get := httptest.NewRecorder()
	h1.getFile(get, hfReq(t, http.MethodGet, "alice", "f.txt", hfSelf(), nil))
	if get.Code != http.StatusOK || get.Body.String() != "012345" {
		t.Fatalf("restored file: %d %q", get.Code, get.Body.String())
	}
	rec := hfGetQuota(t, h1, "alice", hfSelf())
	var q humanQuotaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatal(err)
	}
	if q.UsedBytes != 6 {
		t.Fatalf("live usage = %d, want 6", q.UsedBytes)
	}
	ledger, _, err := h1.readQuotaLedger(ctx, "alice")
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ledger.UsedBytes != 6 {
		t.Fatalf("ledger usage = %d, want 6", ledger.UsedBytes)
	}
	if rec := hfPutFile(t, h1, "alice", "g.txt", hfSelf(), "0123456789"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload after the stale purge: got %d, want 413: %s", rec.Code, rec.Body.String())
	}
}
