package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

	if !got.Capabilities.Main.CanEdit || got.Capabilities.Main.Inherit ||
		len(got.Capabilities.Main.WriteVia) != 1 || got.Capabilities.Main.WriteVia[0] != "direct" {
		t.Errorf("main capabilities = %+v", got.Capabilities.Main)
	}
	if len(got.Capabilities.Spawn.WriteVia) != 1 || got.Capabilities.Spawn.WriteVia[0] != "direct" {
		t.Errorf("spawn writeVia = %v, want [direct] (admin)", got.Capabilities.Spawn.WriteVia)
	}
	if !got.Capabilities.Spawn.CanEdit || !got.Capabilities.Spawn.Inherit {
		t.Errorf("spawn capabilities = %+v", got.Capabilities.Spawn)
	}
	cli, ok := got.Capabilities.CodingCli["qwen-code"]
	if !ok {
		t.Fatalf("codingCli keys = %v", got.Capabilities.CodingCli)
	}
	if !cli.CanEdit || !cli.Inherit || cli.InheritedFrom == nil ||
		cli.InheritedFrom.Provider != "agentteams-gateway" || cli.InheritedFrom.Model != "qwen3.6-27b" ||
		len(cli.WriteVia) != 1 || cli.WriteVia[0] != "direct" {
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

	// Merged set (PR-B1 §2.2): the explicit row first; the current value is
	// pinned to the default gateway provider, so it dedupes against the row.
	if len(got.Surfaces.Main.Entries) != 1 ||
		got.Surfaces.Main.Entries[0] != (v1beta1.ModelAccessEntry{Provider: "agentteams-gateway", Model: "qwen3.6-27b"}) {
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
	if len(got.Capabilities.Main.WriteVia) != 1 || got.Capabilities.Main.WriteVia[0] != "direct" {
		t.Errorf("admin writeVia = %v, want [direct]", got.Capabilities.Main.WriteVia)
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
	// No explicit/implicit rows: the merged sets carry only the current
	// values (the current value is always available, PR-B1 §2.2).
	if len(got.Surfaces.Main.Entries) != 1 ||
		got.Surfaces.Main.Entries[0] != (v1beta1.ModelAccessEntry{Provider: "agentteams-gateway", Model: "qwen3.6-27b"}) {
		t.Errorf("surfaces.main = %+v, want the current value only", got.Surfaces.Main)
	}
	if len(got.Surfaces.Spawn.Entries) != 1 ||
		got.Surfaces.Spawn.Entries[0] != (v1beta1.ModelAccessEntry{Provider: "agentteams-gateway", Model: "sub-m"}) ||
		got.Surfaces.Spawn.Inherit {
		t.Errorf("surfaces.spawn = %+v, want the current subagent value only", got.Surfaces.Spawn)
	}
	// No granted CLI rows: the codingCli surface map is empty.
	if len(got.Surfaces.CodingCli) != 0 {
		t.Errorf("surfaces.codingCli = %+v, want empty", got.Surfaces.CodingCli)
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
	if got.Capabilities.Main.Reason != "no_write_access" {
		t.Errorf("L3 main reason = %q, want no_write_access", got.Capabilities.Main.Reason)
	}
	if len(got.Capabilities.Main.WriteVia) != 1 || got.Capabilities.Main.WriteVia[0] != "admin" {
		t.Errorf("L3 main writeVia = %v, want [admin]", got.Capabilities.Main.WriteVia)
	}
}

// TestModelAccess_TwoHumansOwnViews is the caller-view guard (PR-A1b §4.1,
// #1514 port): two humans in the same team with different grants on the same
// worker each see their OWN grant view — the worker owner's rows must not
// leak into a teammate's preview.
//
// Mutation target: reverting the human resolution to findWorkerOwner (owner
// = first human by name) makes dave preview alice's rows, so his view
// collapses onto the owner view and this test goes red.
func TestModelAccess_TwoHumansOwnViews(t *testing.T) {
	aliceGrant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role: "worker",
		Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "alice-m"}}},
	}}}
	daveGrant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role: "worker",
		Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "dave-m"}}},
	}}}
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maWorker("w1", "qwen3.6-27b", "", "", "openclaw"),
		// alice is first by name — under the old findWorkerOwner lookup she
		// is the "owner" for every caller on this team.
		maHuman("alice", []string{"alpha"}, nil, aliceGrant),
		maHuman("dave", []string{"alpha"}, nil, daveGrant),
	)
	alice := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha"}}
	dave := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "dave", Teams: []string{"alpha"}}

	var aliceBody, daveBody modelAccessWorkerResponse

	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", dave))
	if rec.Code != http.StatusOK {
		t.Fatalf("dave: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &daveBody); err != nil {
		t.Fatalf("dave: unmarshal: %v", err)
	}
	if len(daveBody.Surfaces.Main.Entries) != 1 || daveBody.Surfaces.Main.Entries[0].Model != "dave-m" {
		t.Errorf("dave surfaces.main = %+v, want own row dave-m", daveBody.Surfaces.Main)
	}
	if !daveBody.Capabilities.Main.CanEdit || daveBody.Capabilities.Main.Reason != "" {
		t.Errorf("dave main capabilities = %+v, want canEdit=true (own team, own rows)", daveBody.Capabilities.Main)
	}
	if daveBody.Locked {
		t.Errorf("dave locked = true, want false (own rows open the main surface)")
	}
	// The model fields stay L1-only in this release: the write routes
	// through an L1, but the coding-CLI surface is dave's own API write.
	if len(daveBody.Capabilities.Main.WriteVia) != 1 || daveBody.Capabilities.Main.WriteVia[0] != "admin" {
		t.Errorf("dave main writeVia = %v, want [admin]", daveBody.Capabilities.Main.WriteVia)
	}
	if got := daveBody.Capabilities.CodingCli["qwen-code"]; len(got.WriteVia) != 1 || got.WriteVia[0] != "direct" {
		t.Errorf("dave codingCli writeVia = %v, want [direct]", got.WriteVia)
	}
	if strings.Contains(rec.Body.String(), "alice-m") {
		t.Errorf("dave body leaks the owner's row: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", alice))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &aliceBody); err != nil {
		t.Fatalf("alice: unmarshal: %v", err)
	}
	if len(aliceBody.Surfaces.Main.Entries) != 1 || aliceBody.Surfaces.Main.Entries[0].Model != "alice-m" {
		t.Errorf("alice surfaces.main = %+v, want own row alice-m", aliceBody.Surfaces.Main)
	}
	if strings.Contains(rec.Body.String(), "dave-m") {
		t.Errorf("alice body leaks dave's row: %s", rec.Body.String())
	}

	if reflect.DeepEqual(aliceBody, daveBody) {
		t.Fatalf("alice and dave got the same preview — the caller view collapsed to the owner view")
	}
}

