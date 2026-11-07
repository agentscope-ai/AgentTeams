package httputil

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ETagCacheControl is the caching contract for ETag-protected responses
// (issue #1475 PR-3): always revalidate against the origin, and never
// share the cached representation between users.
const ETagCacheControl = "private, max-age=0, must-revalidate"

// ETagMiddleware adds a strong ETag to a handler's 200 JSON responses and
// answers a repeat request that offers the current validator via
// If-None-Match with 304 (empty body).
//
// The ETag is the first 16 bytes of the SHA-256 of the exact response
// body, hex-encoded (32 chars): a pure function of the serialized bytes,
// so the validator and the body can never drift apart (issue #1475 PR-3).
// It is computed only for 200 responses whose Content-Type is
// application/json; every other status or media type passes through
// byte-identical with no ETag.
//
// The benefit is wire bytes, not backend work: the handler — and the
// backend queries it issues — runs in full on every request, including
// a revalidation that ends in 304; a hit only saves the transfer of an
// unchanged representation.
//
// Caller scoping is inherited, not computed: authorization runs before
// this middleware, so the handler writes a body already reduced to the
// caller's visibility, and the validator is a function of exactly those
// bytes. Callers with different visibility scopes therefore serialize
// different bytes and never share a validator; a foreign If-None-Match
// can only match when the two representations are byte-identical, in
// which case the 304 replays nothing the caller does not already hold —
// no visibility difference can leak through it.
//
// The middleware buffers the whole body before writing it out, so it must
// only wrap endpoints with bounded JSON responses — never file downloads
// or streaming routes.
//
// Every ETag-protected response (200 and 304) carries
// "Vary: Authorization": the wrapped list endpoints serve caller-scoped
// bodies, so a shared cache must never serve one caller's
// representation to another.
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
			h.Set("ETag", EtagQuoted(etag))
		}

		if etag != "" && ifNoneMatchMatches(r.Header.Get("If-None-Match"), etag) {
			// A 304 carries the validators of the current representation
			// and no body (RFC 7232 §4.1).
			h.Set("Content-Length", "0")
			w.WriteHeader(http.StatusNotModified)
			return
		}

		if etag != "" {
			h.Set("Content-Length", strconv.Itoa(bw.buf.Len()))
		}
		w.WriteHeader(bw.status)
		if bw.buf.Len() > 0 {
			_, _ = io.Copy(w, &bw.buf)
		}
	})
}

// etagBuffer captures a handler's response body in memory without sending
// it: WriteHeader/Write are deferred until ETagMiddleware decides between
// the full response and a 304. Header() is deliberately NOT intercepted —
// the handler writes response headers straight into the real header map,
// which the middleware finalizes (ETag / Vary / Cache-Control /
// Content-Length) before the deferred WriteHeader.
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

// isJSONContentType reports whether a Content-Type header names
// application/json (parameters such as "; charset=utf-8" are ignored).
func isJSONContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// ifNoneMatchMatches reports whether a raw If-None-Match header value
// (a comma-separated entity-tag list, RFC 7232 §3.2) matches etag under
// weak comparison: the special value "*" matches, and a W/"..." validator
// compares equal to its strong form.
func ifNoneMatchMatches(ifNoneMatch, etag string) bool {
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

// IfNoneMatchMatches is the exported form of the middleware's weak
// comparison (RFC 7232 §3.2): the header is a comma-separated tag list,
// "*" matches any current representation, and a W/"..." validator
// compares equal to its strong form. etag is the BARE validator (the
// storage form); the response form is what EtagQuoted renders.
func IfNoneMatchMatches(ifNoneMatch, etag string) bool {
	return ifNoneMatchMatches(ifNoneMatch, etag)
}

// EtagQuoted renders a bare (opaque) ETag in the response form RFC 9110
// requires: a quoted-string. An empty validator renders as "" (the
// caller omits the ETag header).
func EtagQuoted(opaque string) string {
	if opaque == "" {
		return ""
	}
	return `"` + opaque + `"`
}

// BareETag normalizes one entity-tag to its bare opaque form: it strips a
// leading weak prefix (W/) and the surrounding quotes. It is the inverse
// of EtagQuoted and the normalization every conditional path runs a
// client-supplied validator through before comparing it or passing it to
// storage — a client echoing the quoted ETag straight back from a GET
// must not 412. A multi-tag list comes back with only its outer quotes
// trimmed, so it can never equal a single bare validator and keeps the
// pre-existing refusal behavior.
func BareETag(raw string) string {
	tag := strings.TrimSpace(raw)
	if strings.HasPrefix(tag, "W/") {
		tag = strings.TrimPrefix(tag, "W/")
	}
	return strings.Trim(tag, `"`)
}

// IfRangeApplies evaluates an If-Range header (RFC 9110 §13.2.3) against
// the representation's bare validator and last-modified time. It answers
// whether the Range header of the same request should be honored:
//
//   - no If-Range: the range applies (there is nothing to check);
//   - entity-tag form (a quoted tag): STRONG comparison against the
//     current bare validator — a W/-prefixed tag never matches, so a
//     range gated on a weak validator is ignored, and so is any range
//     whose validator the representation does not have;
//   - HTTP-date form: the range applies only if the representation has
//     not changed since the date (modTime not after it) — a last
//     modification after the date means the client's base is stale and
//     the full current body answers;
//   - anything else that parses as neither: the range is ignored.
func IfRangeApplies(ifRange, etag string, modTime time.Time) bool {
	if ifRange == "" {
		return true
	}
	tag := strings.TrimSpace(ifRange)
	if strings.HasPrefix(tag, `"`) {
		return etag != "" && strings.Trim(tag, `"`) == etag
	}
	t, err := http.ParseTime(tag)
	if err != nil {
		return false
	}
	return !modTime.After(t)
}
