package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- route-level coverage of the listing pipeline ------------------------
//
// These tests run the production mux (NewHTTPServer) against a fake
// cluster and an in-memory OSS store, so the pipeline wiring is exercised
// end to end: which routes are ETag-wrapped, gzip-wrapped, or neither,
// and how the two layers combine (shared ETag, merged Vary, no body or
// encoding on a 304).

var listOptETagRe = regexp.MustCompile(`^"[0-9a-f]{32}"$`)

// listOptServer builds the production mux around a fake cluster with the
// human alice and an in-memory OSS store.
func listOptServer(t *testing.T) (*HTTPServer, *ossfake.Memory) {
	t.Helper()
	alice := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec:       v1beta1.HumanSpec{DisplayName: "Alice"},
	}
	k8s := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(alice).Build()
	store := ossfake.NewMemory()
	mw := authpkg.NewMiddleware(&alwaysAdminAuth{}, authpkg.NewCREnricher(k8s, "default"), authpkg.NewAuthorizer(), k8s, "default")
	srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", OSS: store, AuthMw: mw})
	return srv, store
}

// listOptDo performs an authenticated request through the real mux as the
// admin (alwaysAdminAuth accepts any bearer token).
func listOptDo(t *testing.T, srv *HTTPServer, method, path string, hdrs map[string]string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer sa-token")
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rec, req)
	return rec
}

// listOptSeed writes a file straight into alice's area, bypassing the file
// API (the listing endpoints under test only read from storage).
func listOptSeed(t *testing.T, store *ossfake.Memory, path, content string) {
	t.Helper()
	if err := store.PutObject(context.Background(), "humans/alice/files/"+path, []byte(content)); err != nil {
		t.Fatal(err)
	}
}

// listOptGunzip decodes a gzip response body.
func listOptGunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip read: %v", err)
	}
	return out
}

// assertListOptHeaders pins the header contract of a 200 listing:
// a quoted 32-hex sha256-128 ETag, the revalidate-only caching contract,
// and the merged Vary (Authorization from the ETag layer,
// Accept-Encoding from the gzip layer).
func assertListOptHeaders(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	etag := rec.Header().Get("ETag")
	if !listOptETagRe.MatchString(etag) {
		t.Fatalf("ETag = %q, want a quoted 32-hex sha256-128", etag)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=0, must-revalidate" {
		t.Errorf("Cache-Control = %q, want the revalidate-only contract", got)
	}
	if got := rec.Header().Get("Vary"); got != "Authorization, Accept-Encoding" {
		t.Errorf("Vary = %q, want the merged form", got)
	}
	return etag
}

// keySet returns the sorted JSON keys of an object decoded as
// map[string]any.
func listOptKeySet(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func listOptEqualKeys(got []string, want ...string) bool {
	w := append([]string(nil), want...)
	sort.Strings(w)
	return reflect.DeepEqual(got, w)
}

func listOptErrorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, rec.Body.String())
	}
	return body["message"].(string)
}

// TestHumanFileListOpt_DirectoryListETag304 pins the P4 contract on both
// listing routes: the root (mux-level pipeline) and a subdirectory
// (branch-level pipeline inside getFile). 200 carries the validator and
// the caching headers; the replay with If-None-Match is a 304 with no
// body and Content-Length 0.
func TestHumanFileListOpt_DirectoryListETag304(t *testing.T) {
	srv, store := listOptServer(t)
	listOptSeed(t, store, "a.txt", "hello")
	listOptSeed(t, store, "docs/b.txt", "world!")

	for _, path := range []string{
		"/api/v1/humans/alice/files",       // root, mux-level pipeline
		"/api/v1/humans/alice/files/docs/", // subdirectory, branch-level pipeline
	} {
		t.Run(path, func(t *testing.T) {
			rec1 := listOptDo(t, srv, http.MethodGet, path, nil, nil)
			if rec1.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec1.Code, rec1.Body.String())
			}
			etag := assertListOptHeaders(t, rec1)

			rec2 := listOptDo(t, srv, http.MethodGet, path, map[string]string{"If-None-Match": etag}, nil)
			if rec2.Code != http.StatusNotModified {
				t.Fatalf("replay status = %d, want 304 (body %s)", rec2.Code, rec2.Body.String())
			}
			if rec2.Body.Len() != 0 {
				t.Errorf("304 body = %q, want empty", rec2.Body.String())
			}
			if got := rec2.Header().Get("Content-Length"); got != "0" {
				t.Errorf("304 Content-Length = %q, want 0", got)
			}
			if got := rec2.Header().Get("ETag"); got != etag {
				t.Errorf("304 ETag = %q, want the current validator %q", got, etag)
			}
		})
	}
}

