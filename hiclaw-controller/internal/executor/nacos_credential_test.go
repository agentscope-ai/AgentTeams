package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/hiclaw/hiclaw-controller/internal/credprovider"
)

func TestNacosSTSCredential_RefreshAndApply_SpasHeaders(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stub := stubCredForCredentialTest()
	c := newNacosSTSCredential("team-ns", stub)
	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example/nacos", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Apply(req)
	if g := req.Header.Get("Spas-AccessKey"); g != "test-ak" {
		t.Errorf("Spas-AccessKey = %q", g)
	}
	if req.Header.Get("Spas-SecurityToken") == "" {
		t.Error("expected non-empty Spas-SecurityToken")
	}
	if req.Header.Get("Spas-Signature") == "" {
		t.Error("expected non-empty Spas-Signature")
	}
	if req.Header.Get("Timestamp") == "" {
		t.Error("expected non-empty Timestamp")
	}
}

// Empty namespace: signing uses timestamp-only path (see nacos_credential Apply).
func TestNacosSTSCredential_Apply_EmptyNamespace_StillSetsHeaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stub := stubCredForCredentialTest()
	c := newNacosSTSCredential("", stub)
	if err := c.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example", nil)
	c.Apply(req)
	if req.Header.Get("Spas-AccessKey") == "" {
		t.Error("expected Spas-AccessKey")
	}
}

func TestNacosSTSCredential_Apply_WithoutRefresh_DoesNotSetSpas(t *testing.T) {
	t.Parallel()
	stub := stubCredForCredentialTest()
	c := newNacosSTSCredential("ns", stub)
	req, _ := http.NewRequest(http.MethodGet, "http://example", nil)
	c.Apply(req)
	if req.Header.Get("Spas-AccessKey") != "" {
		t.Error("expected no Spas headers when cached is nil")
	}
}

type stubTokenIssuer struct {
	out *credprovider.IssueResponse
}

func (s stubTokenIssuer) Issue(_ context.Context, _ credprovider.IssueRequest) (*credprovider.IssueResponse, error) {
	return s.out, nil
}

func stubCredForCredentialTest() credprovider.Client {
	return stubTokenIssuer{
		out: &credprovider.IssueResponse{
			AccessKeyID:     "test-ak",
			AccessKeySecret: "test-sk",
			SecurityToken:   "test-sts",
			ExpiresInSec:    3600,
		},
	}
}

func TestNacosNoneCredential_RefreshAndApply_DoesNotInjectAuth(t *testing.T) {
	t.Parallel()
	var c nacosNoneCredential
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://nacos", nil)
	c.Apply(req)
	if h := req.Header.Get("Authorization"); h != "" {
		t.Errorf("Authorization = %q, want empty", h)
	}
	if req.Header.Get("Spas-AccessKey") != "" {
		t.Error("unexpected Spas-AccessKey")
	}
}

func TestNacosUserPass_Apply_WithoutLogin_NoBearer(t *testing.T) {
	t.Parallel()
	c := &nacosUserPassCredential{serverAddr: "127.0.0.1:8848"}
	req, _ := http.NewRequest(http.MethodGet, "http://nacos", nil)
	c.Apply(req)
	if req.Header.Get("Authorization") != "" {
		t.Error("expected no Bearer before login")
	}
}

func TestNacosUserPass_tryLogin_Non200ReturnsFalse(t *testing.T) {
	// 404: tryLogin returns (false, nil) — not an HTTP transport error
	s := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(s.Close)

	c := nacosUserPassCredential{
		serverAddr: s.Listener.Addr().String(),
		username:   "u",
		password:   "p",
		httpClient: s.Client(),
	}
	form := url.Values{}
	form.Set("username", "u")
	form.Set("password", "p")
	ok, err := c.tryLogin(context.Background(), s.URL+"/nacos/v3/auth/user/login", form)
	if err != nil {
		t.Fatalf("tryLogin: %v", err)
	}
	if ok {
		t.Error("expected ok=false for non-200")
	}
}
