package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func mcpTestWorker(name, team string, mcpNames ...string) *v1beta1.Worker {
	servers := make([]v1beta1.MCPServer, 0, len(mcpNames))
	for _, n := range mcpNames {
		servers = append(servers, v1beta1.MCPServer{
			Name:      n,
			URL:       "https://user:secret@apig.example.com/mcp-servers/" + n + "/mcp?key=consumer-key",
			Transport: "http",
		})
	}
	return &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.WorkerSpec{McpServers: servers},
	}
}

func mcpTestTeam(name string, members ...string) *v1beta1.Team {
	refs := make([]v1beta1.TeamWorkerRef, 0, len(members))
	for _, m := range members {
		refs = append(refs, v1beta1.TeamWorkerRef{Name: m})
	}
	return &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1beta1.TeamSpec{WorkerMembers: refs},
	}
}

func mcpPutRegistry(t *testing.T, store *ossfake.Memory, name, doc string) {
	t.Helper()
	if err := store.PutObject(context.Background(), "mcp-servers/"+name+".json", []byte(doc)); err != nil {
		t.Fatalf("put registry %s: %v", name, err)
	}
}

func mcpCallerCtx(role string, team string, teams []string) context.Context {
	return context.WithValue(context.Background(), authpkg.CallerKeyForTest(), &authpkg.CallerIdentity{
		Role:  role,
		Team:  team,
		Teams: teams,
	})
}

func mcpGet(t *testing.T, h *ResourceHandler, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp-servers", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ListMCPServers(rec, req)
	return rec
}

func mcpDecode(t *testing.T, rec *httptest.ResponseRecorder) MCPListResponse {
	t.Helper()
	var resp MCPListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func mcpFind(t *testing.T, resp MCPListResponse, name string) *MCPInfo {
	t.Helper()
	for i := range resp.Servers {
		if resp.Servers[i].Name == name {
			return &resp.Servers[i]
		}
	}
	t.Fatalf("server %q not in catalog: %+v", name, resp.Servers)
	return nil
}

func TestListMCPServers_AdminSeesMergedCatalog(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mcpTestTeam("t1", "w1"),
		mcpTestTeam("t2", "w2"),
		mcpTestWorker("w1", "t1", "github", "local"),
		mcpTestWorker("w2", "t2", "github"),
	).Build()
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "github", `{"name":"github","url":"https://apig.example.com/mcp-servers/github/mcp","transport":"http","timeout":60,"trusted":false}`)
	mcpPutRegistry(t, store, "external", `{"name":"external","url":"https://ext.example.com/mcp","transport":"sse","trusted":true}`)
	// Non-JSON and nested keys are ignored.
	if err := store.PutObject(context.Background(), "mcp-servers/README.md", []byte("nope")); err != nil {
		t.Fatal(err)
	}
	if err := store.PutObject(context.Background(), "mcp-servers/sub/x.json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	h := NewResourceHandler(k8s, "default", nil, "", nil).WithOSS(store)
	resp := mcpDecode(t, mcpGet(t, h, mcpCallerCtx(authpkg.RoleAdmin, "", nil)))

	if !resp.RegistryAvailable {
		t.Fatal("registry_available = false, want true")
	}
	if resp.Total != 3 {
		t.Fatalf("total = %d, want 3: %+v", resp.Total, resp.Servers)
	}

	g := mcpFind(t, resp, "github")
	if g.Source != "registry+worker-spec" || !g.Trusted {
		t.Fatalf("github merged entry wrong: %+v", g)
	}
	if g.URL != "apig.example.com/mcp-servers/github/mcp" {
		t.Fatalf("github url not redacted: %q", g.URL)
	}
	if g.Timeout != 60 {
		t.Fatalf("github timeout = %d, want 60", g.Timeout)
	}
	if len(g.Workers) != 2 {
		t.Fatalf("github workers = %+v, want 2", g.Workers)
	}

	l := mcpFind(t, resp, "local")
	if l.Source != "worker-spec" || !l.Trusted {
		t.Fatalf("local entry wrong: %+v", l)
	}
	if l.URL != "apig.example.com/mcp-servers/local/mcp" {
		t.Fatalf("local url not redacted (query leaked?): %q", l.URL)
	}

	e := mcpFind(t, resp, "external")
	if e.Source != "registry" || !e.Trusted {
		t.Fatalf("external entry wrong: %+v", e)
	}
	if len(e.Workers) != 0 {
		t.Fatalf("external should have no workers: %+v", e.Workers)
	}
}

func TestListMCPServers_L2HumanScopedToOwnTeams(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mcpTestTeam("t1", "w1"),
		mcpTestTeam("t2", "w2"),
		mcpTestWorker("w1", "t1", "github"),
		mcpTestWorker("w2", "t2", "other"),
	).Build()
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "orphan", `{"name":"orphan","url":"https://orphan.example.com/mcp"}`)

	h := NewResourceHandler(k8s, "default", nil, "", nil).WithOSS(store)
	// L2 human with accessibleTeams=[t1]: sees only github (referenced by
	// w1); "other" (t2) and "orphan" (no in-scope reference) are hidden.
	resp := mcpDecode(t, mcpGet(t, h, mcpCallerCtx(authpkg.RoleHuman, "", []string{"t1"})))

	if resp.Total != 1 || resp.Servers[0].Name != "github" {
		t.Fatalf("L2 t1 scope wrong: %+v", resp.Servers)
	}
	g := &resp.Servers[0]
	if len(g.Workers) != 1 || g.Workers[0].Name != "w1" || g.Workers[0].Team != "t1" {
		t.Fatalf("L2 workers not filtered: %+v", g.Workers)
	}
}