// TestModelAccess_HumanWithoutGrantNoLeak: a human in the worker's team with
// no grant (CR without modelAccess, or no CR at all) sees a locked, empty
// view — and never the owner's rows.
func TestModelAccess_HumanWithoutGrantNoLeak(t *testing.T) {
	ownerGrant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role: "worker",
		Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "alice-m"}}},
	}}}
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maWorker("w1", "qwen3.6-27b", "", "", "openclaw"),
		maHuman("alice", []string{"alpha"}, nil, ownerGrant),
		// bob is in the team but carries no grant.
		maHuman("bob", []string{"alpha"}, nil, nil),
	)

	for _, caller := range []*authpkg.CallerIdentity{
		{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"alpha"}},
		// ghost has no Human CR at all: deleted mid-session, fail closed.
		{Role: authpkg.RoleHuman, Username: "ghost", Teams: []string{"alpha"}},
	} {
		rec := httptest.NewRecorder()
		h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", caller))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200 (locked view, never 404/500), got %d: %s", caller.Username, rec.Code, rec.Body.String())
		}
		var got modelAccessWorkerResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", caller.Username, err)
		}
		if !got.Locked {
			t.Errorf("%s: locked = false, want true", caller.Username)
		}
		if got.Capabilities.Main.CanEdit || got.Capabilities.Main.Reason != "not_granted" {
			t.Errorf("%s: main = %+v, want canEdit=false not_granted", caller.Username, got.Capabilities.Main)
		}
		if got.Capabilities.CodingCli["qwen-code"].CanEdit || got.Capabilities.CodingCli["qoder"].CanEdit {
			t.Errorf("%s: CLI surfaces must stay closed, got %+v", caller.Username, got.Capabilities.CodingCli)
		}
		if len(got.Surfaces.Main.Entries) != 0 {
			t.Errorf("%s: surfaces.main = %+v, want no rows", caller.Username, got.Surfaces.Main)
		}
		if strings.Contains(rec.Body.String(), "alice-m") {
			t.Errorf("%s: body leaks the owner's rows: %s", caller.Username, rec.Body.String())
		}
	}
}

