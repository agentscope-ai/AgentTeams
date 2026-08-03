package remoteclient

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/credprovider"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testKubeconfig is a minimal-but-valid kubeconfig that clientcmd accepts
// and from which corev1client.NewForConfig can build a CoreV1Client. It
// never connects to the fake server because the cache tests only inspect
// metadata (entry.Expiration / entry.Client wrapper); no API calls fire.
const testKubeconfig = `apiVersion: v1
clusters:
- cluster:
    server: https://localhost:6443
    insecure-skip-tls-verify: true
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
kind: Config
users:
- name: test
  user:
    token: test-token
`

// fakeCredClient is a stub credprovider.Client whose GetKubeconfig
// returns a configurable response and counts invocations.
type fakeCredClient struct {
	kubeconfig string
	expiration string
	err        error
	calls      int32
}

func (f *fakeCredClient) Issue(ctx context.Context, req credprovider.IssueRequest) (*credprovider.IssueResponse, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeCredClient) GetKubeconfig(ctx context.Context, clusterID string) (*credprovider.KubeconfigResponse, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}
	return &credprovider.KubeconfigResponse{
		ClusterID:  clusterID,
		Kubeconfig: f.kubeconfig,
		Expiration: f.expiration,
	}, nil
}

func (f *fakeCredClient) callCount() int { return int(atomic.LoadInt32(&f.calls)) }

// newTestScheme builds a scheme containing the v1beta1 CRD types so the
// fake controller-runtime client can List Workers/Teams.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

// newTestCache returns a Cache wired with the supplied credprovider stub
// and an empty controller-runtime fake client (no Workers/Teams).
func newTestCache(t *testing.T, cred *fakeCredClient) *Cache {
	t.Helper()
	scheme := newTestScheme(t)
	ctrl := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewCache(CacheConfig{
		CredClient: cred,
		CtrlClient: ctrl,
		Scheme:     scheme,
	})
	// Disable /healthz probe so unit tests do not require a reachable API server.
	c.skipHealthCheck = true
	return c
}

// futureRFC3339 returns an RFC3339 timestamp d ahead of now.
func futureRFC3339(d time.Duration) string {
	return time.Now().Add(d).UTC().Format(time.RFC3339)
}

func TestCache_GetOrCreate_CreatesAndCaches(t *testing.T) {
	cred := &fakeCredClient{
		kubeconfig: testKubeconfig,
		expiration: futureRFC3339(24 * time.Hour),
	}
	c := newTestCache(t, cred)

	entry, err := c.GetOrCreate(context.Background(), "c-1")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if entry == nil || entry.Client == nil {
		t.Fatalf("expected non-nil entry/client")
	}
	if entry.ClusterID != "c-1" {
		t.Errorf("ClusterID = %q, want c-1", entry.ClusterID)
	}
	if cred.callCount() != 1 {
		t.Errorf("cred.GetKubeconfig calls = %d, want 1", cred.callCount())
	}

	// Second call returns cached entry without hitting credprovider.
	entry2, err := c.GetOrCreate(context.Background(), "c-1")
	if err != nil {
		t.Fatalf("GetOrCreate (2nd): %v", err)
	}
	if entry2 != entry {
		t.Errorf("expected same cached entry on second call")
	}
	if cred.callCount() != 1 {
		t.Errorf("cred.GetKubeconfig calls after second GetOrCreate = %d, want 1", cred.callCount())
	}
}

func TestCache_GetOrCreate_ExpiredTriggersRefresh(t *testing.T) {
	cred := &fakeCredClient{
		kubeconfig: testKubeconfig,
		expiration: futureRFC3339(24 * time.Hour),
	}
	c := newTestCache(t, cred)

	// First fetch — caches.
	if _, err := c.GetOrCreate(context.Background(), "c-1"); err != nil {
		t.Fatalf("initial GetOrCreate: %v", err)
	}

	// Force the cached entry to be expired.
	c.mu.Lock()
	c.entries["c-1"].Expiration = time.Now().Add(-1 * time.Minute)
	c.mu.Unlock()

	// Update credprovider to issue a fresh kubeconfig.
	cred.expiration = futureRFC3339(48 * time.Hour)

	if _, err := c.GetOrCreate(context.Background(), "c-1"); err != nil {
		t.Fatalf("refresh GetOrCreate: %v", err)
	}
	if cred.callCount() != 2 {
		t.Errorf("cred.GetKubeconfig calls = %d, want 2 (refresh)", cred.callCount())
	}
}

func TestCache_GetOrCreate_PropagatesProviderError(t *testing.T) {
	cred := &fakeCredClient{err: errors.New("sts down")}
	c := newTestCache(t, cred)

	if _, err := c.GetOrCreate(context.Background(), "c-bad"); err == nil {
		t.Fatal("expected error when credprovider fails")
	}
	// No entry should be cached on failure.
	c.mu.RLock()
	_, exists := c.entries["c-bad"]
	c.mu.RUnlock()
	if exists {
		t.Error("failed buildEntry must not leave an entry in the cache")
	}
}

