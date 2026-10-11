package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Team leaders cannot create infrastructure resources through /api/v1/workers.
func TestCreateWorkerRejectsTeamLeaderCaller(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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

// A directly-created malformed Team may temporarily reference a missing
// Worker. Creating that Worker repairs the reference and must be allowed.
func TestCreateWorkerAllowsRepairingMissingTeamReference(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	body := []byte(`{"name":"alpha-dev","model":"qwen3.5-plus"}`)
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
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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

func TestCreateTeamDoesNotOwnWorkerRuntimeConfig(t *testing.T) {
	scheme := newServerTestScheme(t)
	leader := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "resource-lead", Namespace: "default"},
		Spec: v1beta1.WorkerSpec{
			Model: "qwen3.5-plus",
			Resources: &v1beta1.AgentResourceRequirements{
				Requests: v1beta1.AgentResourceValues{CPU: "300m", Memory: "768Mi"},
				Limits:   v1beta1.AgentResourceValues{CPU: "2", Memory: "3Gi"},
			},
		},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "resource-dev", Namespace: "default"},
		Spec: v1beta1.WorkerSpec{
			Model: "qwen3.5-plus",
			Resources: &v1beta1.AgentResourceRequirements{
				Requests: v1beta1.AgentResourceValues{CPU: "200m", Memory: "512Mi"},
				Limits:   v1beta1.AgentResourceValues{CPU: "1", Memory: "2Gi"},
			},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader, worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	body := []byte(`{
		"name":"resource-team",
		"workerMembers":[
			{"name":"resource-lead","role":"team_leader"},
			{"name":"resource-dev","role":"worker"}
		]
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
	if len(team.Spec.WorkerMembers) != 2 {
		t.Fatalf("workerMembers len=%d, want 2", len(team.Spec.WorkerMembers))
	}
	var storedLeader, storedWorker v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "resource-lead", Namespace: "default"}, &storedLeader); err != nil {
		t.Fatalf("get leader Worker: %v", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "resource-dev", Namespace: "default"}, &storedWorker); err != nil {
		t.Fatalf("get member Worker: %v", err)
	}
	assertAgentResources(t, storedLeader.Spec.Resources, "300m", "768Mi", "2", "3Gi")
	assertAgentResources(t, storedWorker.Spec.Resources, "200m", "512Mi", "1", "2Gi")
}

func TestCreateTeamReferencesExistingWorkerCRs(t *testing.T) {
	scheme := newServerTestScheme(t)
	leader := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen3.5-plus"},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen3.5-plus"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader, worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	body := []byte(`{
		"name":"alpha-team",
		"heartbeatEvery":"30m",
		"workerMembers":[
			{"name":"alpha-lead","role":"team_leader"},
			{"name":"alpha-dev","role":"worker"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var team v1beta1.Team
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-team", Namespace: "default"}, &team); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if got, want := team.Spec.WorkerMembers, []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("workerMembers = %#v, want %#v", got, want)
	}
	if team.Spec.HeartbeatEvery != "30m" {
		t.Fatalf("heartbeatEvery = %q, want 30m", team.Spec.HeartbeatEvery)
	}
}

func TestCreateTeamRejectsMissingWorkerReference(t *testing.T) {
	scheme := newServerTestScheme(t)
	leader := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-lead", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen3.5-plus"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	body := []byte(`{
		"name":"alpha-team",
		"workerMembers":[
			{"name":"alpha-lead","role":"team_leader"},
			{"name":"missing-worker","role":"worker"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "missing-worker") {
		t.Fatalf("response should identify the missing Worker: %s", rec.Body.String())
	}
}

func TestCreateTeamRejectsEmptyWorkerRole(t *testing.T) {
	scheme := newServerTestScheme(t)
	leader := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-lead", Namespace: "default"},
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader, worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	body := []byte(`{
		"name":"alpha-team",
		"workerMembers":[
			{"name":"alpha-lead","role":"team_leader"},
			{"name":"alpha-dev"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid role") {
		t.Fatalf("response should identify the empty role: %s", rec.Body.String())
	}
}

func TestCreateManagerPreservesResources(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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

func TestGetWorkerRequiresWorkerCRForTeamMember(t *testing.T) {
	scheme := newServerTestScheme(t)
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()
	handler.GetWorker(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestGetWorkerEnrichesTeamReferencesMemberCR(t *testing.T) {
	scheme := newServerTestScheme(t)
	worker := &v1beta1.Worker{}
	worker.Name = "alpha-dev"
	worker.Namespace = "default"
	worker.Spec.Runtime = "copaw"
	worker.Spec.Identity = "backend engineer"
	worker.Spec.Skills = []string{"github-operations"}
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
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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
	if resp.Identity != "backend engineer" || !reflect.DeepEqual(resp.Skills, []string{"github-operations"}) {
		t.Fatalf("Worker spec not preserved in response: %+v", resp)
	}
}

func TestListWorkersDoesNotSynthesizeMissingTeamMembers(t *testing.T) {
	scheme := newServerTestScheme(t)

	standalone := &v1beta1.Worker{}
	standalone.Name = "solo"
	standalone.Namespace = "default"

	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(standalone, team).
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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
	if list.Total != 1 || list.Workers[0].Name != "solo" {
		t.Fatalf("expected only the existing Worker CR, got %+v", list.Workers)
	}
}

func TestListWorkersTeamFilterIncludesTeamReferencesMembers(t *testing.T) {
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
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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

func TestUpdateWorkerAllowsTeamMember(t *testing.T) {
	scheme := newServerTestScheme(t)
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"}}
	team := &v1beta1.Team{}
	team.Name = "alpha-team"
	team.Namespace = "default"
	team.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev", Role: "worker"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker, team).
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/alpha-dev", bytes.NewReader([]byte(`{"model":"new-model"}`)))
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()
	handler.UpdateWorker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var got v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "alpha-dev", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get Worker: %v", err)
	}
	if got.Spec.Model != "new-model" {
		t.Fatalf("model = %q, want new-model", got.Spec.Model)
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
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(team).
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workers/alpha-dev", nil)
	req.SetPathValue("name", "alpha-dev")
	rec := httptest.NewRecorder()
	handler.DeleteWorker(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
}

func TestUpdateTeamMembershipAndHeartbeat(t *testing.T) {
	scheme := newServerTestScheme(t)
	leader := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alpha-lead", Namespace: "default"}}
	dev := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alpha-dev", Namespace: "default"}}
	qa := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "alpha-qa", Namespace: "default"}}
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-team", Namespace: "default"},
		Spec: v1beta1.TeamSpec{
			HeartbeatEvery: "30m",
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: "alpha-lead", Role: "team_leader"},
				{Name: "alpha-dev", Role: "worker"},
			},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader, dev, qa, team).
		Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	updateBody := []byte(`{
		"heartbeatEvery":"45m",
		"workerMembers":[
			{"name":"alpha-lead","role":"team_leader"},
			{"name":"alpha-qa","role":"worker"}
		]
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
		t.Fatalf("heartbeatEvery = %q, want 45m", updated.Spec.HeartbeatEvery)
	}
	if got, want := updated.Spec.WorkerMembers, []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-qa", Role: "worker"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("workerMembers = %#v, want %#v", got, want)
	}

	var resp TeamResponse
	if err := json.Unmarshal(updateRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.LeaderName != "alpha-lead" || !reflect.DeepEqual(resp.WorkerNames, []string{"alpha-qa"}) {
		t.Fatalf("unexpected response membership: %+v", resp)
	}
	if !reflect.DeepEqual(resp.WorkerMembers, updated.Spec.WorkerMembers) {
		t.Fatalf("workerMembers = %#v, want %#v", resp.WorkerMembers, updated.Spec.WorkerMembers)
	}
}

func TestCreateTeamResponseDerivesNamesFromWorkerReferences(t *testing.T) {
	scheme := newServerTestScheme(t)
	leader := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "lead-cr", Namespace: "default"}}
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "dev-cr", Namespace: "default"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader, worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	body := []byte(`{
		"name":"alpha-team",
		"teamName":"alpha",
		"workerMembers":[
			{"name":"lead-cr","role":"team_leader"},
			{"name":"dev-cr","role":"worker"}
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
	var resp TeamResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.LeaderName != "lead-cr" || !reflect.DeepEqual(resp.WorkerNames, []string{"dev-cr"}) {
		t.Fatalf("unexpected response membership: %+v", resp)
	}
}

func TestCreateAndUpdateManagerPersistsModelProvider(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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

func TestUpdateManagerClearsModelProviderWhenExplicitlyEmpty(t *testing.T) {
	scheme := newServerTestScheme(t)
	manager := &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"},
		Spec: v1beta1.ManagerSpec{
			Model:         "custom-model",
			ModelProvider: "deleted-provider",
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(manager).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	updateReq := httptest.NewRequest(http.MethodPut, "/api/v1/managers/default", bytes.NewReader([]byte(`{
		"model":"known-good-model",
		"modelProvider":""
	}`)))
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
	if updated.Spec.Model != "known-good-model" {
		t.Fatalf("updated manager model=%q, want known-good-model", updated.Spec.Model)
	}
	if updated.Spec.ModelProvider != "" {
		t.Fatalf("updated manager modelProvider=%q, want empty", updated.Spec.ModelProvider)
	}
}

func TestCreateTeamRequiresExactlyOneLeaderReference(t *testing.T) {
	scheme := newServerTestScheme(t)
	leadOne := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "lead-one", Namespace: "default"}}
	leadTwo := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "lead-two", Namespace: "default"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leadOne, leadTwo).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	body := []byte(`{
		"name":"invalid-team",
		"workerMembers":[
			{"name":"lead-one","role":"team_leader"},
			{"name":"lead-two","role":"team_leader"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateTeam(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

// TestCreateWorker_StampsControllerLabel verifies that the HTTP API
// force-overwrites the agentteams.io/controller label on Create. A caller
// attempting to smuggle a different controller value must not succeed:
// the serving controller's own name always wins.
func TestCreateWorker_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a", nil)

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
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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

func TestCreateWorkerUsesConfiguredDefaultRuntime(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)
	handler.defaultWorkerRuntime = backend.RuntimeQwenPaw

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
	if got := stored.Spec.Runtime; got != backend.RuntimeQwenPaw {
		t.Fatalf("worker.spec.runtime = %q, want %q", got, backend.RuntimeQwenPaw)
	}
}

// CoPaw is upgrade-compat only (issue #1310): new workers may not be created
// on it, and a worker may not be switched onto it — but existing CoPaw
// workers keep working and can be migrated to QwenPaw.
func TestCreateWorkerRejectsLegacyCopawRuntime(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	body := []byte(`{"name":"legacy-w1","model":"qwen3.5-plus","runtime":"copaw"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateWorker(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "qwenpaw") || !strings.Contains(rec.Body.String(), "1310") {
		t.Fatalf("error body should steer to qwenpaw and cite issue 1310, got: %s", rec.Body.String())
	}

	// A copaw *default* runtime resolves the same way: still rejected.
	defaultCopaw := NewResourceHandler(k8sClient, "default", nil, "", nil)
	defaultCopaw.defaultWorkerRuntime = backend.RuntimeCopaw
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/workers",
		bytes.NewReader([]byte(`{"name":"legacy-w2","model":"qwen3.5-plus"}`)))
	rec2 := httptest.NewRecorder()
	defaultCopaw.CreateWorker(rec2, req2)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d for copaw default runtime, got %d: %s",
			http.StatusBadRequest, rec2.Code, rec2.Body.String())
	}
	var stored v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "legacy-w1", Namespace: "default"}, &stored); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get worker: %v", err)
		}
	} else {
		t.Fatalf("worker legacy-w1 should not have been created")
	}
}

func TestUpdateWorkerRejectsSwitchToLegacyCopaw(t *testing.T) {
	scheme := newServerTestScheme(t)
	qwenpawWorker := &v1beta1.Worker{}
	qwenpawWorker.Name = "upgrade-candidate"
	qwenpawWorker.Namespace = "default"
	qwenpawWorker.Spec.Model = "qwen3.5-plus"
	qwenpawWorker.Spec.Runtime = backend.RuntimeQwenPaw

	copawWorker := &v1beta1.Worker{}
	copawWorker.Name = "legacy-in-place"
	copawWorker.Namespace = "default"
	copawWorker.Spec.Model = "qwen3.5-plus"
	copawWorker.Spec.Runtime = backend.RuntimeCopaw

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(qwenpawWorker, copawWorker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	// 1. qwenpaw → copaw switch is rejected; the CR keeps its runtime.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/upgrade-candidate",
		bytes.NewReader([]byte(`{"runtime":"copaw"}`)))
	req.SetPathValue("name", "upgrade-candidate")
	rec := httptest.NewRecorder()
	handler.UpdateWorker(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "qwenpaw") {
		t.Fatalf("error body should steer to qwenpaw, got: %s", rec.Body.String())
	}
	var got v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "upgrade-candidate", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got.Spec.Runtime != backend.RuntimeQwenPaw {
		t.Fatalf("runtime changed to %q, want %q", got.Spec.Runtime, backend.RuntimeQwenPaw)
	}

	// 2. Existing copaw worker: a runtime-less update (e.g. model) still works.
	req = httptest.NewRequest(http.MethodPut, "/api/v1/workers/legacy-in-place",
		bytes.NewReader([]byte(`{"model":"new-model"}`)))
	req.SetPathValue("name", "legacy-in-place")
	rec = httptest.NewRecorder()
	handler.UpdateWorker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "legacy-in-place", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got.Spec.Model != "new-model" || got.Spec.Runtime != backend.RuntimeCopaw {
		t.Fatalf("model/runtime = %q/%q, want new-model/%q", got.Spec.Model, got.Spec.Runtime, backend.RuntimeCopaw)
	}

	// 3. copaw → qwenpaw is the upgrade path and must be allowed.
	req = httptest.NewRequest(http.MethodPut, "/api/v1/workers/legacy-in-place",
		bytes.NewReader([]byte(`{"runtime":"qwenpaw"}`)))
	req.SetPathValue("name", "legacy-in-place")
	rec = httptest.NewRecorder()
	handler.UpdateWorker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "legacy-in-place", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got.Spec.Runtime != backend.RuntimeQwenPaw {
		t.Fatalf("runtime = %q, want %q after upgrade", got.Spec.Runtime, backend.RuntimeQwenPaw)
	}
}

// A runtime switch must not keep the previous runtime's image pin: the
// recreated container has to resolve the new runtime's image (issue #1310
// upgrade path, exercised end-to-end by test-29).
func TestUpdateWorkerRuntimeSwitchDropsStaleImagePin(t *testing.T) {
	scheme := newServerTestScheme(t)
	legacy := &v1beta1.Worker{}
	legacy.Name = "pinned-legacy"
	legacy.Namespace = "default"
	legacy.Spec.Model = "qwen3.5-plus"
	legacy.Spec.Runtime = backend.RuntimeCopaw
	legacy.Spec.Image = "registry.example.com/agentteams-copaw-worker:v1.2.4"

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	// 1. copaw → qwenpaw without an explicit image clears the stale pin.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/pinned-legacy",
		bytes.NewReader([]byte(`{"runtime":"qwenpaw"}`)))
	req.SetPathValue("name", "pinned-legacy")
	rec := httptest.NewRecorder()
	handler.UpdateWorker(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var got v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "pinned-legacy", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got.Spec.Runtime != backend.RuntimeQwenPaw {
		t.Fatalf("runtime = %q, want %q", got.Spec.Runtime, backend.RuntimeQwenPaw)
	}
	if got.Spec.Image != "" {
		t.Fatalf("image pin = %q, want cleared when the runtime changes", got.Spec.Image)
	}

	// 2. An explicit image in the same request still wins.
	req = httptest.NewRequest(http.MethodPut, "/api/v1/workers/pinned-legacy",
		bytes.NewReader([]byte(`{"runtime":"qwenpaw","image":"registry.example.com/agentteams-qwenpaw-worker:v1.2.4"}`)))
	req.SetPathValue("name", "pinned-legacy")
	rec = httptest.NewRecorder()
	handler.UpdateWorker(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "pinned-legacy", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got.Spec.Image != "registry.example.com/agentteams-qwenpaw-worker:v1.2.4" {
		t.Fatalf("image = %q, want the explicitly provided image", got.Spec.Image)
	}
}

func TestWorkerResponseFlagsLegacyCopawRuntime(t *testing.T) {
	copawWorker := &v1beta1.Worker{}
	copawWorker.Name = "legacy"
	copawWorker.Namespace = "default"
	copawWorker.Spec.Runtime = backend.RuntimeCopaw

	qwenpawWorker := &v1beta1.Worker{}
	qwenpawWorker.Name = "modern"
	qwenpawWorker.Namespace = "default"
	qwenpawWorker.Spec.Runtime = backend.RuntimeQwenPaw

	if got := workerToResponse(copawWorker); !got.RuntimeDeprecated {
		t.Fatalf("copaw worker: RuntimeDeprecated = false, want true")
	}
	if got := workerToResponse(qwenpawWorker); got.RuntimeDeprecated {
		t.Fatalf("qwenpaw worker: RuntimeDeprecated = true, want false")
	}
}

func TestCreateTeam_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	leader := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: "default"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a", nil)

	body := []byte(`{"name":"t1","workerMembers":[{"name":"l1","role":"team_leader"}]}`)
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
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a", nil)

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
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a", nil)

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
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

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
		t.Fatalf("add agentteams scheme: %v", err)
	}
	return scheme
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

// TestListTeams_L2Scoped guards the L2 team list path: an L2 human only sees
// the teams in its accessibleTeams (the plugin workbench depends on this).
func TestListTeams_L2Scoped(t *testing.T) {
	scheme := newServerTestScheme(t)

	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"

	beta := &v1beta1.Team{}
	beta.Name = "beta-team"
	beta.Namespace = "default"

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(alpha, beta).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{
		Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"},
	}))
	rec := httptest.NewRecorder()
	handler.ListTeams(rec, req)

	var resp TeamListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Teams) != 1 || resp.Teams[0].Name != "alpha-team" {
		t.Fatalf("L2 teams=%+v, want only alpha-team", resp.Teams)
	}
}

// TestListWorkers_L2Scoped guards the L2 worker list path: an L2 human only
// sees workers in the teams they control; standalone and other-team workers
// are hidden.
func TestListWorkers_L2Scoped(t *testing.T) {
	scheme := newServerTestScheme(t)

	solo := &v1beta1.Worker{}
	solo.Name = "solo"
	solo.Namespace = "default"

	alphaLead := &v1beta1.Worker{}
	alphaLead.Name = "alpha-lead"
	alphaLead.Namespace = "default"

	alphaDev := &v1beta1.Worker{}
	alphaDev.Name = "alpha-dev"
	alphaDev.Namespace = "default"

	betaDev := &v1beta1.Worker{}
	betaDev.Name = "beta-dev"
	betaDev.Namespace = "default"

	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "alpha-lead", Role: "team_leader"},
		{Name: "alpha-dev"},
	}

	beta := &v1beta1.Team{}
	beta.Name = "beta-team"
	beta.Namespace = "default"
	beta.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "beta-dev"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(solo, alphaLead, alphaDev, betaDev, alpha, beta).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{
		Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"},
	}))
	rec := httptest.NewRecorder()
	handler.ListWorkers(rec, req)

	var resp WorkerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("L2 workers total=%d, want 2 (alpha-lead + alpha-dev); got %+v", resp.Total, resp.Workers)
	}
	names := map[string]bool{}
	for _, w := range resp.Workers {
		names[w.Name] = true
	}
	if !names["alpha-lead"] || !names["alpha-dev"] || names["beta-dev"] || names["solo"] {
		t.Fatalf("L2 workers=%v, want alpha-lead+alpha-dev only (no beta-dev/solo)", names)
	}
}

