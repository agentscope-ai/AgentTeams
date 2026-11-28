package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskHandoffCompletesAndDispatchesFromWorkerWithoutLeaderReview(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	input := planRequest()
	input.Nodes = []taskPlanNode{input.Nodes[0], {ID: "C", AssignedTo: "second-cr", Spec: "Independently verify A", DependsOn: []string{"A"}}}
	if w := callPlan(h, leader, input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := taskPlanExecutionID("alpha", "main", "A")
	submission := submitPlannedCandidate(t, h, store, id, "worker-cr", "Immutable implementation")
	child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
	if child["status"] != "completed" || child["task_handoff"] == nil || child["task_review"] != nil {
		t.Fatalf("handoff should not invent a Leader review: %+v", child)
	}
	nextID := taskPlanExecutionID("alpha", "main", "C")
	next, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+nextID+"/meta.json")
	if err != nil || next["status"] != "assigned" {
		t.Fatalf("next node not assigned: %+v %v", next, err)
	}
	notice := coordinationMap(coordinationMap(next, "task_notifications"), "assigned")
	if notice["actor_name"] != "worker-cr" || notice["actor_role"] != "worker" {
		t.Fatalf("next-node mention must be sent by completing Worker: %+v", notice)
	}
	intents := coordinationMap(child, "task_notifications")
	if intents["submitted"] != nil {
		t.Fatalf("normal completion still notifies Leader: %+v", intents)
	}
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	nodes, _ := main["tasks"].([]any)
	found := false
	for _, item := range nodes {
		n := item.(map[string]any)
		if n["task_id"] == id {
			found = n["status"] == "completed" && n["submission_id"] == submission
		}
	}
	if !found {
		t.Fatal("main task did not commit completed submission")
	}
	for i := 0; i < 2; i++ {
		h.recoverTaskNotifications(t.Context())
	}
	after, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+nextID+"/meta.json")
	if coordinationString(after, "notifyTxn") != coordinationString(next, "notifyTxn") {
		t.Fatal("handoff replay created a new assignment")
	}
}