// TestModelAccess_L3HumanReadOnly: an L3 (worker-scoped) human sees their
// OWN grant rows (rendering them leaks nothing) but the write entry's
// team-scope decision denies every write: canEdit=false + no_write_access,
// whatever the grant says.
func TestModelAccess_L3HumanReadOnly(t *testing.T) {
	bobGrant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role: "worker",
		Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "bob-m"}}},
	}}}
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maWorker("w1", "qwen3.6-27b", "", "", "openclaw"),
		// L3: no teams, an explicit assignment to w1, and a grant.
		maHuman("bob", nil, []string{"w1"}, bobGrant),
	)
	bob := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", AccessibleWorkers: []string{"w1"}}
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", bob))
	if rec.Code != http.StatusOK {
		t.Fatalf("L3 human on assigned team worker: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Locked {
		t.Fatalf("locked = false, want true (L3 is read-only)")
	}
	if got.Capabilities.Main.CanEdit || got.Capabilities.Main.Reason != "no_write_access" {
		t.Errorf("main = %+v, want canEdit=false no_write_access", got.Capabilities.Main)
	}
	if got.Capabilities.Spawn.CanEdit || got.Capabilities.Spawn.Reason != "no_write_access" {
		t.Errorf("spawn = %+v, want canEdit=false no_write_access", got.Capabilities.Spawn)
	}
	if got.Capabilities.CodingCli["qwen-code"].CanEdit || got.Capabilities.CodingCli["qoder"].CanEdit {
		t.Errorf("CLI = %+v, want all closed", got.Capabilities.CodingCli)
	}
	// The human's OWN rows still render (their table, not a leak) — the
	// denial is the write access, not the view.
	if len(got.Surfaces.Main.Entries) != 1 || got.Surfaces.Main.Entries[0].Model != "bob-m" {
		t.Errorf("surfaces.main = %+v, want the caller's own row", got.Surfaces.Main)
	}
}