// TestHumanFileListOpt_ETagChangesWithContent pins the other half of P4:
// a content-changing write moves the representation, so the ETag moves
// and the stale validator misses with the full current body.
func TestHumanFileListOpt_ETagChangesWithContent(t *testing.T) {
	srv, store := listOptServer(t)
	listOptSeed(t, store, "a.txt", "hello")

	rec1 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files", nil, nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec1.Code, rec1.Body.String())
	}
	etag1 := assertListOptHeaders(t, rec1)

	// The listing reports size and etag, so a content-changing write
	// changes the serialized body.
	listOptSeed(t, store, "a.txt", "hello, world (longer)")

	rec2 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files", nil, nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec2.Code, rec2.Body.String())
	}
	etag2 := assertListOptHeaders(t, rec2)
	if etag2 == etag1 {
		t.Fatalf("content change did not move the ETag (%q)", etag1)
	}

	rec3 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files", map[string]string{"If-None-Match": etag1}, nil)
	if rec3.Code != http.StatusOK {
		t.Fatalf("stale validator status = %d, want 200 (a 304 would replay a moved representation)", rec3.Code)
	}
	if rec3.Body.Len() == 0 {
		t.Errorf("stale-validator response must carry the full body")
	}
	if got := rec3.Header().Get("ETag"); got != etag2 {
		t.Errorf("stale-validator response ETag = %q, want the current %q", got, etag2)
	}
}

// TestHumanFileListOpt_RecursiveListETag304 pins the P4 contract on the
// recursive flat listing (?recursive=1, the mux-level pipeline of the
// files root).
func TestHumanFileListOpt_RecursiveListETag304(t *testing.T) {
	srv, store := listOptServer(t)
	listOptSeed(t, store, "a.txt", "hello")
	listOptSeed(t, store, "x/y/deep.txt", "0123456789")

	const path = "/api/v1/humans/alice/files?recursive=1"
	rec1 := listOptDo(t, srv, http.MethodGet, path, nil, nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec1.Code, rec1.Body.String())
	}
	etag := assertListOptHeaders(t, rec1)

	rec2 := listOptDo(t, srv, http.MethodGet, path, map[string]string{"If-None-Match": etag}, nil)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("replay status = %d, want 304 (body %s)", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 body = %q, want empty", rec2.Body.String())
	}
	if got := rec2.Header().Get("Content-Length"); got != "0" {
		t.Errorf("304 Content-Length = %q, want 0", got)
	}
	if got := rec2.Header().Get("ETag"); got != etag {
		t.Errorf("304 ETag = %q, want the current validator %q", got, etag)
	}
}

