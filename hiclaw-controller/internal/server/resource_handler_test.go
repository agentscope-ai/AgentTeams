package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	authpkg "github.com/hiclaw/hiclaw-controller/internal/auth"
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
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
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
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
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

func TestListWorkersAggregatesDecoupledTeamMembers(t *testing.T) {
	scheme := newServerTestScheme(t)

	lead := &v1beta1.Worker{}
	lead.Name = "alpha-lead"
	lead.Namespace = "default"
	lead.Spec.Model = "qwen3.5-plus"
	lead.Spec.WorkerName = "lead"
	lead.Status.Phase = "Running"
	lead.Status.MatrixUserID = "@alpha-lead:example.com"
	lead.Status.RoomID = "!lead:example.com"

	dev := &v1beta1.Worker{}
	dev.Name = "alpha-dev"
	dev.Namespace = "default"
	dev.Spec.Runtime = "openclaw"
	dev.Spec.Model = "qwen3.5-plus"
	dev.Status.Phase = "Running"

	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(lead, dev, team).
		WithIndex(&v1beta1.Team{}, "spec.workerMembers.name", indexServerTeamWorkerMemberNames).
		Build()
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
	if list.Total != 2 {
		t.Fatalf("expected 2 decoupled team members, got %d: %+v", list.Total, list.Workers)
	}
	byName := map[string]WorkerResponse{}
	for _, worker := range list.Workers {
		if worker.Name == "" {
			t.Fatalf("unexpected empty worker entry: %+v", list.Workers)
		}
		byName[worker.Name] = worker
	}
	if byName["alpha-lead"].Role != "team_leader" || byName["alpha-lead"].Team != "alpha-team" {
		t.Fatalf("alpha-lead response = %+v, want team_leader in alpha-team", byName["alpha-lead"])
	}
	if byName["alpha-dev"].Role != "worker" || byName["alpha-dev"].Runtime != "openclaw" {
		t.Fatalf("alpha-dev response = %+v, want worker with runtime openclaw", byName["alpha-dev"])
	}
	if _, ok := byName["stale-legacy-lead"]; ok {
		t.Fatalf("stale legacy leader should not be listed when workerMembers is set: %+v", list.Workers)
	}
}

func TestUpdateWorkerRejectsTeamMember(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
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

func indexServerTeamWorkerMemberNames(obj client.Object) []string {
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

func TestUpdateTeamDecoupledUpdatesMemberWorkerCR(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
	leader := &v1beta1.Worker{}
	leader.Name = "alpha-lead"
	leader.Namespace = "default"
	leader.Spec.Model = "qwen"
	worker := &v1beta1.Worker{}
	worker.Name = "alpha-dev"
	worker.Namespace = "default"
	worker.Spec.Model = "qwen"
	worker.Spec.Skills = []string{"old-worker-skill"}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, leader, worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"workers":[
			{"name":"alpha-dev","skills":["review","refactor"],"model":"qwen-plus"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/teams/alpha-team", bytes.NewReader(body))
	req.SetPathValue("name", "alpha-team")
	rec := httptest.NewRecorder()
	handler.UpdateTeam(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var updatedWorker v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updatedWorker); err != nil {
		t.Fatalf("get updated worker: %v", err)
	}
	if got := updatedWorker.Spec.Skills; len(got) != 2 || got[0] != "review" || got[1] != "refactor" {
		t.Fatalf("worker skills=%v, want [review refactor]", got)
	}
	if updatedWorker.Spec.Model != "qwen-plus" {
		t.Fatalf("worker model=%q, want qwen-plus", updatedWorker.Spec.Model)
	}

}

func TestUpdateTeamDecoupledPropagatesWorkerIdleTimeout(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
		{Name: "alpha-qa", Role: "worker"},
	}
	leader := &v1beta1.Worker{}
	leader.Name = "alpha-lead"
	leader.Namespace = "default"
	dev := &v1beta1.Worker{}
	dev.Name = "alpha-dev"
	dev.Namespace = "default"
	dev.Spec.IdleTimeout = "1h"
	qa := &v1beta1.Worker{}
	qa.Name = "alpha-qa"
	qa.Namespace = "default"
	qa.Spec.IdleTimeout = "1h"

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, leader, dev, qa).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"leader":{"workerIdleTimeout":"12h"},
		"workers":[{"name":"alpha-qa","idleTimeout":"30m"}]
	}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/teams/alpha-team", bytes.NewReader(body))
	req.SetPathValue("name", "alpha-team")
	rec := httptest.NewRecorder()
	handler.UpdateTeam(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var updatedDev v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updatedDev); err != nil {
		t.Fatalf("get dev: %v", err)
	}
	if updatedDev.Spec.IdleTimeout != "12h" {
		t.Fatalf("dev idleTimeout=%q, want inherited 12h", updatedDev.Spec.IdleTimeout)
	}

	var updatedQA v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-qa", Namespace: "default"}, &updatedQA); err != nil {
		t.Fatalf("get qa: %v", err)
	}
	if updatedQA.Spec.IdleTimeout != "30m" {
		t.Fatalf("qa idleTimeout=%q, want explicit 30m", updatedQA.Spec.IdleTimeout)
	}

	var updatedLeader v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-lead", Namespace: "default"}, &updatedLeader); err != nil {
		t.Fatalf("get leader: %v", err)
	}
	if updatedLeader.Spec.IdleTimeout != "" {
		t.Fatalf("leader idleTimeout=%q, want empty", updatedLeader.Spec.IdleTimeout)
	}

}

