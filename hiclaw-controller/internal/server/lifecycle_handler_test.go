package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLifecycleSleepSetsSleepingPhase(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status:     v1beta1.WorkerStatus{Phase: "Running"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	backendStub := &stubWorkerBackend{status: backend.StatusStopped}
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/sleep", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.Sleep(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if backendStub.stopCalls != 1 {
		t.Fatalf("expected one stop call, got %d", backendStub.stopCalls)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.Phase != "Sleeping" {
		t.Fatalf("expected phase Sleeping, got %q", updated.Status.Phase)
	}
	if updated.Spec.DesiredState() != "Sleeping" {
		t.Fatalf("expected spec.state Sleeping, got %q", updated.Spec.DesiredState())
	}

	var resp WorkerLifecycleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Phase != "Sleeping" {
		t.Fatalf("expected response phase Sleeping, got %q", resp.Phase)
	}
}

func TestLifecycleWakeSetsRunningPhase(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	sleeping := "Sleeping"
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{State: &sleeping},
		Status:     v1beta1.WorkerStatus{Phase: "Sleeping"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	backendStub := &stubWorkerBackend{status: backend.StatusRunning}
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/wake", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.Wake(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.Phase != "Running" {
		t.Fatalf("expected phase Running, got %q", updated.Status.Phase)
	}
	if updated.Spec.DesiredState() != "Running" {
		t.Fatalf("expected spec.state Running, got %q", updated.Spec.DesiredState())
	}
}

func TestLifecycleEnsureReadyStartsSleepingWorker(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status:     v1beta1.WorkerStatus{Phase: "Sleeping"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	backendStub := &stubWorkerBackend{status: backend.StatusRunning}
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ensure-ready", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.EnsureReady(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.Phase != "Running" {
		t.Fatalf("expected phase Running, got %q", updated.Status.Phase)
	}
	if updated.Spec.DesiredState() != "Running" {
		t.Fatalf("expected spec.state Running, got %q", updated.Spec.DesiredState())
	}
}

func TestLifecycleReadyUpdatesWorkerLastActiveAt(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ready", strings.NewReader(`{"lastActiveAt":"2024-05-12T10:00:00+08:00"}`))
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.Ready(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.LastActiveAt != "2024-05-12T02:00:00Z" {
		t.Fatalf("lastActiveAt=%q, want UTC timestamp", updated.Status.LastActiveAt)
	}
}

func TestLifecycleReadyUpdatesTeamWorkerLastActiveAt(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/dev/ready", strings.NewReader(`{"lastActiveAt":"2024-05-12T10:00:00Z"}`))
	req.SetPathValue("name", "dev")
	req = req.WithContext(context.WithValue(req.Context(), auth.CallerKeyForTest(), &auth.CallerIdentity{
		Role:     auth.RoleTeamLeader,
		Username: "lead",
		Team:     "alpha",
	}))
	rec := httptest.NewRecorder()

	handler.Ready(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.LastActiveAt != "2024-05-12T10:00:00Z" {
		t.Fatalf("lastActiveAt=%q, want reported timestamp", updated.Status.LastActiveAt)
	}
}

func TestLifecycleHeartbeatUpdatesWorkerLastHeartbeat(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/heartbeat", strings.NewReader(`{"lastActiveAt":"2024-05-12T10:00:00+08:00"}`))
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.Heartbeat(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.LastHeartbeat == "" {
		t.Fatal("expected lastHeartbeat to be set")
	}
	if updated.Status.Phase != "Running" {
		t.Fatalf("phase=%q, want Running", updated.Status.Phase)
	}
	if updated.Status.LastActiveAt != "2024-05-12T02:00:00Z" {
		t.Fatalf("lastActiveAt=%q, want UTC timestamp", updated.Status.LastActiveAt)
	}
}

func TestLifecycleHeartbeatUpdatesTeamWorkerLastActiveAt(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "default"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/dev/heartbeat", strings.NewReader(`{"lastActiveAt":"2024-05-12T10:00:00Z"}`))
	req.SetPathValue("name", "dev")
	req = req.WithContext(context.WithValue(req.Context(), auth.CallerKeyForTest(), &auth.CallerIdentity{
		Role:     auth.RoleTeamLeader,
		Username: "lead",
		Team:     "alpha",
	}))
	rec := httptest.NewRecorder()

	handler.Heartbeat(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.LastActiveAt != "2024-05-12T10:00:00Z" {
		t.Fatalf("lastActiveAt=%q, want reported timestamp", updated.Status.LastActiveAt)
	}
	if updated.Status.LastHeartbeat == "" {
		t.Fatal("expected team worker lastHeartbeat to be set")
	}
}

func TestLifecycleHeartbeatUpdatesTeamLeaderLastActiveAt(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "web-app-lead", Namespace: "default"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", "")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/web-app-lead/heartbeat", strings.NewReader(`{"lastActiveAt":"2024-05-12T10:00:00Z"}`))
	req.SetPathValue("name", "web-app-lead")
	req = req.WithContext(context.WithValue(req.Context(), auth.CallerKeyForTest(), &auth.CallerIdentity{
		Role:     auth.RoleTeamLeader,
		Username: "web-app-lead",
		Team:     "web-app-dev",
	}))
	rec := httptest.NewRecorder()

	handler.Heartbeat(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "web-app-lead", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.LastActiveAt != "2024-05-12T10:00:00Z" {
		t.Fatalf("leader lastActiveAt=%q, want reported timestamp", updated.Status.LastActiveAt)
	}
	if updated.Status.LastHeartbeat == "" {
		t.Fatal("expected leader lastHeartbeat to be set")
	}
}

func newLifecycleTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add hiclaw scheme: %v", err)
	}
	return scheme
}

type stubWorkerBackend struct {
	status     backend.WorkerStatus
	startCalls int
	stopCalls  int
}

func (s *stubWorkerBackend) Name() string                   { return "stub" }
func (s *stubWorkerBackend) DeploymentMode() string         { return backend.DeployLocal }
func (s *stubWorkerBackend) Available(context.Context) bool { return true }
func (s *stubWorkerBackend) NeedsCredentialInjection() bool { return false }
func (s *stubWorkerBackend) Create(context.Context, backend.CreateRequest) (*backend.WorkerResult, error) {
	return nil, nil
}
func (s *stubWorkerBackend) Delete(context.Context, string) error { return nil }
func (s *stubWorkerBackend) Start(_ context.Context, _ string) error {
	s.startCalls++
	return nil
}
func (s *stubWorkerBackend) Stop(_ context.Context, _ string) error {
	s.stopCalls++
	return nil
}
func (s *stubWorkerBackend) Status(context.Context, string) (*backend.WorkerResult, error) {
	return &backend.WorkerResult{Backend: "stub", Status: s.status}, nil
}