func TestListMCPServers_TeamLeaderScopedToOwnTeam(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mcpTestTeam("t2", "w2"),
		mcpTestWorker("w2", "t2", "other"),
	).Build()
	h := NewResourceHandler(k8s, "default", nil, "", nil).WithOSS(ossfake.NewMemory())
	resp := mcpDecode(t, mcpGet(t, h, mcpCallerCtx(authpkg.RoleTeamLeader, "t2", nil)))
	if resp.Total != 1 || resp.Servers[0].Name != "other" {
		t.Fatalf("leader scope wrong: %+v", resp.Servers)
	}
}

func TestListMCPServers_ManagerAndWorkerDenied(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	h := NewResourceHandler(k8s, "default", nil, "", nil).WithOSS(ossfake.NewMemory())
	for _, role := range []string{authpkg.RoleManager, authpkg.RoleWorker} {
		rec := mcpGet(t, h, mcpCallerCtx(role, "", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("role %s: status = %d, want 403", role, rec.Code)
		}
	}
}

func TestListMCPServers_NoOSS_SharedHalfUnavailable(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mcpTestWorker("w1", "", "solo"),
	).Build()
	h := NewResourceHandler(k8s, "default", nil, "", nil) // no WithOSS
	resp := mcpDecode(t, mcpGet(t, h, mcpCallerCtx(authpkg.RoleAdmin, "", nil)))
	if resp.RegistryAvailable {
		t.Fatal("registry_available = true without OSS")
	}
	if resp.Total != 1 || resp.Servers[0].Name != "solo" {
		t.Fatalf("spec-only catalog wrong: %+v", resp.Servers)
	}
}

// TestListMCPServers_ProductionListingContract pins the production
// listing-contract regression: MinIOClient.ListObjects wraps `mc ls` and
// returns names RELATIVE to the prefix (e.g. "github.json"). The catalog
// must re-attach the registry prefix before GetObject — otherwise the
// registry-only server vanishes into a silent bucket-root read failure
// (total=0, registry_available=true) while worker-spec entries still show.
func TestListMCPServers_ProductionListingContract(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build() // no workers: registry-only
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "github", `{"name":"github","url":"https://apig.example.com/mcp-servers/github/mcp","transport":"http","timeout":60,"trusted":true}`)

	h := NewResourceHandler(k8s, "default", nil, "", nil).WithOSS(store)
	resp := mcpDecode(t, mcpGet(t, h, mcpCallerCtx(authpkg.RoleAdmin, "", nil)))

	if !resp.RegistryAvailable {
		t.Fatal("registry_available = false, want true")
	}
	if resp.Total != 1 {
		t.Fatalf("total = %d, want 1 (registry-only entry lost): %+v", resp.Total, resp.Servers)
	}
	g := mcpFind(t, resp, "github")
	if g.Source != "registry" || !g.Trusted || g.Timeout != 60 {
		t.Fatalf("registry-only entry wrong: %+v", g)
	}
	if g.URL != "apig.example.com/mcp-servers/github/mcp" {
		t.Fatalf("registry-only url wrong: %q", g.URL)
	}
}

func TestListMCPServers_CorruptRegistryDocSkipped(t *testing.T) {
	scheme := newServerTestScheme(t)
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "broken", `{not json`)
	mcpPutRegistry(t, store, "good", `{"name":"good","url":"https://good.example.com/mcp"}`)

	h := NewResourceHandler(k8s, "default", nil, "", nil).WithOSS(store)
	resp := mcpDecode(t, mcpGet(t, h, mcpCallerCtx(authpkg.RoleAdmin, "", nil)))
	if resp.Total != 1 || resp.Servers[0].Name != "good" {
		t.Fatalf("corrupt doc must be skipped: %+v", resp.Servers)
	}
}

func TestRedactMCPURL(t *testing.T) {
	cases := map[string]string{
		"https://user:pass@api.example.com/mcp-servers/github/mcp?key=k": "api.example.com/mcp-servers/github/mcp",
		"https://api.example.com/mcp":                                    "api.example.com/mcp",
		"https://api.example.com":                                        "api.example.com/",
		"not a url":                                                      "",
		"token-only-no-host":                                             "",
	}
	for in, want := range cases {
		if got := redactMCPURL(in); got != want {
			t.Errorf("redactMCPURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthorizer_MCPServerKind(t *testing.T) {
	a := authpkg.NewAuthorizer()
	req := authpkg.AuthzRequest{Action: authpkg.ActionList, ResourceKind: "mcp-server"}

	if err := a.Authorize(&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Teams: []string{"t1"}}, req); err != nil {
		t.Errorf("human list mcp-server denied: %v", err)
	}
	if err := a.Authorize(&authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Team: "t1"}, req); err != nil {
		t.Errorf("team leader list mcp-server denied: %v", err)
	}
	if err := a.Authorize(&authpkg.CallerIdentity{Role: authpkg.RoleWorker}, req); err == nil {
		t.Error("worker list mcp-server allowed, want deny")
	}
	writeReq := authpkg.AuthzRequest{Action: authpkg.ActionCreate, ResourceKind: "mcp-server"}
	if err := a.Authorize(&authpkg.CallerIdentity{Role: authpkg.RoleHuman, Teams: []string{"t1"}}, writeReq); err == nil {
		t.Error("human create mcp-server allowed, want deny")
	}
}
