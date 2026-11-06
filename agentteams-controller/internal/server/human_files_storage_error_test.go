package server

// PR-0: a store that does not answer is 503 storage_unavailable at every
// human-files surface — reads, listings, writes, delete/restore/purge,
// presign, quota, and the sync manifest — while the semantics the mapping
// must never disturb (genuine absence 404, tombstone 404, anti-probing
// 404, If-Match 412) stay put. The fault is exercised at two layers:
// across the real MinIOClient against a fake S3 wire (the SDK's error
// shapes, the transport faults) and through injected method faults on the
// in-memory fake (one storage operation per endpoint).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

// assertStorageUnavailable pins the D1 contract: 503 with the stable
// machine-readable body, and explicitly not the 404 (absence) or 500
// (server fault) the same fault used to surface as.
func assertStorageUnavailable(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("%s: status = %d, want 503 storage_unavailable: %s", what, rec.Code, rec.Body.String())
	}
	var body storageUnavailableBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body is not the JSON error: %v: %s", what, err, rec.Body.String())
	}
	if body.Error != "storage_unavailable" {
		t.Fatalf("%s: body = %s, want {\"error\":\"storage_unavailable\"}", what, rec.Body.String())
	}
}

// faultOSS embeds the in-memory fake and fails the selected StorageClient
// methods with a wrapped oss.ErrStorageUnavailable for keys (or list
// prefixes) under prefix ("" = every key); everything else delegates. It
// stands in for a store that is down at exactly the layer each endpoint
// drives.
type faultOSS struct {
	*ossfake.Memory
	prefix  string
	methods map[string]bool
}

func newFaultOSS(t *testing.T, store *ossfake.Memory, prefix string, methods ...string) *faultOSS {
	t.Helper()
	f := &faultOSS{Memory: store, prefix: prefix, methods: make(map[string]bool, len(methods))}
	for _, m := range methods {
		f.methods[m] = true
	}
	return f
}

// down reports whether the fault fires for this key (list methods pass
// their prefix, which carries the same area shape).
func (f *faultOSS) down(key, method string) bool {
	return f.methods[method] && (f.prefix == "" || strings.HasPrefix(key, f.prefix))
}

func (f *faultOSS) fault(key, method string) error {
	if f.down(key, method) {
		return fmt.Errorf("store down (%s %s): %w", method, key, oss.ErrStorageUnavailable)
	}
	return nil
}

func (f *faultOSS) Stat(ctx context.Context, key string) error {
	if err := f.fault(key, "Stat"); err != nil {
		return err
	}
	return f.Memory.Stat(ctx, key)
}

func (f *faultOSS) StatMeta(ctx context.Context, key string) (oss.ObjectMeta, error) {
	if err := f.fault(key, "StatMeta"); err != nil {
		return oss.ObjectMeta{}, err
	}
	return f.Memory.StatMeta(ctx, key)
}

func (f *faultOSS) GetObject(ctx context.Context, key string) ([]byte, error) {
	if err := f.fault(key, "GetObject"); err != nil {
		return nil, err
	}
	return f.Memory.GetObject(ctx, key)
}

func (f *faultOSS) PutObject(ctx context.Context, key string, data []byte) error {
	if err := f.fault(key, "PutObject"); err != nil {
		return err
	}
	return f.Memory.PutObject(ctx, key, data)
}

func (f *faultOSS) PutObjectIfMatch(ctx context.Context, key string, data []byte, matchETag string) error {
	if err := f.fault(key, "PutObjectIfMatch"); err != nil {
		return err
	}
	return f.Memory.PutObjectIfMatch(ctx, key, data, matchETag)
}

func (f *faultOSS) PutObjectIfMatchWithType(ctx context.Context, key string, data []byte, contentType, matchETag string) error {
	if err := f.fault(key, "PutObjectIfMatchWithType"); err != nil {
		return err
	}
	return f.Memory.PutObjectIfMatchWithType(ctx, key, data, contentType, matchETag)
}

func (f *faultOSS) PutObjectIfAbsent(ctx context.Context, key string, data []byte) error {
	if err := f.fault(key, "PutObjectIfAbsent"); err != nil {
		return err
	}
	return f.Memory.PutObjectIfAbsent(ctx, key, data)
}

