package httputil

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

var etagRe = regexp.MustCompile(`^"[0-9a-f]{32}"$`)

// jsonHandler returns a handler that writes body verbatim as
// application/json with the given status.
func jsonHandler(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func doETag(handler http.Handler, path, ifNoneMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestETag_RoundTrip304 pins the core contract (issue #1475 PR-3): the
// first request gets the body plus a strong ETag and the caching headers;
// the same request replayed with If-None-Match gets 304 with an empty
// body and Content-Length: 0.
func TestETag_RoundTrip304(t *testing.T) {
	const body = `{"items":[1,2,3]}`
	handler := ETagMiddleware(jsonHandler(http.StatusOK, body))

	rec1 := doETag(handler, "/x", "")
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec1.Code)
	}
	if rec1.Body.String() != body {
		t.Fatalf("body = %q, want %q", rec1.Body.String(), body)
	}
	etag := rec1.Header().Get("ETag")
	if !etagRe.MatchString(etag) {
		t.Fatalf("ETag = %q, want a quoted 32-hex sha256-128", etag)
	}
	if got := rec1.Header().Get("Vary"); got != "Authorization" {
		t.Errorf("Vary = %q, want Authorization", got)
	}
	if got := rec1.Header().Get("Cache-Control"); got != "private, max-age=0, must-revalidate" {
		t.Errorf("Cache-Control = %q, want the revalidate-only contract", got)
	}

	rec2 := doETag(handler, "/x", etag)
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
	if got := rec2.Header().Get("Vary"); got != "Authorization" {
		t.Errorf("304 Vary = %q, want Authorization", got)
	}
}

// TestETag_ChangesWithBody pins that the validator is a function of the
// serialized bytes: a changed body changes the ETag, and a stale validator
// then misses (200 with the fresh body, not 304).
func TestETag_ChangesWithBody(t *testing.T) {
	body := `{"v":1}`
	handler := ETagMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))

	rec1 := doETag(handler, "/x", "")
	etag1 := rec1.Header().Get("ETag")
	if !etagRe.MatchString(etag1) {
		t.Fatalf("ETag = %q, want a quoted 32-hex", etag1)
	}

	body = `{"v":2}`
	rec2 := doETag(handler, "/x", etag1)
	if rec2.Code != http.StatusOK {
		t.Fatalf("stale validator status = %d, want 200", rec2.Code)
	}
	etag2 := rec2.Header().Get("ETag")
	if etag2 == etag1 {
		t.Fatalf("ETag unchanged after the body changed: %q", etag1)
	}
	if rec2.Body.String() != `{"v":2}` {
		t.Fatalf("body = %q, want the fresh body", rec2.Body.String())
	}

	rec3 := doETag(handler, "/x", etag2)
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("current validator status = %d, want 304", rec3.Code)
	}
}

// TestETag_IfNoneMatchForms pins the validator-list parsing (RFC 7232
// §3.2): weak (W/) forms compare equal to strong ones, multi-value lists
// match on any entry, and "*" matches the current representation.
func TestETag_IfNoneMatchForms(t *testing.T) {
	const body = `{"k":"v"}`
	handler := ETagMiddleware(jsonHandler(http.StatusOK, body))
	etag := doETag(handler, "/x", "").Header().Get("ETag")
	bare := etag[1 : len(etag)-1] // unquoted hex

	for _, tc := range []struct {
		name string
		inm  string
		want int
	}{
		{name: "weak-prefix", inm: `W/"` + bare + `"`, want: http.StatusNotModified},
		{name: "multi-value-hit", inm: `"nope", W/"` + bare + `"`, want: http.StatusNotModified},
		{name: "star", inm: "*", want: http.StatusNotModified},
		{name: "multi-value-miss", inm: `"other", "nope"`, want: http.StatusOK},
		{name: "weak-miss", inm: `W/"other"`, want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doETag(handler, "/x", tc.inm)
			if rec.Code != tc.want {
				t.Fatalf("If-None-Match %q: status = %d, want %d", tc.inm, rec.Code, tc.want)
			}
		})
	}
}

// TestETag_SkipsNon200AndNonJSON pins the activation boundary: only 200
// application/json responses are protected. Error JSON, non-JSON media
// types, and non-200 JSON all pass through byte-identical without an
// ETag, and never collapse into a 304 even when the client offers a
// validator.
func TestETag_SkipsNon200AndNonJSON(t *testing.T) {
	t.Run("404-json", func(t *testing.T) {
		const body = `{"message":"nope"}`
		rec := doETag(ETagMiddleware(jsonHandler(http.StatusNotFound, body)), "/x", `"anything"`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if rec.Body.String() != body {
			t.Fatalf("body = %q, want %q", rec.Body.String(), body)
		}
		if rec.Header().Get("ETag") != "" {
			t.Errorf("ETag present on a 404: %q", rec.Header().Get("ETag"))
		}
	})

	t.Run("200-text-plain", func(t *testing.T) {
		const body = "hello"
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		})
		rec := doETag(ETagMiddleware(handler), "/x", `"anything"`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.String() != body {
			t.Fatalf("body = %q, want %q", rec.Body.String(), body)
		}
		if rec.Header().Get("ETag") != "" {
			t.Errorf("ETag present on text/plain: %q", rec.Header().Get("ETag"))
		}
	})

	t.Run("201-json", func(t *testing.T) {
		rec := doETag(ETagMiddleware(jsonHandler(http.StatusCreated, `{"ok":true}`)), "/x", "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
		if rec.Header().Get("ETag") != "" {
			t.Errorf("ETag present on a 201: %q", rec.Header().Get("ETag"))
		}
	})

	t.Run("200-json-with-charset", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		rec := doETag(ETagMiddleware(handler), "/x", "")
		if !etagRe.MatchString(rec.Header().Get("ETag")) {
			t.Fatalf("ETag = %q, want a quoted 32-hex (charset parameter must be ignored)", rec.Header().Get("ETag"))
		}
	})
}