func TestCache_Remove(t *testing.T) {
	cred := &fakeCredClient{
		kubeconfig: testKubeconfig,
		expiration: futureRFC3339(time.Hour),
	}
	c := newTestCache(t, cred)
	if _, err := c.GetOrCreate(context.Background(), "c-1"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	c.Remove("c-1")

	c.mu.RLock()
	_, exists := c.entries["c-1"]
	c.mu.RUnlock()
	if exists {
		t.Error("Remove should delete entry")
	}

	// Removing an absent key is a no-op (no panic).
	c.Remove("never-existed")
}

func TestCache_ResolveClient_DelegatesToGetOrCreate(t *testing.T) {
	cred := &fakeCredClient{
		kubeconfig: testKubeconfig,
		expiration: futureRFC3339(time.Hour),
	}
	c := newTestCache(t, cred)

	cli, err := c.ResolveClient(context.Background(), "c-1")
	if err != nil {
		t.Fatalf("ResolveClient: %v", err)
	}
	if cli == nil {
		t.Fatal("expected non-nil K8sCoreClient")
	}
}

func TestCache_HasWorkersDeployed_WorkerCR(t *testing.T) {
	scheme := newTestScheme(t)
	ctrl := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewCache(CacheConfig{
		CredClient: &fakeCredClient{},
		CtrlClient: ctrl,
		Scheme:     scheme,
	})

	got, err := c.hasWorkersDeployed(context.Background(), "c-1")
	if err != nil {
		t.Fatalf("hasWorkersDeployed: %v", err)
	}
	if got {
		t.Error("expected hasWorkersDeployed=false because OSS Worker CRs do not target remote clusters")
	}

	got, err = c.hasWorkersDeployed(context.Background(), "c-other")
	if err != nil {
		t.Fatalf("hasWorkersDeployed (other): %v", err)
	}
	if got {
		t.Error("expected hasWorkersDeployed=false for non-matching cluster")
	}
}

// TestCache_Maintain_RemovesUnused exercises the maintenance loop logic
// directly (without StartMaintenanceLoop) — entries near expiration with
// no referencing Workers must be evicted.
func TestCache_Maintain_RemovesUnused(t *testing.T) {
	cred := &fakeCredClient{
		kubeconfig: testKubeconfig,
		expiration: futureRFC3339(time.Hour),
	}
	c := newTestCache(t, cred)
	if _, err := c.GetOrCreate(context.Background(), "c-stale"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Force expiration to be within the renew threshold so maintain()
	// considers it. With no Workers referencing c-stale, the entry must be
	// removed.
	c.mu.Lock()
	c.entries["c-stale"].Expiration = time.Now().Add(30 * time.Minute)
	c.mu.Unlock()

	c.maintain(context.Background())

	c.mu.RLock()
	_, exists := c.entries["c-stale"]
	c.mu.RUnlock()
	if exists {
		t.Error("maintain() should remove entries with no referencing Worker CRs")
	}
}

// TestCache_Maintain_DoesNotRenewWorkerRemoteRefs verifies that worker CRs
// no longer keep remote cluster credentials alive in the OSS Worker API.
func TestCache_Maintain_DoesNotRenewWorkerRemoteRefs(t *testing.T) {
	cred := &fakeCredClient{
		kubeconfig: testKubeconfig,
		expiration: futureRFC3339(time.Hour),
	}
	scheme := newTestScheme(t)
	ctrl := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewCache(CacheConfig{
		CredClient: cred,
		CtrlClient: ctrl,
		Scheme:     scheme,
	})
	// Disable /healthz probe so unit tests do not require a reachable API server.
	c.skipHealthCheck = true

	if _, err := c.GetOrCreate(context.Background(), "c-active"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	beforeCalls := cred.callCount()

	// Force entry near expiration; renewal should kick in.
	c.mu.Lock()
	c.entries["c-active"].Expiration = time.Now().Add(30 * time.Minute)
	c.mu.Unlock()
	cred.expiration = futureRFC3339(48 * time.Hour)

	c.maintain(context.Background())

	if cred.callCount() != beforeCalls {
		t.Errorf("remote worker refs should not trigger renewal, got %d -> %d",
			beforeCalls, cred.callCount())
	}
	c.mu.RLock()
	_, exists := c.entries["c-active"]
	c.mu.RUnlock()
	if exists {
		t.Error("unreferenced remote entry must be removed instead of renewed")
	}
}

// TestCache_Maintain_SkipsHealthy verifies that entries far from
// expiration are left untouched.
func TestCache_Maintain_SkipsHealthy(t *testing.T) {
	cred := &fakeCredClient{
		kubeconfig: testKubeconfig,
		expiration: futureRFC3339(24 * time.Hour),
	}
	c := newTestCache(t, cred)
	if _, err := c.GetOrCreate(context.Background(), "c-healthy"); err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	beforeCalls := cred.callCount()

	c.maintain(context.Background())

	if cred.callCount() != beforeCalls {
		t.Errorf("healthy entry should not trigger any credprovider call, got %d -> %d",
			beforeCalls, cred.callCount())
	}
	c.mu.RLock()
	_, exists := c.entries["c-healthy"]
	c.mu.RUnlock()
	if !exists {
		t.Error("healthy entry must remain in the cache")
	}
}
