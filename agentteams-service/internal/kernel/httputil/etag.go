package httputil

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// ETagCacheControl mirrors the controller's live value byte for byte
// (agentteams-controller internal/httputil/etag.go:18). 58 previously
// quoted a no-space variant; the implementation is the source of truth
// (58 §11 I15) — a header-level G1 difference otherwise.
const ETagCacheControl = "private, max-age=0, must-revalidate"

// StrongETag is the spec-only ETag: first 16 bytes of SHA-256 of the exact
// response body, hex — a pure function of the serialized bytes, identical
// to the controller's ETagMiddleware, so a controller ETag validates the
// service representation of the same body (G1).
func StrongETag(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

// BareETag normalizes one entity-tag to its bare opaque form (strip W/
// and quotes), mirroring the controller.
func BareETag(raw string) string {
	tag := strings.TrimPrefix(strings.TrimSpace(raw), "W/")
	return strings.Trim(tag, `"`)
}

// IfNoneMatchMatches implements weak comparison (RFC 7232 §3.2): comma
// list, "*" matches, W/ prefix tolerated. etag is the bare validator.
func IfNoneMatchMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	for _, part := range strings.Split(ifNoneMatch, ",") {
		tag := strings.TrimSpace(part)
		if tag == "" {
			continue
		}
		if tag == "*" {
			return true
		}
		if BareETag(tag) == etag {
			return true
		}
	}
	return false
}

// etagBuffer captures a handler's response body in memory without sending
// it. Header() is deliberately NOT intercepted — the handler writes
// response headers straight into the real header map, which the
// middleware finalizes before the deferred WriteHeader.
type etagBuffer struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (b *etagBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *etagBuffer) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}

// ETagMiddleware adds a strong ETag to a handler's 200 JSON responses and
// answers a repeat request offering the current validator via
// If-None-Match with 304 (Content-Length: 0, no body). Every ETag-protected
// response carries Vary: Authorization + the frozen Cache-Control.
func ETagMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bw := &etagBuffer{ResponseWriter: w}
		next.ServeHTTP(bw, r)
		if bw.status == 0 {
			bw.status = http.StatusOK
		}

		etag := ""
		if bw.status == http.StatusOK && isJSONContentType(bw.Header().Get("Content-Type")) {
			sum := sha256.Sum256(bw.buf.Bytes())
			etag = hex.EncodeToString(sum[:16])
		}

		h := w.Header()
		if etag != "" {
			h.Set("Vary", "Authorization")
			h.Set("Cache-Control", ETagCacheControl)
			h.Set("ETag", `"`+etag+`"`)
		}

		if etag != "" && IfNoneMatchMatches(r.Header.Get("If-None-Match"), etag) {
			h.Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if etag != "" {
			h.Set("Content-Length", strconv.Itoa(bw.buf.Len()))
		}
		w.WriteHeader(bw.status)
		if bw.buf.Len() > 0 {
			if _, err := w.Write(bw.buf.Bytes()); err != nil {
				return
			}
		}
	})
}

func isJSONContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}
