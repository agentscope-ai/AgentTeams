package controller

import (
	"context"
	"errors"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/test/testutil/mocks"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// fakeServiceClient is a stateful K8sServiceClient used by the
// member-Service reconcile tests. It records every call so tests can
// assert which Service operations fired.
type fakeServiceClient struct {
	store map[string]*corev1.Service

	getCalls    int
	createCalls int
	updateCalls int
	deleteCalls int

	deleteErr error
}

func newFakeServiceClient() *fakeServiceClient {
	return &fakeServiceClient{store: map[string]*corev1.Service{}}
}

func (f *fakeServiceClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.Service, error) {
	f.getCalls++
	if svc, ok := f.store[name]; ok {
		return svc.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, name)
}

func (f *fakeServiceClient) Create(_ context.Context, svc *corev1.Service, _ metav1.CreateOptions) (*corev1.Service, error) {
	f.createCalls++
	cp := svc.DeepCopy()
	if _, exists := f.store[cp.Name]; exists {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "services"}, cp.Name)
	}
	f.store[cp.Name] = cp
	return cp.DeepCopy(), nil
}

func (f *fakeServiceClient) Update(_ context.Context, svc *corev1.Service, _ metav1.UpdateOptions) (*corev1.Service, error) {
	f.updateCalls++
	cp := svc.DeepCopy()
	f.store[cp.Name] = cp
	return cp.DeepCopy(), nil
}

func (f *fakeServiceClient) Delete(_ context.Context, name string, _ metav1.DeleteOptions) error {
	f.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, exists := f.store[name]; !exists {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, name)
	}
	delete(f.store, name)
	return nil
}

// fakeServiceBackend implements both backend.WorkerBackend and
// backend.ServiceBackend, returning a controllable K8sServiceClient.
type fakeServiceBackend struct {
	svc *fakeServiceClient
	ns  string
}

func (f *fakeServiceBackend) Name() string                     { return "fake" }
func (f *fakeServiceBackend) DeploymentMode() string           { return backend.DeployCloud }
func (f *fakeServiceBackend) Available(_ context.Context) bool { return true }
func (f *fakeServiceBackend) NeedsCredentialInjection() bool   { return true }
func (f *fakeServiceBackend) Create(_ context.Context, _ backend.CreateRequest) (*backend.WorkerResult, error) {
	return nil, nil
}
func (f *fakeServiceBackend) Delete(_ context.Context, _ string) error { return nil }
func (f *fakeServiceBackend) Start(_ context.Context, _ string) error  { return nil }
func (f *fakeServiceBackend) Stop(_ context.Context, _ string) error   { return nil }
func (f *fakeServiceBackend) Status(_ context.Context, _ string) (*backend.WorkerResult, error) {
	return nil, nil
}
func (f *fakeServiceBackend) ServiceClient(_ context.Context, _, _, _ string) (backend.K8sServiceClient, string, error) {
	return f.svc, f.ns, nil
}

// noopWorkerBackend is a WorkerBackend that does NOT implement ServiceBackend
// — used to assert that ensureServiceDeleted gracefully tolerates such a
// backend (e.g. Docker) without erroring out.
type noopWorkerBackend struct{}

func (n *noopWorkerBackend) Name() string                     { return "noop" }
func (n *noopWorkerBackend) DeploymentMode() string           { return backend.DeployLocal }
func (n *noopWorkerBackend) Available(_ context.Context) bool { return true }
func (n *noopWorkerBackend) NeedsCredentialInjection() bool   { return false }
func (n *noopWorkerBackend) Create(_ context.Context, _ backend.CreateRequest) (*backend.WorkerResult, error) {
	return nil, nil
}
func (n *noopWorkerBackend) Delete(_ context.Context, _ string) error { return nil }
func (n *noopWorkerBackend) Start(_ context.Context, _ string) error  { return nil }
func (n *noopWorkerBackend) Stop(_ context.Context, _ string) error   { return nil }
func (n *noopWorkerBackend) Status(_ context.Context, _ string) (*backend.WorkerResult, error) {
	return nil, nil
}

