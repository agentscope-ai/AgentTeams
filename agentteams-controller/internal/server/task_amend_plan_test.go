package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
)

func TestLeaderAmendsRunningNodeAssignmentAndDependenciesWithoutSubmission(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	if response := callPlan(h, leader, planRequest()); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	mainKey := "teams/alpha/shared/tasks/main/meta.json"
	mainBytes, _ := store.GetObject(t.Context(), mainKey)
	var main map[string]any
	_ = json.Unmarshal(mainBytes, &main)
	nodes := main["tasks"].([]any)
	oldID := coordinationString(nodes[0].(map[string]any), "task_id")
	childKey := path.Join(path.Dir(path.Dir(mainKey)), oldID, "meta.json")
	body, _ := store.GetObject(t.Context(), childKey)
	var child map[string]any
	_ = json.Unmarshal(body, &child)
	child["status"] = "in_progress"
	putTask(store, childKey, child)
	request := `{"spec":"Reassign implementation and validate it independently","note":"user changed responsibilities","replacementNodes":[{"id":"fix","assignedTo":"second-cr","spec":"Implement updated requirements","dependsOn":[]},{"id":"check","assignedTo":"worker-cr","spec":"Validate the same result","dependsOn":["fix"]}]}`
	for attempt := 0; attempt < 2; attempt++ {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+oldID+"/amend?team=alpha", strings.NewReader(request))
		r.SetPathValue("id", oldID)
		w := httptest.NewRecorder()
		h.AmendTask(w, withCaller(r, leader))
		if w.Code != 200 {
			t.Fatalf("amend attempt %d: %d %s", attempt, w.Code, w.Body.String())
		}
	}
	body, _ = store.GetObject(t.Context(), childKey)
	_ = json.Unmarshal(body, &child)
	if child["status"] != "revision" || child["task_revision"] == nil || child["task_submission"] != nil || child["task_review"] != nil {
		t.Fatalf("invented submission or missing administrative revision: %s", body)
	}
	mainBytes, _ = store.GetObject(t.Context(), mainKey)
	_ = json.Unmarshal(mainBytes, &main)
	if len(main["tasks"].([]any)) != 4 || coordinationMap(main, "task_attempts")[oldID] == nil {
		t.Fatalf("replacement/history not retained: %s", mainBytes)
	}
	for _, item := range main["tasks"].([]any) {
		node := item.(map[string]any)
		if node["task_id"] == oldID {
			t.Fatal("retired node remains active")
		}
		for _, dependency := range node["depends_on"].([]any) {
			if dependency == oldID {
				t.Fatal("consumer still depends on retired node")
			}
		}
	}
}

func TestAmendedNodeRevisionCannotBeForgedThroughMetadata(t *testing.T) {
	w := httptest.NewRecorder()
	if validateTaskReviewCommit(w, []byte(`{"status":"in_progress"}`), []byte(`{"status":"revision","task_revision":{"id":"fake"}}`)) || w.Code != 403 {
		t.Fatal("forged administrative revision accepted")
	}
}

func TestLeaderStructuralAmendPreservesUnverifiedOperation(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	if response := callPlan(h, leader, planRequest()); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	mainKey := "teams/alpha/shared/tasks/main/meta.json"
	mainBody, _ := store.GetObject(t.Context(), mainKey)
	var main map[string]any
	_ = json.Unmarshal(mainBody, &main)
	id := coordinationString(main["tasks"].([]any)[0].(map[string]any), "task_id")
	childKey := path.Join(path.Dir(path.Dir(mainKey)), id, "meta.json")
	childBody, _ := store.GetObject(t.Context(), childKey)
	var child map[string]any
	_ = json.Unmarshal(childBody, &child)
	child["execution"] = map[string]any{"state": "needs_verification", "checkpoint": map[string]any{"operation": map[string]any{"state": "started", "id": "unconfirmed-write"}}}
	putTask(store, childKey, child)
	before, _ := store.GetObject(t.Context(), childKey)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/"+id+"/amend?team=alpha", strings.NewReader(`{"spec":"updated","note":"reassign","replacementNodes":[{"id":"fix","assignedTo":"second-cr","spec":"new","dependsOn":[]}]}`))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h.AmendTask(w, withCaller(r, leader))
	after, _ := store.GetObject(t.Context(), childKey)
	if w.Code != 409 || string(before) != string(after) {
		t.Fatalf("unconfirmed write changed: %d %s", w.Code, w.Body.String())
	}
}
