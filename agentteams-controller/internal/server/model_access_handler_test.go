package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- PR-A1b §6: GET /api/v1/model-access tests ---

// maRig builds a ResourceHandler over a fake client seeded with objs.
func maRig(t *testing.T, objs ...client.Object) *ResourceHandler {
	t.Helper()
	scheme := newServerTestScheme(t)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return NewResourceHandler(k8sClient, "default", nil, "", nil)
}

// maReq builds a GET /api/v1/model-access request carrying the caller the
// way the auth middleware injects it.
func maReq(t *testing.T, target string, caller *authpkg.CallerIdentity) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	return r.WithContext(context.WithValue(r.Context(), authpkg.CallerKeyForTest(), caller))
}

func maWorker(name, model, provider, subagent, runtime string) *v1beta1.Worker {
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1beta1.WorkerSpec{
			Model:         model,
			ModelProvider: provider,
			SubagentModel: subagent,
			Runtime:       runtime,
		},
	}
}

func maTestBool(b bool) *bool { return &b }

func maTeam(name string, members ...v1beta1.TeamWorkerRef) *v1beta1.Team {
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.TeamSpec{WorkerMembers: members},
	}
}

func maHuman(name string, teams, workers []string, modelAccess *v1beta1.ModelAccessSpec) *v1beta1.Human {
	return &v1beta1.Human{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1beta1.HumanSpec{
			DisplayName:       name,
			AccessibleTeams:   teams,
			AccessibleWorkers: workers,
			ModelAccess:       modelAccess,
		},
	}
}

func TestModelAccessNoParamByRole(t *testing.T) {
	grant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role: "worker",
		Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "m1"}}},
	}}}
	h := maRig(t, maHuman("alice", []string{"alpha"}, nil, grant))

	// admin -> exactly {"unrestricted": true}.
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var adminBody map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &adminBody); err != nil {
		t.Fatalf("admin: unmarshal: %v", err)
	}
	if len(adminBody) != 1 {
		t.Fatalf("admin: want exactly one field, got %s", rec.Body.String())
	}
	var unrestricted bool
	if err := json.Unmarshal(adminBody["unrestricted"], &unrestricted); err != nil || !unrestricted {
		t.Fatalf("admin: want unrestricted=true, got %s", rec.Body.String())
	}

	// manager -> unrestricted as well.
	rec = httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access", &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"unrestricted":true`) {
		t.Fatalf("manager: expected unrestricted, got %d %s", rec.Code, rec.Body.String())
	}

	// human -> raw scopes, untouched.
	rec = httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha"}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("human: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var humanBody struct {
		Scopes []v1beta1.ModelAccessScope `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &humanBody); err != nil {
		t.Fatalf("human: unmarshal: %v", err)
	}
	if len(humanBody.Scopes) != 1 || humanBody.Scopes[0].Role != "worker" ||
		humanBody.Scopes[0].Main == nil || len(humanBody.Scopes[0].Main.Entries) != 1 ||
		humanBody.Scopes[0].Main.Entries[0].Model != "m1" {
		t.Fatalf("human: scopes = %+v, want the raw grant", humanBody.Scopes)
	}

	// human without a stored grant (CR absent) -> empty array, not null.
	rec = httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "ghost", Teams: []string{"alpha"}}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"scopes":[]`) {
		t.Fatalf("human without CR: expected empty scopes array, got %d %s", rec.Code, rec.Body.String())
	}

	// SA callers (team leader / worker) -> locked stub.
	for _, role := range []string{authpkg.RoleTeamLeader, authpkg.RoleWorker} {
		rec = httptest.NewRecorder()
		h.GetModelAccess(rec, maReq(t, "/api/v1/model-access", &authpkg.CallerIdentity{Role: role, Username: role}))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", role, rec.Code, rec.Body.String())
		}
		var lockedBody struct {
			Locked       bool                       `json:"locked"`
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &lockedBody); err != nil {
			t.Fatalf("%s: unmarshal: %v", role, err)
		}
		if !lockedBody.Locked || len(lockedBody.Capabilities) != 0 {
			t.Errorf("%s: want locked=true with empty capabilities, got %s", role, rec.Body.String())
		}
	}
}

