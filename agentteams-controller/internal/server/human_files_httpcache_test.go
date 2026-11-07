package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// --- Representation caching: ETag / 304 / Range / If-Range / HEAD --------------
//
// These tests cover the GET-path caching contract (issue #1475 PR-4):
// every response carries a strong ETag with private revalidate-only
// caching, revalidation answers 304 with no body, GET/HEAD support
// Range and If-Range, and a single-range GET opens the object exactly
// once.

// hfGetFile performs a raw GET/HEAD against the file route, applying the
// given conditional/Range headers, and returns the recorder.
func hfGetFile(t *testing.T, hfh *humanFilesHandler, name, path, method string, caller *authpkg.CallerIdentity, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := hfReq(t, method, name, path, caller, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	hfh.getFile(rec, req)
	return rec
}

// hfPutFileWithMatch is a PUT that carries the given conditional headers
// (bare or quoted If-Match).
func hfPutFileWithMatch(t *testing.T, hfh *humanFilesHandler, name, path string, caller *authpkg.CallerIdentity, content string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := hfReq(t, http.MethodPut, name, path, caller, strings.NewReader(content))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	hfh.putFile(rec, req)
	return rec
}

func TestHumanFiles_GetCachingHeaders(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	content := "hello world"
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), content); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	etag := md5Hex([]byte(content))

	rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != content {
		t.Fatalf("body = %q, want %q", rec.Body.String(), content)
	}
	h := rec.Result().Header
	if h.Get("ETag") != `"`+etag+`"` {
		t.Fatalf("ETag = %q, want %q", h.Get("ETag"), `"`+etag+`"`)
	}
	if h.Get("Cache-Control") != httputil.ETagCacheControl {
		t.Fatalf("Cache-Control = %q, want %q", h.Get("Cache-Control"), httputil.ETagCacheControl)
	}
	if h.Get("Vary") != "Authorization" {
		t.Fatalf("Vary = %q, want Authorization", h.Get("Vary"))
	}
	if h.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", h.Get("Accept-Ranges"))
	}
}

func TestHumanFiles_GetRevalidation304(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	content := "hello world"
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), content); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	etag := md5Hex([]byte(content))

	// Every form of the current validator must revalidate to 304 —
	// quoted (what the server sends), bare (the JSON/audit form), weak,
	// the wildcard, and a list containing the tag.
	for _, inm := range []string{
		`"` + etag + `"`,
		etag,
		`W/"` + etag + `"`,
		"*",
		`"other", ` + etag,
	} {
		rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), map[string]string{"If-None-Match": inm})
		if rec.Code != http.StatusNotModified {
			t.Fatalf("INM %q: got %d, want 304: %s", inm, rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("INM %q: 304 must carry no body: %q", inm, rec.Body.String())
		}
		h := rec.Result().Header
		if h.Get("ETag") != `"`+etag+`"` || h.Get("Cache-Control") != httputil.ETagCacheControl || h.Get("Vary") != "Authorization" {
			t.Fatalf("INM %q: 304 must carry the validator headers (ETag=%q, CC=%q, Vary=%q)",
				inm, h.Get("ETag"), h.Get("Cache-Control"), h.Get("Vary"))
		}
	}

	// A list that does not contain the current validator gets the full
	// current representation.
	rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), map[string]string{"If-None-Match": `"a", "b"`})
	if rec.Code != http.StatusOK || rec.Body.String() != content {
		t.Fatalf("miss: got %d %q, want 200 %q", rec.Code, rec.Body.String(), content)
	}
}

func TestHumanFiles_GetRange206(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	content := "hello world" // 11 bytes
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), content); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	etag := md5Hex([]byte(content))

	cases := []struct {
		header string
		body   string
		cre    string
	}{
		{"bytes=0-4", "hello", "bytes 0-4/11"},
		{"bytes=6-", "world", "bytes 6-10/11"},
		{"bytes=-5", "world", "bytes 6-10/11"},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), map[string]string{"Range": tc.header})
			if rec.Code != http.StatusPartialContent {
				t.Fatalf("Range %s: got %d, want 206: %s", tc.header, rec.Code, rec.Body.String())
			}
			if rec.Body.String() != tc.body {
				t.Fatalf("Range %s: body = %q, want %q", tc.header, rec.Body.String(), tc.body)
			}
			h := rec.Result().Header
			if h.Get("Content-Range") != tc.cre {
				t.Fatalf("Range %s: Content-Range = %q, want %q", tc.header, h.Get("Content-Range"), tc.cre)
			}
			if h.Get("ETag") != `"`+etag+`"` {
				t.Fatalf("Range %s: ETag = %q, want the quoted validator", tc.header, h.Get("ETag"))
			}
		})
	}
}