// TestGetTeam_L2Scoped guards single-team fetch: an L2 human may only read a
// team in its accessibleTeams; other teams are hidden (404, W8).
func TestGetTeam_L2Scoped(t *testing.T) {
	scheme := newServerTestScheme(t)
	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	beta := &v1beta1.Team{}
	beta.Name = "beta-team"
	beta.Namespace = "default"

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(alpha, beta).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)
	l2 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/teams/alpha-team", nil)
	req.SetPathValue("name", "alpha-team")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), l2))
	rec := httptest.NewRecorder()
	handler.GetTeam(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("accessible team status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/teams/beta-team", nil)
	req2.SetPathValue("name", "beta-team")
	req2 = req2.WithContext(context.WithValue(req2.Context(), authpkg.CallerKeyForTest(), l2))
	rec2 := httptest.NewRecorder()
	handler.GetTeam(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("non-accessible team status=%d, want 404 (W8: hide existence)", rec2.Code)
	}
}

// TestGetWorker_L2Scoped guards single-worker fetch: an L2 human may only read
// workers in its accessible teams; other-team/standalone workers are hidden
// (404, W8).
func TestGetWorker_L2Scoped(t *testing.T) {
	scheme := newServerTestScheme(t)
	alphaLead := &v1beta1.Worker{}
	alphaLead.Name = "alpha-lead"
	alphaLead.Namespace = "default"
	betaDev := &v1beta1.Worker{}
	betaDev.Name = "beta-dev"
	betaDev.Namespace = "default"
	solo := &v1beta1.Worker{}
	solo.Name = "solo"
	solo.Namespace = "default"

	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "alpha-lead", Role: "team_leader"}}
	beta := &v1beta1.Team{}
	beta.Name = "beta-team"
	beta.Namespace = "default"
	beta.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "beta-dev"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(alphaLead, betaDev, solo, alpha, beta).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)
	l2 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}

	// Own team worker -> 200.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-lead", nil)
	req.SetPathValue("name", "alpha-lead")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), l2))
	rec := httptest.NewRecorder()
	handler.GetWorker(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("own team worker status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Other team worker -> 404 (hidden, W8).
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/workers/beta-dev", nil)
	req2.SetPathValue("name", "beta-dev")
	req2 = req2.WithContext(context.WithValue(req2.Context(), authpkg.CallerKeyForTest(), l2))
	rec2 := httptest.NewRecorder()
	handler.GetWorker(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("other team worker status=%d, want 404 (W8: hide existence)", rec2.Code)
	}

	// Standalone worker -> 404 (hidden, W8).
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/workers/solo", nil)
	req3.SetPathValue("name", "solo")
	req3 = req3.WithContext(context.WithValue(req3.Context(), authpkg.CallerKeyForTest(), l2))
	rec3 := httptest.NewRecorder()
	handler.GetWorker(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("standalone worker status=%d, want 404 (W8: hide existence)", rec3.Code)
	}
}

// TestGetWorker_L3Scoped guards single-worker fetch for L3 (worker-scoped)
// humans: exactly the assigned workers are readable (team members and
// standalone alike); everything else is hidden (404, W8).
func TestGetWorker_L3Scoped(t *testing.T) {
	scheme := newServerTestScheme(t)
	alphaDev := &v1beta1.Worker{}
	alphaDev.Name = "alpha-dev"
	alphaDev.Namespace = "default"
	betaDev := &v1beta1.Worker{}
	betaDev.Name = "beta-dev"
	betaDev.Namespace = "default"
	solo := &v1beta1.Worker{}
	solo.Name = "solo"
	solo.Namespace = "default"

	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "alpha-dev"}}
	beta := &v1beta1.Team{}
	beta.Name = "beta-team"
	beta.Namespace = "default"
	beta.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "beta-dev"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(alphaDev, betaDev, solo, alpha, beta).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)
	l3 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "viewer", AccessibleWorkers: []string{"alpha-dev", "solo"}}

	get := func(name string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/"+name, nil)
		req.SetPathValue("name", name)
		req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), l3))
		rec := httptest.NewRecorder()
		handler.GetWorker(rec, req)
		return rec.Code
	}

	// Assigned team member (of a team the human does NOT control) -> 200.
	if code := get("alpha-dev"); code != http.StatusOK {
		t.Fatalf("assigned team worker status=%d, want 200", code)
	}
	// Assigned standalone worker -> 200.
	if code := get("solo"); code != http.StatusOK {
		t.Fatalf("assigned standalone worker status=%d, want 200", code)
	}
	// Unassigned worker in an uncontrolled team -> 404 (hidden, W8).
	if code := get("beta-dev"); code != http.StatusNotFound {
		t.Fatalf("unassigned other-team worker status=%d, want 404 (W8)", code)
	}
}

