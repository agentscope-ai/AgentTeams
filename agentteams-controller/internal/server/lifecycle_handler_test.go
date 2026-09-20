package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
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
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default")

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
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default")

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
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default")

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

// TestLifecycleEnsureReadyProbesBackendWhenRunning covers #1260: a Running CR and a
// ready flag from an earlier self-report must not produce "Ready" once the backend
// itself says the worker is gone or cannot say anything at all.
func TestLifecycleEnsureReadyProbesBackendWhenRunning(t *testing.T) {
	tests := []struct {
		name          string
		status        backend.WorkerStatus
		statusErr     error
		wantPhase     string
		wantStarts    int
		wantReadyFlag bool
	}{
		{name: "running backend stays ready", status: backend.StatusRunning, wantPhase: "Ready", wantReadyFlag: true},
		{name: "stopped backend is started", status: backend.StatusStopped, wantPhase: "Running", wantStarts: 1},
		{name: "missing backend is started", status: backend.StatusNotFound, wantPhase: "Running", wantStarts: 1},
		{name: "starting backend is not ready yet", status: backend.StatusStarting, wantPhase: "Running", wantReadyFlag: true},
		{name: "failed backend is not ready", status: backend.StatusFailed, wantPhase: "Running", wantReadyFlag: true},
		{name: "status error fails closed", statusErr: errors.New("docker unreachable"), wantPhase: "Running", wantReadyFlag: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newLifecycleTestScheme(t)
			running := "Running"
			worker := &v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
				Spec:       v1beta1.WorkerSpec{State: &running},
				Status:     v1beta1.WorkerStatus{Phase: "Running"},
			}
			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&v1beta1.Worker{}).
				WithObjects(worker).
				Build()
			backendStub := &stubWorkerBackend{status: tt.status, statusErr: tt.statusErr}
			handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default")
			handler.setReady("alpha-dev", true)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ensure-ready", nil)
			req.SetPathValue("name", "alpha-dev")
			rec := httptest.NewRecorder()

			handler.EnsureReady(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
			}
			var resp WorkerLifecycleResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Phase != tt.wantPhase {
				t.Errorf("expected response phase %q, got %q", tt.wantPhase, resp.Phase)
			}
			if backendStub.startCalls != tt.wantStarts {
				t.Errorf("expected %d start call(s), got %d", tt.wantStarts, backendStub.startCalls)
			}
			if got := handler.isReady("alpha-dev"); got != tt.wantReadyFlag {
				t.Errorf("expected ready flag %v, got %v", tt.wantReadyFlag, got)
			}

			var updated v1beta1.Worker
			if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updated); err != nil {
				t.Fatalf("get worker: %v", err)
			}
			if updated.Status.Phase != "Running" {
				t.Errorf("expected phase Running, got %q", updated.Status.Phase)
			}
		})
	}
}

// With no backend detected there is nothing to probe, so the self-report still decides.
func TestLifecycleEnsureReadyWithoutBackendKeepsSelfReport(t *testing.T) {
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
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry(nil), "default")
	handler.setReady("alpha-dev", true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ensure-ready", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.EnsureReady(rec, req)

	var resp WorkerLifecycleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Phase != "Ready" {
		t.Fatalf("expected response phase Ready, got %q", resp.Phase)
	}
}

// A configured backend that is unreachable is not the same as no backend at
// all. Detection returns nil for both, but here the ready flag may be stale --
// the daemon went away after the worker last reported in -- so ensure-ready
// must fail closed instead of answering Ready.
func TestLifecycleEnsureReadyWithUnavailableBackendIsNotReady(t *testing.T) {
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
	backendStub := &stubWorkerBackend{unavailable: true, status: backend.StatusRunning}
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default")
	handler.setReady("alpha-dev", true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ensure-ready", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.EnsureReady(rec, req)

	var resp WorkerLifecycleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Phase != "Running" {
		t.Fatalf("expected response phase Running, got %q", resp.Phase)
	}
	if backendStub.statusCalls != 0 {
		t.Fatalf("expected no Status call on an unavailable backend, got %d", backendStub.statusCalls)
	}
}

func TestLifecycleWorkerStatusIncludesTeamMemberInfo(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status:     v1beta1.WorkerStatus{Phase: "Running"},
	}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-team", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "alpha-dev", Role: "worker"},
			},
		},
		Status: v1beta1.TeamStatus{
			TeamRoomID: "!team:matrix.org",
			Members: []v1beta1.TeamMemberStatus{{
				Name:         "alpha-dev",
				Role:         "worker",
				RoomID:       "!personal:matrix.org",
				MatrixUserID: "@alpha-dev:matrix.org",
			}},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}, &v1beta1.Team{}).
		WithObjects(worker, team).
		Build()
	backendStub := &stubWorkerBackend{status: backend.StatusRunning, message: "healthy"}
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev/status", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()

	handler.GetWorkerRuntimeStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Team != "alpha-team" {
		t.Errorf("expected Team=alpha-team, got %q", resp.Team)
	}
	if resp.Role != "worker" {
		t.Errorf("expected Role=worker, got %q", resp.Role)
	}
	if resp.RoomID != "!personal:matrix.org" {
		t.Errorf("expected personal RoomID, got %q", resp.RoomID)
	}
	if resp.RoomID == team.Status.TeamRoomID {
		t.Errorf("worker RoomID must not use shared TeamRoomID %q", team.Status.TeamRoomID)
	}
	if resp.MatrixUserID != "@alpha-dev:matrix.org" {
		t.Errorf("expected MatrixUserID=@alpha-dev:matrix.org, got %q", resp.MatrixUserID)
	}
	if resp.Message != "backend=stub status=running message=healthy" {
		t.Errorf("unexpected runtime message %q", resp.Message)
	}
}

func newLifecycleTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add agentteams scheme: %v", err)
	}
	return scheme
}

type stubWorkerBackend struct {
	status      backend.WorkerStatus
	statusErr   error
	message     string
	startCalls  int
	stopCalls   int
	unavailable bool
	statusCalls int
}

func (s *stubWorkerBackend) Name() string                   { return "stub" }
func (s *stubWorkerBackend) DeploymentMode() string         { return backend.DeployLocal }
func (s *stubWorkerBackend) Available(context.Context) bool { return !s.unavailable }
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
	s.statusCalls++
	if s.statusErr != nil {
		return nil, s.statusErr
	}
	return &backend.WorkerResult{Backend: "stub", Status: s.status, Message: s.message}, nil
}

func TestLifecycleReadyPersistsRuntimeReport(t *testing.T) {
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
	backendStub := &stubWorkerBackend{status: backend.StatusRunning}
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default")

	payload := `{"lastActiveAt":"2026-09-14T10:00:00Z","agentStatus":"running","runningTaskCount":2,"lastRunAt":"2026-09-14T09:58:00Z","lastFinishAt":"2026-09-14T09:50:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ready", strings.NewReader(payload))
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
	if updated.Status.LastActiveAt != "2026-09-14T10:00:00Z" {
		t.Errorf("expected lastActiveAt persisted, got %q", updated.Status.LastActiveAt)
	}
	if updated.Status.AgentStatus != "running" {
		t.Errorf("expected agentStatus running, got %q", updated.Status.AgentStatus)
	}
	if updated.Status.RunningTaskCount == nil || *updated.Status.RunningTaskCount != 2 {
		t.Errorf("expected runningTaskCount 2, got %v", updated.Status.RunningTaskCount)
	}
	if updated.Status.LastRunAt != "2026-09-14T09:58:00Z" {
		t.Errorf("expected lastRunAt persisted, got %q", updated.Status.LastRunAt)
	}
	if updated.Status.LastFinishAt != "2026-09-14T09:50:00Z" {
		t.Errorf("expected lastFinishAt persisted, got %q", updated.Status.LastFinishAt)
	}
}

func TestLifecycleReadyEmptyBodyBackwardCompatible(t *testing.T) {
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
	backendStub := &stubWorkerBackend{status: backend.StatusRunning}
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ready", nil)
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
	if updated.Status.AgentStatus != "" || updated.Status.RunningTaskCount != nil {
		t.Errorf("expected no runtime report fields from empty body, got %+v", updated.Status)
	}
}

func TestLifecycleReadyStaleLastActiveAtNotOverwritten(t *testing.T) {
	scheme := newLifecycleTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Status: v1beta1.WorkerStatus{
			Phase:        "Running",
			LastActiveAt: "2026-09-14T12:00:00Z",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.Worker{}).
		WithObjects(worker).
		Build()
	backendStub := &stubWorkerBackend{status: backend.StatusRunning}
	handler := NewLifecycleHandler(k8sClient, backend.NewRegistry([]backend.WorkerBackend{backendStub}), "default")

	payload := `{"lastActiveAt":"2026-09-14T10:00:00Z","agentStatus":"idle"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers/alpha-dev/ready", strings.NewReader(payload))
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
	if updated.Status.LastActiveAt != "2026-09-14T12:00:00Z" {
		t.Errorf("stale lastActiveAt must not regress, got %q", updated.Status.LastActiveAt)
	}
	if updated.Status.AgentStatus != "idle" {
		t.Errorf("agentStatus should still update independently, got %q", updated.Status.AgentStatus)
	}
}