// TestHumanFileListOpt_TrashListETag304 pins the P4 contract on the trash
// listing: a route-level delete creates the entry, then the listing
// revalidates to 304.
func TestHumanFileListOpt_TrashListETag304(t *testing.T) {
	srv, store := listOptServer(t)
	listOptSeed(t, store, "doomed.txt", "doomed")

	if rec := listOptDo(t, srv, http.MethodDelete, "/api/v1/humans/alice/files/doomed.txt", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}

	rec1 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/trash", nil, nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec1.Code, rec1.Body.String())
	}
	var body struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec1.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode trash body: %v", err)
	}
	if len(body.Entries) != 1 {
		t.Fatalf("trash entries = %d, want 1 (body %s)", len(body.Entries), rec1.Body.String())
	}
	etag := assertListOptHeaders(t, rec1)

	rec2 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/trash", map[string]string{"If-None-Match": etag}, nil)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("replay status = %d, want 304 (body %s)", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 body = %q, want empty", rec2.Body.String())
	}
	if got := rec2.Header().Get("Content-Length"); got != "0" {
		t.Errorf("304 Content-Length = %q, want 0", got)
	}
}

// TestHumanFileListOpt_FieldsDirectoryProjection pins the P6 directory
// shape: the exact key set of every entry, zero values serialized
// explicitly (an empty file and a directory both report size 0), and
// whitespace/duplicate tolerance of the field list.
func TestHumanFileListOpt_FieldsDirectoryProjection(t *testing.T) {
	srv, store := listOptServer(t)
	listOptSeed(t, store, "a.txt", "hello")
	listOptSeed(t, store, "empty.txt", "")
	listOptSeed(t, store, "docs/b.txt", "world!")

	// The second query is the whitespace/duplicate-tolerance form:
	// " name , , size " must project exactly {name, size}.
	loose := url.Values{}
	loose.Set("fields", " name , , size ")
	for _, path := range []string{
		"/api/v1/humans/alice/files?fields=name,size",
		"/api/v1/humans/alice/files?" + loose.Encode(),
	} {
		rec := listOptDo(t, srv, http.MethodGet, path, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", path, rec.Code, rec.Body.String())
		}
		var body struct {
			Path    string           `json:"path"`
			Entries []map[string]any `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if body.Path != "files/" {
			t.Errorf("%s: path = %q, want files/", path, body.Path)
		}
		if len(body.Entries) != 3 {
			t.Fatalf("%s: entries = %d, want 3 (a.txt, docs, empty.txt)", path, len(body.Entries))
		}
		byName := make(map[string]map[string]any, len(body.Entries))
		for _, e := range body.Entries {
			if keys := listOptKeySet(e); !listOptEqualKeys(keys, "name", "size") {
				t.Fatalf("%s: entry %v keys = %v, want {name size}", path, e["name"], keys)
			}
			byName[e["name"].(string)] = e
		}
		if got := byName["a.txt"]["size"]; got != float64(5) {
			t.Errorf("%s: a.txt size = %v, want 5", path, got)
		}
		if got := byName["empty.txt"]["size"]; got != float64(0) {
			t.Errorf("%s: zero value must be explicit, empty.txt = %v", path, byName["empty.txt"])
		}
		if got := byName["docs"]["size"]; got != float64(0) {
			t.Errorf("%s: zero value must be explicit, docs = %v", path, byName["docs"])
		}
	}

	// A different projection (all four fields) yields the full key set.
	rec := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files?fields=name,type,size,updatedAt", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range body.Entries {
		if keys := listOptKeySet(e); !listOptEqualKeys(keys, "name", "size", "type", "updatedAt") {
			t.Fatalf("entry %v keys = %v, want all four", e["name"], keys)
		}
	}
}

// TestHumanFileListOpt_FieldsRecursiveProjection pins the P6 recursive
// shape: the exact key set of every entry under the projection, and
// pagination continuity (the key cursor survives ?fields= and pages
// remain contiguous).
func TestHumanFileListOpt_FieldsRecursiveProjection(t *testing.T) {
	srv, store := listOptServer(t)
	for _, p := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
		listOptSeed(t, store, p, "x")
	}

	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		q := url.Values{}
		q.Set("recursive", "1")
		q.Set("fields", "path,size")
		q.Set("limit", "2")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		rec := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files?"+q.Encode(), nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", page, rec.Code, rec.Body.String())
		}
		var body struct {
			Entries []map[string]any `json:"entries"`
			NextKey *string          `json:"nextKey"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("page %d: decode: %v", page, err)
		}
		for _, e := range body.Entries {
			if keys := listOptKeySet(e); !listOptEqualKeys(keys, "path", "size") {
				t.Fatalf("page %d: entry %v keys = %v, want {path size}", page, e["path"], keys)
			}
			got = append(got, e["path"].(string))
		}
		if body.NextKey == nil {
			break
		}
		cursor = *body.NextKey
	}
	want := []string{"files/a.txt", "files/b.txt", "files/c.txt", "files/d.txt", "files/e.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged paths = %v, want %v (cursor must stay contiguous under the projection)", got, want)
	}
}

