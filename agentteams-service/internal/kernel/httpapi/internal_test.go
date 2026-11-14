package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInternalAuth(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC) }
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := InternalAuth("secret", 5*time.Minute, now, ok)

	mk := func(token, ts, rid string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/internal/v1/workers/status", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if ts != "" {
			r.Header.Set("X-AT-Timestamp", ts)
		}
		if rid != "" {
			r.Header.Set("X-AT-Request-Id", rid)
		}
		return r
	}
	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"ok", mk("secret", now().Format(time.RFC3339), "r1"), http.StatusNoContent},
		{"bad token", mk("wrong", now().Format(time.RFC3339), "r1"), http.StatusUnauthorized},
		{"missing token", mk("", now().Format(time.RFC3339), "r1"), http.StatusUnauthorized},
		{"bad timestamp", mk("secret", "not-a-time", "r1"), http.StatusUnauthorized},
		{"stale timestamp", mk("secret", now().Add(-6*time.Minute).Format(time.RFC3339), "r1"), http.StatusUnauthorized},
		{"future within window", mk("secret", now().Add(4*time.Minute).Format(time.RFC3339), "r1"), http.StatusNoContent},
		{"missing request id", mk("secret", now().Format(time.RFC3339), ""), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tc.req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestMountRoutesThroughKernelMiddleware(t *testing.T) {
	s := New(nil)
	s.MountFunc("GET /api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "pong" {
		t.Fatalf("mount = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatalf("kernel middleware did not stamp the request id")
	}
}
