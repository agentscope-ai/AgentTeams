package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default", nil)

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
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default", nil)

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
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default", nil)

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

func TestLifecycleReadyUpdatesLastHeartbeat(t *testing.T) {
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
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", nil)

	before := time.Now().UTC()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ready", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.Ready(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	if !handler.isReady("alpha-dev") {
		t.Fatal("expected worker to be marked ready in memory")
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.LastHeartbeat == "" {
		t.Fatal("expected LastHeartbeat to be set")
	}
	ts, err := time.Parse(time.RFC3339, updated.Status.LastHeartbeat)
	if err != nil {
		t.Fatalf("parse LastHeartbeat: %v", err)
	}
	if ts.Before(before.Add(-time.Second)) || ts.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("LastHeartbeat out of range: %v", ts)
	}
}

func TestLifecycleReadyUpdatesLLMCountsWithJSONBody(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status: v1beta1.WorkerStatus{
			Phase:         "Running",
			LLMCallsTotal: 5,
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", nil)

	before := time.Now().UTC()
	body := strings.NewReader(`{"llmCallsSinceLastHeartbeat":12}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ready", body)
	req.Header.Set("Content-Type", "application/json")
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
	if updated.Status.LastHeartbeat == "" {
		t.Fatal("expected LastHeartbeat to be set")
	}
	ts, err := time.Parse(time.RFC3339, updated.Status.LastHeartbeat)
	if err != nil {
		t.Fatalf("parse LastHeartbeat: %v", err)
	}
	if ts.Before(before.Add(-time.Second)) || ts.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("LastHeartbeat out of range: %v", ts)
	}
	if updated.Status.LLMCallsLastHeartbeat != 12 {
		t.Fatalf("LLMCallsLastHeartbeat = %d, want 12", updated.Status.LLMCallsLastHeartbeat)
	}
	if updated.Status.LLMCallsTotal != 17 {
		t.Fatalf("LLMCallsTotal = %d, want 17", updated.Status.LLMCallsTotal)
	}
}

func TestLifecycleReadyIgnoresNegativeLLMCount(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status: v1beta1.WorkerStatus{
			Phase:                 "Running",
			LLMCallsLastHeartbeat: 3,
			LLMCallsTotal:         10,
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", nil)

	body := strings.NewReader(`{"llmCallsSinceLastHeartbeat":-5}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ready", body)
	req.Header.Set("Content-Type", "application/json")
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
	if updated.Status.LastHeartbeat == "" {
		t.Fatal("expected LastHeartbeat to be set despite negative LLM count")
	}
	if updated.Status.LLMCallsLastHeartbeat != 3 {
		t.Fatalf("LLMCallsLastHeartbeat = %d, want 3 (unchanged)", updated.Status.LLMCallsLastHeartbeat)
	}
	if updated.Status.LLMCallsTotal != 10 {
		t.Fatalf("LLMCallsTotal = %d, want 10 (unchanged)", updated.Status.LLMCallsTotal)
	}
}

func TestLifecycleReadyStill204OnStatusUpdateFailure(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status:     v1beta1.WorkerStatus{Phase: "Running"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker).
		Build()
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ready", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.Ready(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	if !handler.isReady("alpha-dev") {
		t.Fatal("expected worker to remain ready in memory when status update fails")
	}
}

func TestLifecycleReadyRetriesStatusUpdateOnConflict(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status: v1beta1.WorkerStatus{
			Phase:         "Running",
			LLMCallsTotal: 4,
		},
	}

	inner := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()

	statusUpdates := 0
	k8sClient := interceptor.NewClient(inner, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, _ client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" {
				statusUpdates++
				if statusUpdates <= 2 {
					return apierrors.NewConflict(
						schema.GroupResource{Group: "agentteams.io", Resource: "workers"},
						worker.Name,
						errors.New("simulated conflict"),
					)
				}
			}
			return inner.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	})

	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default", nil)

	body := strings.NewReader(`{"llmCallsSinceLastHeartbeat":6}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ready", body)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.Ready(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	if statusUpdates != 3 {
		t.Fatalf("status update attempts = %d, want 3 (2 conflicts + success)", statusUpdates)
	}

	var updated v1beta1.Worker
	if err := inner.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if updated.Status.LLMCallsLastHeartbeat != 6 {
		t.Fatalf("LLMCallsLastHeartbeat = %d, want 6", updated.Status.LLMCallsLastHeartbeat)
	}
	if updated.Status.LLMCallsTotal != 10 {
		t.Fatalf("LLMCallsTotal = %d, want 10", updated.Status.LLMCallsTotal)
	}
}

func TestWorkerToResponseIncludesHeartbeatFields(t *testing.T) {
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev"},
		Status: v1beta1.WorkerStatus{
			Phase:                 "Running",
			LastHeartbeat:         "2026-07-17T10:00:00Z",
			LastActiveAt:          "2026-07-17T09:30:00Z",
			LLMCallsLastHeartbeat: 12,
			LLMCallsTotal:         340,
		},
	}

	resp := workerToResponse(worker)
	if resp.LastHeartbeat != "2026-07-17T10:00:00Z" {
		t.Fatalf("lastHeartbeat = %q, want 2026-07-17T10:00:00Z", resp.LastHeartbeat)
	}
	if resp.LastActiveAt != "2026-07-17T09:30:00Z" {
		t.Fatalf("lastActiveAt = %q, want 2026-07-17T09:30:00Z", resp.LastActiveAt)
	}
	if resp.LLMCallsLastHeartbeat != 12 {
		t.Fatalf("llmCallsLastHeartbeat = %d, want 12", resp.LLMCallsLastHeartbeat)
	}
	if resp.LLMCallsTotal != 340 {
		t.Fatalf("llmCallsTotal = %d, want 340", resp.LLMCallsTotal)
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