func newServiceTestDeps(svc *fakeServiceClient) *MemberDeps {
	return &MemberDeps{
		Backend: backend.NewRegistry([]backend.WorkerBackend{
			&fakeServiceBackend{svc: svc, ns: "hiclaw"},
		}),
		ResourcePrefix: authpkg.DefaultResourcePrefix,
	}
}

func newServiceTestMember(name string, enabled bool, ports ...int) *MemberContext {
	expose := make([]v1beta1.ExposePort, 0, len(ports))
	for _, p := range ports {
		expose = append(expose, v1beta1.ExposePort{Port: p})
	}
	return &MemberContext{
		Name:           name,
		Spec:           v1beta1.WorkerSpec{Expose: expose},
		ServiceEnabled: enabled,
	}
}

func TestReconcileMemberService_CreatesWhenEnabled(t *testing.T) {
	svc := newFakeServiceClient()
	deps := newServiceTestDeps(svc)
	mc := newServiceTestMember("alice", true, 8080, 9090)

	if err := ReconcileMemberService(context.Background(), mc, deps); err != nil {
		t.Fatalf("ReconcileMemberService: %v", err)
	}

	stored, ok := svc.store["hiclaw-worker-alice"]
	if !ok {
		t.Fatalf("expected Service hiclaw-worker-alice to be created; got %+v", svc.store)
	}
	if svc.createCalls != 1 {
		t.Errorf("Create calls = %d, want 1", svc.createCalls)
	}
	if got := stored.Spec.Selector["hiclaw.io/worker"]; got != "alice" {
		t.Errorf("Selector hiclaw.io/worker = %q, want alice", got)
	}
	if len(stored.Spec.Ports) != 2 {
		t.Errorf("Ports len = %d, want 2", len(stored.Spec.Ports))
	}
	if stored.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service.Spec.Type = %q, want ClusterIP", stored.Spec.Type)
	}
}

func TestReconcileMemberService_SkipsWhenDisabled(t *testing.T) {
	svc := newFakeServiceClient()
	deps := newServiceTestDeps(svc)
	mc := newServiceTestMember("bob", false, 8080)

	if err := ReconcileMemberService(context.Background(), mc, deps); err != nil {
		t.Fatalf("ReconcileMemberService: %v", err)
	}
	if svc.createCalls != 0 {
		t.Errorf("expected no Service.Create calls when disabled, got %d", svc.createCalls)
	}
	// ServiceEnabled=false routes through ensureServiceDeleted which
	// performs a Get first; the Service does not exist so the fake
	// returns NotFound and the reconciler short-circuits without Delete.
	if svc.getCalls != 1 {
		t.Errorf("expected 1 Get attempt for existence check, got %d", svc.getCalls)
	}
	if svc.deleteCalls != 0 {
		t.Errorf("expected 0 Delete calls (Get returned NotFound), got %d", svc.deleteCalls)
	}
}

// TestReconcileMemberService_DeletesWhenExposeEmpty covers the edge case
// where ServiceEnabled is true but the member exposes no ports. A portless
// Service is useless, and an existing Service from a previous expose config
// must be removed.
func TestReconcileMemberService_DeletesWhenExposeEmpty(t *testing.T) {
	svc := newFakeServiceClient()
	svc.store["hiclaw-worker-carol"] = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "hiclaw-worker-carol", Namespace: "hiclaw"},
	}
	deps := newServiceTestDeps(svc)
	mc := newServiceTestMember("carol", true) // no ports

	if err := ReconcileMemberService(context.Background(), mc, deps); err != nil {
		t.Fatalf("ReconcileMemberService: %v", err)
	}
	if svc.createCalls != 0 {
		t.Errorf("Create calls = %d, want 0 when expose is empty", svc.createCalls)
	}
	if svc.deleteCalls != 1 {
		t.Errorf("Delete calls = %d, want 1 when expose is empty and Service exists", svc.deleteCalls)
	}
	if _, ok := svc.store["hiclaw-worker-carol"]; ok {
		t.Fatal("expected stale Service to be deleted when expose is empty")
	}
}

