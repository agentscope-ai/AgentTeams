package auth

import (
	"context"
	"fmt"
	"testing"

	"github.com/hiclaw/hiclaw-controller/internal/backend"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclient "k8s.io/client-go/kubernetes/fake"
)

func TestParseSAUsername_Admin(t *testing.T) {
	id, err := DefaultResourcePrefix.ParseSAUsername("system:serviceaccount:hiclaw:hiclaw-admin")
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != RoleAdmin || id.Username != "admin" {
		t.Errorf("expected admin, got %+v", id)
	}
}

func TestParseSAUsername_Manager(t *testing.T) {
	id, err := DefaultResourcePrefix.ParseSAUsername("system:serviceaccount:hiclaw:hiclaw-manager")
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != RoleManager || id.Username != "manager" {
		t.Errorf("expected manager, got %+v", id)
	}
}

func TestParseSAUsername_Worker(t *testing.T) {
	id, err := DefaultResourcePrefix.ParseSAUsername("system:serviceaccount:hiclaw:hiclaw-worker-alice")
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != RoleWorker || id.Username != "alice" || id.WorkerName != "alice" {
		t.Errorf("expected worker alice, got %+v", id)
	}
}

func TestParseSAUsername_WorkerHyphenatedName(t *testing.T) {
	id, err := DefaultResourcePrefix.ParseSAUsername("system:serviceaccount:default:hiclaw-worker-alpha-dev")
	if err != nil {
		t.Fatal(err)
	}
	if id.Username != "alpha-dev" {
		t.Errorf("expected alpha-dev, got %q", id.Username)
	}
}

func TestParseSAUsername_InvalidFormat(t *testing.T) {
	for _, input := range []string{
		"",
		"admin",
		"system:serviceaccount:hiclaw",
		"system:serviceaccount:hiclaw:unknown-sa",
	} {
		if _, err := DefaultResourcePrefix.ParseSAUsername(input); err == nil {
			t.Errorf("expected error for %q", input)
		}
	}
}

// TestParseSAUsername_CustomPrefix covers the multi-tenant case where a second
// HiClaw instance runs with HICLAW_RESOURCE_PREFIX=teamB-. SA names coined by
// the other prefix must be unrecognized, and names coined by the local prefix
// must round-trip cleanly.
func TestParseSAUsername_CustomPrefix(t *testing.T) {
	p := ResourcePrefix("teamB-")

	id, err := p.ParseSAUsername("system:serviceaccount:hiclaw:teamB-worker-alice")
	if err != nil {
		t.Fatalf("expected teamB-worker-alice to parse: %v", err)
	}
	if id.Role != RoleWorker || id.Username != "alice" {
		t.Errorf("expected worker alice, got %+v", id)
	}

	if _, err := p.ParseSAUsername("system:serviceaccount:hiclaw:hiclaw-worker-alice"); err == nil {
		t.Errorf("default-prefixed SA must not match the teamB prefix")
	}

	id, err = p.ParseSAUsername("system:serviceaccount:hiclaw:teamB-manager")
	if err != nil {
		t.Fatalf("expected teamB-manager to parse: %v", err)
	}
	if id.Role != RoleManager || id.Username != "manager" {
		t.Errorf("expected manager, got %+v", id)
	}
}

func TestSAName(t *testing.T) {
	p := DefaultResourcePrefix
	tests := []struct {
		role, name, expected string
	}{
		{RoleAdmin, "admin", "hiclaw-admin"},
		{RoleManager, "manager", "hiclaw-manager"},
		{RoleManager, "staging", "hiclaw-manager"}, // Manager SA is shared per tenant
		{RoleWorker, "alice", "hiclaw-worker-alice"},
		{RoleTeamLeader, "alpha-lead", "hiclaw-worker-alpha-lead"},
	}
	for _, tc := range tests {
		got := p.SAName(tc.role, tc.name)
		if got != tc.expected {
			t.Errorf("SAName(%q, %q) = %q, want %q", tc.role, tc.name, got, tc.expected)
		}
	}
}

