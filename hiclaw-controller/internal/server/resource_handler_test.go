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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			"heartbeat":{"enabled":true,"every":"30m"},
			"workerIdleTimeout":"12h"
		},
		"workers":[]
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

	updateBody := []byte(`{
		"leader":{
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
	if updated.Spec.Leader.Heartbeat == nil || updated.Spec.Leader.Heartbeat.Every != "45m" {
		t.Fatalf("unexpected heartbeat config after update: %#v", updated.Spec.Leader.Heartbeat)
	}
	if updated.Spec.Leader.WorkerIdleTimeout != "24h" {
		t.Fatalf("expected worker idle timeout 24h, got %q", updated.Spec.Leader.WorkerIdleTimeout)
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
// force-overwrites the hiclaw.io/controller label on Create. A caller
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

// TestUpdateHumanAccessibleTeams verifies the PUT /api/v1/humans/{name}
// route (issue #729 closure proof). Before this PR, the REST API exposed
// only POST/GET/DELETE for Humans; HiClawReconciler internally supported
// declarative reconcile but no transport layer surface invoked it on
// spec changes. `hiclaw apply -f` on an existing Human returned HTTP 405.
//
// This test asserts the symmetric pattern matching UpdateWorker /
// UpdateTeam handlers: PUT body replaces / patches spec fields, controller
// retries on optimistic-lock conflict, response shape is humanToResponse.
func TestUpdateHumanAccessibleTeams(t *testing.T) {
	scheme := newServerTestScheme(t)
	existing := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:     "Alice",
			PermissionLevel: 2,
			AccessibleTeams: []string{"team-1"},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).WithObjects(existing).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	// PUT with new accessibleTeams; PermissionLevel + DisplayName unchanged.
	body := []byte(`{"accessibleTeams":["team-1","team-2"]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/humans/alice", bytes.NewReader(body))
	req.SetPathValue("name", "alice")
	rec := httptest.NewRecorder()
	handler.UpdateHuman(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var updated v1beta1.Human
	if err := k8sClient.Get(context.Background(),
		client.ObjectKey{Name: "alice", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated human: %v", err)
	}
	if got := updated.Spec.AccessibleTeams; len(got) != 2 || got[0] != "team-1" || got[1] != "team-2" {
		t.Fatalf("expected accessibleTeams=[team-1,team-2], got %v", got)
	}
	// Unchanged fields preserved.
	if updated.Spec.DisplayName != "Alice" {
		t.Fatalf("DisplayName must be preserved on partial update, got %q", updated.Spec.DisplayName)
	}
	if updated.Spec.PermissionLevel != 2 {
		t.Fatalf("PermissionLevel must be preserved on partial update, got %d", updated.Spec.PermissionLevel)
	}
}

// TestUpdateHumanRemoveAccessibleTeams verifies that an explicit empty
// slice removes all team accesses (vs nil which preserves). JSON
// {"accessibleTeams": []} → []string{} non-nil; the handler writes
// it back so the user no longer has access to any team.
//
// This is the haopaw dismiss flow's prod-quality path: revoking a
// Team Room from a user without recreating the Human CR.
func TestUpdateHumanRemoveAccessibleTeams(t *testing.T) {
	scheme := newServerTestScheme(t)
	existing := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:     "Bob",
			AccessibleTeams: []string{"team-1", "team-2"},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).WithObjects(existing).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"accessibleTeams":[]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/humans/bob", bytes.NewReader(body))
	req.SetPathValue("name", "bob")
	rec := httptest.NewRecorder()
	handler.UpdateHuman(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var updated v1beta1.Human
	if err := k8sClient.Get(context.Background(),
		client.ObjectKey{Name: "bob", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated human: %v", err)
	}
	if len(updated.Spec.AccessibleTeams) != 0 {
		t.Fatalf("expected accessibleTeams=[], got %v", updated.Spec.AccessibleTeams)
	}
}

// TestUpdateHumanPermissionLevel verifies the optional *int pointer
// pattern: only update PermissionLevel when explicitly sent. JSON
// {"permissionLevel": 1} sets to admin tier; absent field preserves.
//
// Note: PermissionLevel = 0 is technically valid per Kubernetes
// validation but is outside the semantic enum (1=Admin, 2=Team,
// 3=Worker per types.go:372 comment) and indistinguishable from "field
// absent" if we used plain int. Pointer disambiguates.
func TestUpdateHumanPermissionLevel(t *testing.T) {
	scheme := newServerTestScheme(t)
	existing := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "carol", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:     "Carol",
			PermissionLevel: 3,
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).WithObjects(existing).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"permissionLevel":2}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/humans/carol", bytes.NewReader(body))
	req.SetPathValue("name", "carol")
	rec := httptest.NewRecorder()
	handler.UpdateHuman(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var updated v1beta1.Human
	if err := k8sClient.Get(context.Background(),
		client.ObjectKey{Name: "carol", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("get updated human: %v", err)
	}
	if updated.Spec.PermissionLevel != 2 {
		t.Fatalf("expected permissionLevel=2, got %d", updated.Spec.PermissionLevel)
	}
}

// TestGetHumanEchoesSpecFields verifies the response shape carries the
// spec-level fields needed for read-modify-write callers (downstream
// dependents like haopaw BFF that need to add/remove a single team from
// accessibleTeams without overwriting other spec state). Before this PR,
// HumanResponse only echoed DisplayName + Status fields; spec fields like
// AccessibleTeams / Email / PermissionLevel were invisible, forcing
// callers to use raw kubectl or DELETE+CREATE workarounds.
func TestGetHumanEchoesSpecFields(t *testing.T) {
	scheme := newServerTestScheme(t)
	existing := &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:       "Dave",
			Email:             "dave@example.com",
			PermissionLevel:   2,
			AccessibleTeams:   []string{"alpha", "beta"},
			AccessibleWorkers: []string{"w1"},
			Note:              "team admin for alpha",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).WithObjects(existing).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/humans/dave", nil)
	req.SetPathValue("name", "dave")
	rec := httptest.NewRecorder()
	handler.GetHuman(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var resp HumanResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Email != "dave@example.com" {
		t.Fatalf("Email not echoed: got %q", resp.Email)
	}
	if resp.PermissionLevel != 2 {
		t.Fatalf("PermissionLevel not echoed: got %d", resp.PermissionLevel)
	}
	if len(resp.AccessibleTeams) != 2 || resp.AccessibleTeams[0] != "alpha" || resp.AccessibleTeams[1] != "beta" {
		t.Fatalf("AccessibleTeams not echoed: got %v", resp.AccessibleTeams)
	}
	if len(resp.AccessibleWorkers) != 1 || resp.AccessibleWorkers[0] != "w1" {
		t.Fatalf("AccessibleWorkers not echoed: got %v", resp.AccessibleWorkers)
	}
	if resp.Note != "team admin for alpha" {
		t.Fatalf("Note not echoed: got %q", resp.Note)
	}
}

// TestUpdateHumanNotFound verifies the same writeK8sError flow as
// UpdateWorker / UpdateTeam: missing resource → 404, not 500.
func TestUpdateHumanNotFound(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	handler := NewResourceHandler(k8sClient, "default", nil, "")

	body := []byte(`{"accessibleTeams":["team-1"]}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/humans/ghost", bytes.NewReader(body))
	req.SetPathValue("name", "ghost")
	rec := httptest.NewRecorder()
	handler.UpdateHuman(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
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
