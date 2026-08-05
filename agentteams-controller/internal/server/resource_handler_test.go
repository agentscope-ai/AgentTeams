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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Team leaders cannot create infrastructure resources through /api/v1/workers.
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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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

func TestCreateWorkerPersistsRuntimeConfigAndReturnsDetachedResponse(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{
		"name":"deepagents-worker",
		"runtime":"deepagents",
		"runtimeConfig":{"deepagents":{
			"approvals":{"fileWrites":"required","mcpDefault":"required","coordinators":["@human:example.org"]},
			"execution":{"mode":"sandbox"}
		}}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.CreateWorker(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var response WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := response.RuntimeConfig.DeepAgents.Approvals.Coordinators; !reflect.DeepEqual(got, []string{"@human:example.org"}) {
		t.Fatalf("response runtimeConfig coordinators = %#v, want @human:example.org", got)
	}

	var stored v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "deepagents-worker", Namespace: "default"}, &stored); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := stored.Spec.RuntimeConfig.DeepAgents.Execution.Mode; got != "sandbox" {
		t.Fatalf("stored execution mode = %q, want sandbox", got)
	}

	response.RuntimeConfig.DeepAgents.Approvals.Coordinators[0] = "@mutated:example.org"
	if got := stored.Spec.RuntimeConfig.DeepAgents.Approvals.Coordinators[0]; got != "@human:example.org" {
		t.Fatalf("response runtimeConfig changed persisted Worker config: %q", got)
	}

	source := &v1beta1.Worker{Spec: v1beta1.WorkerSpec{RuntimeConfig: &v1beta1.WorkerRuntimeConfig{
		DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{Approvals: v1beta1.DeepAgentsApprovalConfig{
			Coordinators: []string{"@human:example.org"},
		}},
	}}}
	converted := workerToResponse(source)
	converted.RuntimeConfig.DeepAgents.Approvals.Coordinators[0] = "@aliased:example.org"
	if got := source.Spec.RuntimeConfig.DeepAgents.Approvals.Coordinators[0]; got != "@human:example.org" {
		t.Fatalf("worker response runtimeConfig aliases Worker spec: %q", got)
	}
}

