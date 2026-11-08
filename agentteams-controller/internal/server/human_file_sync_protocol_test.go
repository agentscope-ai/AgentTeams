package server

// TestHumanFileSyncProtocol is the real-storage protocol regression for the
// sync endpoints (#1414 P3, #1499), mandated before merge: independent HTTP
// clients over the real controller stack (httptest server + bearer-token
// auth middleware + human sync handlers) against a real S3-compatible
// backend (RustFS/MinIO via mc, like production). The in-memory fixture
// tests (human_sync_handler_test.go) cover the same logic against a fake
// OSS, which cannot catch wire-level assumptions — the ETag shape that
// survives the `mc stat --json` round trip, the SDK conditional-write
// round trip, and the auth middleware chain.
//
// Gated on AGENTTEAMS_TEST_S3_* — a distinct namespace from the
// HUMAN_FILES_S3_* gate so the two regressions can run against different
// buckets — and skipped with a reason when unset. The CI job "sync-protocol"
// in .github/workflows/test-human-files-s3.yml starts RustFS 1.0.4 + mc and
// provides the vars.
//
// Writer identities. authorizeHumanSync grants write access to one human's
// manifest to exactly two identity classes: the human themself
// (RoleHuman + file_sync) and admin/manager. Two different humans can
// therefore never both write one manifest — a foreign human is 404-walled
// (per-person surface, anti-probing). The dual-writer conflict is
// accordingly driven by human A and admin, the only cross-identity
// combination the protocol admits; human B is a second person exercising
// per-human isolation (scenario 5), and human C the capability wall.
//
// Conflict status codes. The protocol answers a stale If-Match with
// 412 etag_mismatch, not 409: 409 is reserved for manifest_exists
// (first-write race) and file_busy (lease). The 412 body carries the coded
// error only — no current etag — so the client re-GETs. Both branches are
// asserted below against real storage.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	audit "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// syncProtocolAuthenticator maps static bearer tokens to caller identities.
// Same pattern as the auth package's mockAuthenticator (copied: a test
// helper from another package is not importable).
type syncProtocolAuthenticator struct {
	tokens map[string]*authpkg.CallerIdentity
}

func (a *syncProtocolAuthenticator) Authenticate(_ context.Context, token string) (*authpkg.CallerIdentity, error) {
	id, ok := a.tokens[token]
	if !ok {
		return nil, fmt.Errorf("invalid token")
	}
	cp := *id
	return &cp, nil
}