func TestModelAccessWorkerResolution(t *testing.T) {
	grant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role:  "worker",
		Main:  &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "agentteams-gateway", Model: "qwen3.6-27b"}}},
		Spawn: &v1beta1.ModelAccessSurface{Inherit: maTestBool(true)},
		CodingCli: []v1beta1.ModelAccessCli{
			{CLI: "qwen-code", Inherit: maTestBool(true)},
		},
	}}}
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maWorker("w1", "qwen3.6-27b", "", "", "openclaw"),
		maHuman("alice", []string{"alpha"}, nil, grant),
	)

	// L1 callers preview the same real resolution (no unrestricted).
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Worker != "w1" || got.Role != "worker" || got.Team != "alpha" || got.Runtime != "openclaw" {
		t.Fatalf("header = %s/%s/%s/%s", got.Worker, got.Role, got.Team, got.Runtime)
	}
	if got.Locked || !got.CatalogUnavailable {
		t.Fatalf("locked=%v catalogUnavailable=%v", got.Locked, got.CatalogUnavailable)
	}

	if !got.Capabilities.Main.CanEdit || got.Capabilities.Main.Inherit || len(got.Capabilities.Main.WriteVia) != 2 {
		t.Errorf("main capabilities = %+v", got.Capabilities.Main)
	}
	if !got.Capabilities.Spawn.CanEdit || !got.Capabilities.Spawn.Inherit {
		t.Errorf("spawn capabilities = %+v", got.Capabilities.Spawn)
	}
	cli, ok := got.Capabilities.CodingCli["qwen-code"]
	if !ok {
		t.Fatalf("codingCli keys = %v", got.Capabilities.CodingCli)
	}
	if !cli.CanEdit || !cli.Inherit || cli.InheritedFrom == nil ||
		cli.InheritedFrom.Provider != "agentteams-gateway" || cli.InheritedFrom.Model != "qwen3.6-27b" {
		t.Errorf("qwen-code capabilities = %+v", cli)
	}
	if opencode := got.Capabilities.CodingCli["opencode"]; opencode.CanEdit || opencode.Reason != "cli_not_granted" {
		t.Errorf("opencode capabilities = %+v", opencode)
	}
	if qoder := got.Capabilities.CodingCli["qoder"]; qoder.CanEdit || qoder.Reason != "byok_unsupported" {
		t.Errorf("qoder capabilities = %+v", qoder)
	}
	if len(got.Capabilities.CodingCli) != 3 {
		t.Errorf("codingCli must carry all three keys, got %v", got.Capabilities.CodingCli)
	}

	if len(got.Surfaces.Main.Entries) != 1 || got.Surfaces.Main.Entries[0].Model != "qwen3.6-27b" {
		t.Errorf("surfaces.main = %+v", got.Surfaces.Main)
	}
	if len(got.Surfaces.Spawn.Entries) != 0 || !got.Surfaces.Spawn.Inherit {
		t.Errorf("surfaces.spawn = %+v", got.Surfaces.Spawn)
	}
	if len(got.Surfaces.CodingCli) != 1 {
		t.Errorf("surfaces.codingCli = %+v, want only the granted CLI", got.Surfaces.CodingCli)
	}

	if got.Current.Model != "qwen3.6-27b" || got.Current.ModelProvider != "" || got.Current.SubagentModel != "" {
		t.Errorf("current = %+v", got.Current)
	}

	// Empty entry slices must render as [] (the UI renders row lists).
	if !strings.Contains(rec.Body.String(), `"main":{"entries":[{"provider":"agentteams-gateway","model":"qwen3.6-27b"}]}`) {
		t.Errorf("surfaces.main entries missing from body: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"spawn":{"entries":[],"inherit":true}`) {
		t.Errorf("surfaces.spawn empty-entries shape missing from body: %s", rec.Body.String())
	}
}

func TestModelAccessWorkerNotFoundAndCrossTeam(t *testing.T) {
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maTeam("beta", v1beta1.TeamWorkerRef{Name: "w2", Role: "team_leader"}),
		maWorker("w1", "m1", "", "", ""),
		maWorker("w2", "m2", "", "", ""),
		maHuman("alice", []string{"alpha"}, nil, nil),
	)
	admin := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}
	alice := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha"}}

	// Unknown worker -> 404 for everyone.
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=missing", admin))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing worker: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Human probing a worker outside accessibleTeams -> 404 (W8).
	rec = httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w2", alice))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team human: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Pinned team the worker does not belong to -> 404, even for admin.
	rec = httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1&team=beta", admin))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("pinned foreign team: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Pinned team the worker does belong to -> 200 with that team.
	rec = httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1&team=alpha", admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("pinned own team: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Team != "alpha" || got.Role != "worker" {
		t.Fatalf("team/role = %s/%s", got.Team, got.Role)
	}
}

func TestModelAccessNoOwnerLockedView(t *testing.T) {
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maWorker("w1", "qwen3.6-27b", "", "sub-m", "qwenpaw"),
		// No human carries team alpha: nobody owns the worker.
	)
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("no owner: expected 200 (locked view, never 500), got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Locked {
		t.Fatalf("locked = false, want true")
	}
	if got.Capabilities.Main.CanEdit || got.Capabilities.Main.Reason != "not_granted" {
		t.Errorf("main = %+v", got.Capabilities.Main)
	}
	if len(got.Capabilities.Main.WriteVia) != 1 || got.Capabilities.Main.WriteVia[0] != "crd" {
		t.Errorf("qwenpaw writeVia = %v, want [crd]", got.Capabilities.Main.WriteVia)
	}
	if len(got.Capabilities.CodingCli) != 3 {
		t.Errorf("codingCli keys = %v", got.Capabilities.CodingCli)
	}
	if got.Current.Model != "qwen3.6-27b" || got.Current.SubagentModel != "sub-m" {
		t.Errorf("current = %+v", got.Current)
	}
	if !got.CatalogUnavailable {
		t.Errorf("catalogUnavailable = false, want true")
	}
	// No granted CLI rows: the codingCli surface map is empty.
	if len(got.Surfaces.CodingCli) != 0 || len(got.Surfaces.Main.Entries) != 0 {
		t.Errorf("surfaces = %+v, want empty", got.Surfaces)
	}
}

func TestModelAccessWorkerScopedHumanStandaloneWorker(t *testing.T) {
	h := maRig(t,
		maWorker("s1", "m1", "", "", ""),
		// L3 human: no teams, only an explicit worker assignment. The
		// worker is standalone (no team), so nobody owns it.
		maHuman("bob", nil, []string{"s1"}, nil),
	)
	bob := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", AccessibleWorkers: []string{"s1"}}
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=s1", bob))
	if rec.Code != http.StatusOK {
		t.Fatalf("L3 human on assigned standalone worker: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Team != "" || got.Role != "worker" || !got.Locked {
		t.Fatalf("team/role/locked = %q/%q/%v", got.Team, got.Role, got.Locked)
	}
}
