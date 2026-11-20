package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func TestTaskResourceHidesStorageLayoutAndPreservesSubmissionIdentity(t *testing.T) {
	store := ossfake.NewMemory()
	meta := `{"task_id":"t1","task_kind":"execution","title":"Analyse input","status":"submitted","assigned_to":"dev","spec":"Produce the report","summary":"Report ready","submission_id":"s1","main_task_id":"main","deliverables":["shared/tasks/t1/workspace/report.md"],"storagePrefix":"internal-bucket/teams/alpha-team","reply_route":{"target_session":"!private:test"}}`
	if err := store.PutObject(t.Context(), taskMetaKey, []byte(meta)); err != nil {
		t.Fatal(err)
	}
	h := newProjectTestHandler(t, store, team("alpha-team"))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1?team=alpha-team", nil)
	req.SetPathValue("id", "t1")
	rec := httptest.NewRecorder()
	h.GetTaskResource(rec, withCaller(req, &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "admin"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Version string `json:"version"`
		Task    struct {
			ID           string `json:"id"`
			Team         string `json:"team"`
			Status       string `json:"status"`
			SubmissionID string `json:"submissionId"`
		} `json:"task"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Task.ID != "t1" || response.Task.Team != "alpha-team" || response.Task.Status != "submitted" || response.Task.SubmissionID != "s1" {
		t.Fatalf("wrong identity/state: %s", rec.Body.String())
	}
	if response.Version == "" || rec.Header().Get("ETag") != response.Version {
		t.Fatalf("missing version: %s", rec.Body.String())
	}
	for _, internal := range []string{"shared/tasks", "internal-bucket", "!private:test", "meta.json"} {
		if strings.Contains(rec.Body.String(), internal) {
			t.Errorf("resource exposed internal layout %q", internal)
		}
	}
}

func TestTaskResourceKeepsTeamAuthorizationAndRejectsInvalidID(t *testing.T) {
	for _, tc := range []struct {
		name, id, team string
		caller         *authpkg.CallerIdentity
		want           int
	}{
		{"own team", "t1", "alpha-team", &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}, http.StatusOK},
		{"foreign team", "t1", "alpha-team", &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "other", Team: "other-team"}, http.StatusForbidden},
		{"missing", "missing", "alpha-team", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin}, http.StatusNotFound},
		{"invalid", "../t1", "alpha-team", &authpkg.CallerIdentity{Role: authpkg.RoleAdmin}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := ossfake.NewMemory()
			if err := store.PutObject(t.Context(), taskMetaKey, []byte(`{"task_id":"t1","status":"assigned"}`)); err != nil {
				t.Fatal(err)
			}
			h := newProjectTestHandler(t, store, team("alpha-team"))
			req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1?team="+tc.team, nil)
			req.SetPathValue("id", tc.id)
			rec := httptest.NewRecorder()
			h.GetTaskResource(rec, withCaller(req, tc.caller))
			if rec.Code != tc.want {
				t.Fatalf("status=%d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestTaskResourceInfersWorkerTeamForRepeatedTaskIDs(t *testing.T) {
	store := ossfake.NewMemory()
	for _, teamName := range []string{"alpha-team", "beta-team"} {
		if err := store.PutObject(t.Context(), "teams/"+teamName+"/shared/tasks/t1/meta.json", []byte(`{"task_id":"t1","status":"assigned"}`)); err != nil {
			t.Fatal(err)
		}
	}
	h := newProjectTestHandler(t, store, team("alpha-team"), team("beta-team"))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1", nil)
	r.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	h.GetTaskResource(w, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "dev", Team: "alpha-team"}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"team":"alpha-team"`) {
		t.Fatalf("own scope not inferred: %d %s", w.Code, w.Body.String())
	}
}