func TestSAName_CustomPrefix(t *testing.T) {
	p := ResourcePrefix("teamB-")
	if got := p.SAName(RoleWorker, "alice"); got != "teamB-worker-alice" {
		t.Errorf("worker SA = %q, want teamB-worker-alice", got)
	}
	if got := p.SAName(RoleManager, "any"); got != "teamB-manager" {
		t.Errorf("manager SA = %q, want teamB-manager", got)
	}
	if got := p.SAName(RoleAdmin, ""); got != "teamB-admin" {
		t.Errorf("admin SA = %q, want teamB-admin", got)
	}
}

func TestResourcePrefix_Labels(t *testing.T) {
	p := DefaultResourcePrefix
	if p.WorkerAppLabel() != "hiclaw-worker" {
		t.Errorf("WorkerAppLabel = %q", p.WorkerAppLabel())
	}
	if p.ManagerAppLabel() != "hiclaw-manager" {
		t.Errorf("ManagerAppLabel = %q", p.ManagerAppLabel())
	}

	p2 := ResourcePrefix("acme-")
	if p2.WorkerAppLabel() != "acme-worker" {
		t.Errorf("WorkerAppLabel = %q", p2.WorkerAppLabel())
	}
}

func TestResourcePrefix_ManagerPodName(t *testing.T) {
	p := DefaultResourcePrefix
	if got := p.ManagerPodName("default"); got != "hiclaw-manager" {
		t.Errorf("ManagerPodName(default) = %q, want hiclaw-manager", got)
	}
	if got := p.ManagerPodName("staging"); got != "hiclaw-manager-staging" {
		t.Errorf("ManagerPodName(staging) = %q, want hiclaw-manager-staging", got)
	}
}

func TestResourcePrefix_EmptyFallsBackToDefault(t *testing.T) {
	var p ResourcePrefix
	if p.WorkerNamePrefix() != "hiclaw-worker-" {
		t.Errorf("empty prefix should fall back to default, got %q", p.WorkerNamePrefix())
	}
}

// --- Mock types for remote TokenReview tests ---

// fakeTokenReviewClient implements backend.K8sTokenReviewClient.
type fakeTokenReviewClient struct {
	authenticated bool
	username      string
	err           error
}

func (f *fakeTokenReviewClient) Create(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	if f.err != nil {
		return nil, f.err
	}
	review.Status = authenticationv1.TokenReviewStatus{
		Authenticated: f.authenticated,
		User: authenticationv1.UserInfo{
			Username: f.username,
		},
	}
	if !f.authenticated {
		review.Status.Error = "token is invalid"
	}
	return review, nil
}

// fakeRemoteCoreClient implements backend.K8sCoreClient for authenticator test.
type fakeRemoteCoreClient struct {
	tokenReviewClient *fakeTokenReviewClient
}

func (f *fakeRemoteCoreClient) Pods(_ string) backend.K8sPodClient                       { return nil }
func (f *fakeRemoteCoreClient) ConfigMaps(_ string) backend.K8sConfigMapClient           { return nil }
func (f *fakeRemoteCoreClient) Services(_ string) backend.K8sServiceClient               { return nil }
func (f *fakeRemoteCoreClient) Namespaces() backend.K8sNamespaceClient                   { return nil }
func (f *fakeRemoteCoreClient) ServiceAccounts(_ string) backend.K8sServiceAccountClient { return nil }
func (f *fakeRemoteCoreClient) TokenReviews() backend.K8sTokenReviewClient {
	return f.tokenReviewClient
}

// fakeRemoteProvider implements backend.RemoteClientProvider for authenticator test.
type fakeRemoteProvider struct {
	clients map[string]backend.K8sCoreClient
}

func (f *fakeRemoteProvider) ResolveClient(_ context.Context, clusterID string) (backend.K8sCoreClient, error) {
	if cli, ok := f.clients[clusterID]; ok {
		return cli, nil
	}
	return nil, fmt.Errorf("cluster %q not found", clusterID)
}

