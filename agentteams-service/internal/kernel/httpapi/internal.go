package httpapi

import (
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/httputil"
)

// InternalAuth guards the controller→service internal routes
// (/internal/v1/workers/status): shared bearer token plus an
// X-AT-Timestamp inside the ±window replay window and an X-AT-Request-Id.
//
// The shared token is compared in constant time. The replay window is a
// coarse clock-skew guard, not a nonce store: R1 accepts the same timestamp
// being replayed within the window (an idempotent status write makes that
// harmless); HMAC dual keys are the R2 enhancement.
func InternalAuth(token string, window time.Duration, now func() time.Time, next http.Handler) http.Handler {
	if now == nil {
		now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		want := "Bearer " + token
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			httputil.WriteError(w, http.StatusUnauthorized, httputil.CodeUnauthenticated,
				"internal bearer token required")
			return
		}
		ts := r.Header.Get("X-AT-Timestamp")
		when, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			httputil.WriteError(w, http.StatusUnauthorized, httputil.CodeUnauthenticated,
				"X-AT-Timestamp is required (RFC3339)")
			return
		}
		d := now().Sub(when)
		if d < 0 {
			d = -d
		}
		if window > 0 && d > window {
			httputil.WriteError(w, http.StatusUnauthorized, httputil.CodeUnauthenticated,
				"X-AT-Timestamp outside the replay window")
			return
		}
		if r.Header.Get("X-AT-Request-Id") == "" {
			httputil.WriteError(w, http.StatusBadRequest, httputil.CodeInvalidRequest,
				"X-AT-Request-Id is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
