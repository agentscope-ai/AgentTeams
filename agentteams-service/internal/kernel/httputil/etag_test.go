package httputil

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStrongETagIsBodyDigestPrefix(t *testing.T) {
	body := []byte("{\"workers\":[],\"total\":0}\n")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:16])
	if got := StrongETag(body); got != want {
		t.Fatalf("StrongETag = %q, want %q", got, want)
	}
	if len(StrongETag(body)) != 32 {
		t.Fatalf("ETag must be 32 hex chars")
	}
	if StrongETag(body) == StrongETag([]byte("other")) {
		t.Fatalf("different bodies must not share an ETag")
	}
}

func TestIfNoneMatchMatches(t *testing.T) {
	cases := []struct {
		header string
		etag   string
		want   bool
	}{
		{"", "abc", false},
		{"abc", "abc", true}, // BareETag tolerates an unquoted echo
		{"\"abc\"", "abc", true},
		{"W/\"abc\"", "abc", true},
		{"*", "abc", true},
		{"\"x\", \"abc\"", "abc", true},
		{"\"x\"", "abc", false},
	}
	for _, tc := range cases {
		if got := IfNoneMatchMatches(tc.header, tc.etag); got != tc.want {
			t.Errorf("IfNoneMatchMatches(%q,%q) = %v, want %v", tc.header, tc.etag, got, tc.want)
		}
	}
}

func TestETagMiddlewareAddsValidatorsAnd304(t *testing.T) {
	h := ETagMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, "\"") {
		t.Fatalf("ETag = %q, want quoted strong validator", etag)
	}
	if rec.Header().Get("Vary") != "Authorization" {
		t.Fatalf("Vary = %q", rec.Header().Get("Vary"))
	}
	if rec.Header().Get("Cache-Control") != ETagCacheControl {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("304 status = %d", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("304 body = %q, want empty", rec2.Body.String())
	}
}

func TestETagMiddlewareSkipsNonJSONAndNon200(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		ct     string
	}{
		"non-json": {200, "plain", "text/plain"},
		"error":    {500, "{}", "application/json"},
	} {
		t.Run(name, func(t *testing.T) {
			h := ETagMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Header().Get("ETag") != "" {
				t.Fatalf("unexpected ETag on %s/%d", tc.ct, tc.status)
			}
		})
	}
}

func TestWriteErrorAndJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusForbidden, CodeForbidden, "nope")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "{\"code\":\"forbidden\",\"message\":\"nope\"}\n" {
		t.Fatalf("body = %q", got)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestRequestID(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if RequestID(r) != "" {
		t.Fatalf("empty request id = %q", RequestID(r))
	}
	r.Header.Set("X-Request-Id", "abc")
	if RequestID(r) != "abc" {
		t.Fatalf("request id = %q", RequestID(r))
	}
}