// TestListWorkers_L3Scoped guards list filtering: an L3 human sees exactly
// its assigned workers (team + standalone), nothing else — no team-scope
// leakage, no existence probe.
func TestListWorkers_L3Scoped(t *testing.T) {
	scheme := newServerTestScheme(t)
	alphaDev := &v1beta1.Worker{}
	alphaDev.Name = "alpha-dev"
	alphaDev.Namespace = "default"
	betaDev := &v1beta1.Worker{}
	betaDev.Name = "beta-dev"
	betaDev.Namespace = "default"
	solo := &v1beta1.Worker{}
	solo.Name = "solo"
	solo.Namespace = "default"

	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "alpha-dev"}}
	beta := &v1beta1.Team{}
	beta.Name = "beta-team"
	beta.Namespace = "default"
	beta.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "beta-dev"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(alphaDev, betaDev, solo, alpha, beta).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)
	l3 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "viewer", AccessibleWorkers: []string{"beta-dev"}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), l3))
	rec := httptest.NewRecorder()
	handler.ListWorkers(rec, req)

	var resp WorkerListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Workers) != 1 || resp.Workers[0].Name != "beta-dev" {
		t.Fatalf("L3 workers=%+v, want exactly [beta-dev]", resp)
	}
}

// TestUpdateWorker_L3Denied pins the read-only contract (Q2): even for a
// worker the L3 human is assigned to, the update path stays denied. The
// middleware (authorizer requireSameTeam) answers 403 first for team
// workers; this handler-level probe pins the defense-in-depth fallback —
// the handler's own team-scope check hides the worker as 404, assigned or
// not, so no handler path can widen L3 into a writer.
func TestUpdateWorker_L3Denied(t *testing.T) {
	scheme := newServerTestScheme(t)
	alphaDev := &v1beta1.Worker{}
	alphaDev.Name = "alpha-dev"
	alphaDev.Namespace = "default"
	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "alpha-dev"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(alphaDev, alpha).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)
	l3 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "viewer", AccessibleWorkers: []string{"alpha-dev"}}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/alpha-dev", strings.NewReader(`{"skills":[]}`))
	req.SetPathValue("name", "alpha-dev")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), l3))
	rec := httptest.NewRecorder()
	handler.UpdateWorker(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("L3 update of an ASSIGNED worker status=%d, want 404 (read-only, W8)", rec.Code)
	}
}

