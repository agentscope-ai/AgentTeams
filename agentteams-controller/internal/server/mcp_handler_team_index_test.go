package server

import (
	"context"
	"net/http"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestMCPCatalogTeamIndexListCount pins the issue #1475 fix in the MCP
// catalog: 3 workers + 1 team cost exactly 2 K8s List calls during the whole
// request (one WorkerList + one TeamList) instead of the old 1 WorkerList +
// 3 TeamList, and every catalog entry's worker/team refs match the old
// per-worker findTeamMember path run on the same fake, field for field.
func TestMCPCatalogTeamIndexListCount(t *testing.T) {
	scheme := newServerTestScheme(t)

	fixtures := []*v1beta1.Worker{
		mcpTestWorker("w1", "t1", "github", "local"),
		mcpTestWorker("w2", "t1", "github"),
		mcpTestWorker("w3", "t1", "solo3"),
	}
	rawClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mcpTestTeam("t1", "w1", "w2", "w3"), fixtures[0], fixtures[1], fixtures[2]).
		Build()
	counting := &countingClient{Client: rawClient}
	handler := NewResourceHandler(counting, "default", nil, "", nil)

	rec := mcpGet(t, handler, mcpCallerCtx(authpkg.RoleAdmin, "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := counting.listCalls; got != 2 {
		t.Fatalf("List calls = %d, want 2 (one WorkerList + one TeamList; the old code paid 1 + 3, one per worker)", got)
	}
	resp := mcpDecode(t, rec)

	// Equivalence: the old per-worker findTeamMember path on the same fake
	// must produce the same worker->team refs for every catalog entry.
	wantRefs := map[string]map[string]string{} // server -> worker -> team
	for _, wkr := range fixtures {
		team := ""
		tm, _, ok, err := findTeamMember(context.Background(), rawClient, "default", wkr.Name)
		if err != nil {
			t.Fatalf("findTeamMember(%s): %v", wkr.Name, err)
		}
		if ok {
			team = tm.Name
		}
		for _, m := range wkr.Spec.McpServers {
			if m.Name == "" {
				continue
			}
			if wantRefs[m.Name] == nil {
				wantRefs[m.Name] = map[string]string{}
			}
			wantRefs[m.Name][wkr.Name] = team
		}
	}
	if resp.Total != len(wantRefs) {
		t.Fatalf("total = %d, want %d: %+v", resp.Total, len(wantRefs), resp.Servers)
	}
	for _, info := range resp.Servers {
		want, known := wantRefs[info.Name]
		if !known {
			t.Fatalf("unexpected server %q in catalog: %+v", info.Name, resp.Servers)
		}
		if len(info.Workers) != len(want) {
			t.Fatalf("server %q: %d worker refs, want %d: %+v", info.Name, len(info.Workers), len(want), info.Workers)
		}
		for _, ref := range info.Workers {
			if want[ref.Name] != ref.Team {
				t.Errorf("server %q: worker %s team = %q, want %q", info.Name, ref.Name, ref.Team, want[ref.Name])
			}
		}
		// Golden: all three workers sit in t1, so every ref carries it.
		for _, ref := range info.Workers {
			if ref.Team != "t1" {
				t.Errorf("server %q worker %s: team = %q, want %q", info.Name, ref.Name, ref.Team, "t1")
			}
		}
	}

	g := mcpFind(t, resp, "github")
	gotNames := map[string]bool{}
	for _, ref := range g.Workers {
		gotNames[ref.Name] = true
	}
	if !gotNames["w1"] || !gotNames["w2"] || len(gotNames) != 2 {
		t.Fatalf("github worker refs = %+v, want exactly {w1, w2}", g.Workers)
	}
}

// TestMCPCatalogTeamIndexFirstHitWins pins the preserved first-hit
// semantics in the MCP catalog: when a worker is referenced by more than one
// team, the first team IN LIST ORDER wins (findTeamMember's behavior). The
// fixture lists zeta-team before alpha-team on purpose — alphabetically
// alpha-team would come first, so resolving shared-dev to zeta-team proves
// list order, not name order, decides.
func TestMCPCatalogTeamIndexFirstHitWins(t *testing.T) {
	shared := v1beta1.Worker{}
	shared.Name = "shared-dev"
	shared.Namespace = "default"
	shared.Spec.McpServers = []v1beta1.MCPServer{
		{Name: "svc", URL: "https://apig.example.com/mcp-servers/svc/mcp", Transport: "http"},
	}

	solo := v1beta1.Worker{}
	solo.Name = "solo-dev"
	solo.Namespace = "default"
	solo.Spec.McpServers = []v1beta1.MCPServer{
		{Name: "solo-svc", URL: "https://apig.example.com/mcp-servers/solo-svc/mcp", Transport: "http"},
	}

	// Slice order is the list order: zeta-team first, alpha-team second.
	zeta := v1beta1.Team{}
	zeta.Name = "zeta-team"
	zeta.Namespace = "default"
	zeta.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "shared-dev", Role: "worker"},
	}
	alpha := v1beta1.Team{}
	alpha.Name = "alpha-team"
	alpha.Namespace = "default"
	alpha.Spec.WorkerMembers = []v1beta1.TeamWorkerRef{
		{Name: "shared-dev", Role: "team_leader"},
		{Name: "solo-dev"},
	}

	newClient := func() *listFixtureClient {
		return &listFixtureClient{
			workers: []v1beta1.Worker{shared, solo},
			teams:   []v1beta1.Team{zeta, alpha},
		}
	}

	counting := newClient()
	handler := NewResourceHandler(counting, "default", nil, "", nil)

	rec := mcpGet(t, handler, mcpCallerCtx(authpkg.RoleAdmin, "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := counting.listCalls; got != 2 {
		t.Fatalf("List calls = %d, want 2 (one WorkerList + one TeamList)", got)
	}
	resp := mcpDecode(t, rec)

	gotSvc := mcpFind(t, resp, "svc")
	if len(gotSvc.Workers) != 1 || gotSvc.Workers[0].Name != "shared-dev" || gotSvc.Workers[0].Team != "zeta-team" {
		t.Fatalf("svc refs = %+v, want shared-dev -> zeta-team — the first team in list order must win, not the alphabetically first", gotSvc.Workers)
	}
	gotSolo := mcpFind(t, resp, "solo-svc")
	if len(gotSolo.Workers) != 1 || gotSolo.Workers[0].Name != "solo-dev" || gotSolo.Workers[0].Team != "alpha-team" {
		t.Fatalf("solo-svc refs = %+v, want solo-dev -> alpha-team", gotSolo.Workers)
	}

	// Equivalence against the old per-worker findTeamMember path on a fresh
	// deterministic fixture.
	gotTeam := map[string]string{}
	for _, info := range resp.Servers {
		for _, ref := range info.Workers {
			gotTeam[ref.Name] = ref.Team
		}
	}
	old := newClient()
	for _, name := range []string{"shared-dev", "solo-dev"} {
		team, _, ok, err := findTeamMember(context.Background(), old, "default", name)
		if err != nil {
			t.Fatalf("findTeamMember(%s): %v", name, err)
		}
		if !ok {
			t.Fatalf("findTeamMember(%s): not found, but the catalog refs it", name)
		}
		if gotTeam[name] != team.Name {
			t.Errorf("worker %s: catalog team = %q, findTeamMember path says %q", name, gotTeam[name], team.Name)
		}
	}
}

