package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credprovider"
)

func TestNewNacosAIClient_UnsupportedAuthType(t *testing.T) {
	_, err := NewNacosAIClient(context.Background(), "127.0.0.1:8848", "public", "not-a-mode", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unsupported nacos auth type") {
		t.Fatalf("got: %v", err)
	}
}

func TestNewNacosAIClient_ExplicitNacos_RequiresCredsInAddrOrEnv(t *testing.T) {
	t.Setenv("AGENTTEAMS_NACOS_USERNAME", "")
	t.Setenv("AGENTTEAMS_NACOS_PASSWORD", "")

	_, err := NewNacosAIClient(context.Background(), "127.0.0.1:8848", "public", nacosAuthTypeNacos, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "nacos auth requires username and password") {
		t.Fatalf("got: %v", err)
	}
}

func TestNewNacosAIClient_PreflightFails_WhenUnreachable(t *testing.T) {
	_, err := NewNacosAIClient(context.Background(), "127.0.0.1:19999", "public", nacosAuthTypeNone, nil)
	if err == nil {
		t.Fatal("expected connection error")
	}
	if !strings.Contains(err.Error(), "failed to connect") {
		t.Fatalf("got: %v", err)
	}
}

func TestNewNacosAIClient_ExplicitNone_Succeeds(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewNacosAIClient(context.Background(), u.Host, "public", nacosAuthTypeNone, nil)
	if err != nil {
		t.Fatalf("NewNacosAIClient: %v", err)
	}
}

func TestNewNacosAIClient_AutoDetect_None_WhenNoCredentials(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(s.Close)

	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c, err := NewNacosAIClient(ctx, u.Host, "public", "", nil)
	if err != nil {
		t.Fatalf("expected auto none + preflight to succeed: %v", err)
	}
	_ = c
}

func TestNewNacosAIClient_STS_RequiresCredClient(t *testing.T) {
	_, err := NewNacosAIClient(context.Background(), "127.0.0.1:8848", "public", nacosAuthTypeSTS, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "sts-hiclaw auth requires a credprovider.Client") {
		t.Fatalf("got: %v", err)
	}
}

// recordingIssueClient captures the last IssueRequest for assertions.
type recordingIssueClient struct {
	last credprovider.IssueRequest
}

func (r *recordingIssueClient) Issue(ctx context.Context, req credprovider.IssueRequest) (*credprovider.IssueResponse, error) {
	r.last = req
	return &credprovider.IssueResponse{
		AccessKeyID:     "test-ak",
		AccessKeySecret: "test-sk",
		SecurityToken:   "test-sts",
		ExpiresInSec:    3600,
	}, nil
}

// GetKubeconfig is a stub to satisfy the credprovider.Client interface; this
// test only exercises the STS issue path.
func (r *recordingIssueClient) GetKubeconfig(_ context.Context, _ string) (*credprovider.KubeconfigResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func TestNewNacosAIClient_STS_IssueUsesAIRegistryForNamespace(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	rec := &recordingIssueClient{}
	_, err = NewNacosAIClient(ctx, u.Host, "my-nacos-ns", nacosAuthTypeSTS, rec)
	if err != nil {
		t.Fatalf("NewNacosAIClient: %v", err)
	}

	if _, err := uuid.Parse(rec.last.SessionName); err != nil {
		t.Errorf("SessionName = %q, want UUID: %v", rec.last.SessionName, err)
	}
	if len(rec.last.SessionName) != 36 {
		t.Errorf("SessionName length = %d, want 36", len(rec.last.SessionName))
	}
	if len(rec.last.Entries) != 1 {
		t.Fatalf("Entries: %+v", rec.last.Entries)
	}
	if rec.last.Entries[0].Service != credprovider.ServiceAIRegistry {
		t.Errorf("Service = %q", rec.last.Entries[0].Service)
	}
	if rec.last.Entries[0].Scope.NamespaceID != "my-nacos-ns" {
		t.Errorf("Scope.NamespaceID = %q", rec.last.Entries[0].Scope.NamespaceID)
	}
	if len(rec.last.Entries[0].Permissions) != 2 ||
		rec.last.Entries[0].Permissions[0] != "read" ||
		rec.last.Entries[0].Permissions[1] != "list" {
		t.Errorf("Permissions = %+v, want [read list]", rec.last.Entries[0].Permissions)
	}
	if len(rec.last.Entries[0].Scope.Resources) != 2 ||
		rec.last.Entries[0].Scope.Resources[0] != "agentSpec/*" ||
		rec.last.Entries[0].Scope.Resources[1] != "skill/*" {
		t.Errorf("Scope.Resources = %+v, want [agentSpec/* skill/*]", rec.last.Entries[0].Scope.Resources)
	}
}

func TestNewNacosAIClient_STS_UsesCustomResources(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}

	rec := &recordingIssueClient{}
	_, err = NewNacosAIClient(
		context.Background(),
		u.Host,
		"my-nacos-ns",
		nacosAuthTypeSTS,
		rec,
		WithNacosSTSResources([]string{"skill/demo"}),
	)
	if err != nil {
		t.Fatalf("NewNacosAIClient: %v", err)
	}

	got := rec.last.Entries[0].Scope.Resources
	if len(got) != 1 || got[0] != "skill/demo" {
		t.Fatalf("Scope.Resources = %+v, want [skill/demo]", got)
	}
}