// TestModelAccess_HumanWriteEntryConsistency: the preview and the real write
// entry agree. An L2 human of the worker's team previews coding-cli
// canEdit=true (writeVia direct), then writes the CLI settings (200) and
// reads the merged value back. A cross-team human is hidden as 404 (W8) and
// no write lands. (The model fields stay L1-only in this release, so the
// coding-CLI settings surface is the one a human actually writes.)
func TestModelAccess_HumanWriteEntryConsistency(t *testing.T) {
	grant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role: "worker",
		CodingCli: []v1beta1.ModelAccessCli{
			{CLI: "qwen-code", Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "m-dave"}}},
		},
	}}}
	// The exec fake reports the effective home and the CURRENT contents of
	// the settings file (seeded once, re-read after the write), so the
	// read-back sees exactly what the PUT stored.
	exec := &fakeExecBackend{fileWrites: map[string]string{
		"/root/.qwen/settings.json": `{"keepme":1}`,
	}}
	exec.respond = func(cmd []string) (string, string, int, error) {
		if cmd[0] == "sh" {
			switch {
			case strings.Contains(cmd[2], "printf %s"):
				return "/root", "", 0, nil
			case strings.Contains(cmd[2], "cat "):
				c, ok := exec.written("/root/.qwen/settings.json")
				if !ok {
					return "", "cat: no such file", 1, nil
				}
				return c, "", 0, nil
			}
		}
		return "", "", 1, nil
	}
	k8s := fake.NewClientBuilder().WithScheme(newServerTestScheme(t)).WithObjects(
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maWorker("w1", "qwen3.6-27b", "", "", "openclaw"),
		maHuman("dave", []string{"alpha"}, nil, grant),
	).Build()
	rh := NewResourceHandler(k8s, "default", nil, "", nil)
	ch := NewCodingCliHandler(k8s, "default", "embedded", nil, nil)
	ch.execBackend = func(context.Context, string) (codingCliExecer, error) { return exec, nil }

	dave := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "dave", Teams: []string{"alpha"}}

	// 1) Preview: dave's own grant opens the qwen-code surface via his own
	//    API write; the main surface stays closed (he has no main rows).
	rec := httptest.NewRecorder()
	rh.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", dave))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("preview unmarshal: %v", err)
	}
	cli := got.Capabilities.CodingCli["qwen-code"]
	if !cli.CanEdit || cli.Reason != "" || len(cli.WriteVia) != 1 || cli.WriteVia[0] != "direct" {
		t.Fatalf("preview qwen-code = %+v, want canEdit=true writeVia=[direct]", cli)
	}
	if got.Capabilities.Main.CanEdit || got.Capabilities.Main.Reason != "not_granted" {
		t.Errorf("preview main = %+v, want not_granted (no main rows)", got.Capabilities.Main)
	}
	if len(got.Capabilities.Main.WriteVia) != 1 || got.Capabilities.Main.WriteVia[0] != "admin" {
		t.Errorf("preview main writeVia = %v, want [admin] (model fields are L1-only)", got.Capabilities.Main.WriteVia)
	}

	// 2) Write entry: the PUT the preview promised is accepted (merge).
	putReq := withCaller(codingCliRequest(http.MethodPut, "/api/v1/workers/w1/coding-cli/qwen-code/settings", `{"model":"m-dave"}`, "name", "w1", "cli", "qwen-code"), dave)
	rec = httptest.NewRecorder()
	ch.putCliSettings(rec, putReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT settings: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3) Read back: the merged value is what the preview promised — the
	//    pre-existing key survived the read-modify-merge.
	getReq := withCaller(codingCliRequest(http.MethodGet, "/api/v1/workers/w1/coding-cli/qwen-code/settings", "", "name", "w1", "cli", "qwen-code"), dave)
	rec = httptest.NewRecorder()
	ch.getCliSettings(rec, getReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var readBack struct {
		Settings map[string]interface{} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &readBack); err != nil {
		t.Fatalf("read-back unmarshal: %v", err)
	}
	if readBack.Settings["model"] != "m-dave" || readBack.Settings["keepme"] != float64(1) {
		t.Errorf("read-back settings = %v, want model=m-dave + keepme=1", readBack.Settings)
	}

	// 4) Cross-team human: hidden as 404 (W8), state unchanged.
	erin := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "erin", Teams: []string{"beta"}}
	erinReq := withCaller(codingCliRequest(http.MethodPut, "/api/v1/workers/w1/coding-cli/qwen-code/settings", `{"model":"m-erin"}`, "name", "w1", "cli", "qwen-code"), erin)
	rec = httptest.NewRecorder()
	ch.putCliSettings(rec, erinReq)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team PUT: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := len(exec.fileWrites); n != 1 {
		t.Errorf("file writes after cross-team attempt = %d, want 1 (state unchanged)", n)
	}
}

// TestModelAccess_ManagerOwnerView: the manager keeps the resource (owner)
// view — the owner's rows, unchanged semantics, and L1 "direct" writes.
func TestModelAccess_ManagerOwnerView(t *testing.T) {
	ownerGrant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role: "worker",
		Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "owner-m"}}},
	}}}
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maWorker("w1", "qwen3.6-27b", "", "", "openclaw"),
		maHuman("alice", []string{"alpha"}, nil, ownerGrant),
	)
	manager := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", manager))
	if rec.Code != http.StatusOK {
		t.Fatalf("manager: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Surfaces.Main.Entries) != 1 || got.Surfaces.Main.Entries[0].Model != "owner-m" {
		t.Errorf("manager surfaces.main = %+v, want the owner's row", got.Surfaces.Main)
	}
	if !got.Capabilities.Main.CanEdit || got.Locked {
		t.Errorf("manager main/locked = %+v/%v", got.Capabilities.Main, got.Locked)
	}
	if len(got.Capabilities.Main.WriteVia) != 1 || got.Capabilities.Main.WriteVia[0] != "direct" {
		t.Errorf("manager main writeVia = %v, want [direct]", got.Capabilities.Main.WriteVia)
	}
	if got := got.Capabilities.CodingCli["qwen-code"]; len(got.WriteVia) != 1 || got.WriteVia[0] != "direct" {
		t.Errorf("manager codingCli writeVia = %v, want [direct]", got.WriteVia)
	}
}