// TestMCPCatalogTeamIndexEmptyWorkersSkipsTeamList pins the call pattern for
// an empty worker list: with no workers there is nothing to index, so only
// the WorkerList round-trip happens — the old per-worker lookups were never
// reached either. The registry (shared) half is unaffected.
func TestMCPCatalogTeamIndexEmptyWorkersSkipsTeamList(t *testing.T) {
	scheme := newServerTestScheme(t)
	rawClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mcpTestTeam("t1")).Build()
	counting := &countingClient{Client: rawClient}
	store := ossfake.NewMemory()
	mcpPutRegistry(t, store, "ext", `{"name":"ext","url":"https://ext.example.com/mcp","transport":"sse","trusted":true}`)

	handler := NewResourceHandler(counting, "default", nil, "", nil).WithOSS(store)
	rec := mcpGet(t, handler, mcpCallerCtx(authpkg.RoleAdmin, "", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := counting.listCalls; got != 1 {
		t.Fatalf("List calls = %d, want 1 (WorkerList only — the team lookup was also skipped by the old code)", got)
	}
	resp := mcpDecode(t, rec)
	if !resp.RegistryAvailable {
		t.Fatal("registry_available = false, want true (shared half unaffected)")
	}
	if resp.Total != 1 || resp.Servers[0].Name != "ext" {
		t.Fatalf("registry-only catalog wrong: %+v", resp.Servers)
	}
}