func (f *faultOSS) ListObjectsDetailed(ctx context.Context, prefix string) ([]oss.ObjectInfo, error) {
	if err := f.fault(prefix, "ListObjectsDetailed"); err != nil {
		return nil, err
	}
	return f.Memory.ListObjectsDetailed(ctx, prefix)
}

func (f *faultOSS) ListObjectsRecursive(ctx context.Context, prefix, startAfter string, limit int) ([]oss.ObjectInfo, string, error) {
	if err := f.fault(prefix, "ListObjectsRecursive"); err != nil {
		return nil, "", err
	}
	return f.Memory.ListObjectsRecursive(ctx, prefix, startAfter, limit)
}

func (f *faultOSS) DeleteObject(ctx context.Context, key string) error {
	if err := f.fault(key, "DeleteObject"); err != nil {
		return err
	}
	return f.Memory.DeleteObject(ctx, key)
}

func (f *faultOSS) DeletePrefix(ctx context.Context, prefix string) error {
	if err := f.fault(prefix, "DeletePrefix"); err != nil {
		return err
	}
	return f.Memory.DeletePrefix(ctx, prefix)
}

// presignFaultOSS is the in-memory fake whose PresignGet answers with a
// store-availability fault: the minting layer is unreachable even though
// the object bytes are fine.
type presignFaultOSS struct {
	*ossfake.Memory
}

func (f *presignFaultOSS) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "", fmt.Errorf("store down (PresignGet %s): %w", key, oss.ErrStorageUnavailable)
}

// --- Wire layer: the real MinIOClient against a canned S3 endpoint ---------