// TestHumanFileListOpt_FieldsUnknown400 pins the P6 failure contract: an
// unknown field is a 400 whose message names the field and lists the
// allowed set of the requested shape (the two shapes have disjoint
// allowed sets). The error is plain JSON: no ETag, no gzip.
func TestHumanFileListOpt_FieldsUnknown400(t *testing.T) {
	srv, store := listOptServer(t)
	listOptSeed(t, store, "a.txt", "hello")

	// Directory shape: "etag" belongs to the recursive shape only.
	rec := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files?fields=name,etag", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("directory fields=name,etag: status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if msg := listOptErrorMessage(t, rec); !strings.Contains(msg, `"etag"`) || !strings.Contains(msg, "name, type, size, updatedAt") {
		t.Errorf("400 message = %q, want the unknown field and the directory allowed set", msg)
	}
	if rec.Header().Get("ETag") != "" {
		t.Errorf("400 carries an ETag, want none")
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("400 carries Content-Encoding, want none (error JSON is never compressed)")
	}

	// Recursive shape: "name" belongs to the directory shape only.
	q := url.Values{}
	q.Set("recursive", "1")
	q.Set("fields", "name")
	rec2 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files?"+q.Encode(), nil, nil)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("recursive fields=name: status = %d, want 400 (body %s)", rec2.Code, rec2.Body.String())
	}
	if msg := listOptErrorMessage(t, rec2); !strings.Contains(msg, `"name"`) || !strings.Contains(msg, "path, size, etag, updatedAt") {
		t.Errorf("400 message = %q, want the unknown field and the recursive allowed set", msg)
	}
}

// TestHumanFileListOpt_FieldsDefaultUnchanged pins the red line of P6:
// without fields (or with an empty value) the response is the pre-existing
// full shape, byte-identical to today's serialization.
func TestHumanFileListOpt_FieldsDefaultUnchanged(t *testing.T) {
	srv, store := listOptServer(t)
	listOptSeed(t, store, "a.txt", "hello")
	listOptSeed(t, store, "docs/b.txt", "world!")

	rec0 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files", nil, nil)
	if rec0.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec0.Code, rec0.Body.String())
	}
	etag0 := rec0.Header().Get("ETag")

	// An empty fields value selects the default shape: byte-identical
	// body, therefore the same ETag.
	recE := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files?fields=", nil, nil)
	if recE.Code != http.StatusOK {
		t.Fatalf("fields= status = %d, want 200 (body %s)", recE.Code, recE.Body.String())
	}
	if recE.Body.String() != rec0.Body.String() {
		t.Fatalf("fields= body differs from the default:\n%s\nvs\n%s", recE.Body.String(), rec0.Body.String())
	}
	if recE.Header().Get("ETag") != etag0 {
		t.Errorf("ETag differs for an identical body: %q vs %q", recE.Header().Get("ETag"), etag0)
	}

	// The default directory entry is the full four-field form.
	var m map[string]any
	if err := json.Unmarshal(rec0.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, raw := range m["entries"].([]any) {
		if keys := listOptKeySet(raw.(map[string]any)); !listOptEqualKeys(keys, "name", "size", "type", "updatedAt") {
			t.Fatalf("default entry keys = %v, want {name size type updatedAt}", keys)
		}
	}

	// Same guarantee on the recursive shape.
	recR0 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files?recursive=1", nil, nil)
	if recR0.Code != http.StatusOK {
		t.Fatalf("recursive status = %d, want 200 (body %s)", recR0.Code, recR0.Body.String())
	}
	q := url.Values{}
	q.Set("recursive", "1")
	q.Set("fields", "")
	recRE := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files?"+q.Encode(), nil, nil)
	if recRE.Code != http.StatusOK {
		t.Fatalf("recursive fields= status = %d, want 200 (body %s)", recRE.Code, recRE.Body.String())
	}
	if recRE.Body.String() != recR0.Body.String() {
		t.Fatalf("recursive fields= body differs from the default:\n%s\nvs\n%s", recRE.Body.String(), recR0.Body.String())
	}
	var rm map[string]any
	if err := json.Unmarshal(recR0.Body.Bytes(), &rm); err != nil {
		t.Fatalf("decode recursive: %v", err)
	}
	for _, raw := range rm["entries"].([]any) {
		// size is always serialized; etag/updatedAt are omitempty in the
		// default shape (the fake storage reports both, so both appear).
		if keys := listOptKeySet(raw.(map[string]any)); !listOptEqualKeys(keys, "etag", "path", "size", "updatedAt") {
			t.Fatalf("default recursive entry keys = %v, want {path size etag updatedAt}", keys)
		}
	}
}

