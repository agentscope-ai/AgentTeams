package auth

import (
	"context"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestParseSAUsername_Admin(t *testing.T) {
	id, err := DefaultResourcePrefix.ParseSAUsername("system:serviceaccount:hiclaw:agentteams-admin")
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != RoleAdmin || id.Username != "admin" {
		t.Errorf("expected admin, got %+v", id)
	}
	if id.ServiceAccountNamespace != "hiclaw" || id.ServiceAccountName != "agentteams-admin" {
		t.Errorf("unexpected service account identity: %+v", id)
	}
}

func TestParseSAUsername_Manager(t *testing.T) {
	id, err := DefaultResourcePrefix.ParseSAUsername("system:serviceaccount:hiclaw:agentteams-manager")
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != RoleManager || id.Username != "manager" {
		t.Errorf("expected manager, got %+v", id)
	}
}

func TestParseSAUsername_Worker(t *testing.T) {
	id, err := DefaultResourcePrefix.ParseSAUsername("system:serviceaccount:hiclaw:agentteams-worker-alice")
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != RoleWorker || id.Username != "alice" || id.WorkerName != "alice" {
		t.Errorf("expected worker alice, got %+v", id)
	}
	if id.ServiceAccountNamespace != "hiclaw" || id.ServiceAccountName != "agentteams-worker-alice" {
		t.Errorf("unexpected service account identity: %+v", id)
	}
}

func TestParseSAUsername_WorkerHyphenatedName(t *testing.T) {
	id, err := DefaultResourcePrefix.ParseSAUsername("system:serviceaccount:default:agentteams-worker-alpha-dev")
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
// AgentTeams instance runs with AGENTTEAMS_RESOURCE_PREFIX=teamB-. SA names coined by
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
	if id.ServiceAccountNamespace != "hiclaw" || id.ServiceAccountName != "teamB-worker-alice" {
		t.Errorf("unexpected service account identity: %+v", id)
	}

	if _, err := p.ParseSAUsername("system:serviceaccount:hiclaw:agentteams-worker-alice"); err == nil {
		t.Errorf("default-prefixed SA must not match the teamB prefix")
	}

	id, err = p.ParseSAUsername("system:serviceaccount:hiclaw:teamB-manager")
	if err != nil {
		t.Fatalf("expected teamB-manager to parse: %v", err)
	}
	if id.Role != RoleManager || id.Username != "manager" {
		t.Errorf("expected manager, got %+v", id)
	}
	if id.ServiceAccountNamespace != "hiclaw" || id.ServiceAccountName != "teamB-manager" {
		t.Errorf("unexpected service account identity: %+v", id)
	}
}

func TestSAName(t *testing.T) {
	p := DefaultResourcePrefix
	tests := []struct {
		role, name, expected string
	}{
		{RoleAdmin, "admin", "agentteams-admin"},
		{RoleManager, "manager", "agentteams-manager"},
		{RoleManager, "staging", "agentteams-manager"}, // Manager SA is shared per tenant
		{RoleWorker, "alice", "agentteams-worker-alice"},
		{RoleTeamLeader, "alpha-lead", "agentteams-worker-alpha-lead"},
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
	if p.WorkerAppLabel() != "agentteams-worker" {
		t.Errorf("WorkerAppLabel = %q", p.WorkerAppLabel())
	}
	if p.ManagerAppLabel() != "agentteams-manager" {
		t.Errorf("ManagerAppLabel = %q", p.ManagerAppLabel())
	}

	p2 := ResourcePrefix("acme-")
	if p2.WorkerAppLabel() != "acme-worker" {
		t.Errorf("WorkerAppLabel = %q", p2.WorkerAppLabel())
	}
}

func TestResourcePrefix_ManagerPodName(t *testing.T) {
	p := DefaultResourcePrefix
	if got := p.ManagerPodName("default"); got != "agentteams-manager" {
		t.Errorf("ManagerPodName(default) = %q, want agentteams-manager", got)
	}
	if got := p.ManagerPodName("staging"); got != "agentteams-manager-staging" {
		t.Errorf("ManagerPodName(staging) = %q, want agentteams-manager-staging", got)
	}
}

func TestResourcePrefix_EmptyFallsBackToDefault(t *testing.T) {
	var p ResourcePrefix
	if p.WorkerNamePrefix() != "agentteams-worker-" {
		t.Errorf("empty prefix should fall back to default, got %q", p.WorkerNamePrefix())
	}
}

func TestAuthenticate_UsesLocalTokenReview(t *testing.T) {
	localClient := fakeclient.NewSimpleClientset()
	localClient.Fake.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		review.Status = authenticationv1.TokenReviewStatus{
			Authenticated: true,
			User: authenticationv1.UserInfo{
				Username: "system:serviceaccount:hiclaw:agentteams-worker-alice",
			},
		}
		return true, review, nil
	})

	auth := NewTokenReviewAuthenticator(localClient, DefaultAudience, DefaultResourcePrefix)

	id, err := auth.Authenticate(context.Background(), "local-token")
	if err != nil {
		t.Fatalf("Authenticate local: %v", err)
	}
	if id.Role != RoleWorker || id.Username != "alice" {
		t.Fatalf("expected worker alice, got %+v", id)
	}
	if id.ServiceAccountNamespace != "hiclaw" || id.ServiceAccountName != "agentteams-worker-alice" {
		t.Fatalf("unexpected identity metadata: %+v", id)
	}
}