func TestUpdateTeamDecoupledRejectsNonMemberWorker(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
	leader := &v1beta1.Worker{}
	leader.Name = "alpha-lead"
	leader.Namespace = "default"
	worker := &v1beta1.Worker{}
	worker.Name = "stranger"
	worker.Namespace = "default"
	worker.Spec.Skills = []string{"old-skill"}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, leader, worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"workers":[{"name":"stranger","skills":["review"]}]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/teams/alpha-team", bytes.NewReader(body))
	req.SetPathValue("name", "alpha-team")
	rec := httptest.NewRecorder()
	handler.UpdateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}

	var unchanged v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "stranger", Namespace: "default"}, &unchanged); err != nil {
		t.Fatalf("get unchanged worker: %v", err)
	}
	if got := unchanged.Spec.Skills; len(got) != 1 || got[0] != "old-skill" {
		t.Fatalf("non-member worker skills changed: %v", got)
	}
}

func TestUpdateTeamDecoupledRejectsInvalidLeaderRuntime(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
	leader := &v1beta1.Worker{}
	leader.Name = "alpha-lead"
	leader.Namespace = "default"
	leader.Spec.Runtime = "copaw"

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, leader).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"leader":{"runtime":"openclaw"}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/teams/alpha-team", bytes.NewReader(body))
	req.SetPathValue("name", "alpha-team")
	rec := httptest.NewRecorder()
	handler.UpdateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "leader.runtime must be qwenpaw or copaw") {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}

	var unchanged v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-lead", Namespace: "default"}, &unchanged); err != nil {
		t.Fatalf("get unchanged leader: %v", err)
	}
	if unchanged.Spec.Runtime != "copaw" {
		t.Fatalf("leader worker runtime changed to %q, want copaw", unchanged.Spec.Runtime)
	}
}

