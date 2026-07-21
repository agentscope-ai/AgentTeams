package service

import (
	"context"
	"fmt"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakeclient "k8s.io/client-go/kubernetes/fake"
)

// TestProvisioner_EnsureServiceAccount_StampsControllerLabel verifies that
// Worker and Manager SAs created by the Provisioner carry
// agentteams.io/controller so peer instances do not treat them as their own.
func TestProvisioner_EnsureServiceAccount_StampsControllerLabel(t *testing.T) {
	client := fakeclient.NewSimpleClientset()
	p := NewProvisioner(ProvisionerConfig{
		K8sClient:      client,
		Namespace:      "hiclaw",
		ResourcePrefix: authpkg.ResourcePrefix("agentteams-"),
		ControllerName: "ctl-b",
	})

	if err := p.EnsureServiceAccount(context.Background(), "alice"); err != nil {
		t.Fatalf("EnsureServiceAccount: %v", err)
	}
	sa, err := client.CoreV1().ServiceAccounts("hiclaw").Get(context.Background(), "agentteams-worker-alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get SA: %v", err)
	}
	if got := sa.Labels[v1beta1.LabelController]; got != "ctl-b" {
		t.Fatalf("worker SA: expected controller label ctl-b, got %q (labels=%v)", got, sa.Labels)
	}

	if err := p.EnsureManagerServiceAccount(context.Background(), "default"); err != nil {
		t.Fatalf("EnsureManagerServiceAccount: %v", err)
	}
	mgrSA, err := client.CoreV1().ServiceAccounts("hiclaw").Get(context.Background(), "agentteams-manager", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get manager SA: %v", err)
	}
	if got := mgrSA.Labels[v1beta1.LabelController]; got != "ctl-b" {
		t.Fatalf("manager SA: expected controller label ctl-b, got %q (labels=%v)", got, mgrSA.Labels)
	}
}

// --- Mock types for remote SA tests ---

// fakeServiceAccountClient implements backend.K8sServiceAccountClient for testing.
type fakeServiceAccountClient struct {
	created   map[string]*corev1.ServiceAccount
	deleted   []string
	createErr error
	deleteErr error
}

func newFakeServiceAccountClient() *fakeServiceAccountClient {
	return &fakeServiceAccountClient{created: make(map[string]*corev1.ServiceAccount)}
}

func (f *fakeServiceAccountClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ServiceAccount, error) {
	if sa, ok := f.created[name]; ok {
		return sa, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, name)
}

func (f *fakeServiceAccountClient) Create(_ context.Context, sa *corev1.ServiceAccount, _ metav1.CreateOptions) (*corev1.ServiceAccount, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created[sa.Name] = sa
	return sa, nil
}

func (f *fakeServiceAccountClient) Delete(_ context.Context, name string, _ metav1.DeleteOptions) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, name)
	delete(f.created, name)
	return nil
}

type fakeNamespaceClient struct {
	created map[string]*corev1.Namespace
}

func newFakeNamespaceClient() *fakeNamespaceClient {
	return &fakeNamespaceClient{created: make(map[string]*corev1.Namespace)}
}

func (f *fakeNamespaceClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.Namespace, error) {
	if ns, ok := f.created[name]; ok {
		return ns, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, name)
}

func (f *fakeNamespaceClient) Create(_ context.Context, ns *corev1.Namespace, _ metav1.CreateOptions) (*corev1.Namespace, error) {
	f.created[ns.Name] = ns
	return ns, nil
}

// fakeCoreClient implements backend.K8sCoreClient for testing.
type fakeCoreClient struct {
	saClient *fakeServiceAccountClient
	nsClient *fakeNamespaceClient
}

func (f *fakeCoreClient) Pods(_ string) backend.K8sPodClient                       { return nil }
func (f *fakeCoreClient) ConfigMaps(_ string) backend.K8sConfigMapClient           { return nil }
func (f *fakeCoreClient) Services(_ string) backend.K8sServiceClient               { return nil }
func (f *fakeCoreClient) Namespaces() backend.K8sNamespaceClient                   { return f.nsClient }
func (f *fakeCoreClient) ServiceAccounts(_ string) backend.K8sServiceAccountClient { return f.saClient }
func (f *fakeCoreClient) TokenReviews() backend.K8sTokenReviewClient               { return nil }

// fakeRemoteClientProvider implements backend.RemoteClientProvider for testing.
type fakeRemoteClientProvider struct {
	clients map[string]backend.K8sCoreClient
}

func (f *fakeRemoteClientProvider) ResolveClient(_ context.Context, clusterID string) (backend.K8sCoreClient, error) {
	if cli, ok := f.clients[clusterID]; ok {
		return cli, nil
	}
	return nil, fmt.Errorf("cluster %q not found", clusterID)
}

// --- Remote SA tests ---