// protoSuffix returns n random hex characters for the per-run unique human
// names: two runs never collide, and cleanup removes each run's objects,
// so the regression is repeatable on one shared bucket.
func protoSuffix(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func TestHumanFileSyncProtocol(t *testing.T) {
	endpoint := os.Getenv("AGENTTEAMS_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set AGENTTEAMS_TEST_S3_ENDPOINT/AGENTTEAMS_TEST_S3_ACCESS_KEY/AGENTTEAMS_TEST_S3_SECRET_KEY/AGENTTEAMS_TEST_S3_BUCKET to run the sync protocol regression against a real S3 backend (CI job: sync-protocol)")
	}
	access := os.Getenv("AGENTTEAMS_TEST_S3_ACCESS_KEY")
	secret := os.Getenv("AGENTTEAMS_TEST_S3_SECRET_KEY")
	bucket := os.Getenv("AGENTTEAMS_TEST_S3_BUCKET")
	if access == "" || secret == "" || bucket == "" {
		t.Fatal("AGENTTEAMS_TEST_S3_ACCESS_KEY, AGENTTEAMS_TEST_S3_SECRET_KEY and AGENTTEAMS_TEST_S3_BUCKET are all required when AGENTTEAMS_TEST_S3_ENDPOINT is set")
	}

	suffix := protoSuffix(t, 4)
	nameA := "sync-proto-" + suffix + "-a"
	nameB := "sync-proto-" + suffix + "-b"
	nameC := "sync-proto-" + suffix + "-c"

	newHuman := func(name, display string) *v1beta1.Human {
		return &v1beta1.Human{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       v1beta1.HumanSpec{DisplayName: display},
		}
	}
	scheme := newServerTestScheme(t)
	k8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(newHuman(nameA, "Sync A"), newHuman(nameB, "Sync B"), newHuman(nameC, "Sync C")).
		Build()

	// StoragePrefix is "<mc alias>/<bucket>": fullPath() prefixes it, so the
	// first element must be the alias that ensureAlias registers.
	alias := "humanfilessyncproto"
	realOSS := oss.NewMinIOClient(oss.Config{
		Endpoint:      endpoint,
		AccessKey:     access,
		SecretKey:     secret,
		Alias:         alias,
		Bucket:        bucket,
		StoragePrefix: alias + "/" + bucket,
	})
	ctx := context.Background()
	t.Cleanup(func() {
		_ = realOSS.DeletePrefix(ctx, humanAreaPrefix(nameA))
		_ = realOSS.DeletePrefix(ctx, humanAreaPrefix(nameB))
		_ = realOSS.DeletePrefix(ctx, humanAreaPrefix(nameC))
	})

	// Audit lands in an in-memory store: the protocol under test is the
	// manifest wire protocol, and the audit path is covered by the fixture
	// tests. The real OSS client is only on the handler's data path.
	auditStore := ossfake.NewMemory()
	hfh := NewHumanFilesHandler(k8s, "default", realOSS, audit.NewClient(auditStore))

	tokens := map[string]*authpkg.CallerIdentity{
		"token-a":     {Role: authpkg.RoleHuman, Username: nameA, Capabilities: []string{string(authpkg.CapabilityFileSync)}},
		"token-b":     {Role: authpkg.RoleHuman, Username: nameB, Capabilities: []string{string(authpkg.CapabilityFileSync)}},
		"token-c":     {Role: authpkg.RoleHuman, Username: nameC}, // no file_sync
		"token-admin": {Role: authpkg.RoleAdmin, Username: "admin"},
	}
	mw := authpkg.NewMiddleware(&syncProtocolAuthenticator{tokens: tokens}, nil, authpkg.NewAuthorizer(), k8s, "default")
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/humans/{name}/sync/manifest", mw.Authenticate(http.HandlerFunc(hfh.getSyncManifest)))
	mux.Handle("PUT /api/v1/humans/{name}/sync/manifest", mw.Authenticate(http.HandlerFunc(hfh.putSyncManifest)))
	mux.Handle("GET /api/v1/humans/{name}/sync/status", mw.Authenticate(http.HandlerFunc(hfh.getSyncStatus)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// One client per identity: the wire boundary (bearer token -> identity)
	// is exercised for real, and no identity ever reuses another's client.
	newClient := func() *http.Client { return &http.Client{Timeout: 60 * time.Second} }
	clientA := newClient()
	clientB := newClient()
	clientC := newClient()
	clientAdmin := newClient()
	tokA, tokB, tokC, tokAdmin := "token-a", "token-b", "token-c", "token-admin"

	manifestPath := func(name string) string { return "/api/v1/humans/" + name + "/sync/manifest" }
	statusPath := func(name string) string { return "/api/v1/humans/" + name + "/sync/status" }

	do := func(t *testing.T, c *http.Client, token, method, path, body string, headers map[string]string) (int, []byte, http.Header) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, srv.URL+path, rd)
		if err != nil {
			t.Fatalf("build %s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s %s: read body: %v", method, path, err)
		}
		return resp.StatusCode, data, resp.Header
	}

	mkManifest := func(entries map[string]any, tombstones ...string) string {
		raw := make(map[string]json.RawMessage, len(entries))
		for p, v := range entries {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal entry %s: %v", p, err)
			}
			raw[p] = b
		}
		tombs := make([]json.RawMessage, 0, len(tombstones))
		for _, p := range tombstones {
			b, err := json.Marshal(map[string]string{"path": p})
			if err != nil {
				t.Fatalf("marshal tombstone %s: %v", p, err)
			}
			tombs = append(tombs, b)
		}
		out, err := json.Marshal(map[string]any{
			"version":    syncManifestVersion,
			"entries":    raw,
			"tombstones": tombs,
		})
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		return string(out)
	}

	type syncWriteResp struct {
		ETag       string `json:"etag"`
		Generation int64  `json:"generation"`
		UpdatedAt  string `json:"updatedAt"`
	}
	type syncStatusResp struct {
		Entries    int    `json:"entries"`
		Tombstones int    `json:"tombstones"`
		Generation int64  `json:"generation"`
		UpdatedAt  string `json:"updatedAt"`
		UpdatedBy  string `json:"updatedBy"`
	}
	type storedManifest struct {
		Version    int                        `json:"version"`
		Generation int64                      `json:"generation"`
		Entries    map[string]json.RawMessage `json:"entries"`
		Tombstones []json.RawMessage          `json:"tombstones"`
	}

	recordWrite := func(t *testing.T, what string, code int, body []byte) syncWriteResp {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200: %s", what, code, body)
		}
		var w syncWriteResp
		if err := json.Unmarshal(body, &w); err != nil {
			t.Fatalf("%s: decode: %v: %s", what, err, body)
		}
		if w.ETag == "" || w.Generation <= 0 {
			t.Fatalf("%s: response missing etag/generation: %s", what, body)
		}
		return w
	}

	assertCoded := func(t *testing.T, what string, body []byte, wantCode string) {
		t.Helper()
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatalf("%s: body is not a coded error: %s", what, body)
		}
		if e.Code != wantCode {
			t.Fatalf("%s: code = %q, want %q (body: %s)", what, e.Code, wantCode, body)
		}
	}

	entryA := map[string]any{"size": 3, "sha256": "aaa"}
	entryB := map[string]any{"size": 4, "sha256": "bbb"}
	bodyA := mkManifest(map[string]any{"files/a.txt": entryA})
	bodyAB := mkManifest(map[string]any{"files/a.txt": entryA, "files/b.txt": entryB})

	t.Log("scenario 1: first-time create (empty manifest -> generation 1 + ETag)")
	// 1.1 Status of an area with no manifest: 200 + zero summary, not 404.
	code, body, _ := do(t, clientA, tokA, http.MethodGet, statusPath(nameA), "", nil)
	if code != http.StatusOK {
		t.Fatalf("status before first write: got %d, want 200: %s", code, body)
	}
	var st syncStatusResp
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("status decode: %v: %s", err, body)
	}
	if st.Entries != 0 || st.Tombstones != 0 || st.Generation != 0 {
		t.Fatalf("status before first write: %+v, want a zero summary", st)
	}
	// 1.2 The manifest itself is 404 before the first write.
	code, body, _ = do(t, clientA, tokA, http.MethodGet, manifestPath(nameA), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("GET manifest before create: got %d, want 404: %s", code, body)
	}
	// 1.3 The first write is a conditional create; a bare PUT (no
	// conditional header) is refused with 400.
	code, body, _ = do(t, clientA, tokA, http.MethodPut, manifestPath(nameA), bodyA, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("unconditional PUT: got %d, want 400: %s", code, body)
	}
	assertCoded(t, "unconditional PUT", body, "conditional_write_required")
	code, body, _ = do(t, clientA, tokA, http.MethodPut, manifestPath(nameA), bodyA, map[string]string{"If-None-Match": "*"})
	p1 := recordWrite(t, "first write", code, body)
	if p1.Generation != 1 {
		t.Fatalf("first write generation = %d, want 1", p1.Generation)
	}
	// 1.4 The stored manifest: verbatim entries, server-owned generation,
	// and an ETag header that equals the write response's etag (the write
	// base for the next round).
	code, body, hdr := do(t, clientA, tokA, http.MethodGet, manifestPath(nameA), "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET manifest after create: got %d, want 200: %s", code, body)
	}
	// The response ETag is the quoted wire form of the write's bare etag.
	if got, want := hdr.Get("ETag"), `"`+p1.ETag+`"`; got != want {
		t.Fatalf("GET ETag = %q, want the quoted write response etag %q", got, want)
	}
	var sm storedManifest
	sm = storedManifest{}
	if err := json.Unmarshal(body, &sm); err != nil {
		t.Fatalf("manifest decode: %v: %s", err, body)
	}
	if sm.Version != syncManifestVersion || sm.Generation != 1 || len(sm.Entries) != 1 {
		t.Fatalf("stored manifest after first write: version=%d generation=%d entries=%d, want 1/1/1", sm.Version, sm.Generation, len(sm.Entries))
	}
	if _, ok := sm.Entries["files/a.txt"]; !ok {
		t.Fatalf("stored manifest missing files/a.txt: %s", body)
	}

	t.Log("scenario 2: a newer base wins; the stale If-Match is 412 etag_mismatch")
	// 2.1 A second authorized writer (admin — the only identity class
	// besides A herself that may write A's manifest) advances the base.
	code, body, _ = do(t, clientAdmin, tokAdmin, http.MethodPut, manifestPath(nameA), bodyAB, map[string]string{"If-Match": p1.ETag})
	p2 := recordWrite(t, "admin write on A's base", code, body)
	if p2.Generation != 2 {
		t.Fatalf("admin write generation = %d, want 2", p2.Generation)
	}
	// 2.2 A's stale base (etag 1) is refused: 412 etag_mismatch, coded, no
	// current etag in the body — the client re-GETs.
	code, body, _ = do(t, clientA, tokA, http.MethodPut, manifestPath(nameA), bodyA, map[string]string{"If-Match": p1.ETag})
	if code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: got %d, want 412: %s", code, body)
	}
	assertCoded(t, "stale If-Match", body, "etag_mismatch")
	// 2.3 A blind first-write retry against the now-existing manifest:
	// 409 manifest_exists.
	code, body, _ = do(t, clientA, tokA, http.MethodPut, manifestPath(nameA), bodyA, map[string]string{"If-None-Match": "*"})
	if code != http.StatusConflict {
		t.Fatalf("first-write retry on existing manifest: got %d, want 409: %s", code, body)
	}
	assertCoded(t, "first-write retry", body, "manifest_exists")

	t.Log("scenario 3: the re-read retry is idempotent; tombstones persist")
	// 3.1 A re-reads (now on etag 2) and writes the same state back: the
	// retry must not duplicate entries.
	code, body, _ = do(t, clientA, tokA, http.MethodPut, manifestPath(nameA), bodyAB, map[string]string{"If-Match": p2.ETag})
	p3 := recordWrite(t, "A retry on the fresh base", code, body)
	if p3.Generation != 3 {
		t.Fatalf("retry generation = %d, want 3", p3.Generation)
	}
	code, body, _ = do(t, clientA, tokA, http.MethodGet, manifestPath(nameA), "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET manifest after retry: got %d, want 200: %s", code, body)
	}
	sm = storedManifest{}
	if err := json.Unmarshal(body, &sm); err != nil {
		t.Fatalf("manifest decode: %v: %s", err, body)
	}
	if len(sm.Entries) != 2 || sm.Generation != 3 {
		t.Fatalf("after idempotent retry: entries=%d generation=%d, want 2/3: %s", len(sm.Entries), sm.Generation, body)
	}
	if _, ok := sm.Entries["files/a.txt"]; !ok {
		t.Fatalf("files/a.txt lost by the retry: %s", body)
	}
	if _, ok := sm.Entries["files/b.txt"]; !ok {
		t.Fatalf("files/b.txt lost by the retry: %s", body)
	}
	// 3.2 A deletes files/b.txt: the tombstone is the client's deletion
	// record and must survive the round trip.
	bodyATombB := mkManifest(map[string]any{"files/a.txt": entryA}, "files/b.txt")
	code, body, _ = do(t, clientA, tokA, http.MethodPut, manifestPath(nameA), bodyATombB, map[string]string{"If-Match": p3.ETag})
	p4 := recordWrite(t, "A delete + tombstone", code, body)
	if p4.Generation != 4 {
		t.Fatalf("delete generation = %d, want 4", p4.Generation)
	}
	code, body, _ = do(t, clientA, tokA, http.MethodGet, manifestPath(nameA), "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET manifest after delete: got %d, want 200: %s", code, body)
	}
	sm = storedManifest{}
	if err := json.Unmarshal(body, &sm); err != nil {
		t.Fatalf("manifest decode: %v: %s", err, body)
	}
	if len(sm.Entries) != 1 {
		t.Fatalf("entries after delete = %d, want 1: %s", len(sm.Entries), body)
	}
	if _, ok := sm.Entries["files/a.txt"]; !ok {
		t.Fatalf("files/a.txt lost by the delete: %s", body)
	}
	if len(sm.Tombstones) != 1 {
		t.Fatalf("tombstones after delete = %d, want 1: %s", len(sm.Tombstones), body)
	}
	var ts struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(sm.Tombstones[0], &ts); err != nil {
		t.Fatalf("tombstone decode: %v", err)
	}
	if ts.Path != "files/b.txt" {
		t.Fatalf("tombstone path = %q, want files/b.txt", ts.Path)
	}

	t.Log("scenario 4: generation is server-owned, +1 per accepted write")
	gens := []int64{p1.Generation, p2.Generation, p3.Generation, p4.Generation}
	for i, g := range gens {
		if g != int64(i+1) {
			t.Fatalf("accepted write %d has generation %d, want %d (sequence %v)", i+1, g, i+1, gens)
		}
	}
	// The read side agrees with the last accepted write: GET's body
	// generation and ETag header both match the final PUT response.
	code, body, hdr = do(t, clientA, tokA, http.MethodGet, manifestPath(nameA), "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET manifest final: got %d, want 200: %s", code, body)
	}
	// The response ETag is the quoted wire form of the write's bare etag.
	if got, want := hdr.Get("ETag"), `"`+p4.ETag+`"`; got != want {
		t.Fatalf("final GET ETag = %q, want the quoted last write's etag %q", got, want)
	}
	sm = storedManifest{}
	if err := json.Unmarshal(body, &sm); err != nil {
		t.Fatalf("manifest decode: %v: %s", err, body)
	}
	if sm.Generation != 4 {
		t.Fatalf("stored generation = %d, want 4", sm.Generation)
	}
	code, body, _ = do(t, clientA, tokA, http.MethodGet, statusPath(nameA), "", nil)
	if code != http.StatusOK {
		t.Fatalf("status final: got %d, want 200: %s", code, body)
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("status decode: %v: %s", err, body)
	}
	if st.Generation != 4 || st.Entries != 1 || st.Tombstones != 1 {
		t.Fatalf("status after 4 writes: %+v, want generation 4 / 1 entry / 1 tombstone", st)
	}

	t.Log("scenario 5: per-human isolation (plus the capability wall)")
	bodyX := mkManifest(map[string]any{"files/x.txt": map[string]any{"size": 5, "sha256": "xxx"}})
	// B's manifest has its own generation counter, independent of A's.
	code, body, _ = do(t, clientB, tokB, http.MethodPut, manifestPath(nameB), bodyX, map[string]string{"If-None-Match": "*"})
	pB := recordWrite(t, "B first write", code, body)
	if pB.Generation != 1 {
		t.Fatalf("B's first write generation = %d, want 1 (independent of A's counter)", pB.Generation)
	}
	code, body, _ = do(t, clientB, tokB, http.MethodGet, manifestPath(nameB), "", nil)
	if code != http.StatusOK {
		t.Fatalf("B GET own manifest: got %d, want 200: %s", code, body)
	}
	// admin can read both areas.
	for _, n := range []string{nameA, nameB} {
		code, body, _ = do(t, clientAdmin, tokAdmin, http.MethodGet, manifestPath(n), "", nil)
		if code != http.StatusOK {
			t.Fatalf("admin GET %s manifest: got %d, want 200: %s", n, code, body)
		}
	}
	// A's area is a 404 wall for B on all three endpoints.
	var foreign404Body []byte
	code, body, _ = do(t, clientB, tokB, http.MethodGet, manifestPath(nameA), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("B GET A manifest: got %d, want 404: %s", code, body)
	}
	foreign404Body = body
	code, body, _ = do(t, clientB, tokB, http.MethodPut, manifestPath(nameA), bodyX, map[string]string{"If-None-Match": "*"})
	if code != http.StatusNotFound {
		t.Fatalf("B PUT A manifest: got %d, want 404: %s", code, body)
	}
	code, body, _ = do(t, clientB, tokB, http.MethodGet, statusPath(nameA), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("B GET A status: got %d, want 404: %s", code, body)
	}
	// Symmetric: B's area is a 404 wall for A.
	code, body, _ = do(t, clientA, tokA, http.MethodGet, manifestPath(nameB), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("A GET B manifest: got %d, want 404: %s", code, body)
	}
	// Anti-probing: the foreign 404 is byte-identical to a ghost-human
	// 404, so B cannot learn that A's area exists.
	code, body, _ = do(t, clientB, tokB, http.MethodGet, manifestPath("sync-proto-"+suffix+"-ghost"), "", nil)
	if code != http.StatusNotFound {
		t.Fatalf("B GET ghost manifest: got %d, want 404: %s", code, body)
	}
	if !bytes.Equal(body, foreign404Body) {
		t.Fatalf("ghost 404 body differs from the foreign 404 body: %q vs %q", body, foreign404Body)
	}
	// The capability wall: a known human without file_sync gets 403 (a
	// known identity short of a permission), not the foreign 404.
	code, body, _ = do(t, clientC, tokC, http.MethodGet, statusPath(nameC), "", nil)
	if code != http.StatusForbidden {
		t.Fatalf("no-capability human GET status: got %d, want 403: %s", code, body)
	}
	code, body, _ = do(t, clientC, tokC, http.MethodPut, manifestPath(nameC), bodyX, map[string]string{"If-None-Match": "*"})
	if code != http.StatusForbidden {
		t.Fatalf("no-capability human PUT: got %d, want 403: %s", code, body)
	}
}