// wireS3Server stands up the S3 wire the fault matrix runs against: the
// SDK's one-time ?location probe is always answered, then every object
// request gets the canned failure the mode names. The fault SHAPE (a 5xx
// body, a dead connection, a hung one, a 404 error code, a tombstone
// object) is what the classification must key off.
func wireS3Server(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		switch mode {
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`<Error><Code>InternalError</Code><Message>Internal Error.</Message></Error>`))
		case "403":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>Access Denied.</Message></Error>`))
		case "nosuchbucket":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist.</Message></Error>`))
		case "nosuchkey":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`))
		case "close":
			// Accept, then drop the connection without a response: the
			// client sees a reset/EOF mid-request.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, herr := hj.Hijack(); herr == nil {
					conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusInternalServerError)
		case "slow":
			time.Sleep(500 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		case "tombstone":
			w.Header().Set("Content-Type", "application/vnd.agentteams.tombstone")
			w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
			w.Header().Set("Last-Modified", "Fri, 30 Oct 2026 21:00:00 GMT")
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", "0")
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unknown wire mode %q", mode)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wireRig swaps the rig's in-memory store for a real MinIOClient pointed
// at the canned S3 endpoint, so the fault travels the full SDK stack
// (request, error decoding, classification) instead of a stubbed method.
// PublicEndpoint is the same server: the presign path dials the public
// origin at issuance, and in these tests the issuer is what is faulted
// through the object requests around it.
func wireRig(t *testing.T, mode string) *humanFilesHandler {
	t.Helper()
	hfh, _, _ := newHumanFilesRig(t)
	srv := wireS3Server(t, mode)
	hfh.oss = oss.NewMinIOClient(oss.Config{
		Endpoint:       srv.URL,
		PublicEndpoint: srv.URL,
		AccessKey:      "access-key",
		SecretKey:      "secret-key",
		Bucket:         "bucket",
	})
	return hfh
}

// TestStorageWire_HTTP500Is503 pins the maintainer's repro at the wire:
// the store answers 5xx, and the file surfaces answer 503
// storage_unavailable — never the 404/500 the fault used to collapse
// into.
func TestStorageWire_HTTP500Is503(t *testing.T) {
	hfh := wireRig(t, "500")

	// A plain read (its stat faults)...
	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil))
	assertStorageUnavailable(t, rec, "file GET")

	// ...a presigned download (its stat faults before minting)...
	req := hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil)
	req.URL.RawQuery = "presign=1"
	rec = httptest.NewRecorder()
	hfh.getFile(rec, req)
	assertStorageUnavailable(t, rec, "presign GET")

	// ...and a presigned upload (the commit-state read faults after the
	// offline mint).
	rec = hfPostFile(t, hfh, "alice", "big.bin", hfSelf(), "presign=1")
	assertStorageUnavailable(t, rec, "presign upload")
}

// TestStorageWire_ConnectionResetIs503: the connection dies without a
// response — a transport fault, still a 503.
func TestStorageWire_ConnectionResetIs503(t *testing.T) {
	hfh := wireRig(t, "close")
	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil))
	assertStorageUnavailable(t, rec, "file GET over a dead connection")
}

// TestStorageWire_TimeoutIs503: the store hangs past the caller's budget
// — the store did not answer, which from the caller's view is the same
// availability fault.
func TestStorageWire_TimeoutIs503(t *testing.T) {
	hfh := wireRig(t, "slow")
	req := hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil)
	ctx, cancel := context.WithTimeout(req.Context(), 100*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	hfh.getFile(rec, req)
	assertStorageUnavailable(t, rec, "file GET over a hung store")
}

// TestStorageWire_NoSuchBucketIs500: a bucket-level 404 is a deployment
// fault. It is neither the object's absence (404) nor an availability
// fault (503) — it stays a plain 500.
func TestStorageWire_NoSuchBucketIs500(t *testing.T) {
	hfh := wireRig(t, "nosuchbucket")
	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("NoSuchBucket: status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "storage_unavailable") {
		t.Fatalf("NoSuchBucket must not be reported storage_unavailable: %s", rec.Body.String())
	}
}

// TestStorageWire_AuthFaultIs500: a 403 from the store is a
// credential/configuration fault — a 500, never a 503 the caller would
// retry into the same wall.
func TestStorageWire_AuthFaultIs500(t *testing.T) {
	hfh := wireRig(t, "403")
	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("AccessDenied: status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "storage_unavailable") {
		t.Fatalf("AccessDenied must not be reported storage_unavailable: %s", rec.Body.String())
	}
}

// TestStorageWire_NoSuchKeyIs404: a genuine object absence stays the 404
// the local-delete signal relies on.
func TestStorageWire_NoSuchKeyIs404(t *testing.T) {
	hfh := wireRig(t, "nosuchkey")
	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("NoSuchKey: status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestStorageWire_TombstoneStays404: a readable tombstone object outranks
// nothing — the soft-deleted path answers the same 404 it always did, and
// a store that serves it normally is not unavailable.
func TestStorageWire_TombstoneStays404(t *testing.T) {
	hfh := wireRig(t, "tombstone")
	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("tombstone read: status = %d, want the tombstone 404: %s", rec.Code, rec.Body.String())
	}
}

// --- Injected layer: one storage operation per endpoint -------------------

// TestStorageFault_ListingIs503: the directory listing and the trash
// listing both answer 503 when the list call faults.
func TestStorageFault_ListingIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	hfh.oss = newFaultOSS(t, store, "", "ListObjectsDetailed")

	rec := httptest.NewRecorder()
	hfh.getFile(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	assertStorageUnavailable(t, rec, "directory listing")

	rec = httptest.NewRecorder()
	hfh.listTrash(rec, hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil))
	assertStorageUnavailable(t, rec, "trash listing")
}

// TestStorageFault_RecursiveListingIs503: the recursive flat listing
// answers 503 when its walk faults. The root's ?recursive=1 listing is
// served by listFiles (the GET .../files route), not getFile — getFile
// treats the empty path as a plain directory listing.
func TestStorageFault_RecursiveListingIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	hfh.oss = newFaultOSS(t, store, "", "ListObjectsRecursive")

	req := hfReq(t, http.MethodGet, "alice", "", hfSelf(), nil)
	req.URL.RawQuery = "recursive=1"
	rec := httptest.NewRecorder()
	hfh.listFiles(rec, req)
	assertStorageUnavailable(t, rec, "recursive listing")
}

// TestStorageFault_QuotaUsageWalkIs503: getQuota's live usage walk faults.
func TestStorageFault_QuotaUsageWalkIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	hfh.oss = newFaultOSS(t, store, humanFilesPrefix("alice"), "ListObjectsDetailed")

	rec := hfGetQuota(t, hfh, "alice", hfSelf())
	assertStorageUnavailable(t, rec, "getQuota")
}

// TestStorageFault_PresignUploadQuotaReadIs503: the presign issuer's
// fail-closed quota pre-check cannot verify usage — no URL is issued, and
// the answer is the availability fault, not a 500 the client would treat
// as a server bug.
func TestStorageFault_PresignUploadQuotaReadIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	if rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":100}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	// The ledger read (not the walk) is the faulted operation: with a
	// ledger present the pre-check reads it instead of walking.
	hfh.oss = newFaultOSS(t, store, humanQuotaLedgerKey("alice"), "GetObject")

	rec := hfPostFile(t, hfh, "alice", "big.bin", hfSelf(), "presign=1")
	assertStorageUnavailable(t, rec, "presign upload")
}

// TestStorageFault_PresignGetIs503: the download-URL mint itself faults
// (the object is fine; the issuing layer is not).
func TestStorageFault_PresignGetIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "a.txt", hfSelf(), "hello"); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	hfh.oss = &presignFaultOSS{Memory: store}

	req := hfReq(t, http.MethodGet, "alice", "a.txt", hfSelf(), nil)
	req.URL.RawQuery = "presign=1"
	rec := httptest.NewRecorder()
	hfh.getFile(rec, req)
	assertStorageUnavailable(t, rec, "presign GET")
}

// TestStorageFault_WriteIs503: the publish faults; the write answers 503
// (the client may retry) and nothing is half-recorded as success.
func TestStorageFault_WriteIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	hfh.oss = newFaultOSS(t, store, humanFilesPrefix("alice"), "PutObject")

	rec := hfPutFile(t, hfh, "alice", "new.txt", hfSelf(), "hello")
	assertStorageUnavailable(t, rec, "file PUT")
}

// TestStorageFault_DeleteIs503: the trash write faults and the delete
// answers 503; the live file is untouched by the failed delete.
func TestStorageFault_DeleteIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "del.txt", hfSelf(), "hello"); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	hfh.oss = newFaultOSS(t, store, humanTrashPrefix("alice"), "PutObject")

	rec := httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", "del.txt", hfSelf(), nil))
	assertStorageUnavailable(t, rec, "file DELETE")

	if got := hfGetLive(t, hfh, "alice", "del.txt"); got.Code != http.StatusOK || got.Body.String() != "hello" {
		t.Fatalf("file after the failed delete: %d %q, want 200 hello", got.Code, got.Body.String())
	}
}

// seedTrashEntry puts a file and soft-deletes it, returning the trash id.
func seedTrashEntry(t *testing.T, hfh *humanFilesHandler, path string) string {
	t.Helper()
	if rec := hfPutFile(t, hfh, "alice", path, hfSelf(), "hello"); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	hfh.deleteFile(rec, hfReq(t, http.MethodDelete, "alice", path, hfSelf(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	var delResp struct {
		TrashID string `json:"trashId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delResp); err != nil {
		t.Fatalf("decode delete response: %v: %s", err, rec.Body.String())
	}
	if delResp.TrashID == "" {
		t.Fatalf("delete response carried no trashId: %s", rec.Body.String())
	}
	return delResp.TrashID
}

