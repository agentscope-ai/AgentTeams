package httputil

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// GzipMiddleware serves a handler's 200 JSON responses as
// Content-Encoding: gzip to callers whose Accept-Encoding explicitly
// allows it, and as identity to everyone else.
//
// It is the OUTER layer of the list-optimization pipeline ("gzip outer,
// ETag inner"): the inner ETagMiddleware computes its validator over the
// UNCOMPRESSED JSON, so gzip and identity requests of the same
// representation share one ETag, and a revalidation that the inner layer
// answers 304 (no body) leaves this layer nothing to compress — a 304
// therefore never carries Content-Encoding (RFC 9110 §15.4.5).
//
// Like ETagMiddleware it buffers the whole body before the final headers
// go out, so it must only wrap endpoints with bounded JSON responses —
// never file downloads or streaming routes.
//
// Every response of a wrapped route carries "Vary: Accept-Encoding",
// merged with the tokens the inner layers already declared (never
// overwriting them): the same route serves different encodings to
// different callers, so a shared cache must key on the header.
//
// Only an explicit "gzip" token with a non-zero quality activates
// compression; a bare "*" is not treated as a gzip offer, and identity
// stays the safe default.
func GzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gz := acceptsGzipEncoding(r.Header)
		bw := &gzipBuffer{ResponseWriter: w}
		next.ServeHTTP(bw, r)
		if bw.status == 0 {
			bw.status = http.StatusOK
		}

		h := w.Header()
		addVaryValue(h, "Accept-Encoding")

		// Compression is confined to 200 JSON bodies: a 304 carries no
		// body by definition, and error JSON stays identity — the wrapped
		// routes answer with these shapes only.
		if gz && bw.status == http.StatusOK &&
			isJSONContentType(bw.Header().Get("Content-Type")) && bw.buf.Len() > 0 {
			var compressed bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&compressed, gzip.DefaultCompression)
			_, _ = zw.Write(bw.buf.Bytes())
			_ = zw.Close()
			// The inner ETag layer already fixed Content-Length to the
			// UNCOMPRESSED byte count; the header must answer the wire
			// body, so it is replaced, not kept.
			h.Set("Content-Encoding", "gzip")
			h.Set("Content-Length", strconv.Itoa(compressed.Len()))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(compressed.Bytes())
			return
		}

		// Identity and every non-compressible shape (304, error JSON,
		// non-JSON) pass through verbatim. The Content-Length always
		// answers the FINAL body: for a 304 it is 0 — the same value the
		// inner ETag layer already wrote.
		h.Set("Content-Length", strconv.Itoa(bw.buf.Len()))
		w.WriteHeader(bw.status)
		if bw.buf.Len() > 0 {
			_, _ = io.Copy(w, &bw.buf)
		}
	})
}

// acceptsGzipEncoding reports whether the request's Accept-Encoding header
// explicitly offers gzip with a non-zero quality. A bare "*" is not an
// explicit gzip offer: the middleware is per-route opt-in and identity is
// the safe default.
func acceptsGzipEncoding(h http.Header) bool {
	for _, part := range strings.Split(h.Get("Accept-Encoding"), ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		if i := strings.IndexByte(token, ';'); i >= 0 {
			if q, ok := qualityOf(token[i+1:]); ok && q == 0 {
				continue
			}
			token = strings.TrimSpace(token[:i])
		}
		if strings.EqualFold(token, "gzip") {
			return true
		}
	}
	return false
}

// qualityOf extracts the q= value of a parameter list ("q=0.5"); it
// reports ok=false when the list carries no parseable q parameter (the
// default quality is 1, so the token stays active).
func qualityOf(params string) (float64, bool) {
	for _, p := range strings.Split(params, ",") {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(strings.ToLower(p), "q=") {
			continue
		}
		q, err := strconv.ParseFloat(p[len("q="):], 64)
		if err != nil {
			continue
		}
		return q, true
	}
	return 0, false
}

// addVaryValue appends value to the response's Vary header, preserving the
// tokens the inner handler or middleware already declared and never
// duplicating value (case-insensitive token compare).
func addVaryValue(h http.Header, value string) {
	for _, existing := range h.Values("Vary") {
		for _, tok := range strings.Split(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), value) {
				return
			}
		}
	}
	if cur := h.Get("Vary"); cur == "" {
		h.Set("Vary", value)
	} else {
		h.Set("Vary", cur+", "+value)
	}
}

// gzipBuffer captures a handler's response body in memory without sending
// it: WriteHeader/Write are deferred until GzipMiddleware decides between
// the gzip and the identity encoding. Header() is deliberately NOT
// intercepted — the handler writes response headers straight into the
// real header map, which the middleware finalizes (Content-Encoding /
// Content-Length / Vary) before the deferred WriteHeader.
type gzipBuffer struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (b *gzipBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *gzipBuffer) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}