// TestModelAccess_LeaderViews: team leaders are not grant-bound. Own team:
// canEdit mirrors the leader worker-write rule (true, routed via admin in
// this release) and the leader NEVER sees the owner's grant rows. Cross
// team: out of read scope — 404 before anything is serialized (the earlier
// "200 + locked view" leaked the target team/runtime/current model).
func TestModelAccess_LeaderViews(t *testing.T) {
	ownerGrant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role: "worker",
		Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "owner-m"}}},
	}}}
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maWorker("w1", "qwen3.6-27b", "", "", "openclaw"),
		maHuman("alice", []string{"alpha"}, nil, ownerGrant),
	)

	// Own team: editable (write rule), no grant rows, BYOK qoder closed.
	lead := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha"}
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", lead))
	if rec.Code != http.StatusOK {
		t.Fatalf("own-team leader: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var own modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &own); err != nil {
		t.Fatalf("own-team unmarshal: %v", err)
	}
	if own.Locked {
		t.Errorf("own-team locked = true, want false (own-team write rule opens the surfaces)")
	}
	if !own.Capabilities.Main.CanEdit || own.Capabilities.Main.Reason != "" {
		t.Errorf("own-team main = %+v, want canEdit=true", own.Capabilities.Main)
	}
	if !own.Capabilities.Spawn.CanEdit || own.Capabilities.Spawn.Reason != "" {
		t.Errorf("own-team spawn = %+v, want canEdit=true", own.Capabilities.Spawn)
	}
	if qwen := own.Capabilities.CodingCli["qwen-code"]; !qwen.CanEdit || qwen.Reason != "" {
		t.Errorf("own-team qwen-code = %+v, want canEdit=true", qwen)
	}
	if qoder := own.Capabilities.CodingCli["qoder"]; qoder.CanEdit || qoder.Reason != "byok_unsupported" {
		t.Errorf("own-team qoder = %+v, want byok_unsupported", qoder)
	}
	// The write routes through an L1 in this release (leader model writes
	// are 400 and CLI writes 403 on the real write entries).
	if len(own.Capabilities.Main.WriteVia) != 1 || own.Capabilities.Main.WriteVia[0] != "admin" {
		t.Errorf("own-team main writeVia = %v, want [admin]", own.Capabilities.Main.WriteVia)
	}
	// No grant table: empty rows, and the owner's rows never render.
	if len(own.Surfaces.Main.Entries) != 0 || strings.Contains(rec.Body.String(), "owner-m") {
		t.Errorf("own-team leader leaked grant rows: %s", rec.Body.String())
	}

	// Cross team: out of read scope — 404, and nothing about the target
	// leaks (team, runtime, current model/provider/subagent).
	otherLead := &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "beta-lead", Team: "beta"}
	rec = httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", otherLead))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-team leader: expected 404 (read scope), got %d: %s", rec.Code, rec.Body.String())
	}
	for _, leak := range []string{"alpha", "qwen3.6-27b", "owner-m", "openclaw"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("cross-team leader leaked %q: %s", leak, rec.Body.String())
		}
	}
}

// TestModelAccess_WorkerSelfScope: a worker reads only its own preview; any
// other target is 404 (same anti-probing rule as the human/leader legs).
func TestModelAccess_WorkerSelfScope(t *testing.T) {
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maTeam("beta", v1beta1.TeamWorkerRef{Name: "w2", Role: "worker"}),
		maWorker("w1", "qwen3.6-27b", "", "", "openclaw"),
		maWorker("w2", "other-model", "", "", "openclaw"),
	)
	self := &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "w1", WorkerName: "w1"}
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", self))
	if rec.Code != http.StatusOK {
		t.Fatalf("worker self: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("self unmarshal: %v", err)
	}
	if got.Team != "alpha" || got.Current.Model != "qwen3.6-27b" {
		t.Errorf("self view = team %q current %+v", got.Team, got.Current)
	}

	// Another worker in another team: 404, nothing leaks.
	rec = httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w2", self))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unrelated worker: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "other-model") {
		t.Errorf("unrelated worker target leaked: %s", rec.Body.String())
	}
}

// maStaticAuth authenticates every request as one fixed identity so the real
// route + authorizer chain can be exercised without tokens.
type maStaticAuth struct{ id *authpkg.CallerIdentity }

func (s *maStaticAuth) Authenticate(context.Context, string) (*authpkg.CallerIdentity, error) {
	return s.id, nil
}

