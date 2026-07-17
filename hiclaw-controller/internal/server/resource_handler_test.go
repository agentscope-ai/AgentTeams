package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Post-refactor contract: team leaders cannot create team members via
// /api/v1/workers. They must use /api/v1/teams. The handler must return 409.
func TestCreateWorkerRejectsTeamLeaderCaller(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"alpha-temp","model":"qwen3.5-plus"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{
		Role:     authpkg.RoleTeamLeader,
		Username: "alpha-lead",
		Team:     "alpha-team",
	}))
	rec := httptest.NewRecorder()

	handler.CreateWorker(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
}

// When the worker name is a member of an existing Team, CreateWorker must
// return 409 regardless of caller role.
func TestCreateWorkerRejectsExistingTeamMemberName(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Leader.Name = "alpha-lead"
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{{Name: "alpha-dev"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"alpha-dev","model":"qwen3.5-plus"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateWorker(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
}

// TestCreateWorkerRejectsInvalidName covers #22: server-side name validation
// must reject names that don't match ^[a-z0-9][a-z0-9-]*$, mirroring the
// client-side rule in cmd/hiclaw/create.go so the HTTP API cannot be used to
// bypass it directly.
func TestCreateWorkerRejectsInvalidName(t *testing.T) {
	scheme := newServerTestScheme(t)

	invalidNames := []string{
		"Worker",      // uppercase
		"-worker",     // leading hyphen
		"worker_name", // underscore
		"worker name", // space
		"worker.name", // dot
	}

	for _, name := range invalidNames {
		k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		handler := NewResourceHandler(k8sClient, "default", nil, "")

		body := []byte(`{"name":"` + name + `","model":"qwen3.5-plus"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		handler.CreateWorker(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name %q: expected status %d, got %d: %s", name, http.StatusBadRequest, rec.Code, rec.Body.String())
		}

		var worker v1beta1.Worker
		if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &worker); err == nil {
			t.Fatalf("name %q: expected worker NOT to be created, but it was", name)
		}
	}
}

func TestCreateWorkerAcceptsValidName(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"valid-worker-1","model":"qwen3.5-plus"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateWorker(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
}

func TestCreateWorkerPreservesResources(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"resource-worker","model":"qwen3.5-plus","resources":{"requests":{"cpu":"250m","memory":"512Mi"},"limits":{"cpu":"2","memory":"4Gi"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateWorker(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var worker v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "resource-worker", Namespace: "default"}, &worker); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	assertAgentResources(t, worker.Spec.Resources, "250m", "512Mi", "2", "4Gi")
}

func TestUpdateWorkerPreservesResources(t *testing.T) {
	scheme := newServerTestScheme(t)
	worker := &v1beta1.Worker{}
	worker.Name = "resource-worker"
	worker.Namespace = "default"
	worker.Spec.Model = "qwen3.5-plus"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"resources":{"requests":{"cpu":"300m","memory":"768Mi"},"limits":{"cpu":"3","memory":"5Gi"}}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/resource-worker", bytes.NewReader(body))
	req.SetPathValue("name", "resource-worker")
	rec := httptest.NewRecorder()
	handler.UpdateWorker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var got v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "resource-worker", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	assertAgentResources(t, got.Spec.Resources, "300m", "768Mi", "3", "5Gi")
}

func TestCreateTeamPreservesMemberResources(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name":"resource-team",
		"leader":{"name":"resource-lead","resources":{"requests":{"cpu":"300m","memory":"768Mi"},"limits":{"cpu":"2","memory":"3Gi"}}},
		"workers":[{"name":"resource-dev","resources":{"requests":{"cpu":"200m","memory":"512Mi"},"limits":{"cpu":"1","memory":"2Gi"}}}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var team v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "resource-team", Namespace: "default"}, &team); err != nil {
		t.Fatalf("get team: %v", err)
	}
	assertAgentResources(t, team.Spec.Leader.Resources, "300m", "768Mi", "2", "3Gi")
	if len(team.Spec.Workers) != 1 {
		t.Fatalf("workers len=%d, want 1", len(team.Spec.Workers))
	}
	assertAgentResources(t, team.Spec.Workers[0].Resources, "200m", "512Mi", "1", "2Gi")
}

func TestCreateManagerPreservesResources(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"default","model":"qwen3.5-plus","resources":{"requests":{"cpu":"500m","memory":"1Gi"},"limits":{"cpu":"3","memory":"5Gi"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/managers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateManager(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var mgr v1beta1.Manager
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "default", Namespace: "default"}, &mgr); err != nil {
		t.Fatalf("get manager: %v", err)
	}
	assertAgentResources(t, mgr.Spec.Resources, "500m", "1Gi", "3", "5Gi")
}

// /api/v1/workers/{name} must synthesize a response for a team member even
// though no Worker CR exists. The synthesized response MUST carry the
// RoomID + MatrixUserID recorded in Team.Status.Members so that clients like
// the Manager Agent and `hiclaw get workers <name> -o json | jq .roomID`
// (exercised by test-21-team-project-dag) can resolve a member's room.
//
// This is the regression guard for the PR #666 bug where teamMemberToResponse
// synthesized an empty RoomID because Team.Status had no per-member RoomID
// field.
func TestGetWorkerSynthesizesTeamMember(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Leader = v1beta1.LeaderSpec{Name: "alpha-lead", Model: "qwen3.5-plus"}
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{{Name: "alpha-dev", Model: "qwen3.5-plus"}}
	team.Status.Members = []v1beta1.TeamMemberStatus{
		{
			Name:         "alpha-dev",
			Role:         "worker",
			RoomID:       "!dev-room:example.com",
			MatrixUserID: "@alpha-dev:example.com",
			Observed:     true,
		},
		{
			Name:         "alpha-lead",
			Role:         "team_leader",
			RoomID:       "!lead-room:example.com",
			MatrixUserID: "@alpha-lead:example.com",
			Observed:     true,
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()
	handler.GetWorker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Team != "alpha-team" || resp.Name != "alpha-dev" || resp.Role != "worker" {
		t.Fatalf("unexpected synthesized response: %+v", resp)
	}
	if resp.RoomID != "!dev-room:example.com" {
		t.Errorf("RoomID=%q, want %q (not propagated from Team.Status.Members)", resp.RoomID, "!dev-room:example.com")
	}
	if resp.MatrixUserID != "@alpha-dev:example.com" {
		t.Errorf("MatrixUserID=%q, want %q", resp.MatrixUserID, "@alpha-dev:example.com")
	}
}

func TestGetWorkerSynthesizedTeamMemberIncludesHealthChecks(t *testing.T) {
	modelsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(modelsServer.Close)

	scheme := newServerTestScheme(t)
	_ = corev1.AddToScheme(scheme)

	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{{Name: "alpha-dev", Model: "qwen3.5-plus"}}
	team.Status.Members = []v1beta1.TeamMemberStatus{{
		Name:         "alpha-dev",
		Role:         "worker",
		RoomID:       "!dev-room:example.com",
		MatrixUserID: "@alpha-dev:example.com",
	}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")
	handler.SetWorkerHealthProber(NewWorkerHealthProber(k8sClient, "default", &healthTestGateway{}, modelsServer.URL))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()
	handler.GetWorker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.HealthChecks == nil {
		t.Fatal("expected healthChecks on synthesized GetWorker response")
	}
}

func TestTeamMemberToResponseCopiesHeartbeatTimestamps(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{{Name: "alpha-dev", Model: "qwen3.5-plus"}}
	team.Status.Members = []v1beta1.TeamMemberStatus{{
		Name:          "alpha-dev",
		Role:          "worker",
		LastHeartbeat: "2026-07-17T10:00:00Z",
		LastActiveAt:  "2026-07-17T09:30:00Z",
	}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	resp := handler.teamMemberToResponse(context.Background(), team, "alpha-dev")
	if resp.LastHeartbeat != "2026-07-17T10:00:00Z" {
		t.Fatalf("lastHeartbeat = %q, want 2026-07-17T10:00:00Z", resp.LastHeartbeat)
	}
	if resp.LastActiveAt != "2026-07-17T09:30:00Z" {
		t.Fatalf("lastActiveAt = %q, want 2026-07-17T09:30:00Z", resp.LastActiveAt)
	}
}

func TestGetWorkerEnrichesDecoupledMemberCR(t *testing.T) {
	scheme := newServerTestScheme(t)
	worker := &v1beta1.Worker{}
	worker.Name = "alpha-dev"
	worker.Namespace = "default"
	worker.Spec.Runtime = "copaw"
	worker.Status.RoomID = "!worker-room:example.com"
	worker.Status.MatrixUserID = "@alpha-dev:example.com"

	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(worker, team).
		WithIndex(&v1beta1.Team{}, teamWorkerMembersField, indexTeamWorkerMemberNames).
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()
	handler.GetWorker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Name != "alpha-dev" || resp.Team != "alpha-team" || resp.Role != "worker" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Runtime != "copaw" || resp.RoomID != "!worker-room:example.com" {
		t.Fatalf("runtime/room not preserved from Worker CR: %+v", resp)
	}
}

// /api/v1/workers must list standalone workers and synthetic team members.
// Workers with team annotations (legacy CRs) must NOT be duplicated.
func TestListWorkersAggregatesTeamMembers(t *testing.T) {
	scheme := newServerTestScheme(t)

	standalone := &v1beta1.Worker{}
	standalone.Name = "solo"
	standalone.Namespace = "default"

	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Leader = v1beta1.LeaderSpec{Name: "alpha-lead", Model: "qwen3.5-plus"}
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{{Name: "alpha-dev", Model: "qwen3.5-plus"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(standalone, team).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	rec := httptest.NewRecorder()
	handler.ListWorkers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var list WorkerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Total != 3 {
		t.Fatalf("expected 3 workers (solo + leader + dev), got %d: %+v", list.Total, list.Workers)
	}
	names := map[string]bool{}
	for _, w := range list.Workers {
		names[w.Name] = true
	}
	for _, want := range []string{"solo", "alpha-lead", "alpha-dev"} {
		if !names[want] {
			t.Errorf("missing %q in aggregated list: %+v", want, list.Workers)
		}
	}
}

func TestListWorkersTeamFilterIncludesDecoupledMembers(t *testing.T) {
	scheme := newServerTestScheme(t)

	solo := &v1beta1.Worker{}
	solo.Name = "solo"
	solo.Namespace = "default"

	lead := &v1beta1.Worker{}
	lead.Name = "alpha-lead"
	lead.Namespace = "default"
	lead.Spec.Runtime = "copaw"

	dev := &v1beta1.Worker{}
	dev.Name = "alpha-dev"
	dev.Namespace = "default"
	dev.Spec.Runtime = "openclaw"

	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(solo, lead, dev, team).
		WithIndex(&v1beta1.Team{}, teamWorkerMembersField, indexTeamWorkerMemberNames).
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers?team=alpha-team", nil)
	rec := httptest.NewRecorder()
	handler.ListWorkers(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var list WorkerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Total != 2 {
		t.Fatalf("expected 2 team members, got %d: %+v", list.Total, list.Workers)
	}
	roles := map[string]string{}
	for _, w := range list.Workers {
		if w.Team != "alpha-team" {
			t.Fatalf("unexpected team for %s: %+v", w.Name, w)
		}
		roles[w.Name] = w.Role
	}
	if roles["alpha-lead"] != "team_leader" || roles["alpha-dev"] != "worker" {
		t.Fatalf("roles=%v, want lead team_leader and dev worker", roles)
	}
	if _, ok := roles["solo"]; ok {
		t.Fatalf("solo worker leaked into team filter: %+v", list.Workers)
	}
}

func TestUpdateWorkerRejectsTeamMember(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Leader.Name = "alpha-lead"
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{{Name: "alpha-dev"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/alpha-dev", bytes.NewReader([]byte(`{"model":"new-model"}`)))
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()
	handler.UpdateWorker(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
}

func TestDeleteWorkerRejectsTeamMember(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.Leader.Name = "alpha-lead"
	team.Spec.Workers = []v1beta1.TeamWorkerSpec{{Name: "alpha-dev"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workers/alpha-dev", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()
	handler.DeleteWorker(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
}

func TestCreateAndUpdateTeamLeaderRuntimeConfig(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	createBody := []byte(`{
		"name":"alpha-team",
		"leader":{
			"name":"alpha-lead",
			"modelProvider":"qwen",
			"heartbeat":{"enabled":true,"every":"30m"},
			"workerIdleTimeout":"12h"
		},
		"workers":[{"name":"alpha-dev","modelProvider":"openai"}]
	}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(createBody))
	createRec := httptest.NewRecorder()
	handler.CreateTeam(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-team", Namespace: "default"}, &created); err != nil {
		t.Fatalf("get created team: %v", err)
	}
	if created.Spec.Leader.Heartbeat == nil || !created.Spec.Leader.Heartbeat.Enabled || created.Spec.Leader.Heartbeat.Every != "30m" {
		t.Fatalf("unexpected heartbeat config after create: %#v", created.Spec.Leader.Heartbeat)
	}
	if created.Spec.Leader.WorkerIdleTimeout != "12h" {
		t.Fatalf("expected worker idle timeout 12h, got %q", created.Spec.Leader.WorkerIdleTimeout)
	}
	if created.Spec.Leader.ModelProvider != "qwen" {
		t.Fatalf("leader.modelProvider=%q, want qwen", created.Spec.Leader.ModelProvider)
	}
	if len(created.Spec.Workers) != 1 || created.Spec.Workers[0].ModelProvider != "openai" {
		t.Fatalf("workers modelProvider not persisted: %#v", created.Spec.Workers)
	}

	updateBody := []byte(`{
		"leader":{
			"modelProvider":"dashscope",
			"heartbeat":{"enabled":true,"every":"45m"},
			"workerIdleTimeout":"24h"
		},
		"workers":[{"name":"alpha-qa","modelProvider":"qwen"}]
	}`)
	updateReq := httptest.NewRequest(http.MethodPut, "/api/v1/teams/alpha-team", bytes.NewReader(updateBody))
	updateReq.SetPathValue("name", "alpha-team")
	updateRec := httptest.NewRecorder()
	handler.UpdateTeam(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected update status %d, got %d: %s", http.StatusOK, updateRec.Code, updateRec.Body.String())
	}

	var updated v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-team", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated team: %v", err)
	}
	if updated.Spec.Leader.Heartbeat == nil || updated.Spec.Leader.Heartbeat.Every != "45m" {
		t.Fatalf("unexpected heartbeat config after update: %#v", updated.Spec.Leader.Heartbeat)
	}
	if updated.Spec.Leader.WorkerIdleTimeout != "24h" {
		t.Fatalf("expected worker idle timeout 24h, got %q", updated.Spec.Leader.WorkerIdleTimeout)
	}
	if updated.Spec.Leader.ModelProvider != "dashscope" {
		t.Fatalf("leader.modelProvider=%q, want dashscope", updated.Spec.Leader.ModelProvider)
	}
	if len(updated.Spec.Workers) != 1 || updated.Spec.Workers[0].Name != "alpha-qa" || updated.Spec.Workers[0].ModelProvider != "qwen" {
		t.Fatalf("workers after update=%#v, want alpha-qa with qwen modelProvider", updated.Spec.Workers)
	}

	var resp TeamResponse
	if err := json.Unmarshal(updateRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.LeaderHeartbeat == nil || resp.LeaderHeartbeat.Every != "45m" {
		t.Fatalf("unexpected response heartbeat: %#v", resp.LeaderHeartbeat)
	}
	if resp.WorkerIdleTimeout != "24h" {
		t.Fatalf("expected response worker idle timeout 24h, got %q", resp.WorkerIdleTimeout)
	}
}

// TestCreateAndUpdateTeamPersistsTeamWideModelProvider is the REST-parity
// guard for docs/implementation-milestone-3.md Step 4: without ModelProvider
// on CreateTeamRequest/UpdateTeamRequest/TeamResponse, `hiclaw apply` would
// silently drop the team-wide field.
func TestCreateAndUpdateTeamPersistsTeamWideModelProvider(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	createBody := []byte(`{
		"name":"alpha-team",
		"modelProvider":"qwen",
		"leader":{"name":"alpha-lead"},
		"workers":[{"name":"alpha-dev"}]
	}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(createBody))
	createRec := httptest.NewRecorder()
	handler.CreateTeam(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-team", Namespace: "default"}, &created); err != nil {
		t.Fatalf("get created team: %v", err)
	}
	if created.Spec.ModelProvider != "qwen" {
		t.Fatalf("spec.modelProvider=%q, want qwen", created.Spec.ModelProvider)
	}
	// Per-agent fields were left empty on create, so the projection fallback
	// (not persistence) resolves them — the raw stored spec stays empty.
	if created.Spec.Leader.ModelProvider != "" {
		t.Fatalf("leader.modelProvider=%q, want empty (falls back at projection time)", created.Spec.Leader.ModelProvider)
	}

	var createResp TeamResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if createResp.ModelProvider != "qwen" {
		t.Fatalf("create response modelProvider=%q, want qwen", createResp.ModelProvider)
	}

	updateBody := []byte(`{"modelProvider":"dashscope"}`)
	updateReq := httptest.NewRequest(http.MethodPut, "/api/v1/teams/alpha-team", bytes.NewReader(updateBody))
	updateReq.SetPathValue("name", "alpha-team")
	updateRec := httptest.NewRecorder()
	handler.UpdateTeam(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected update status %d, got %d: %s", http.StatusOK, updateRec.Code, updateRec.Body.String())
	}

	var updated v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-team", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated team: %v", err)
	}
	if updated.Spec.ModelProvider != "dashscope" {
		t.Fatalf("spec.modelProvider after update=%q, want dashscope", updated.Spec.ModelProvider)
	}

	var updateResp TeamResponse
	if err := json.Unmarshal(updateRec.Body.Bytes(), &updateResp); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updateResp.ModelProvider != "dashscope" {
		t.Fatalf("update response modelProvider=%q, want dashscope", updateResp.ModelProvider)
	}
}

func TestCreateTeamPersistsRuntimeWorkerNames(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name":"alpha-team",
		"teamName":"alpha",
		"leader":{"name":"lead-cr","workerName":"lead-runtime"},
		"workers":[{"name":"dev-cr","workerName":"dev-runtime"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var stored v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-team", Namespace: "default"}, &stored); err != nil {
		t.Fatalf("get created team: %v", err)
	}
	if got := stored.Spec.Leader.WorkerName; got != "lead-runtime" {
		t.Fatalf("leader.workerName = %q, want lead-runtime", got)
	}
	if got := stored.Spec.TeamName; got != "alpha" {
		t.Fatalf("teamName = %q, want alpha", got)
	}
	if got := stored.Spec.Workers[0].WorkerName; got != "dev-runtime" {
		t.Fatalf("workers[0].workerName = %q, want dev-runtime", got)
	}
}

func TestCreateAndUpdateManagerPersistsModelProvider(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	createBody := []byte(`{"name":"default","model":"qwen-plus","modelProvider":"qwen"}`)
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/managers", bytes.NewReader(createBody))
	createRec := httptest.NewRecorder()
	handler.CreateManager(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected create status %d, got %d: %s", http.StatusCreated, createRec.Code, createRec.Body.String())
	}

	var created v1beta1.Manager
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "default", Namespace: "default"}, &created); err != nil {
		t.Fatalf("get created manager: %v", err)
	}
	if created.Spec.ModelProvider != "qwen" {
		t.Fatalf("created manager modelProvider=%q, want qwen", created.Spec.ModelProvider)
	}

	updateBody := []byte(`{"modelProvider":"openai"}`)
	updateReq := httptest.NewRequest(http.MethodPut, "/api/v1/managers/default", bytes.NewReader(updateBody))
	updateReq.SetPathValue("name", "default")
	updateRec := httptest.NewRecorder()
	handler.UpdateManager(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("expected update status %d, got %d: %s", http.StatusOK, updateRec.Code, updateRec.Body.String())
	}

	var updated v1beta1.Manager
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "default", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated manager: %v", err)
	}
	if updated.Spec.ModelProvider != "openai" {
		t.Fatalf("updated manager modelProvider=%q, want openai", updated.Spec.ModelProvider)
	}
}

// CreateTeam must accept a payload that omits `workers` entirely (leader-only
// team). The CRD no longer lists `workers` in its required-properties set and
// both TeamSpec.Workers / CreateTeamRequest.Workers carry `omitempty`, so a
// caller posting just {name, leader} should get a 201 and the stored CR must
// have Spec.Workers == nil (no implicit empty-slice conversion).
func TestCreateTeam_WithoutWorkers(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"leader-only-team","leader":{"name":"lead","model":"qwen3.5-plus"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var resp TeamResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Name != "leader-only-team" {
		t.Errorf("response Name=%q, want %q", resp.Name, "leader-only-team")
	}
	if resp.LeaderName != "lead" {
		t.Errorf("response LeaderName=%q, want %q", resp.LeaderName, "lead")
	}
	if len(resp.WorkerNames) != 0 {
		t.Errorf("response WorkerNames=%+v, want empty", resp.WorkerNames)
	}
	if resp.TotalWorkers != 0 {
		t.Errorf("response TotalWorkers=%d, want 0", resp.TotalWorkers)
	}

	var stored v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "leader-only-team", Namespace: "default"}, &stored); err != nil {
		t.Fatalf("get stored team: %v", err)
	}
	if stored.Spec.Workers != nil {
		t.Errorf("stored Spec.Workers=%+v, want nil (no implicit [] from handler)", stored.Spec.Workers)
	}
	if stored.Spec.Leader.Name != "lead" {
		t.Errorf("stored Leader.Name=%q, want %q", stored.Spec.Leader.Name, "lead")
	}
}

// TestCreateWorker_StampsControllerLabel verifies that the HTTP API
// force-overwrites the agentteams.io/controller label on Create. A caller
// attempting to smuggle a different controller value must not succeed:
// the serving controller's own name always wins.
func TestCreateWorker_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a")

	body := []byte(`{"name":"w1","model":"qwen3.5-plus"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateWorker(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var worker v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "w1", Namespace: "default"}, &worker); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := worker.Labels[v1beta1.LabelController]; got != "ctrl-a" {
		t.Fatalf("expected controller label ctrl-a, got %q", got)
	}
}

func TestCreateWorkerPersistsRuntimeWorkerName(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"worker-cr","workerName":"worker-runtime"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateWorker(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var stored v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "worker-cr", Namespace: "default"}, &stored); err != nil {
		t.Fatalf("get created worker: %v", err)
	}
	if got := stored.Spec.WorkerName; got != "worker-runtime" {
		t.Fatalf("worker.spec.workerName = %q, want worker-runtime", got)
	}
}

func TestCreateWorkerDefaultsRuntime(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"worker-cr"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateWorker(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var stored v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "worker-cr", Namespace: "default"}, &stored); err != nil {
		t.Fatalf("get created worker: %v", err)
	}
	if got := stored.Spec.Runtime; got != "openclaw" {
		t.Fatalf("worker.spec.runtime = %q, want openclaw", got)
	}
}

func TestCreateTeam_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a")

	body := []byte(`{"name":"t1","leader":{"name":"l1"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var team v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "t1", Namespace: "default"}, &team); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if got := team.Labels[v1beta1.LabelController]; got != "ctrl-a" {
		t.Fatalf("expected controller label ctrl-a, got %q", got)
	}
}

func TestCreateHuman_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a")

	body := []byte(`{"name":"h1","displayName":"Human One"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/humans", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateHuman(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var human v1beta1.Human
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "h1", Namespace: "default"}, &human); err != nil {
		t.Fatalf("get human: %v", err)
	}
	if got := human.Labels[v1beta1.LabelController]; got != "ctrl-a" {
		t.Fatalf("expected controller label ctrl-a, got %q", got)
	}
}

func TestCreateManager_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a")

	body := []byte(`{"name":"m1","model":"qwen3.5-plus"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/managers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateManager(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var mgr v1beta1.Manager
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "m1", Namespace: "default"}, &mgr); err != nil {
		t.Fatalf("get manager: %v", err)
	}
	if got := mgr.Labels[v1beta1.LabelController]; got != "ctrl-a" {
		t.Fatalf("expected controller label ctrl-a, got %q", got)
	}
}

// TestCreate_EmptyControllerName_NoLabel verifies embedded-mode behavior:
// when controllerName is empty, the handler does not stamp any controller
// label (and does not introduce a stray labels map on resources that had
// none), preserving existing embedded deployments.
func TestCreate_EmptyControllerName_NoLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"h2","displayName":"Human Two"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/humans", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateHuman(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var human v1beta1.Human
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "h2", Namespace: "default"}, &human); err != nil {
		t.Fatalf("get human: %v", err)
	}
	if _, present := human.Labels[v1beta1.LabelController]; present {
		t.Fatalf("expected no controller label when controllerName is empty, got %q", human.Labels[v1beta1.LabelController])
	}
}

// TestCreateHuman_SoloOperatorDefaultsToAdmin verifies that in solo mode,
// a CreateHuman request that omits permissionLevel is created as Admin (1).
func TestCreateHuman_SoloOperatorDefaultsToAdmin(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")
	handler.SetSoloOperator(true)

	body := []byte(`{"name":"solo-human","displayName":"Solo Human"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/humans", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateHuman(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var human v1beta1.Human
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "solo-human", Namespace: "default"}, &human); err != nil {
		t.Fatalf("get human: %v", err)
	}
	if human.Spec.PermissionLevel != 1 {
		t.Fatalf("PermissionLevel = %d, want 1 (Admin) in solo mode with unset request field", human.Spec.PermissionLevel)
	}
}