func TestUpdateWorkerReplacesRuntimeConfigAndKeepsItWhenOmitted(t *testing.T) {
	scheme := newServerTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "deepagents-worker", Namespace: "default"},
		Spec: v1beta1.WorkerSpec{Runtime: "deepagents", RuntimeConfig: &v1beta1.WorkerRuntimeConfig{
			DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"}},
		}},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/deepagents-worker", bytes.NewBufferString(`{
		"runtimeConfig":{"deepagents":{"approvals":{"fileWrites":"required","mcpDefault":"required","coordinators":["@approver:example.org"]}}}
	}`))
	req.SetPathValue("name", "deepagents-worker")
	rec := httptest.NewRecorder()
	handler.UpdateWorker(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var response WorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := response.RuntimeConfig.DeepAgents.Approvals.MCPDefault; got != "required" {
		t.Fatalf("response mcpDefault = %q, want required", got)
	}

	unchanged := httptest.NewRequest(http.MethodPut, "/api/v1/workers/deepagents-worker", bytes.NewBufferString(`{"model":"qwen-max"}`))
	unchanged.SetPathValue("name", "deepagents-worker")
	unchangedRec := httptest.NewRecorder()
	handler.UpdateWorker(unchangedRec, unchanged)
	if unchangedRec.Code != http.StatusOK {
		t.Fatalf("omitted runtimeConfig update status = %d, want %d: %s", unchangedRec.Code, http.StatusOK, unchangedRec.Body.String())
	}

	var stored v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: "deepagents-worker", Namespace: "default"}, &stored); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := stored.Spec.RuntimeConfig.DeepAgents.Approvals.Coordinators; !reflect.DeepEqual(got, []string{"@approver:example.org"}) {
		t.Fatalf("stored coordinators after omitted update = %#v, want @approver:example.org", got)
	}
}

func TestCreateWorkerRejectsNonStrictJSONWithoutCreatingWorker(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown top-level field", body: `{"name":"strict-worker","unexpected":true}`},
		{name: "single top-level case alias", body: `{"Name":"strict-worker"}`},
		{name: "paired top-level case alias", body: `{"name":"strict-worker","Name":"other-worker"}`},
		{name: "unknown nested field", body: `{"name":"strict-worker","runtimeConfig":{"deepagents":{"execution":{"mode":"sandbox","unexpected":true}}}}`},
		{name: "single nested case alias", body: `{"name":"strict-worker","runtimeConfig":{"deepagents":{"execution":{"Mode":"sandbox"}}}}`},
		{name: "paired nested case alias", body: `{"name":"strict-worker","runtimeConfig":{"deepagents":{"execution":{"mode":"sandbox","Mode":"local"}}}}`},
		{name: "duplicate escaped-equivalent top-level field", body: `{"name":"strict-worker","na\u006de":"other-worker"}`},
		{name: "duplicate escaped-equivalent nested field", body: `{"name":"strict-worker","runtimeConfig":{"deepagents":{"execution":{"mode":"sandbox","mo\u0064e":"local"}}}}`},
		{name: "duplicate field in array object", body: `{"name":"strict-worker","runtimeConfig":{"deepagents":{"approvals":{"mcpRules":[{"server":"one","ser\u0076er":"two","tool":"run","mode":"required"}]}}}}`},
		{name: "trailing JSON", body: `{"name":"strict-worker"} {"name":"second"}`},
		{name: "empty body", body: ``},
		{name: "malformed JSON", body: `{"name":"strict-worker"`},
		{name: "oversized body", body: strings.Repeat(" ", 1024*1024+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newServerTestScheme(t)
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			handler := NewResourceHandler(k8sClient, "default", nil, "")
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			handler.CreateWorker(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			var workers v1beta1.WorkerList
			if err := k8sClient.List(context.Background(), &workers, client.InNamespace("default")); err != nil {
				t.Fatal(err)
			}
			if len(workers.Items) != 0 {
				t.Fatalf("invalid request created Workers: %#v", workers.Items)
			}
		})
	}
}

func TestUpdateWorkerRejectsNonStrictJSONWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown top-level field", body: `{"model":"changed","unexpected":true}`},
		{name: "single top-level case alias", body: `{"Model":"changed"}`},
		{name: "paired top-level case alias", body: `{"model":"changed","Model":"other"}`},
		{name: "unknown nested field", body: `{"runtimeConfig":{"deepagents":{"approvals":{"fileWrites":"required","unexpected":true}}}}`},
		{name: "single nested case alias", body: `{"runtimeConfig":{"deepagents":{"execution":{"Mode":"sandbox"}}}}`},
		{name: "paired nested case alias", body: `{"runtimeConfig":{"deepagents":{"execution":{"mode":"sandbox","Mode":"local"}}}}`},
		{name: "duplicate escaped-equivalent top-level field", body: `{"model":"changed","mo\u0064el":"other"}`},
		{name: "duplicate escaped-equivalent nested field", body: `{"runtimeConfig":{"deepagents":{"execution":{"mode":"sandbox","mo\u0064e":"local"}}}}`},
		{name: "duplicate field in array object", body: `{"runtimeConfig":{"deepagents":{"approvals":{"mcpRules":[{"server":"one","ser\u0076er":"two","tool":"run","mode":"required"}]}}}}`},
		{name: "trailing JSON", body: `{"model":"changed"} null`},
		{name: "empty body", body: ``},
		{name: "malformed JSON", body: `{"model":"changed"`},
		{name: "oversized body", body: strings.Repeat(" ", 1024*1024+1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := newServerTestScheme(t)
			worker := &v1beta1.Worker{
				ObjectMeta: metav1.ObjectMeta{Name: "strict-worker", Namespace: "default"},
				Spec: v1beta1.WorkerSpec{Model: "original", Runtime: "deepagents", RuntimeConfig: &v1beta1.WorkerRuntimeConfig{
					DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"}},
				}},
			}
			before := worker.Spec.DeepCopy()
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
			handler := NewResourceHandler(k8sClient, "default", nil, "")
			req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/strict-worker", strings.NewReader(tt.body))
			req.SetPathValue("name", worker.Name)
			rec := httptest.NewRecorder()

			handler.UpdateWorker(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			var stored v1beta1.Worker
			if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(worker), &stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Spec, *before) {
				t.Fatalf("invalid update mutated Worker: got=%#v want=%#v", stored.Spec, *before)
			}
		})
	}
}

func TestUpdateWorkerStrictJSONPreservesOmittedFieldsAndAcceptsZeroValues(t *testing.T) {
	scheme := newServerTestScheme(t)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "strict-worker", Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{Model: "original", Runtime: "deepagents"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workers/strict-worker", strings.NewReader(
		`{"containerManaged":false,"runtimeConfig":{"deepagents":{"execution":{"resources":{"requests":{"cpu":"0"}}}}}}`,
	))
	req.SetPathValue("name", worker.Name)
	rec := httptest.NewRecorder()

	handler.UpdateWorker(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var stored v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(worker), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Model != "original" || stored.Spec.ContainerManaged == nil || *stored.Spec.ContainerManaged ||
		stored.Spec.RuntimeConfig.DeepAgents.Execution.Resources.Requests.CPU != "0" {
		t.Fatalf("valid zero/omitted update semantics changed: %#v", stored.Spec)
	}
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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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

func TestCreateTeamRequiresExactlyOneLeaderReference(t *testing.T) {
	scheme := newServerTestScheme(t)
	leadOne := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "lead-one", Namespace: "default"}}
	leadTwo := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "lead-two", Namespace: "default"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leadOne, leadTwo).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

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

func TestCreateWorkerUsesConfiguredDefaultRuntime(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")
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

func TestCreateTeam_StampsControllerLabel(t *testing.T) {
	scheme := newServerTestScheme(t)
	leader := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: "default"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leader).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "ctrl-a")

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
