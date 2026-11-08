package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMetaReadRoutesUseScopedReaderAuthorization(t *testing.T) {
	store := ossfake.NewMemory()
	for _, seed := range []struct{ team, project, task string }{{"alpha-team", "pa", "ta"}, {"beta-team", "pb", "tb"}} {
		putProject(store, metaCommitKey(seed.team, seed.project), map[string]any{"project_id": seed.project, "team_id": seed.team, "status": "active", "plan_type": "dag"})
		putProject(store, "teams/"+seed.team+"/shared/tasks/"+seed.task+"/meta.json", map[string]any{"task_id": seed.task, "status": "running"})
	}
	k8s := fake.NewClientBuilder().WithScheme(newProjectTestScheme(t)).WithObjects(team("alpha-team"), team("beta-team")).Build()
	cases := []struct {
		name       string
		caller     *authpkg.CallerIdentity
		own, cross int
	}{
		{"human", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "alice", Teams: []string{"alpha-team"}}, 200, 404},
		{"human_without_access", &authpkg.CallerIdentity{Role: authpkg.RoleHuman, Username: "outsider"}, 404, 404},
		{"leader", &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead", Team: "alpha-team"}, 200, 404},
		{"admin", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}, 200, 200},
		{"manager", &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "manager"}, 200, 200},
		// The existing worker read prohibition remains: this addition serves
		// the workbench, without granting a new worker/project control surface.
		{"worker", &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "worker", Team: "alpha-team"}, 403, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw := authpkg.NewMiddleware(&roleAuthenticator{identity: tc.caller}, nil, authpkg.NewAuthorizer(), k8s, "default")
			srv := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", OSS: &mcLikeOSS{Memory: store}, AuthMw: mw})
			for _, path := range []struct {
				path string
				want int
			}{{"projects/pa/meta", tc.own}, {"projects/pb/meta", tc.cross}, {"tasks/ta/meta", tc.own}, {"tasks/tb/meta", tc.cross}} {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/"+path.path, nil)
				req.Header.Set("Authorization", "Bearer test-identity")
				rec := httptest.NewRecorder()
				srv.Mux.ServeHTTP(rec, req)
				if rec.Code != path.want {
					t.Errorf("GET %s: got%d want%d: %s", path.path, rec.Code, path.want, rec.Body.String())
				}
			}
		})
	}
}
