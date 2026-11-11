package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestWorkerMetaCommit_EffectiveTeamName(t *testing.T) {
	for _, kind := range []string{"tasks", "projects"} {
		for _, callerTeam := range []string{"delivery-team", "other-team", ""} {
			t.Run(kind+"/"+callerTeam, func(t *testing.T) {
				store := ossfake.NewMemory()
				owner := teamWithRoster("delivery-team")
				owner.Spec.TeamName = "业务交付团队"
				h := newProjectTestHandler(t, store, owner, team("other-team"))
				body := `{"task_id":"t1","project_id":"p1","title":"Delivery","status":"in_progress","room_id":"!room:example.org"}`
				id := "t1"
				if kind == "projects" {
					body = `{"project_id":"p1","title":"Delivery","status":"active","plan_type":"dag","team_id":"业务交付团队","tasks":[]}`
					id = "p1"
				}
				key := "teams/业务交付团队/shared/" + kind + "/" + id + "/meta.json"
				if err := store.PutObject(context.Background(), key, []byte(body)); err != nil {
					t.Fatal(err)
				}
				version, err := store.StatMeta(context.Background(), key)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPut, "/api/v1/"+kind+"/"+id+"/meta", strings.NewReader(body))
				req.SetPathValue("id", id)
				req.Header.Set("If-Match", version.ETag)
				req = withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "alpha-dev", Team: callerTeam})
				rec := httptest.NewRecorder()
				if kind == "tasks" {
					h.PutTaskMeta(rec, req)
				} else {
					h.PutProjectMeta(rec, req)
				}
				want := http.StatusForbidden
				if callerTeam == "delivery-team" {
					want = http.StatusOK
				}
				if rec.Code != want {
					t.Fatalf("status %d, want %d: %s", rec.Code, want, rec.Body.String())
				}
			})
		}
	}
}
