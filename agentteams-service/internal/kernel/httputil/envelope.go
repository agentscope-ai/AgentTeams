// Package httputil implements the service HTTP layer: the 58 §2 error
// envelope, the 11 frozen error codes, JSON writing, and the spec-view
// ETag (semantics byte-aligned with agentteams-controller
// internal/httputil/etag.go, re-implemented — the two are separate modules).
package httputil

import (
	"encoding/json"
	"net/http"
)

// ErrorEnvelope is the unified error body (58 §2).
type ErrorEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The 11 frozen error codes (58 §2). No 422.
const (
	CodeUnauthenticated     = "unauthenticated"      // 401
	CodeInvalidRequest      = "invalid_request"      // 400
	CodeInvalidName         = "invalid_name"         // 400
	CodeDeprecatedRuntime   = "deprecated_runtime"   // 400
	CodeInvalidEnv          = "invalid_env"          // 400
	CodeForbiddenField      = "forbidden_field"      // 400 (现网 400，非 403)
	CodeForbidden           = "forbidden"            // 403
	CodeNotFound            = "not_found"            // 404
	CodeAlreadyExists       = "already_exists"       // 409
	CodeIdempotencyConflict = "idempotency_conflict" // 409
	CodeInternal            = "internal"             // 500
	CodeUpstreamUnavailable = "upstream_unavailable" // 502
)

// WriteJSON encodes v with a trailing newline (json.Encoder), matching the
// controller's WriteJSON so 2xx bodies stay byte-comparable in G1.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		// Headers already sent; nothing sensible left to do.
		return
	}
}

// WriteError writes the unified {code,message} envelope.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorEnvelope{Code: code, Message: message})
}

// RequestID returns the inbound X-Request-Id, or an empty string.
func RequestID(r *http.Request) string { return r.Header.Get("X-Request-Id") }