// TestGetWorker_L3MCPCredentialsSanitized pins the L3 no-plaintext-
// credentials requirement on the worker detail surface: MCP endpoint URLs
// may embed credentials in the query (?api_key=...) or the userinfo
// component (user:pass@host); both must be absent from the raw L3 response,
// while non-secret URL metadata (host, path, non-credential query values)
// is retained.
func TestGetWorker_L3MCPCredentialsSanitized(t *testing.T) {
	scheme := newServerTestScheme(t)
	alphaDev := &v1beta1.Worker{}
	alphaDev.Name = "alpha-dev"
	alphaDev.Namespace = "default"
	alphaDev.Spec.McpServers = []v1beta1.MCPServer{
		{Name: "ext", URL: "https://mcp.example.test/mcp?api_key=REVIEW_SENTINEL_SECRET"},
		{Name: "apiKey", URL: "https://mcp.example.test/mcp?apiKey=REVIEW_SENTINEL_SECRET"},
		{Name: "key", URL: "https://mcp.example.test/key?key=REVIEW_SENTINEL_SECRET"},
		{Name: "auth", URL: "https://review-user:REVIEW_SENTINEL_PASS@mcp.example.test/auth"},
		{Name: "clean", URL: "https://plain.example.test/mcp?limit=5"},
	}
	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "alpha-dev"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(alphaDev, alpha).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)
	l3 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "viewer", AccessibleWorkers: []string{"alpha-dev"}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev", nil)
	req.SetPathValue("name", "alpha-dev")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), l3))
	rec := httptest.NewRecorder()
	handler.GetWorker(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("assigned worker detail status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, sentinel := range []string{"REVIEW_SENTINEL_SECRET", "REVIEW_SENTINEL_PASS", "review-user"} {
		if strings.Contains(body, sentinel) {
			t.Fatalf("L3 raw response leaks credential material %q: %s", sentinel, body)
		}
	}
	// Non-secret metadata (scheme://host[:port]/path) must survive the scrub.
	for _, want := range []string{
		"https://mcp.example.test/mcp",
		"https://mcp.example.test/auth",
		"https://plain.example.test/mcp",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("L3 response lost non-secret metadata %q: %s", want, body)
		}
	}
	// Query values are unclassified input for arbitrary external MCP
	// endpoints: even a name that does not look like a credential must not
	// be exposed to L3 (fail-closed, not denylisted).
	if strings.Contains(body, "limit=5") {
		t.Fatalf("L3 response exposed an unclassified query value: %s", body)
	}
}