// --- Remote Authenticate tests ---

func TestAuthenticate_RemoteCluster(t *testing.T) {
	remoteTR := &fakeTokenReviewClient{
		authenticated: true,
		username:      "system:serviceaccount:hiclaw:hiclaw-worker-alice",
	}
	remoteCli := &fakeRemoteCoreClient{tokenReviewClient: remoteTR}
	remoteProvider := &fakeRemoteProvider{
		clients: map[string]backend.K8sCoreClient{"remote-cluster": remoteCli},
	}

	localClient := fakeclient.NewSimpleClientset()
	auth := NewTokenReviewAuthenticator(localClient, DefaultAudience, DefaultResourcePrefix, remoteProvider)

	// Remote cluster path: clusterID != "" uses remote client.
	id, err := auth.Authenticate(context.Background(), "remote-token", "remote-cluster")
	if err != nil {
		t.Fatalf("Authenticate remote: %v", err)
	}
	if id.Role != RoleWorker || id.Username != "alice" {
		t.Fatalf("expected worker alice, got %+v", id)
	}
}

func TestAuthenticate_RemoteClusterRejectsAdminAndManager(t *testing.T) {
	for _, username := range []string{
		"system:serviceaccount:hiclaw:hiclaw-admin",
		"system:serviceaccount:hiclaw:hiclaw-manager",
	} {
		remoteTR := &fakeTokenReviewClient{
			authenticated: true,
			username:      username,
		}
		remoteCli := &fakeRemoteCoreClient{tokenReviewClient: remoteTR}
		remoteProvider := &fakeRemoteProvider{
			clients: map[string]backend.K8sCoreClient{"remote-cluster": remoteCli},
		}
		auth := NewTokenReviewAuthenticator(fakeclient.NewSimpleClientset(), DefaultAudience, DefaultResourcePrefix, remoteProvider)

		if _, err := auth.Authenticate(context.Background(), "remote-token-"+username, "remote-cluster"); err == nil {
			t.Fatalf("expected remote token for %s to be rejected", username)
		}
	}
}

func TestAuthenticate_RemoteCacheNil(t *testing.T) {
	// When remoteCache is nil, remote authentication should fail.
	localClient := fakeclient.NewSimpleClientset()
	auth := NewTokenReviewAuthenticator(localClient, DefaultAudience, DefaultResourcePrefix, nil)

	_, err := auth.Authenticate(context.Background(), "some-token", "remote-cluster")
	if err == nil {
		t.Fatal("expected error when remoteCache is nil and clusterID is non-empty")
	}
}

func TestAuthenticate_EmptyClusterID_UsesLocalPath(t *testing.T) {
	// When clusterID is empty, should use the local k8s client (regression test).
	// We use the fake k8s client which won't have a proper TokenReview handler,
	// so this tests that the code path goes through local client, not remote.
	localClient := fakeclient.NewSimpleClientset()

	remoteTR := &fakeTokenReviewClient{
		authenticated: true,
		username:      "system:serviceaccount:hiclaw:hiclaw-worker-remote-only",
	}
	remoteCli := &fakeRemoteCoreClient{tokenReviewClient: remoteTR}
	remoteProvider := &fakeRemoteProvider{
		clients: map[string]backend.K8sCoreClient{"remote-cluster": remoteCli},
	}

	auth := NewTokenReviewAuthenticator(localClient, DefaultAudience, DefaultResourcePrefix, remoteProvider)

	// Empty clusterID goes local. fake k8s client returns Authenticated=false
	// (TokenReview is not supported by fakeclient), so we expect an auth error.
	_, err := auth.Authenticate(context.Background(), "local-token", "")
	if err == nil {
		t.Fatal("expected error from local fake client (no TokenReview support)")
	}
	// Confirm it's NOT a "remote cluster authentication not supported" error.
	if err.Error() == "remote cluster authentication not supported" {
		t.Fatal("should not take remote path when clusterID is empty")
	}
}

// Ensure unused imports are consumed.
var (
	_ = corev1.ServiceAccount{}
)
