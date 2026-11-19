package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type mwReviewer struct {
	id  *Identity
	err error
}

func (f mwReviewer) TokenReview(ctx context.Context, token string) (*Identity, error) {
	return f.id, f.err
}

type unavailableErr struct{}

func (unavailableErr) Error() string     { return "controller down" }
func (unavailableErr) Unavailable() bool { return true }

func TestMiddleware(t *testing.T) {
	admin := &Identity{Role: RoleAdmin, Username: "admin"}
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if FromContext(r.Context()) == nil {
			t.Errorf("handler saw no identity")
		}
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name   string
		review Reviewer
		header string
		want   int
	}{
		{"valid", mwReviewer{id: admin}, "Bearer t", http.StatusNoContent},
		{"missing", mwReviewer{id: admin}, "", http.StatusUnauthorized},
		{"invalid", mwReviewer{err: errors.New("bad token")}, "Bearer t", http.StatusUnauthorized},
		{"upstream", mwReviewer{err: unavailableErr{}}, "Bearer t", http.StatusBadGateway},
		{"lowercase scheme", mwReviewer{id: admin}, "bearer t", http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Cache{Reviewer: tc.review, TTL: time.Minute, Now: time.Now}
			h := Middleware(c)(okHandler)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]bool{
		"":            false,
		"Bearer":      false,
		"Bearer ":     false,
		"Bearer abc":  true,
		"bearer abc":  true,
		"Basic abc":   false,
		"Bearer  abc": true,
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		_, ok := BearerToken(r)
		if ok != want {
			t.Errorf("BearerToken(%q) = %v, want %v", header, ok, want)
		}
	}
}