func TestTaskHandoffMainResultContainsExactDeliveryWithoutFabricatedReview(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	input := planRequest()
	input.Nodes = input.Nodes[:1]
	if w := callPlan(h, leader, input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := taskPlanExecutionID("alpha", "main", "A")
	submission := submitPlannedCandidate(t, h, store, id, "worker-cr", "Delivered output")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/main", nil)
	r.SetPathValue("id", "main")
	w := httptest.NewRecorder()
	h.GetTaskResource(w, withCaller(r, leader))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var d struct {
		Task struct {
			AcceptedResults []map[string]any `json:"acceptedResults"`
		} `json:"task"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	notice := coordinationMap(coordinationMap(main, "task_notifications"), "ready")
	if notice["actor_name"] != "worker-cr" || notice["actor_role"] != "worker" || notice["recipient"] != "lead-cr" || notice["status"] != "queued" {
		t.Fatalf("last Worker must notify Leader: %+v", notice)
	}
	if len(d.Task.AcceptedResults) != 1 || d.Task.AcceptedResults[0]["submissionId"] != submission || d.Task.AcceptedResults[0]["handoff"] == nil || d.Task.AcceptedResults[0]["review"] != nil {
		t.Fatalf("incorrect final evidence: %s", w.Body.String())
	}
}

func TestTaskHandoffReviewFailureRepairsAndRechecksWithinSameMainTask(t *testing.T) {
	h, store, leader, mx := taskPlanFixture(t)
	input := taskPlanRequest{Nodes: []taskPlanNode{
		{ID: "A", AssignedTo: "worker-cr", Spec: "Produce implementation", DependsOn: []string{}},
		{ID: "R", AssignedTo: "second-cr", Spec: "Independently review implementation", DependsOn: []string{"A"}},
		{ID: "T", AssignedTo: "worker-cr", Spec: "Test reviewed implementation", DependsOn: []string{"R"}},
	}}
	if w := callPlan(h, leader, input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	original := taskPlanExecutionID("alpha", "main", "A")
	submitPlannedCandidate(t, h, store, original, "worker-cr", "Initial implementation")
	failed := taskPlanExecutionID("alpha", "main", "R")
	submission := submitPlannedCandidate(t, h, store, failed, "second-cr", "Wrong title; must match original request", "REVISION_NEEDED")
	tester := taskPlanExecutionID("alpha", "main", "T")
	assertPlannedTaskWaiting(t, store, "teams/alpha/shared/tasks/"+tester+"/meta.json")
	req := taskReviewRequest{SubmissionID: submission, Note: "Review found exact title mismatch", ReplacementSpec: "Restore exact requested title and independently recheck corrected head", ReplacementNodes: []taskPlanNode{
		{ID: "fix", AssignedTo: "worker-cr", Spec: "Correct implementation using immutable original input", DependsOn: []string{}},
		{ID: "review", AssignedTo: "second-cr", Spec: "Independently review correction", DependsOn: []string{"fix"}},
	}}
	body, _ := json.Marshal(req)
	reject := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/reject", strings.NewReader(string(body)))
		r.SetPathValue("id", failed)
		w := httptest.NewRecorder()
		h.ReviewTask(false)(w, withCaller(r, leader))
		return w
	}
	first := reject()
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	mainKey := "teams/alpha/shared/tasks/main/meta.json"
	main, _ := h.coordination.read(t.Context(), mainKey)
	nodes := main["tasks"].([]any)
	if len(nodes) != 4 || len(coordinationMap(main, "task_attempts")) != 1 || mx.creates != 1 {
		t.Fatalf("main task/attempt/room not preserved: %+v", main)
	}
	fix, review := "", ""
	for _, item := range nodes {
		n := item.(map[string]any)
		if n["revision_of"] == failed {
			if n["assigned_to"] == "worker-cr" {
				fix = n["task_id"].(string)
			} else {
				review = n["task_id"].(string)
			}
		}
	}
	if fix == "" || review == "" {
		t.Fatal("missing correction or independent review")
	}
	assertSender := func(taskID, want string) {
		t.Helper()
		child, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+taskID+"/meta.json")
		if err != nil {
			t.Fatal(err)
		}
		notice := coordinationMap(coordinationMap(child, "task_notifications"), "assigned")
		if notice["actor_name"] != want {
			t.Fatalf("correction dispatch sender = %v, want %s", notice["actor_name"], want)
		}
	}
	assertSender(fix, "lead-cr")
	for i := 0; i < 2; i++ {
		h.recoverTaskNotifications(t.Context())
	}
	if retry := reject(); retry.Code != 200 {
		t.Fatalf("retry: %d %s", retry.Code, retry.Body.String())
	}
	submitPlannedCandidate(t, h, store, fix, "worker-cr", "Corrected title at new head")
	assertPlannedTaskWaiting(t, store, "teams/alpha/shared/tasks/"+tester+"/meta.json")
	assertSender(review, "worker-cr")
	submitPlannedCandidate(t, h, store, review, "second-cr", "Correction independently verified")
	next, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+tester+"/meta.json")
	if err != nil || next["status"] != "assigned" {
		t.Fatalf("tester not released: %+v %v", next, err)
	}
	encodedDependencies, _ := json.Marshal(next)
	// Verify that testing can consume the corrected Developer artifact as well
	// as the independent review, without substituting its old ancestor's output.
	main, _ = h.coordination.read(t.Context(), mainKey)
	for _, item := range main["tasks"].([]any) {
		node := item.(map[string]any)
		if node["task_id"] == tester {
			encodedDependencies, _ = json.Marshal(node["depends_on"])
		}
	}
	if !strings.Contains(string(encodedDependencies), fix) || !strings.Contains(string(encodedDependencies), review) {
		t.Fatal("consumer lost corrected implementation or independent review input")
	}

	originalTask, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+original+"/meta.json")
	if originalTask["status"] != "completed" {
		t.Fatal("immutable ancestor reopened")
	}
	old, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+failed+"/meta.json")
	if old["status"] != "revision" || old["submission_id"] != submission {
		t.Fatal("failed review history lost")
	}
}

func TestTaskHandoffRecoversSavedDeliveryAfterParentOutage(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	input := planRequest()
	input.Nodes = []taskPlanNode{input.Nodes[0], {ID: "C", AssignedTo: "second-cr", Spec: "Verify output", DependsOn: []string{"A"}}}
	if w := callPlan(h, leader, input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	fault := &handoffProjectionFailure{reviewProjectionFailure: &reviewProjectionFailure{Memory: store, fail: true}}
	h.oss = fault
	id := taskPlanExecutionID("alpha", "main", "A")
	submission := submitPlannedCandidate(t, h, store, id, "worker-cr", "Delivered once")
	nextID := taskPlanExecutionID("alpha", "main", "C")
	assertPlannedTaskWaiting(t, store, "teams/alpha/shared/tasks/"+nextID+"/meta.json")
	fault.fail = false
	for i := 0; i < 2; i++ {
		h.recoverTaskNotifications(t.Context())
	}
	next, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+nextID+"/meta.json")
	if err != nil || next["status"] != "assigned" {
		t.Fatalf("durable handoff not recovered: %+v %v", next, err)
	}
	original, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
	if original["submission_id"] != submission || original["task_review"] != nil {
		t.Fatal("recovery changed immutable delivery")
	}
}

func TestTaskHandoffCompletionKeepsOnlyUnresolvedSubmitReturnDiscoverable(t *testing.T) {
	for _, state := range []string{"started", "unknown", "verified"} {
		t.Run(state, func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"task_id": "t1", "status": "completed", "submission_id": "s1", "task_handoff": taskHandoff{SubmissionID: "s1", ActorName: "dev", ActorRole: "worker"}, "execution": taskExecution{Generation: 1, Checkpoint: &taskCheckpoint{Operation: &taskOperation{ID: "native-submit", State: state}}}})
			var task map[string]json.RawMessage
			_ = json.Unmarshal(data, &task)
			if got := recoveryStatus(task); got != (state != "verified") {
				t.Fatalf("return state %s discoverable=%v", state, got)
			}
			task["task_handoff"] = json.RawMessage(`{"submissionId":"other"}`)
			if recoveryStatus(task) {
				t.Fatal("unrelated handoff admitted terminal recovery")
			}
		})
	}
}

func TestTaskHandoffCannotBeForgedThroughMetadataCommit(t *testing.T) {
	w := httptest.NewRecorder()
	if validateTaskSubmissionCommit(w, []byte(`{"status":"in_progress"}`), []byte(`{"status":"completed","task_handoff":{"submissionId":"forged"}}`)) || w.Code != 403 {
		t.Fatal("metadata writer forged direct completion")
	}
}

// Let begin commit its acknowledgment; fail only the later delivery projection.
type handoffProjectionFailure struct{ *reviewProjectionFailure }

func (s *handoffProjectionFailure) PutObjectIfMatchStrict(ctx context.Context, key string, body []byte, etag string) error {
	if !strings.Contains(string(body), `"submission_id"`) {
		return s.Memory.PutObjectIfMatchStrict(ctx, key, body, etag)
	}
	return s.reviewProjectionFailure.PutObjectIfMatchStrict(ctx, key, body, etag)
}
