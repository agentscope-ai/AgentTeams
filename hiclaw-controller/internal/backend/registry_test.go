package backend

import (
	"context"
	"testing"
)

// mockWorkerBackend implements WorkerBackend for testing.
type mockWorkerBackend struct {
	name      string
	available bool
}

func (m *mockWorkerBackend) Name() string                     { return m.name }
func (m *mockWorkerBackend) DeploymentMode() string           { return DeployLocal }
func (m *mockWorkerBackend) Available(_ context.Context) bool { return m.available }
func (m *mockWorkerBackend) NeedsCredentialInjection() bool   { return false }
func (m *mockWorkerBackend) Create(_ context.Context, _ CreateRequest) (*WorkerResult, error) {
	return nil, nil
}
func (m *mockWorkerBackend) Delete(_ context.Context, _ string) error { return nil }
func (m *mockWorkerBackend) Start(_ context.Context, _ string) error  { return nil }
func (m *mockWorkerBackend) Stop(_ context.Context, _ string) error   { return nil }
func (m *mockWorkerBackend) Status(_ context.Context, _ string) (*WorkerResult, error) {
	return nil, nil
}

func TestDetectWorkerBackend_Priority(t *testing.T) {
	docker := &mockWorkerBackend{name: "docker", available: true}
	k8s := &mockWorkerBackend{name: "k8s", available: true}

	reg := NewRegistry([]WorkerBackend{docker, k8s})
	got := reg.DetectWorkerBackend(context.Background())
	if got == nil || got.Name() != "docker" {
		t.Errorf("expected docker backend (first available), got %v", got)
	}
}

func TestDetectWorkerBackend_Fallback(t *testing.T) {
	docker := &mockWorkerBackend{name: "docker", available: false}
	k8s := &mockWorkerBackend{name: "k8s", available: true}

	reg := NewRegistry([]WorkerBackend{docker, k8s})
	got := reg.DetectWorkerBackend(context.Background())
	if got == nil || got.Name() != "k8s" {
		t.Errorf("expected k8s backend (fallback), got %v", got)
	}
}

func TestDetectWorkerBackend_None(t *testing.T) {
	docker := &mockWorkerBackend{name: "docker", available: false}

	reg := NewRegistry([]WorkerBackend{docker})
	got := reg.DetectWorkerBackend(context.Background())
	if got != nil {
		t.Errorf("expected nil when no backend available, got %v", got.Name())
	}
}

func TestGetWorkerBackend_ByName(t *testing.T) {
	docker := &mockWorkerBackend{name: "docker", available: true}
	k8s := &mockWorkerBackend{name: "k8s", available: false}

	reg := NewRegistry([]WorkerBackend{docker, k8s})

	got, err := reg.GetWorkerBackend(context.Background(), "k8s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name() != "k8s" {
		t.Errorf("expected k8s, got %s", got.Name())
	}
}

func TestGetWorkerBackend_UnknownName(t *testing.T) {
	docker := &mockWorkerBackend{name: "docker", available: true}

	reg := NewRegistry([]WorkerBackend{docker})

	_, err := reg.GetWorkerBackend(context.Background(), "k8s")
	if err == nil {
		t.Error("expected error for unknown backend")
	}
}

func TestGetWorkerBackend_AutoDetect(t *testing.T) {
	docker := &mockWorkerBackend{name: "docker", available: true}

	reg := NewRegistry([]WorkerBackend{docker})

	got, err := reg.GetWorkerBackend(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name() != "docker" {
		t.Errorf("expected docker, got %s", got.Name())
	}
}

// mockServiceBackend implements both WorkerBackend and ServiceBackend
// for FindServiceBackend tests.
type mockServiceBackend struct {
	mockWorkerBackend
}

func (m *mockServiceBackend) ServiceClient(_ context.Context) (K8sServiceClient, string, error) {
	return nil, "", nil
}

func TestFindServiceBackend_Found(t *testing.T) {
	sb := &mockServiceBackend{mockWorkerBackend: mockWorkerBackend{name: "k8s", available: true}}

	reg := NewRegistry([]WorkerBackend{sb})
	got := reg.FindServiceBackend(context.Background())
	if got == nil {
		t.Fatal("expected ServiceBackend, got nil")
	}
	if got != sb {
		t.Errorf("expected the registered ServiceBackend, got different instance")
	}
}

func TestFindServiceBackend_SkipsNonService(t *testing.T) {
	// docker only implements WorkerBackend, not ServiceBackend.
	docker := &mockWorkerBackend{name: "docker", available: true}

	reg := NewRegistry([]WorkerBackend{docker})
	if got := reg.FindServiceBackend(context.Background()); got != nil {
		t.Errorf("expected nil when no backend implements ServiceBackend, got %T", got)
	}
}

func TestFindServiceBackend_SkipsUnavailable(t *testing.T) {
	sb := &mockServiceBackend{mockWorkerBackend: mockWorkerBackend{name: "k8s", available: false}}

	reg := NewRegistry([]WorkerBackend{sb})
	if got := reg.FindServiceBackend(context.Background()); got != nil {
		t.Errorf("expected nil when ServiceBackend is unavailable, got %T", got)
	}
}