// TestListWorkers_L3MCPCredentialsSanitized pins the same contract on the
// list surface: the sentinel must be absent from the raw list JSON for an
// L3 viewer, with the non-credential metadata retained.
func TestListWorkers_L3MCPCredentialsSanitized(t *testing.T) {
	scheme := newServerTestScheme(t)
	betaDev := &v1beta1.Worker{}
	betaDev.Name = "beta-dev"
	betaDev.Namespace = "default"
	betaDev.Spec.McpServers = []v1beta1.MCPServer{
		{Name: "ext", URL: "https://mcp.example.test/mcp?api_key=REVIEW_SENTINEL_SECRET"},
		{Name: "apiKey", URL: "https://mcp.example.test/mcp?apiKey=REVIEW_SENTINEL_SECRET"},
		{Name: "key", URL: "https://mcp.example.test/key?key=REVIEW_SENTINEL_SECRET"},
	}
	beta := &v1beta1.Team{}
	beta.Name = "beta-team"
	beta.Namespace = "default"
	beta.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "beta-dev"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(betaDev, beta).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)
	l3 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "viewer", AccessibleWorkers: []string{"beta-dev"}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), l3))
	rec := httptest.NewRecorder()
	handler.ListWorkers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "REVIEW_SENTINEL_SECRET") {
		t.Fatalf("L3 raw list response leaks the MCP api_key: %s", body)
	}
	if !strings.Contains(body, "https://mcp.example.test/mcp") {
		t.Fatalf("L3 list response lost the MCP endpoint metadata: %s", body)
	}
}