func TestEnsureRemoteNamespace(t *testing.T) {
	nsClient := newFakeNamespaceClient()
	coreClient := &fakeCoreClient{nsClient: nsClient}
	remoteProvider := &fakeRemoteClientProvider{
		clients: map[string]backend.K8sCoreClient{"cluster-a": coreClient},
	}

	p := NewProvisioner(ProvisionerConfig{
		Namespace:      "hiclaw",
		ResourcePrefix: authpkg.ResourcePrefix("agentteams-"),
		RemoteCache:    remoteProvider,
	})

	if err := p.EnsureRemoteNamespace(context.Background(), "cluster-a", "remote-ns"); err != nil {
		t.Fatalf("EnsureRemoteNamespace: %v", err)
	}
	if _, ok := nsClient.created["remote-ns"]; !ok {
		t.Fatal("expected remote-ns namespace to be created")
	}
	if err := p.EnsureRemoteNamespace(context.Background(), "cluster-a", "remote-ns"); err != nil {
		t.Fatalf("EnsureRemoteNamespace existing: %v", err)
	}

	p2 := NewProvisioner(ProvisionerConfig{
		Namespace:      "hiclaw",
		ResourcePrefix: authpkg.ResourcePrefix("agentteams-"),
	})
	if err := p2.EnsureRemoteNamespace(context.Background(), "cluster-a", "remote-ns"); err == nil {
		t.Fatal("expected error when remoteCache is nil")
	}
	if err := p.EnsureRemoteNamespace(context.Background(), "unknown-cluster", "remote-ns"); err == nil {
		t.Fatal("expected error for unknown cluster")
	}
}

func TestEnsureRemoteServiceAccount(t *testing.T) {
	saClient := newFakeServiceAccountClient()
	coreClient := &fakeCoreClient{saClient: saClient, nsClient: newFakeNamespaceClient()}
	remoteProvider := &fakeRemoteClientProvider{
		clients: map[string]backend.K8sCoreClient{"cluster-a": coreClient},
	}

	p := NewProvisioner(ProvisionerConfig{
		Namespace:      "hiclaw",
		ResourcePrefix: authpkg.ResourcePrefix("agentteams-"),
		RemoteCache:    remoteProvider,
	})

	// Test successful creation.
	if err := p.EnsureRemoteServiceAccount(context.Background(), "bob", "cluster-a", "remote-ns"); err != nil {
		t.Fatalf("EnsureRemoteServiceAccount: %v", err)
	}
	if _, ok := saClient.created["agentteams-worker-bob"]; !ok {
		t.Fatal("expected SA agentteams-worker-bob to be created")
	}

	// Test idempotency: AlreadyExists returns nil.
	saClient.createErr = apierrors.NewAlreadyExists(schema.GroupResource{Resource: "serviceaccounts"}, "agentteams-worker-bob")
	if err := p.EnsureRemoteServiceAccount(context.Background(), "bob", "cluster-a", "remote-ns"); err != nil {
		t.Fatalf("expected nil on AlreadyExists, got: %v", err)
	}

	// Test error when remote cache is nil.
	p2 := NewProvisioner(ProvisionerConfig{
		Namespace:      "hiclaw",
		ResourcePrefix: authpkg.ResourcePrefix("agentteams-"),
	})
	if err := p2.EnsureRemoteServiceAccount(context.Background(), "bob", "cluster-a", "remote-ns"); err == nil {
		t.Fatal("expected error when remoteCache is nil")
	}

	// Test error when cluster not found.
	if err := p.EnsureRemoteServiceAccount(context.Background(), "bob", "unknown-cluster", "ns"); err == nil {
		t.Fatal("expected error for unknown cluster")
	}
}

func TestDeleteRemoteServiceAccount(t *testing.T) {
	saClient := newFakeServiceAccountClient()
	saClient.created["agentteams-worker-bob"] = &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "agentteams-worker-bob"},
	}
	coreClient := &fakeCoreClient{saClient: saClient}
	remoteProvider := &fakeRemoteClientProvider{
		clients: map[string]backend.K8sCoreClient{"cluster-a": coreClient},
	}

	p := NewProvisioner(ProvisionerConfig{
		Namespace:      "hiclaw",
		ResourcePrefix: authpkg.ResourcePrefix("agentteams-"),
		RemoteCache:    remoteProvider,
	})

	// Test successful deletion.
	if err := p.DeleteRemoteServiceAccount(context.Background(), "bob", "cluster-a", "remote-ns"); err != nil {
		t.Fatalf("DeleteRemoteServiceAccount: %v", err)
	}
	if len(saClient.deleted) != 1 || saClient.deleted[0] != "agentteams-worker-bob" {
		t.Fatalf("expected agentteams-worker-bob in deleted list, got %v", saClient.deleted)
	}

	// Test idempotency: NotFound returns nil.
	saClient.deleteErr = apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, "agentteams-worker-bob")
	if err := p.DeleteRemoteServiceAccount(context.Background(), "bob", "cluster-a", "remote-ns"); err != nil {
		t.Fatalf("expected nil on NotFound, got: %v", err)
	}

	// Test error when remote cache is nil.
	p2 := NewProvisioner(ProvisionerConfig{
		Namespace:      "hiclaw",
		ResourcePrefix: authpkg.ResourcePrefix("agentteams-"),
	})
	if err := p2.DeleteRemoteServiceAccount(context.Background(), "bob", "cluster-a", "remote-ns"); err == nil {
		t.Fatal("expected error when remoteCache is nil")
	}
}

// Ensure unused imports are used.
var _ = authenticationv1.TokenReview{}