// TestStorageFault_RestoreIs503: the trash metadata read faults; the
// restore answers 503 and neither the file nor the entry is mutated.
func TestStorageFault_RestoreIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	id := seedTrashEntry(t, hfh, "rest.txt")
	hfh.oss = newFaultOSS(t, store, humanTrashPrefix("alice"), "GetObject")

	rec := httptest.NewRecorder()
	hfh.restoreTrash(rec, hfTrashReq(t, http.MethodPost, "alice", id, hfSelf(), ""))
	assertStorageUnavailable(t, rec, "trash restore")
}

// TestStorageFault_PurgeIs503: the recursive trash removal faults; the
// purge answers 503.
func TestStorageFault_PurgeIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	id := seedTrashEntry(t, hfh, "purge.txt")
	hfh.oss = newFaultOSS(t, store, humanTrashPrefix("alice"), "DeletePrefix")

	rec := httptest.NewRecorder()
	hfh.purgeTrash(rec, hfTrashReq(t, http.MethodDelete, "alice", id, hfAdmin(), ""))
	assertStorageUnavailable(t, rec, "trash purge")
}

// TestStorageFault_CommitIs503: the commit's session-state read faults;
// the commit answers 503 and the staged upload is left for a retry.
func TestStorageFault_CommitIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	ctx := context.Background()
	if rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":100}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	pr := hfPresign(t, hfh, "alice", "big.bin", "presign=1")
	stageKey := hfStagingKey(t, &pr)
	if err := store.PutObject(ctx, stageKey, []byte(strings.Repeat("x", 40))); err != nil {
		t.Fatal(err)
	}
	hfh.oss = newFaultOSS(t, store, humanPresignStateKey("alice"), "GetObject")

	rec := hfPostFile(t, hfh, "alice", "big.bin/commit", hfSelf(), "")
	assertStorageUnavailable(t, rec, "presign commit")
}