// TestHumanFileListOpt_GzipIdentityVsGzip pins the P8 contract: identity
// and gzip requests of the same representation transfer the same bytes
// (the gzip body gunzips back to the identity body), share one ETag
// (computed over the uncompressed JSON), and the Content-Length answers
// the wire body of each encoding.
func TestHumanFileListOpt_GzipIdentityVsGzip(t *testing.T) {
	srv, store := listOptServer(t)
	// A listing large enough that gzip actually shrinks the wire bytes
	// (small bodies can be LARGER compressed).
	for i := 0; i < 50; i++ {
		listOptSeed(t, store, fmt.Sprintf("notes/paper-%02d.txt", i), strings.Repeat("x", 40))
	}
	const path = "/api/v1/humans/alice/files?recursive=1"

	recID := listOptDo(t, srv, http.MethodGet, path, nil, nil)
	if recID.Code != http.StatusOK {
		t.Fatalf("identity status = %d, want 200 (body %s)", recID.Code, recID.Body.String())
	}
	if ce := recID.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("identity carries Content-Encoding %q", ce)
	}
	if got, want := recID.Header().Get("Content-Length"), strconv.Itoa(recID.Body.Len()); got != want {
		t.Errorf("identity Content-Length = %q, want the raw length %s", got, want)
	}
	etagID := recID.Header().Get("ETag")
	if !listOptETagRe.MatchString(etagID) {
		t.Fatalf("identity ETag = %q, want a quoted 32-hex sha256-128", etagID)
	}

	recGZ := listOptDo(t, srv, http.MethodGet, path, map[string]string{"Accept-Encoding": "gzip"}, nil)
	if recGZ.Code != http.StatusOK {
		t.Fatalf("gzip status = %d, want 200 (body %q)", recGZ.Code, recGZ.Body.String())
	}
	if ce := recGZ.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("gzip Content-Encoding = %q, want gzip", ce)
	}
	if got := listOptGunzip(t, recGZ.Body.Bytes()); !bytes.Equal(got, recID.Body.Bytes()) {
		t.Fatalf("gunzipped body is not the identity body")
	}
	if got, want := recGZ.Header().Get("Content-Length"), strconv.Itoa(recGZ.Body.Len()); got != want {
		t.Errorf("gzip Content-Length = %q, want the compressed length %s (the inner uncompressed value must be replaced)", got, want)
	}
	if recGZ.Body.Len() >= recID.Body.Len() {
		t.Errorf("gzip did not save bytes: %d >= %d", recGZ.Body.Len(), recID.Body.Len())
	}
	if recGZ.Header().Get("ETag") != etagID {
		t.Errorf("ETag differs by encoding: identity %q vs gzip %q", etagID, recGZ.Header().Get("ETag"))
	}
	if got := recGZ.Header().Get("Vary"); got != "Authorization, Accept-Encoding" {
		t.Errorf("gzip Vary = %q, want the merged form", got)
	}
}