// TestGetWorker_L2MCPCredentialsVerbatim pins that the L3 scrub does not
// over-apply: an L2 human (team scope) reads the assigned-in-team worker's
// MCP URLs verbatim — credential URLs are legitimate for team controllers.
func TestGetWorker_L2MCPCredentialsVerbatim(t *testing.T) {
	scheme := newServerTestScheme(t)
	alphaDev := &v1beta1.Worker{}
	alphaDev.Name = "alpha-dev"
	alphaDev.Namespace = "default"
	alphaDev.Spec.McpServers = []v1beta1.MCPServer{
		{Name: "ext", URL: "https://mcp.example.test/mcp?api_key=REVIEW_SENTINEL_SECRET"},
		{Name: "apiKey", URL: "https://mcp.example.test/mcp?apiKey=REVIEW_SENTINEL_SECRET"},
	}
	alpha := &v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{{Name: "alpha-dev"}}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(alphaDev, alpha).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "", nil)
	l2 := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workers/alpha-dev", nil)
	req.SetPathValue("name", "alpha-dev")
	req = req.WithContext(context.WithValue(req.Context(), authpkg.CallerKeyForTest(), l2))
	rec := httptest.NewRecorder()
	handler.GetWorker(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("L2 own-team worker detail status=%d, want 200", rec.Code)
	}
	// Both the denylisted (api_key) and the non-denylisted (apiKey) forms
	// must reach L2 verbatim — the scrub applies to L3 only.
	for _, want := range []string{"api_key=REVIEW_SENTINEL_SECRET", "apiKey=REVIEW_SENTINEL_SECRET"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("L2 response must carry the MCP URL verbatim (%s): %s", want, rec.Body.String())
		}
	}
}

func TestCreateWorkerDefaultModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"omitted", `{"name":"default-model"}`, "deployment-model"},
		{"empty", `{"name":"default-model","model":""}`, "deployment-model"},
		{"whitespace", `{"name":"default-model","model":"  "}`, "deployment-model"},
		{"explicit", `{"name":"default-model","model":"worker-model"}`, "worker-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k8s := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).Build()
			mw := authpkg.NewMiddleware(&alwaysAdminAuth{}, authpkg.NewCREnricher(k8s, "default"), authpkg.NewAuthorizer(), k8s, "default")
			srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", AuthMw: mw, DefaultModel: "deployment-model"})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer sa-token")
			rec := httptest.NewRecorder()
			srv.Mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var response WorkerResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Model != tc.want {
				t.Errorf("response model=%q, want %q", response.Model, tc.want)
			}
			var worker v1beta1.Worker
			if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "default-model"}, &worker); err != nil {
				t.Fatal(err)
			}
			if worker.Spec.Model != tc.want {
				t.Errorf("persisted model=%q, want %q", worker.Spec.Model, tc.want)
			}
		})
	}
}