// TestCreateHuman_SoloOperatorRespectsExplicitPermissionLevel verifies solo
// mode never overrides an explicit non-zero PermissionLevel from the caller.
func TestCreateHuman_SoloOperatorRespectsExplicitPermissionLevel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")
	handler.SetSoloOperator(true)

	body := []byte(`{"name":"solo-worker-human","displayName":"Solo Worker Human","permissionLevel":3}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/humans", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateHuman(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var human v1beta1.Human
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "solo-worker-human", Namespace: "default"}, &human); err != nil {
		t.Fatalf("get human: %v", err)
	}
	if human.Spec.PermissionLevel != 3 {
		t.Fatalf("PermissionLevel = %d, want 3 (explicit request value must not be overridden)", human.Spec.PermissionLevel)
	}
}

// TestCreateHuman_NonSoloLeavesPermissionLevelZero verifies the default
// (non-solo) behavior is unchanged: an omitted permissionLevel stays 0.
func TestCreateHuman_NonSoloLeavesPermissionLevelZero(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")
	// soloOperator defaults to false; SetSoloOperator intentionally not called.

	body := []byte(`{"name":"multi-human","displayName":"Multi Human"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/humans", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateHuman(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var human v1beta1.Human
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "multi-human", Namespace: "default"}, &human); err != nil {
		t.Fatalf("get human: %v", err)
	}
	if human.Spec.PermissionLevel != 0 {
		t.Fatalf("PermissionLevel = %d, want 0 (unchanged multi-user default)", human.Spec.PermissionLevel)
	}
}