func TestHumanFiles_GetRange416(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), "hello world"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	for _, rangeHdr := range []string{
		"bytes=11-",     // start at size
		"bytes=100-200", // start beyond size
		"bytes=-0",      // zero-length suffix
		"bytes=9-4",     // reversed
		"items=0-4",     // unknown unit
		"bytes=0-4,6-8", // multi-range
	} {
		rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), map[string]string{"Range": rangeHdr})
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("Range %q: got %d, want 416: %s", rangeHdr, rec.Code, rec.Body.String())
		}
		if cr := rec.Result().Header.Get("Content-Range"); cr != "bytes */11" {
			t.Fatalf("Range %q: Content-Range = %q, want \"bytes */11\"", rangeHdr, cr)
		}
	}

	// An empty object satisfies no range at all.
	if rec := hfPutFile(t, hfh, "alice", "empty.txt", hfSelf(), ""); rec.Code != http.StatusOK {
		t.Fatalf("seed empty: %d %s", rec.Code, rec.Body.String())
	}
	rec := hfGetFile(t, hfh, "alice", "empty.txt", http.MethodGet, hfSelf(), map[string]string{"Range": "bytes=0-0"})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("empty + range: got %d, want 416: %s", rec.Code, rec.Body.String())
	}
	if cr := rec.Result().Header.Get("Content-Range"); cr != "bytes */0" {
		t.Fatalf("empty + range: Content-Range = %q, want \"bytes */0\"", cr)
	}
}

func TestHumanFiles_GetIfRange(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), "hello world"); rec.Code != http.StatusOK {
		t.Fatalf("seed v1: %d %s", rec.Code, rec.Body.String())
	}
	etagV1 := md5Hex([]byte("hello world"))
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), "v2 longer payload"); rec.Code != http.StatusOK {
		t.Fatalf("seed v2: %d %s", rec.Code, rec.Body.String())
	}
	etagV2 := md5Hex([]byte("v2 longer payload"))
	fullV2 := "v2 longer payload"

	// Current etag (quoted, strong): the range is honored.
	rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(),
		map[string]string{"Range": "bytes=0-3", "If-Range": `"` + etagV2 + `"`)})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "v2 l" {
		t.Fatalf("fresh etag: got %d %q, want 206 %q", rec.Code, rec.Body.String(), "v2 l")
	}

	// The weak form of the same validator is NOT strong-equal: the range
	// is ignored and the full current body answers (RFC 9110 §13.2.3).
	rec = hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(),
		map[string]string{"Range": "bytes=0-3", "If-Range": `W/"` + etagV2 + `"`)})
	if rec.Code != http.StatusOK || rec.Body.String() != fullV2 {
		t.Fatalf("weak etag: got %d %q, want 200 %q", rec.Code, rec.Body.String(), fullV2)
	}

	// A stale etag: the range is ignored, full current body.
	rec = hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(),
		map[string]string{"Range": "bytes=0-3", "If-Range": `"` + etagV1 + `"`)})
	if rec.Code != http.StatusOK || rec.Body.String() != fullV2 {
		t.Fatalf("stale etag: got %d %q, want 200 %q", rec.Code, rec.Body.String(), fullV2)
	}

	// HTTP-date form against the store write clock: a date NOT before
	// the last modification (future) keeps the client's base current and
	// honors the range; a date before it (past) means the object changed
	// after the client saw it, so the full body answers.
	lt := store.LastWriteTime()
	hdrDate := func(d time.Time) string { return d.UTC().Format(time.RFC1123) }

	rec = hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(),
		map[string]string{"Range": "bytes=0-3", "If-Range": hdrDate(lt.Add(time.Hour))})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "v2 l" {
		t.Fatalf("future date: got %d %q, want 206 %q", rec.Code, rec.Body.String(), "v2 l")
	}
	rec = hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(),
		map[string]string{"Range": "bytes=0-3", "If-Range": hdrDate(lt.Add(-time.Hour))})
	if rec.Code != http.StatusOK || rec.Body.String() != fullV2 {
		t.Fatalf("past date: got %d %q, want 200 %q", rec.Code, rec.Body.String(), fullV2)
	}
}