// TestStorageFault_CommitRetryIs503Not409: a retry of a settled commit
// whose file stat faults answers 503 — the store cannot verify the
// published version, which is not the same as the version being gone (the
// 409 a true loss would give).
func TestStorageFault_CommitRetryIs503Not409(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	ctx := context.Background()
	if rec := hfPutQuota(t, hfh, "alice", hfAdmin(), `{"quotaBytes":100}`); rec.Code != http.StatusOK {
		t.Fatalf("set quota: %d %s", rec.Code, rec.Body.String())
	}
	pr := hfPresign(t, hfh, "alice", "big.bin", "presign=1")
	stageKey := hfStagingKey(t, &pr)
	if err := store.PutObject(ctx, stageKey, []byte(strings.Repeat("x", 40))); err != nil {
		t.Fatal(err)
	}
	if rec := hfPostFile(t, hfh, "alice", "big.bin/commit", hfSelf(), ""); rec.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body.String())
	}
	hfh.oss = newFaultOSS(t, store, humanFilesPrefix("alice")+"big.bin", "StatMeta")

	rec := hfPostFile(t, hfh, "alice", "big.bin/commit", hfSelf(), "")
	assertStorageUnavailable(t, rec, "commit retry")
}

// TestStorageFault_SyncManifestIs503: the sync surface's storage faults
// answer 503 on the manifest write, the manifest read, and the status
// summary — and a missing manifest is still the 200 zero summary, which
// the fault must not become.
func TestStorageFault_SyncManifestIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)

	// First write healthy, so the conditional write has a base...
	rec := hfPutSync(t, hfh, "alice", alice, syncManifestBody([]string{"files/a.txt"}, nil),
		map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("first manifest write: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ETag == "" {
		t.Fatalf("decode first write: %v: %s", err, rec.Body.String())
	}

	// ...then the store goes down for the conditional write...
	hfh.oss = newFaultOSS(t, store, humanSyncManifestKey("alice"), "PutObjectIfMatch")
	rec = hfPutSync(t, hfh, "alice", alice, syncManifestBody(nil, nil),
		map[string]string{"If-Match": created.ETag})
	assertStorageUnavailable(t, rec, "manifest PUT")

	// ...and for the read surface.
	hfh.oss = newFaultOSS(t, store, humanSyncManifestKey("alice"), "GetObject")
	rec = httptest.NewRecorder()
	hfh.getSyncManifest(rec, hfSyncReq(t, http.MethodGet, "alice", "manifest", alice, ""))
	assertStorageUnavailable(t, rec, "manifest GET")

	rec = hfGetSyncStatus(t, hfh, "alice", alice)
	assertStorageUnavailable(t, rec, "sync status")
}

// TestStorageFault_AbsenceStays404: the 503 must not swallow the 404s it
// is carved out of — a genuinely missing file and a tombstoned path both
// keep their 404 while the store is healthy.
func TestStorageFault_AbsenceStays404(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)

	if got := hfGetLive(t, hfh, "alice", "missing.txt"); got.Code != http.StatusNotFound {
		t.Fatalf("missing file: got %d, want 404", got.Code)
	}
	seedTrashEntry(t, hfh, "gone.txt")
	if got := hfGetLive(t, hfh, "alice", "gone.txt"); got.Code != http.StatusNotFound {
		t.Fatalf("tombstoned file: got %d, want the tombstone 404", got.Code)
	}
}