// --- Projects ---

func TestCreateProject_HappyPath(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"proj-1","team":"alpha-team","repos":[{"url":"https://git.pawcommit.com/org/repo.git","access":"rw"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateProject(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var stored v1beta1.Project
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "proj-1", Namespace: "default"}, &stored); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if stored.Spec.Team != "alpha-team" {
		t.Fatalf("team = %q, want alpha-team", stored.Spec.Team)
	}
	if len(stored.Spec.Repos) != 1 || stored.Spec.Repos[0].Access != "rw" {
		t.Fatalf("repos = %+v", stored.Spec.Repos)
	}

	var resp ProjectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Phase != "Pending" {
		t.Fatalf("response phase = %q, want Pending (default)", resp.Phase)
	}
}

func TestCreateProject_WithDependsOn(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"proj-2","team":"alpha-team","dependsOn":["upstream-a","upstream-b"],"repos":[{"url":"https://git.pawcommit.com/org/repo.git","access":"rw"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateProject(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var stored v1beta1.Project
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "proj-2", Namespace: "default"}, &stored); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if len(stored.Spec.DependsOn) != 2 || stored.Spec.DependsOn[0] != "upstream-a" {
		t.Fatalf("dependsOn = %+v", stored.Spec.DependsOn)
	}

	var resp ProjectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.DependsOn) != 2 {
		t.Fatalf("response dependsOn = %+v", resp.DependsOn)
	}
}

func TestCreateProject_MissingName(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"team":"alpha-team","repos":[{"url":"https://git.pawcommit.com/org/repo.git","access":"rw"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateProject(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestCreateProject_MissingTeam(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"proj-2","repos":[{"url":"https://git.pawcommit.com/org/repo.git","access":"rw"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateProject(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestCreateProject_MissingRepos(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"proj-3","team":"alpha-team"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateProject(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestCreateProject_InvalidAccessEnum(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"proj-4","team":"alpha-team","repos":[{"url":"https://git.pawcommit.com/org/repo.git","access":"readwrite"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateProject(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestGetProject_NotFound(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/missing", nil)
	req.SetPathValue("name", "missing")
	rec := httptest.NewRecorder()
	handler.GetProject(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestGetProject_HappyPath(t *testing.T) {
	scheme := newServerTestScheme(t)
	proj := &v1beta1.Project{}
	proj.Name = "proj-5"
	proj.Namespace = "default"
	proj.Spec.Team = "alpha-team"
	proj.Spec.Repos = []v1beta1.ProjectRepo{{URL: "https://git.pawcommit.com/org/repo.git", Access: "rw"}}
	proj.Status.Phase = "Ready"
	proj.Status.RepoCount = 1
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proj).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-5", nil)
	req.SetPathValue("name", "proj-5")
	rec := httptest.NewRecorder()
	handler.GetProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp ProjectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Phase != "Ready" || resp.RepoCount != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestListProjects_FiltersByTeam(t *testing.T) {
	scheme := newServerTestScheme(t)
	p1 := &v1beta1.Project{}
	p1.Name = "proj-a"
	p1.Namespace = "default"
	p1.Spec.Team = "alpha-team"
	p1.Spec.Repos = []v1beta1.ProjectRepo{{URL: "https://x/a.git", Access: "rw"}}

	p2 := &v1beta1.Project{}
	p2.Name = "proj-b"
	p2.Namespace = "default"
	p2.Spec.Team = "beta-team"
	p2.Spec.Repos = []v1beta1.ProjectRepo{{URL: "https://x/b.git", Access: "rw"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p1, p2).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects?team=alpha-team", nil)
	rec := httptest.NewRecorder()
	handler.ListProjects(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var list ProjectListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Total != 1 || list.Projects[0].Name != "proj-a" {
		t.Fatalf("unexpected filtered list: %+v", list)
	}
}

func TestUpdateProject_UpdatesSpecFields(t *testing.T) {
	scheme := newServerTestScheme(t)
	proj := &v1beta1.Project{}
	proj.Name = "proj-6"
	proj.Namespace = "default"
	proj.Spec.Team = "alpha-team"
	proj.Spec.Repos = []v1beta1.ProjectRepo{{URL: "https://x/a.git", Access: "rw"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proj).WithStatusSubresource(&v1beta1.Project{}).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"description":"updated description"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/proj-6", bytes.NewReader(body))
	req.SetPathValue("name", "proj-6")
	rec := httptest.NewRecorder()
	handler.UpdateProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var updated v1beta1.Project
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "proj-6", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated project: %v", err)
	}
	if updated.Spec.Description != "updated description" {
		t.Fatalf("description = %q, want %q", updated.Spec.Description, "updated description")
	}
}

func TestUpdateProject_SetsCompletedPhase(t *testing.T) {
	scheme := newServerTestScheme(t)
	proj := &v1beta1.Project{}
	proj.Name = "proj-7"
	proj.Namespace = "default"
	proj.Spec.Team = "alpha-team"
	proj.Spec.Repos = []v1beta1.ProjectRepo{{URL: "https://x/a.git", Access: "rw"}}
	proj.Status.Phase = "Ready"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proj).WithStatusSubresource(&v1beta1.Project{}).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"phase":"Completed"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/proj-7", bytes.NewReader(body))
	req.SetPathValue("name", "proj-7")
	rec := httptest.NewRecorder()
	handler.UpdateProject(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var updated v1beta1.Project
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "proj-7", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated project: %v", err)
	}
	if updated.Status.Phase != "Completed" {
		t.Fatalf("phase = %q, want Completed", updated.Status.Phase)
	}
}

func TestUpdateProject_RejectsInvalidPhase(t *testing.T) {
	scheme := newServerTestScheme(t)
	proj := &v1beta1.Project{}
	proj.Name = "proj-8"
	proj.Namespace = "default"
	proj.Spec.Team = "alpha-team"
	proj.Spec.Repos = []v1beta1.ProjectRepo{{URL: "https://x/a.git", Access: "rw"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proj).WithStatusSubresource(&v1beta1.Project{}).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"phase":"Ready"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/proj-8", bytes.NewReader(body))
	req.SetPathValue("name", "proj-8")
	rec := httptest.NewRecorder()
	handler.UpdateProject(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestUpdateProject_NotFound(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"description":"x"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/missing", bytes.NewReader(body))
	req.SetPathValue("name", "missing")
	rec := httptest.NewRecorder()
	handler.UpdateProject(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestDeleteProject_HappyPath(t *testing.T) {
	scheme := newServerTestScheme(t)
	proj := &v1beta1.Project{}
	proj.Name = "proj-9"
	proj.Namespace = "default"
	proj.Spec.Team = "alpha-team"
	proj.Spec.Repos = []v1beta1.ProjectRepo{{URL: "https://x/a.git", Access: "rw"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(proj).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/proj-9", nil)
	req.SetPathValue("name", "proj-9")
	rec := httptest.NewRecorder()
	handler.DeleteProject(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected %d, got %d: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
	var after v1beta1.Project
	err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "proj-9", Namespace: "default"}, &after)
	if err == nil {
		t.Fatalf("expected project to be gone after delete")
	}
}

func TestDeleteProject_NotFound(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/missing", nil)
	req.SetPathValue("name", "missing")
	rec := httptest.NewRecorder()
	handler.DeleteProject(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestCreateProject_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a")

	body := []byte(`{"name":"proj-10","team":"alpha-team","repos":[{"url":"https://x/a.git","access":"rw"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateProject(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var proj v1beta1.Project
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "proj-10", Namespace: "default"}, &proj); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got := proj.Labels[v1beta1.LabelController]; got != "ctrl-a" {
		t.Fatalf("expected controller label ctrl-a, got %q", got)
	}
}

func newServerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add hiclaw scheme: %v", err)
	}
	return scheme
}

func indexTeamWorkerMemberNames(obj client.Object) []string {
	team, ok := obj.(*v1beta1.Team)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(team.Spec.WorkerMembers))
	for _, ref := range team.Spec.WorkerMembers {
		if ref.Name != "" {
			names = append(names, ref.Name)
		}
	}
	return names
}

func assertAgentResources(t *testing.T, got *v1beta1.AgentResourceRequirements, cpuReq, memReq, cpuLimit, memLimit string) {
	t.Helper()
	if got == nil {
		t.Fatal("resources = nil")
	}
	if got.Requests.CPU != cpuReq {
		t.Fatalf("requests.cpu = %q, want %q (resources=%+v)", got.Requests.CPU, cpuReq, got)
	}
	if got.Requests.Memory != memReq {
		t.Fatalf("requests.memory = %q, want %q (resources=%+v)", got.Requests.Memory, memReq, got)
	}
	if got.Limits.CPU != cpuLimit {
		t.Fatalf("limits.cpu = %q, want %q (resources=%+v)", got.Limits.CPU, cpuLimit, got)
	}
	if got.Limits.Memory != memLimit {
		t.Fatalf("limits.memory = %q, want %q (resources=%+v)", got.Limits.Memory, memLimit, got)
	}
}