// TestETag_HashIsTruncatedSha256 pins the algorithm: the ETag is the first
// 16 bytes of the SHA-256 of the exact body bytes, hex-encoded — not the
// full 32-byte digest.
func TestETag_HashIsTruncatedSha256(t *testing.T) {
	const body = `{"a":1}`
	rec := doETag(ETagMiddleware(jsonHandler(http.StatusOK, body)), "/x", "")
	sum := sha256.Sum256([]byte(body))
	want := `"` + hex.EncodeToString(sum[:16]) + `"`
	if got := rec.Header().Get("ETag"); got != want {
		t.Fatalf("ETag = %q, want %q (sha256-128 of the body)", got, want)
	}
}

// TestETag_NoBodyNoETag pins that a 200 without a Content-Type (handler
// wrote nothing) is left alone.
func TestETag_NoBodyNoETag(t *testing.T) {
	handler := ETagMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := doETag(handler, "/x", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("ETag") != "" {
		t.Errorf("ETag = %q, want none without a JSON body", rec.Header().Get("ETag"))
	}
}

// TestETag_DistinctBodiesDistinctETags pins the caller-scoping invariant
// (issue #1480) at the middleware level: the validator is a function of
// the exact bytes THIS call serialized, so two handlers standing in for
// two visibility scopes (different row sets) must get different ETags,
// and a foreign validator must miss against the other scope (200, full
// body) — never a 304 that would hide the wider view. The only case in
// which two callers share a validator is byte-identical bodies, where
// the 304 replays nothing the caller does not already hold. (The real
// auth-reduced bodies are pinned at the route level by
// TestRouteETag_CallerScopedValidators.)
func TestETag_DistinctBodiesDistinctETags(t *testing.T) {
	// Two visibility scopes: the wider caller sees three rows, the
	// narrower one only two.
	wide := ETagMiddleware(jsonHandler(http.StatusOK, `{"total":3,"rows":["a","b","c"]}`))
	narrow := ETagMiddleware(jsonHandler(http.StatusOK, `{"total":2,"rows":["a","b"]}`))

	recWide := doETag(wide, "/x", "")
	if recWide.Code != http.StatusOK {
		t.Fatalf("wide status = %d, want 200", recWide.Code)
	}
	recNarrow := doETag(narrow, "/x", "")
	if recNarrow.Code != http.StatusOK {
		t.Fatalf("narrow status = %d, want 200", recNarrow.Code)
	}
	etagWide := recWide.Header().Get("ETag")
	etagNarrow := recNarrow.Header().Get("ETag")
	if !etagRe.MatchString(etagWide) || !etagRe.MatchString(etagNarrow) {
		t.Fatalf("ETags = %q / %q, want quoted 32-hex each", etagWide, etagNarrow)
	}
	if etagWide == etagNarrow {
		t.Fatalf("different row sets shared one ETag: %q", etagWide)
	}

	// Cross-caller: the narrower scope's validator must MISS against the
	// wider scope — a 304 there would hide the extra rows.
	if rec := doETag(wide, "/x", etagNarrow); rec.Code != http.StatusOK {
		t.Fatalf("wide handler with the narrow validator: status = %d, want 200", rec.Code)
	}

	// The sharing boundary: byte-identical bodies are the ONLY case two
	// callers can share a validator, and the 304 it answers replays
	// nothing the caller does not already hold — safe by construction.
	sameA := ETagMiddleware(jsonHandler(http.StatusOK, `{"total":1,"rows":["a"]}`))
	sameB := ETagMiddleware(jsonHandler(http.StatusOK, `{"total":1,"rows":["a"]}`))
	etagA := doETag(sameA, "/x", "").Header().Get("ETag")
	etagB := doETag(sameB, "/x", "").Header().Get("ETag")
	if etagA != etagB {
		t.Fatalf("byte-identical bodies got different ETags: %q vs %q", etagA, etagB)
	}
	if rec := doETag(sameB, "/x", etagA); rec.Code != http.StatusNotModified {
		t.Fatalf("shared validator on identical bytes: status = %d, want 304", rec.Code)
	}
}
