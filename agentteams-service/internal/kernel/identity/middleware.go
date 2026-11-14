package identity

import (
	"errors"
	"net/http"
	"strings"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
)

// Unavailable reports that a review failure was a transport/upstream problem
// rather than a rejected token. The adapter's upstream error implements it;
// the middleware answers 502 (upstream_unavailable) instead of 401 when the
// controller cannot be reached, so a controller outage never looks like a
// credential problem to the caller.
type Unavailable interface {
	Unavailable() bool
}

// Middleware authenticates the public bearer token through the review cache
// and stores the resolved identity on the request context.
//
// Contract:
//   - missing/invalid/expired token → 401 unauthenticated
//   - controller unreachable during the review → 502 upstream_unavailable
//     (the request was never authorized, and retrying may succeed)
//
// The middleware is mounted only on the public /api/v1 subtree; /healthz,
// /readyz and /internal/v1 have their own authentication.
func Middleware(c *Cache) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := BearerToken(r)
			if !ok {
				httputil.WriteError(w, http.StatusUnauthorized, httputil.CodeUnauthenticated,
					"a bearer token is required")
				return
			}
			id, err := c.Resolve(r.Context(), token)
			if err != nil {
				var u Unavailable
				if errors.As(err, &u) && u.Unavailable() {
					httputil.WriteError(w, http.StatusBadGateway, httputil.CodeUpstreamUnavailable,
						"identity resolution unavailable")
					return
				}
				httputil.WriteError(w, http.StatusUnauthorized, httputil.CodeUnauthenticated,
					"invalid or expired token")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
		})
	}
}

// BearerToken extracts the RFC 6750 bearer credential from the Authorization
// header. The scheme is case-insensitive and the token must be non-empty.
func BearerToken(r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(raw[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