// TestModelAccessMiddleware_ScopedReads drives GET /api/v1/model-access
// through NewHTTPServer's mux (real RequireAuthz + authorizer + handler):
// a cross-team leader and an unrelated worker must both be hidden as 404,
// while the own-team leader and the worker reading itself get their preview.
func TestModelAccessMiddleware_ScopedReads(t *testing.T) {
	cases := []struct {
		name   string
		caller *authpkg.CallerIdentity
		worker string
		want   int
	}{
		{"own-team leader", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "alpha-lead", Team: "alpha", WorkerName: "alpha-lead"}, "w1", http.StatusOK},
		{"cross-team leader", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "beta-lead", Team: "beta", WorkerName: "beta-lead"}, "w1", http.StatusNotFound},
		{"worker self", &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "w1", WorkerName: "w1"}, "w1", http.StatusOK},
		{"unrelated worker", &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "w2", WorkerName: "w2"}, "w1", http.StatusNotFound},
		{"human owner", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha"}}, "w1", http.StatusOK},
		{"human other team", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "bob", Teams: []string{"beta"}}, "w1", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := maRig(t,
				maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
				maTeam("beta", v1beta1.TeamWorkerRef{Name: "w2", Role: "worker"}),
				maWorker("w1", "qwen3.6-27b", "", "", "openclaw"),
				maWorker("w2", "other-model", "", "", "openclaw"),
				maHuman("alice", []string{"alpha"}, []string{"w1"}, nil),
				maHuman("bob", []string{"beta"}, nil, nil),
			)
			mw := authpkg.NewMiddleware(&maStaticAuth{id: tc.caller}, nil, authpkg.NewAuthorizer(), h.client, h.namespace)
			srv := NewHTTPServer(":0", ServerDeps{Client: h.client, Namespace: h.namespace, AuthMw: mw})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/model-access?worker="+tc.worker, nil)
			req.Header.Set("Authorization", "Bearer test")
			rec := httptest.NewRecorder()
			srv.Mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s: status=%d want %d body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusNotFound {
				for _, leak := range []string{"qwen3.6-27b", "other-model", `"team":"alpha"`} {
					if strings.Contains(rec.Body.String(), leak) {
						t.Errorf("%s: leaked %q in %s", tc.name, leak, rec.Body.String())
					}
				}
			}
		})
	}
}

// PR-B1 §2.4: an L2 human caller sees their OWN grant set — capabilities
// come from the caller's scope (not the owner's), and the surfaces carry
// the caller's merged set: explicit rows first, implicit grants next, the
// current value last.
func TestModelAccess_HumanSeesOwnGrants(t *testing.T) {
	w := maWorker("w1", "qwen3.6-27b", "", "", "openclaw")
	w.Annotations = map[string]string{modelGrantAnnotation: `[{"provider":"gw","model":"implicit-m"}]`}
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		w,
		// The team owner's table must NOT leak into dave's view.
		maHuman("alice", []string{"alpha"}, nil, &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
			Role: "worker",
			Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "owner-m"}}},
		}}}),
		// dave's own rows are what he must see.
		maHuman("dave", []string{"alpha"}, nil, &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
			Role: "worker",
			Main: &v1beta1.ModelAccessSurface{Entries: []v1beta1.ModelAccessEntry{{Provider: "gw", Model: "qwen3.6-27b"}}},
		}}}),
	)
	dave := &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "dave", Teams: []string{"alpha"}}
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", dave))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Locked {
		t.Fatalf("locked = true, want false (dave carries a grant)")
	}
	if !got.Capabilities.Main.CanEdit {
		t.Errorf("main capabilities = %+v, want canEdit from dave's own scope", got.Capabilities.Main)
	}
	want := []v1beta1.ModelAccessEntry{
		{Provider: "gw", Model: "qwen3.6-27b"},                 // dave's explicit row (first)
		{Provider: "gw", Model: "implicit-m"},                  // the implicit grant (next)
		{Provider: "agentteams-gateway", Model: "qwen3.6-27b"}, // the current value (last)
	}
	if len(got.Surfaces.Main.Entries) != len(want) {
		t.Fatalf("surfaces.main = %+v, want %v", got.Surfaces.Main.Entries, want)
	}
	for i := range want {
		if got.Surfaces.Main.Entries[i] != want[i] {
			t.Fatalf("surfaces.main[%d] = %+v, want %+v", i, got.Surfaces.Main.Entries[i], want[i])
		}
	}
	if strings.Contains(rec.Body.String(), "owner-m") {
		t.Errorf("owner grant row leaked into the human's view: %s", rec.Body.String())
	}
	if got.Current.Model != "qwen3.6-27b" || got.Current.ModelProvider != "" {
		t.Errorf("current = %+v", got.Current)
	}
}