func TestHumanFiles_GetIfRangeNotTruncating(t *testing.T) {
	// A stale If-Range with a wide range must serve the full CURRENT
	// body — the primed hint (sized for the stale version) must never
	// truncate the response.
	hfh, _, _ := newHumanFilesRig(t)
	v1 := strings.Repeat("a", 100)
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), v1); rec.Code != http.StatusOK {
		t.Fatalf("seed v1: %d %s", rec.Code, rec.Body.String())
	}
	etagV1 := md5Hex([]byte(v1))
	v2 := strings.Repeat("b", 30)
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), v2); rec.Code != http.StatusOK {
		t.Fatalf("seed v2: %d %s", rec.Code, rec.Body.String())
	}

	rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(),
		map[string]string{"Range": "bytes=0-99", "If-Range": `"` + etagV1 + `"`)})
	if rec.Code != http.StatusOK || rec.Body.String() != v2 {
		t.Fatalf("stale If-Range: got %d (len %d), want 200 with the full 30-byte v2 body", rec.Code, rec.Body.Len())
	}
}

func TestHumanFiles_Head(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	content := "hello world"
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), content); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodHead, hfSelf(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: got %d: %s", rec.Code, rec.Body.String())
	}
	h := rec.Result().Header
	if h.Get("ETag") == "" || h.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("HEAD headers: ETag=%q Accept-Ranges=%q", h.Get("ETag"), h.Get("Accept-Ranges"))
	}
	// HEAD never primes and never reads the body: no ranged open at all.
	if got := store.OpenRangeCalls.Load(); got != 0 {
		t.Fatalf("HEAD issued %d OpenRange calls, want 0", got)
	}

	// A HEAD with a Range advertises the partial representation without
	// opening the object either.
	rec = hfGetFile(t, hfh, "alice", "note.txt", http.MethodHead, hfSelf(), map[string]string{"Range": "bytes=0-4"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("HEAD + Range: got %d, want 206", rec.Code)
	}
	if cr := rec.Result().Header.Get("Content-Range"); cr != "bytes 0-4/11" {
		t.Fatalf("HEAD + Range: Content-Range = %q, want bytes 0-4/11", cr)
	}
	if got := store.OpenRangeCalls.Load(); got != 0 {
		t.Fatalf("HEAD + Range issued %d OpenRange calls, want 0", got)
	}
}

func TestHumanFiles_GetSingleRangeOpensOnce(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	content := strings.Repeat("z", 64)
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), content); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	// A single in-window range is served from exactly one ranged open.
	rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), map[string]string{"Range": "bytes=10-29"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != strings.Repeat("z", 20) {
		t.Fatalf("GET range: got %d (len %d), want 206 with 20 bytes", rec.Code, rec.Body.Len())
	}
	if got := store.OpenRangeCalls.Load(); got != 1 {
		t.Fatalf("single range issued %d OpenRange calls, want exactly 1", got)
	}
	store.OpenRangeCalls.Store(0)

	// A plain full GET opens the object once for the 64-byte body —
	// never the old double read.
	rec = hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), nil)
	if rec.Code != http.StatusOK || rec.Body.String() != content {
		t.Fatalf("full GET: got %d (len %d), want 200 with the full body", rec.Code, rec.Body.Len())
	}
	if got := store.OpenRangeCalls.Load(); got != 1 {
		t.Fatalf("full GET issued %d OpenRange calls, want exactly 1", got)
	}
}

func TestHumanFiles_TombstoneBeatsRange(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), "hello world"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	del := httptest.NewRecorder()
	hfh.deleteFile(del, hfReq(t, http.MethodDelete, "alice", "note.txt", hfSelf(), nil))
	if del.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", del.Code, del.Body.String())
	}

	// The tombstone 404 wins over the range: a deleted file is not
	// partially served.
	rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), map[string]string{"Range": "bytes=0-4"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted file + Range: got %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestHumanFiles_StorageDownIs503(t *testing.T) {
	hfh, store, _ := newHumanFilesRig(t)
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), "hello world"); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	// StatMeta fault: any GET is 503 before any read is attempted.
	hfh.oss = newFaultOSS(t, store, "", "StatMeta")
	rec := hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), nil)
	assertStorageUnavailable(t, rec, "stat down")

	// Ranged read fault: a Range GET also 503s (the object is not
	// served from a stale prime).
	hfh.oss = newFaultOSS(t, store, "", "OpenRange")
	rec = hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), map[string]string{"Range": "bytes=0-4"})
	assertStorageUnavailable(t, rec, "ranged read down")

	// Full-body read fault: the pre-header prime (the single [0,size)
	// stream) makes a plain GET 503 too — never a 200 that dies on its
	// first byte.
	hfh.oss = newFaultOSS(t, store, "", "OpenRange")
	rec = hfGetFile(t, hfh, "alice", "note.txt", http.MethodGet, hfSelf(), nil)
	assertStorageUnavailable(t, rec, "full-body read down")
}

