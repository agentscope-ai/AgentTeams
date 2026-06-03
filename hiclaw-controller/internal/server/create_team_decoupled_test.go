package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCreateTeamDecoupled_Success(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name": "alpha-team",
		"teamName": "Alpha",
		"description": "test team",
		"heartbeatEvery": "30m",
		"members": [
			{"name": "alpha-lead", "role": "team_leader", "model": "qwen3.5-plus"},
			{"name": "alpha-dev", "role": "worker", "model": "qwen3.5-plus", "runtime": "openclaw"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify Worker CRs were created.
	var lead v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-lead", Namespace: "default"}, &lead); err != nil {
		t.Fatalf("get leader worker: %v", err)
	}
	if lead.Spec.Model != "qwen3.5-plus" {
		t.Errorf("leader model=%q, want qwen3.5-plus", lead.Spec.Model)
	}

	var dev v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &dev); err != nil {
		t.Fatalf("get dev worker: %v", err)
	}
	if dev.Spec.Runtime != "openclaw" {
		t.Errorf("dev runtime=%q, want openclaw", dev.Spec.Runtime)
	}

	// Verify Team CR uses workerMembers.
	var team v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-team", Namespace: "default"}, &team); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if len(team.Spec.WorkerMembers) != 2 {
		t.Fatalf("workerMembers len=%d, want 2", len(team.Spec.WorkerMembers))
	}
	if team.Spec.HeartbeatEvery != "30m" {
		t.Errorf("heartbeatEvery=%q, want 30m", team.Spec.HeartbeatEvery)
	}

	// Verify response includes members.
	var resp TeamResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.LeaderName != "alpha-lead" {
		t.Errorf("resp.LeaderName=%q, want alpha-lead", resp.LeaderName)
	}
	if len(resp.Members) != 2 {
		t.Errorf("resp.Members len=%d, want 2", len(resp.Members))
	}
}

func TestCreateTeamDecoupled_RejectsBothFormats(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name": "alpha-team",
		"leader": {"name": "alpha-lead", "model": "qwen3.5-plus"},
		"members": [
			{"name": "alpha-lead", "role": "team_leader", "model": "qwen3.5-plus"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateTeamDecoupled_NoLeader(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name": "alpha-team",
		"members": [
			{"name": "alpha-dev", "role": "worker", "model": "qwen3.5-plus"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateTeamDecoupled_MultipleLeaders(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name": "alpha-team",
		"members": [
			{"name": "lead-1", "role": "team_leader", "model": "qwen3.5-plus"},
			{"name": "lead-2", "role": "team_leader", "model": "qwen3.5-plus"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateTeamDecoupled_DuplicateNames(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name": "alpha-team",
		"members": [
			{"name": "alpha-lead", "role": "team_leader", "model": "qwen3.5-plus"},
			{"name": "alpha-lead", "role": "worker", "model": "qwen3.5-plus"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateTeamDecoupled_ExistingWorkerConflict(t *testing.T) {
	scheme := newServerTestScheme(t)
	existing := &v1beta1.Worker{}
	existing.Name = "alpha-lead"
	existing.Namespace = "default"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name": "alpha-team",
		"members": [
			{"name": "alpha-lead", "role": "team_leader", "model": "qwen3.5-plus"},
			{"name": "alpha-dev", "role": "worker", "model": "qwen3.5-plus"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateTeamDecoupled_ExistingTeamMemberConflict(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "other-team"
	team.Namespace = "default"
	team.Spec.Leader.Name = "alpha-lead"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name": "alpha-team",
		"members": [
			{"name": "alpha-lead", "role": "team_leader", "model": "qwen3.5-plus"},
			{"name": "alpha-dev", "role": "worker", "model": "qwen3.5-plus"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateTeamDecoupled_LegacyFormatStillWorks(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name": "legacy-team",
		"leader": {"name": "leg-lead", "model": "qwen3.5-plus"},
		"workers": [{"name": "leg-dev", "model": "qwen3.5-plus"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Legacy format should NOT create Worker CRs.
	var worker v1beta1.Worker
	err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "leg-lead", Namespace: "default"}, &worker)
	if err == nil {
		t.Error("legacy format should NOT create standalone Worker CRs")
	}
}

func TestCreateTeamDecoupled_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "my-controller")

	body := []byte(`{
		"name": "alpha-team",
		"members": [
			{"name": "alpha-lead", "role": "team_leader", "model": "qwen3.5-plus"},
			{"name": "alpha-dev", "role": "worker", "model": "qwen3.5-plus"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Check controller label on Worker CRs.
	var lead v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-lead", Namespace: "default"}, &lead); err != nil {
		t.Fatalf("get leader: %v", err)
	}
	if lead.Labels[v1beta1.LabelController] != "my-controller" {
		t.Errorf("worker label=%q, want my-controller", lead.Labels[v1beta1.LabelController])
	}

	// Check controller label on Team CR.
	var team v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-team", Namespace: "default"}, &team); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if team.Labels[v1beta1.LabelController] != "my-controller" {
		t.Errorf("team label=%q, want my-controller", team.Labels[v1beta1.LabelController])
	}
}