// TestModelAccess_PreviewPath_ImplicitCapBoundary (preview path, 32/33
// boundary): the preview runs the same shared boundary check as the
// write paths — both the L1 owner-preview branch (direct
// buildModelGrantSet) and the human self-preview branch (resolveCallerGrant)
// answer 409 with model_grant_overflow and the count for an over-cap
// implicit annotation, and 200 at the cap.
func TestModelAccess_PreviewPath_ImplicitCapBoundary(t *testing.T) {
	cases := []struct {
		name   string
		n      int
		code   int
		caller *authpkg.CallerIdentity
	}{
		{"l1 at cap", modelGrantMax, http.StatusOK, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}},
		{"l1 over cap", modelGrantMax + 1, http.StatusConflict, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}},
		{"human at cap", modelGrantMax, http.StatusOK, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha"}}},
		{"human over cap", modelGrantMax + 1, http.StatusConflict, &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := maWorker("w1", "qwen3.6-27b", "", "", "openclaw")
			w.Annotations = map[string]string{modelGrantAnnotation: implicitAnnotationJSON(tc.n)}
			h := maRig(t,
				maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
				w,
				maHuman("alice", []string{"alpha"}, nil, nil),
			)
			rec := httptest.NewRecorder()
			h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", tc.caller))
			if rec.Code != tc.code {
				t.Fatalf("expected %d, got %d: %s", tc.code, rec.Code, rec.Body.String())
			}
			if tc.code == http.StatusConflict {
				body := rec.Body.String()
				if !strings.Contains(body, "model_grant_overflow") {
					t.Errorf("error must carry the model_grant_overflow code: %s", body)
				}
				if !strings.Contains(body, fmt.Sprintf("%d implicit model grants", tc.n)) ||
					!strings.Contains(body, fmt.Sprintf("cap is %d", modelGrantMax)) {
					t.Errorf("error must carry the actual count and the cap: %s", body)
				}
			}
		})
	}
}

// TestModelAccess_PreviewPath_200ExplicitRows (preview path, 200
// boundary): a grant at the 200-row validation ceiling renders fully in
// the preview — every explicit row stays in surfaces.main (the old
// union-wide cap 32 truncated the view to 32 rows).
func TestModelAccess_PreviewPath_200ExplicitRows(t *testing.T) {
	entries := make([]v1beta1.ModelAccessEntry, 0, 200)
	for i := 0; i < 200; i++ {
		entries = append(entries, v1beta1.ModelAccessEntry{Provider: "p", Model: fmt.Sprintf("m%d", i)})
	}
	grant := &v1beta1.ModelAccessSpec{Scopes: []v1beta1.ModelAccessScope{{
		Role: "worker",
		Main: &v1beta1.ModelAccessSurface{Entries: entries},
	}}}
	// The current value (p/m0) dedupes against the first explicit row, so
	// the view carries exactly the 200 validated rows.
	h := maRig(t,
		maTeam("alpha", v1beta1.TeamWorkerRef{Name: "w1", Role: "worker"}),
		maWorker("w1", "m0", "p", "", "openclaw"),
		maHuman("alice", []string{"alpha"}, nil, grant),
	)
	rec := httptest.NewRecorder()
	h.GetModelAccess(rec, maReq(t, "/api/v1/model-access?worker=w1", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got modelAccessWorkerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Surfaces.Main.Entries) != 200 {
		t.Fatalf("surfaces.main = %d entries, want all 200", len(got.Surfaces.Main.Entries))
	}
	if got.Surfaces.Main.Entries[0] != (v1beta1.ModelAccessEntry{Provider: "p", Model: "m0"}) ||
		got.Surfaces.Main.Entries[199] != (v1beta1.ModelAccessEntry{Provider: "p", Model: "m199"}) {
		t.Errorf("explicit rows truncated or reordered: first=%+v last=%+v",
			got.Surfaces.Main.Entries[0], got.Surfaces.Main.Entries[199])
	}
}
