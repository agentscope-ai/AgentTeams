package credprovider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newMockServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/issue" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestHTTPClient_Issue_FullSTS(t *testing.T) {
	srv := newMockServer(t, 200, `{
	  "access_key_id":"STS.AK","access_key_secret":"SK","security_token":"TOK",
	  "expiration":"2099-01-01T00:00:00Z","expires_in_sec":3600
	}`)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, nil)
	tok, err := c.Issue(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok.SecurityToken != "TOK" {
		t.Fatalf("SecurityToken = %q, want TOK", tok.SecurityToken)
	}
}

func sampleReq() IssueRequest {
	return IssueRequest{
		SessionName: "agentteams-controller",
		Entries: []AccessEntry{
			{
				Service:     ServiceObjectStorage,
				Permissions: []string{"read", "write", "list"},
				Scope:       AccessScope{Bucket: "hiclaw-test", Prefixes: []string{"*"}},
			},
		},
	}
}

// Regression: mock-credential-provider passthrough mode returns an empty
// security_token together with a raw AK/SK pair. Prior to the 2026-04 fix,
// HTTPClient rejected this as "incomplete credentials" and the controller
// initializer got stuck in a silent retry loop on waitForOSS.
func TestHTTPClient_Issue_EmptySecurityTokenAccepted(t *testing.T) {
	srv := newMockServer(t, 200, `{
	  "access_key_id":"LTAI.AK","access_key_secret":"SK","security_token":"",
	  "expiration":"2099-01-01T00:00:00Z","expires_in_sec":3600
	}`)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, nil)
	tok, err := c.Issue(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("expected passthrough response to be accepted, got error: %v", err)
	}
	if tok.AccessKeyID != "LTAI.AK" || tok.SecurityToken != "" {
		t.Fatalf("unexpected token shape: %+v", tok)
	}
}

func TestHTTPClient_Issue_MissingAK(t *testing.T) {
	srv := newMockServer(t, 200, `{
	  "access_key_id":"","access_key_secret":"SK","security_token":"",
	  "expires_in_sec":3600
	}`)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, nil)
	if _, err := c.Issue(context.Background(), sampleReq()); err == nil {
		t.Fatalf("expected error for missing access_key_id")
	} else if !strings.Contains(err.Error(), "incomplete credentials") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPClient_Issue_Non2xx(t *testing.T) {
	srv := newMockServer(t, 502, `{"error":"upstream down"}`)
	defer srv.Close()

	c := NewHTTPClient(srv.URL, nil)
	if _, err := c.Issue(context.Background(), sampleReq()); err == nil {
		t.Fatalf("expected non-2xx to surface as an error")
	} else if !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected status code in error, got: %v", err)
	}
}