// TestHumanFileListOpt_Gzip304NoBody pins requirement 4: a revalidation
// hit is a 304 with no body, no Content-Encoding, Content-Length 0, and
// the current ETag — even when the request offered gzip.
func TestHumanFileListOpt_Gzip304NoBody(t *testing.T) {
	srv, store := listOptServer(t)
	listOptSeed(t, store, "a.txt", "hello")

	rec1 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files", map[string]string{"Accept-Encoding": "gzip"}, nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200 (body %s)", rec1.Code, rec1.Body.String())
	}
	etag := rec1.Header().Get("ETag")
	if !listOptETagRe.MatchString(etag) {
		t.Fatalf("ETag = %q, want a quoted 32-hex sha256-128", etag)
	}

	rec2 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files", map[string]string{
		"Accept-Encoding": "gzip",
		"If-None-Match":   etag,
	}, nil)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("replay status = %d, want 304 (body %s)", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 body = %q, want empty", rec2.Body.String())
	}
	if ce := rec2.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("304 carries Content-Encoding %q, want none", ce)
	}
	if got := rec2.Header().Get("Content-Length"); got != "0" {
		t.Errorf("304 Content-Length = %q, want 0", got)
	}
	if got := rec2.Header().Get("ETag"); got != etag {
		t.Errorf("304 ETag = %q, want the current validator %q", got, etag)
	}
	if got := rec2.Header().Get("Vary"); got != "Authorization, Accept-Encoding" {
		t.Errorf("304 Vary = %q, want the merged form", got)
	}
}