func TestHumanFiles_PutQuotedIfMatch(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	v1 := "first version"
	if rec := hfPutFile(t, hfh, "alice", "note.txt", hfSelf(), v1); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	etagV1 := md5Hex([]byte(v1))

	// A client echoing the QUOTED ETag it received from GET must not 412.
	v2 := "second version"
	rec := hfPutFileWithMatch(t, hfh, "alice", "note.txt", hfSelf(), v2, map[string]string{"If-Match": `"` + etagV1 + `"`)})
	if rec.Code != http.StatusOK {
		t.Fatalf("quoted If-Match: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	etagV2 := md5Hex([]byte(v2))

	// The bare form still works against the new version.
	v3 := "third version"
	rec = hfPutFileWithMatch(t, hfh, "alice", "note.txt", hfSelf(), v3, map[string]string{"If-Match": etagV2})
	if rec.Code != http.StatusOK {
		t.Fatalf("bare If-Match: got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// A stale (quoted) validator still 412s.
	rec = hfPutFileWithMatch(t, hfh, "alice", "note.txt", hfSelf(), v1, map[string]string{"If-Match": `"` + etagV1 + `"`)})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale quoted If-Match: got %d, want 412: %s", rec.Code, rec.Body.String())
	}
}

func TestHumanFiles_Manifest304(t *testing.T) {
	hfh, _, _ := newHumanFilesRig(t)
	alice := hfSyncCaller(authpkg.CapabilityFileSync)
	body := syncManifestBody([]string{"files/a.txt"}, nil)

	rec := hfPutSync(t, hfh, "alice", alice, body, map[string]string{"If-None-Match": "*"})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed manifest: %d %s", rec.Code, rec.Body.String())
	}
	etag := hfSyncJSON(t, rec)["etag"].(string)

	// The manifest GET now carries a strong ETag like the file GET.
	req := hfSyncReq(t, http.MethodGet, "alice", "manifest", alice, "")
	got := httptest.NewRecorder()
	hfh.getSyncManifest(got, req)
	if got.Code != http.StatusOK {
		t.Fatalf("manifest GET: %d %s", got.Code, got.Body.String())
	}
	if h := got.Result().Header; h.Get("ETag") != `"`+etag+`"` || h.Get("Cache-Control") != httputil.ETagCacheControl || h.Get("Vary") != "Authorization" {
		t.Fatalf("manifest GET headers: ETag=%q CC=%q Vary=%q", h.Get("ETag"), h.Get("Cache-Control"), h.Get("Vary"))
	}

	// Revalidation with the current validator: 304, no body.
	req = hfSyncReq(t, http.MethodGet, "alice", "manifest", alice, "")
	req.Header.Set("If-None-Match", `"`+etag+`"`)
	got = httptest.NewRecorder()
	hfh.getSyncManifest(got, req)
	if got.Code != http.StatusNotModified {
		t.Fatalf("manifest revalidation: got %d, want 304: %s", got.Code, got.Body.String())
	}
	if got.Body.Len() != 0 {
		t.Fatalf("manifest 304 must carry no body: %q", got.Body.String())
	}

	// The wildcard revalidates too.
	req = hfSyncReq(t, http.MethodGet, "alice", "manifest", alice, "")
	req.Header.Set("If-None-Match", "*")
	got = httptest.NewRecorder()
	hfh.getSyncManifest(got, req)
	if got.Code != http.StatusNotModified {
		t.Fatalf("manifest wildcard: got %d, want 304", got.Code)
	}

	// A stale validator gets the full current body.
	req = hfSyncReq(t, http.MethodGet, "alice", "manifest", alice, "")
	req.Header.Set("If-None-Match", `"stale"`)
	got = httptest.NewRecorder()
	hfh.getSyncManifest(got, req)
	if got.Code != http.StatusOK || got.Body.Len() == 0 {
		t.Fatalf("manifest stale INM: got %d, want 200 with the body", got.Code)
	}
}
