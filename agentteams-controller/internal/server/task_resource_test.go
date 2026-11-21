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

func TestMainTaskResourceIncludesAcceptedCandidateAndReviewForClosure(t *testing.T) {
	h, store, caller, submission := taskReviewFixture(t)
	if w := callTaskReview(h, caller, submission, "Read published report and verified business evidence", true); w.Code != 200 {
		t.Fatalf("accept: %d %s", w.Code, w.Body.String())
	}
	key := "teams/alpha/shared/tasks/main/meta.json"
	data, _ := store.GetObject(t.Context(), key)
	var main map[string]any
	_ = json.Unmarshal(data, &main)
	main["task_plan"] = taskPlan{ID: "plan-1", ActorName: caller.Username, Request: taskPlanRequest{Nodes: []taskPlanNode{{ID: "implementation", AssignedTo: "worker-cr", Spec: "Create a report."}}}}
	main["tasks"].([]any)[0].(map[string]any)["step_id"] = "implementation"
	putTask(store, key, main)
	read := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/main?team=alpha", nil)
		r.SetPathValue("id", "main")
		w := httptest.NewRecorder()
		h.GetTaskResource(w, withCaller(r, caller))
		return w
	}
	first := read()
	var result struct {
		Version string `json:"version"`
		Task    struct {
			Accepted []taskResource `json:"acceptedResults"`
		} `json:"task"`
	}
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &result) != nil || len(result.Task.Accepted) != 1 {
		t.Fatalf("missing closure evidence: %d %s", first.Code, first.Body.String())
	}
	child := result.Task.Accepted[0]
	if child.ID != "t1" || child.MainTaskID != "main" || child.Status != "completed" || child.Candidate == nil || child.Candidate.ID != submission || child.Candidate.Report != "Verified candidate" || len(child.Candidate.Artifacts) != 1 || child.Review == nil || !child.Review.Accepted || child.Review.SubmissionID != submission || child.Review.Note != "Read published report and verified business evidence" {
		t.Fatalf("incomplete accepted result: %+v", child)
	}
	for _, hidden := range []string{"shared/tasks/", "meta.json", "!source:test", "!private:test"} {
		if strings.Contains(first.Body.String(), hidden) {
			t.Fatalf("storage/route exposed: %s", first.Body.String())
		}
	}
	data, _ = store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	var saved map[string]any
	_ = json.Unmarshal(data, &saved)
	saved["title"] = "Updated display title"
	putTask(store, "teams/alpha/shared/tasks/t1/meta.json", saved)
	second := read()
	if second.Code != 200 || second.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Fatalf("composite version ignored child change: %d %s", second.Code, second.Body.String())
	}
}

func TestMainTaskResourceRejectsChangedAcceptedResult(t *testing.T) {
	for _, scenario := range []string{"wrong-main", "wrong-project", "wrong-assignee", "not-accepted", "missing-review", "wrong-submission", "unsafe-node"} {
		t.Run(scenario, func(t *testing.T) {
			h, store, caller, submission := taskReviewFixture(t)
			if w := callTaskReview(h, caller, submission, "Verified evidence", true); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			mainKey := "teams/alpha/shared/tasks/main/meta.json"
			data, _ := store.GetObject(t.Context(), mainKey)
			var main map[string]any
			_ = json.Unmarshal(data, &main)
			main["task_plan"] = taskPlan{ID: "plan-1", ActorName: caller.Username, Request: taskPlanRequest{Nodes: []taskPlanNode{{ID: "implementation", AssignedTo: "worker-cr", Spec: "Create report"}}}}
			node := main["tasks"].([]any)[0].(map[string]any)
			node["step_id"] = "implementation"
			if scenario == "unsafe-node" {
				node["task_id"] = "../foreign"
			}
			putTask(store, mainKey, main)
			childKey := "teams/alpha/shared/tasks/t1/meta.json"
			data, _ = store.GetObject(t.Context(), childKey)
			var child map[string]any
			_ = json.Unmarshal(data, &child)
			switch scenario {
			case "wrong-main":
				child["main_task_id"] = "other"
			case "wrong-project":
				child["project_id"] = "other"
			case "wrong-assignee":
				child["assigned_to"] = "other"
			case "not-accepted":
				child["status"] = "submitted"
			case "missing-review":
				delete(child, "task_review")
			case "wrong-submission":
				child["task_review"].(map[string]any)["submissionId"] = "other"
			}
			putTask(store, childKey, child)
			r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/main?team=alpha", nil)
			r.SetPathValue("id", "main")
			w := httptest.NewRecorder()
			h.GetTaskResource(w, withCaller(r, caller))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("invalid accepted result returned: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