// TestHumanFileListOpt_GzipSyncEndpoints pins the P8 boundary on the sync
// surface: both endpoints are gzip-only (no generic ETag middleware), and
// the manifest keeps its OWN storage-bound ETag — the generic middleware
// would re-validate the serialized JSON and mask the storage validator.
func TestHumanFileListOpt_GzipSyncEndpoints(t *testing.T) {
	srv, store := listOptServer(t)
	manifest := `{"version":1,"generation":3,"entries":{"files/a.txt":{"size":5}},"tombstones":[],"updatedAt":"2026-11-09T00:00:00Z","updatedBy":"alice"}`
	if err := store.PutObject(context.Background(), "humans/alice/.sync/manifest.json", []byte(manifest)); err != nil {
		t.Fatal(err)
	}

	t.Run("manifest", func(t *testing.T) {
		rec1 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/sync/manifest", map[string]string{"Accept-Encoding": "gzip"}, nil)
		if rec1.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec1.Code, rec1.Body.String())
		}
		if ce := rec1.Header().Get("Content-Encoding"); ce != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", ce)
		}
		if got := listOptGunzip(t, rec1.Body.Bytes()); string(got) != manifest {
			t.Fatalf("gunzipped manifest = %q, want the stored bytes verbatim", got)
		}
		// The storage validator is the content MD5 of the stored bytes —
		// NOT the sha256-128 of the JSON serialization the generic
		// middleware would compute.
		wantETag := `"` + md5Hex([]byte(manifest)) + `"`
		if got := rec1.Header().Get("ETag"); got != wantETag {
			t.Fatalf("ETag = %q, want %q (the storage validator)", got, wantETag)
		}
		if got := rec1.Header().Get("Vary"); got != "Authorization, Accept-Encoding" {
			t.Errorf("Vary = %q, want the merged form", got)
		}
		if got, want := rec1.Header().Get("Content-Length"), strconv.Itoa(rec1.Body.Len()); got != want {
			t.Errorf("Content-Length = %q, want the compressed length %s", got, want)
		}

		// The manifest's own If-None-Match path answers 304: no body, no
		// encoding, the storage validator retained.
		rec2 := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/sync/manifest", map[string]string{
			"Accept-Encoding": "gzip",
			"If-None-Match":   wantETag,
		}, nil)
		if rec2.Code != http.StatusNotModified {
			t.Fatalf("replay status = %d, want 304 (body %q)", rec2.Code, rec2.Body.String())
		}
		if rec2.Body.Len() != 0 {
			t.Errorf("304 body = %q, want empty", rec2.Body.String())
		}
		if ce := rec2.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("304 carries Content-Encoding %q, want none", ce)
		}
		if got := rec2.Header().Get("Content-Length"); got != "0" {
			t.Errorf("304 Content-Length = %q, want 0", got)
		}
		if got := rec2.Header().Get("ETag"); got != wantETag {
			t.Errorf("304 ETag = %q, want the storage validator %q", got, wantETag)
		}
	})

	t.Run("status", func(t *testing.T) {
		rec := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/sync/status", map[string]string{"Accept-Encoding": "gzip"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
			t.Fatalf("Content-Encoding = %q, want gzip", ce)
		}
		var st syncStatus
		if err := json.Unmarshal(listOptGunzip(t, rec.Body.Bytes()), &st); err != nil {
			t.Fatalf("gunzipped status does not decode: %v", err)
		}
		if st.Entries != 1 || st.Tombstones != 0 || st.Generation != 3 || st.UpdatedBy != "alice" {
			t.Fatalf("status = %+v, want entries 1 / tombstones 0 / generation 3 / alice", st)
		}
		if etag := rec.Header().Get("ETag"); etag != "" {
			t.Errorf("status carries ETag %q, want none (gzip-only route)", etag)
		}
		if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("Vary = %q, want Accept-Encoding (no inner ETag layer to merge with)", got)
		}
	})
}

// TestHumanFileListOpt_FileContentGetUnaffected pins the red line: the
// file-content GET (readSingleFile) takes none of the listing pipeline —
// offering gzip changes nothing (no Content-Encoding, the Vary stays the
// single Authorization token), and its own content-bound ETag / Range
// behavior is intact.
func TestHumanFileListOpt_FileContentGetUnaffected(t *testing.T) {
	srv, store := listOptServer(t)
	content := "hello world" // 11 bytes
	listOptSeed(t, store, "note.txt", content)

	rec := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files/note.txt", map[string]string{"Accept-Encoding": "gzip"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != content {
		t.Fatalf("body = %q, want %q", rec.Body.String(), content)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Fatalf("file content carries Content-Encoding %q, want none (the route takes no gzip layer)", ce)
	}
	if got := rec.Header().Get("Vary"); got != "Authorization" {
		t.Fatalf("Vary = %q, want the bare Authorization token", got)
	}
	if got, want := rec.Header().Get("ETag"), `"`+md5Hex([]byte(content))+`"`; got != want {
		t.Errorf("ETag = %q, want %q (the content-bound validator)", got, want)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}

	// Range behavior is intact: a partial read still 206s with the
	// correct slice and Content-Range.
	recR := listOptDo(t, srv, http.MethodGet, "/api/v1/humans/alice/files/note.txt", map[string]string{"Range": "bytes=0-2"}, nil)
	if recR.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206 (body %q)", recR.Code, recR.Body.String())
	}
	if recR.Body.String() != "hel" {
		t.Fatalf("range body = %q, want %q", recR.Body.String(), "hel")
	}
	if got := recR.Header().Get("Content-Range"); got != "bytes 0-2/11" {
		t.Errorf("Content-Range = %q, want bytes 0-2/11", got)
	}
}