func TestDeleteWorkerRejectsTeamMember(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
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
		"heartbeatEvery":"30m",
		"members":[
			{"name":"alpha-lead","role":"team_leader","runtime":"qwenpaw"},
			{"name":"alpha-dev","role":"worker","idleTimeout":"12h"}
		]
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
	if created.Spec.HeartbeatEvery != "30m" {
		t.Fatalf("heartbeatEvery=%q, want 30m", created.Spec.HeartbeatEvery)
	}
	var createdDev v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &createdDev); err != nil {
		t.Fatalf("get created worker: %v", err)
	}
	if createdDev.Spec.IdleTimeout != "12h" {
		t.Fatalf("worker idleTimeout=%q, want 12h", createdDev.Spec.IdleTimeout)
	}

	updateBody := []byte(`{
		"leader":{
			"image":"agentteams/copaw-worker:test",
			"heartbeat":{"enabled":true,"every":"45m"},
			"workerIdleTimeout":"24h"
		}
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
	if updated.Spec.HeartbeatEvery != "45m" {
		t.Fatalf("heartbeatEvery=%q, want 45m", updated.Spec.HeartbeatEvery)
	}
	var updatedLeader v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-lead", Namespace: "default"}, &updatedLeader); err != nil {
		t.Fatalf("get updated leader: %v", err)
	}
	if updatedLeader.Spec.Image != "agentteams/copaw-worker:test" {
		t.Fatalf("updated leader image=%q, want agentteams/copaw-worker:test", updatedLeader.Spec.Image)
	}
	var updatedDev v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &updatedDev); err != nil {
		t.Fatalf("get updated worker: %v", err)
	}
	if updatedDev.Spec.IdleTimeout != "24h" {
		t.Fatalf("worker idleTimeout=%q, want 24h", updatedDev.Spec.IdleTimeout)
	}

	var resp TeamResponse
	if err := json.Unmarshal(updateRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.LeaderHeartbeat == nil || resp.LeaderHeartbeat.Every != "45m" {
		t.Fatalf("unexpected response heartbeat: %#v", resp.LeaderHeartbeat)
	}
}

func TestCreateTeamRejectsInvalidLeaderRuntime(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"alpha-team","members":[{"name":"alpha-lead","role":"team_leader","runtime":"openclaw"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "leader.runtime must be qwenpaw or copaw") {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}
}

func TestCreateTeamPersistsRuntimeWorkerNames(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name":"alpha-team",
		"teamName":"alpha",
		"members":[
			{"name":"lead-cr","role":"team_leader","workerName":"lead-runtime"},
			{"name":"dev-cr","role":"worker","workerName":"dev-runtime"}
		]
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
	if got := stored.Spec.TeamName; got != "alpha" {
		t.Fatalf("teamName = %q, want alpha", got)
	}
	var leader v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "lead-cr", Namespace: "default"}, &leader); err != nil {
		t.Fatalf("get leader worker: %v", err)
	}
	if got := leader.Spec.WorkerName; got != "lead-runtime" {
		t.Fatalf("leader workerName = %q, want lead-runtime", got)
	}
	if got := leader.Spec.Runtime; got != "qwenpaw" {
		t.Fatalf("leader runtime = %q, want qwenpaw", got)
	}
	var dev v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "dev-cr", Namespace: "default"}, &dev); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := dev.Spec.WorkerName; got != "dev-runtime" {
		t.Fatalf("workerName = %q, want dev-runtime", got)
	}
}

func TestCreateTeam_WithLeaderOnlyMember(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"leader-only-team","members":[{"name":"lead","role":"team_leader","model":"qwen3.5-plus"}]}`)
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
	if len(stored.Spec.WorkerMembers) != 1 || stored.Spec.WorkerMembers[0].Name != "lead" {
		t.Errorf("stored WorkerMembers=%+v, want lead", stored.Spec.WorkerMembers)
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

func TestCreateWorkerPersistsCredentialContract(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"worker-cr","agentIdentity":{"workloadIdentityName":"wi-worker"},"credentialBindings":[{"credentialRef":{"tokenVaultName":"default","apiKeyCredentialProviderName":"GITHUB_TOKEN"},"toolWhitelist":["gh"]}]}`)
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
	assertCredentialContract(t, stored.Spec.AgentIdentity, stored.Spec.CredentialBindings, "wi-worker", "default", "GITHUB_TOKEN", "gh")
}

func TestUpdateWorkerPersistsCredentialContract(t *testing.T) {
	scheme := newServerTestScheme(t)
	worker := &v1beta1.Worker{}
	worker.Name = "worker-cr"
	worker.Namespace = "default"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"agentIdentity":{"workloadIdentityName":"wi-worker-updated"},"credentialBindings":[{"credentialRef":{"tokenVaultName":"team","apiKeyCredentialProviderName":"SLACK_TOKEN"},"toolWhitelist":["slack"]}]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/worker-cr", bytes.NewReader(body))
	req.SetPathValue("name", "worker-cr")
	rec := httptest.NewRecorder()
	handler.UpdateWorker(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var stored v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "worker-cr", Namespace: "default"}, &stored); err != nil {
		t.Fatalf("get updated worker: %v", err)
	}
	assertCredentialContract(t, stored.Spec.AgentIdentity, stored.Spec.CredentialBindings, "wi-worker-updated", "team", "SLACK_TOKEN", "slack")
}

func TestCreateTeamDecoupledPersistsCredentialContract(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"name":"team-cr","members":[{"name":"lead","role":"team_leader","agentIdentity":{"workloadIdentityName":"wi-lead"},"credentialBindings":[{"credentialRef":{"tokenVaultName":"leader-vault","apiKeyCredentialProviderName":"LEADER_TOKEN"},"toolWhitelist":["leadctl"]}]},{"name":"dev","role":"worker","agentIdentity":{"workloadIdentityName":"wi-dev"},"credentialBindings":[{"credentialRef":{"tokenVaultName":"worker-vault","apiKeyCredentialProviderName":"DEV_TOKEN"},"toolWhitelist":["devctl"]}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var leader v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "lead", Namespace: "default"}, &leader); err != nil {
		t.Fatalf("get decoupled leader worker: %v", err)
	}
	assertCredentialContract(t, leader.Spec.AgentIdentity, leader.Spec.CredentialBindings, "wi-lead", "leader-vault", "LEADER_TOKEN", "leadctl")

	var dev v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "dev", Namespace: "default"}, &dev); err != nil {
		t.Fatalf("get decoupled team worker: %v", err)
	}
	assertCredentialContract(t, dev.Spec.AgentIdentity, dev.Spec.CredentialBindings, "wi-dev", "worker-vault", "DEV_TOKEN", "devctl")
}

func TestUpdateTeamDecoupledPersistsCredentialContract(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "team-cr"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "lead", Role: "team_leader"},
		{Name: "dev", Role: "worker"},
	}
	leader := &v1beta1.Worker{}
	leader.Name = "lead"
	leader.Namespace = "default"
	dev := &v1beta1.Worker{}
	dev.Name = "dev"
	dev.Namespace = "default"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team, leader, dev).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"leader":{"agentIdentity":{"workloadIdentityName":"wi-lead-updated"},"credentialBindings":[{"credentialRef":{"tokenVaultName":"leader-vault","apiKeyCredentialProviderName":"LEADER_TOKEN"},"toolWhitelist":["leadctl"]}]},"workers":[{"name":"dev","agentIdentity":{"workloadIdentityName":"wi-dev-updated"},"credentialBindings":[{"credentialRef":{"tokenVaultName":"worker-vault","apiKeyCredentialProviderName":"DEV_TOKEN"},"toolWhitelist":["devctl"]}]}]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/teams/team-cr", bytes.NewReader(body))
	req.SetPathValue("name", "team-cr")
	rec := httptest.NewRecorder()
	handler.UpdateTeam(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var storedLeader v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "lead", Namespace: "default"}, &storedLeader); err != nil {
		t.Fatalf("get updated decoupled leader worker: %v", err)
	}
	assertCredentialContract(t, storedLeader.Spec.AgentIdentity, storedLeader.Spec.CredentialBindings, "wi-lead-updated", "leader-vault", "LEADER_TOKEN", "leadctl")

	var storedDev v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "dev", Namespace: "default"}, &storedDev); err != nil {
		t.Fatalf("get updated decoupled team worker: %v", err)
	}
	assertCredentialContract(t, storedDev.Spec.AgentIdentity, storedDev.Spec.CredentialBindings, "wi-dev-updated", "worker-vault", "DEV_TOKEN", "devctl")
}

func assertCredentialContract(t *testing.T, identity *v1beta1.AgentIdentitySpec, bindings []v1beta1.CredentialBinding, workloadIdentityName, tokenVaultName, providerName string, toolWhitelist ...string) {
	t.Helper()
	if identity == nil || identity.WorkloadIdentityName != workloadIdentityName {
		t.Fatalf("agentIdentity=%#v, want workloadIdentityName=%q", identity, workloadIdentityName)
	}
	if len(bindings) != 1 {
		t.Fatalf("credentialBindings=%#v, want one binding", bindings)
	}
	ref := bindings[0].CredentialRef
	if ref.TokenVaultName != tokenVaultName || ref.APIKeyCredentialProviderName != providerName {
		t.Fatalf("credentialRef=%#v, want tokenVaultName=%q apiKeyCredentialProviderName=%q", ref, tokenVaultName, providerName)
	}
	if len(toolWhitelist) > 0 {
		if len(bindings[0].ToolWhitelist) != len(toolWhitelist) {
			t.Fatalf("toolWhitelist=%#v, want %#v", bindings[0].ToolWhitelist, toolWhitelist)
		}
		for i, want := range toolWhitelist {
			if bindings[0].ToolWhitelist[i] != want {
				t.Fatalf("toolWhitelist=%#v, want %#v", bindings[0].ToolWhitelist, toolWhitelist)
			}
		}
	}
}

func TestCreateTeam_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a")

	body := []byte(`{"name":"t1","members":[{"name":"l1","role":"team_leader"}]}`)
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

func newServerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add hiclaw scheme: %v", err)
	}
	return scheme
}