func TestReconcileMemberService_UpdatesWhenPortsDiffer(t *testing.T) {
	svc := newFakeServiceClient()
	// Pre-populate a stale Service that selects the right Pod but exposes
	// the wrong ports.
	svc.store["hiclaw-worker-dave"] = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "hiclaw-worker-dave", Namespace: "hiclaw"},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"hiclaw.io/worker": "dave"},
			Ports: []corev1.ServicePort{
				{Name: "port-1111", Port: 1111, Protocol: corev1.ProtocolTCP},
			},
		},
	}
	deps := newServiceTestDeps(svc)
	mc := newServiceTestMember("dave", true, 8080)

	if err := ReconcileMemberService(context.Background(), mc, deps); err != nil {
		t.Fatalf("ReconcileMemberService: %v", err)
	}
	if svc.updateCalls != 1 {
		t.Errorf("Update calls = %d, want 1 (ports differed)", svc.updateCalls)
	}
}

// TestEnsureServiceDeleted_NotFoundIsOK verifies that ensureServiceDeleted
// swallows an apierrors.NotFound — the desired post-condition (no Service)
// is already satisfied.
func TestEnsureServiceDeleted_NotFoundIsOK(t *testing.T) {
	svc := newFakeServiceClient()
	// Configure the fake's Delete to surface NotFound directly.
	svc.deleteErr = apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, "hiclaw-worker-eve")
	deps := newServiceTestDeps(svc)
	mc := newServiceTestMember("eve", false)

	if err := ensureServiceDeleted(context.Background(), mc, deps); err != nil {
		t.Fatalf("ensureServiceDeleted should swallow NotFound, got %v", err)
	}
}

// TestEnsureServiceDeleted_NoBackendIsTolerated covers the Docker-style
// case where the active worker backend does not implement ServiceBackend.
// ensureServiceDeleted treats the resolve-failure as a no-op so reconcile
// of a Worker with serviceEnabled=false does not block on that distinction.
func TestEnsureServiceDeleted_NoBackendIsTolerated(t *testing.T) {
	deps := &MemberDeps{
		Backend:        backend.NewRegistry([]backend.WorkerBackend{&noopWorkerBackend{}}),
		ResourcePrefix: authpkg.DefaultResourcePrefix,
	}
	mc := newServiceTestMember("frank", false)

	if err := ensureServiceDeleted(context.Background(), mc, deps); err != nil {
		t.Fatalf("ensureServiceDeleted must not error when backend lacks ServiceBackend, got %v", err)
	}
}

// TestEnsureServiceDeleted_PropagatesUnexpectedError ensures that errors
// other than NotFound bubble up so reconcile retries them.
func TestEnsureServiceDeleted_PropagatesUnexpectedError(t *testing.T) {
	svc := newFakeServiceClient()
	// Pre-populate so Get succeeds and Delete is actually attempted.
	svc.store["hiclaw-worker-grace"] = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "hiclaw-worker-grace", Namespace: "hiclaw"},
	}
	svc.deleteErr = errors.New("boom")
	deps := newServiceTestDeps(svc)
	mc := newServiceTestMember("grace", false)

	if err := ensureServiceDeleted(context.Background(), mc, deps); err == nil {
		t.Fatal("expected non-NotFound error to propagate")
	}
}

func TestReconcileMemberDelete_DeletesService(t *testing.T) {
	svc := newFakeServiceClient()
	svc.store["hiclaw-worker-heidi"] = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "hiclaw-worker-heidi", Namespace: "hiclaw"},
	}
	deps := newServiceTestDeps(svc)
	deps.Provisioner = mocks.NewMockProvisioner()
	deps.Deployer = mocks.NewMockDeployer()
	member := MemberContext{
		Name:        "heidi",
		RuntimeName: "heidi",
		Role:        RoleStandalone,
	}

	if err := ReconcileMemberDelete(context.Background(), *deps, member); err != nil {
		t.Fatalf("ReconcileMemberDelete: %v", err)
	}
	if svc.deleteCalls != 1 {
		t.Fatalf("Delete calls = %d, want 1", svc.deleteCalls)
	}
	if _, ok := svc.store["hiclaw-worker-heidi"]; ok {
		t.Fatal("expected Service to be deleted during member finalizer cleanup")
	}
}
