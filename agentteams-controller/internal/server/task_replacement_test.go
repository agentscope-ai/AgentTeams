package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
)

func rejectWithReplacement(h *ProjectHandler, caller *authpkg.CallerIdentity, id, submission, correction string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(taskReviewRequest{SubmissionID: submission, Note: "Independent QA found incorrect rounding", ReplacementSpec: correction})
	r := httptest.NewRequest(http.MethodPost, "/reject", strings.NewReader(string(body)))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h.ReviewTask(false)(w, withCaller(r, caller))
	return w
}

func TestTaskReplacementPreservesRejectedAttemptAndReleasesOnlyCorrectedInput(t *testing.T) {
	h, store, leader, mx := taskPlanFixture(t)
	input := planRequest()
	input.Nodes = []taskPlanNode{input.Nodes[0], {ID: "C", AssignedTo: "second-cr", Spec: "Consume corrected A", DependsOn: []string{"A"}}}
	if w := callPlan(h, leader, input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	oldID := taskPlanExecutionID("alpha", "main", "A")
	submission := submitPlannedCandidate(t, h, store, oldID, "worker-cr", "Incorrect output", "REVISION_NEEDED")
	oldKey := "teams/alpha/shared/tasks/" + oldID + "/meta.json"
	w := rejectWithReplacement(h, leader, oldID, submission, "Use exact integer cents and retain the requested interface")
	var receipt struct {
		ReplacementTaskID string `json:"replacementTaskId"`
		MainProjection    string `json:"mainProjection"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.ReplacementTaskID == "" || receipt.MainProjection != "applied" {
		t.Fatalf("replacement: %d %s", w.Code, w.Body.String())
	}
	newID := receipt.ReplacementTaskID
	oldBefore, _ := store.GetObject(t.Context(), oldKey)
	old, _ := h.coordination.read(t.Context(), oldKey)
	newTask, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+newID+"/meta.json")
	if err != nil || old["status"] != "revision" || newTask["status"] != "assigned" || newTask["main_task_id"] != "main" || newTask["revision_of"] != oldID || !strings.Contains(coordinationString(newTask, "spec"), "integer cents") {
		t.Fatalf("attempt identity: old=%+v new=%+v err=%v", old, newTask, err)
	}
	for i := 0; i < 2; i++ {
		h.recoverTaskNotifications(t.Context())
	}
	retry := rejectWithReplacement(h, leader, oldID, submission, "Use exact integer cents and retain the requested interface")
	if retry.Code != 200 || !strings.Contains(retry.Body.String(), newID) {
		t.Fatalf("lost retry: %d %s", retry.Code, retry.Body.String())
	}
	if changed := rejectWithReplacement(h, leader, oldID, submission, "Different correction"); changed.Code != 409 {
		t.Fatalf("changed retry: %d %s", changed.Code, changed.Body.String())
	}
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	if len(coordinationMap(main, "task_attempts")) != 1 || mx.creates != 1 {
		t.Fatalf("history/room duplicated: %+v rooms=%d", main, mx.creates)
	}
	consumerID := taskPlanExecutionID("alpha", "main", "C")
	assertPlannedTaskWaiting(t, store, "teams/alpha/shared/tasks/"+consumerID+"/meta.json")
	submitPlannedCandidate(t, h, store, newID, "worker-cr", "Corrected output")
	consumer, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+consumerID+"/meta.json")
	if err != nil || consumer["status"] != "assigned" {
		t.Fatalf("corrected prerequisite did not release consumer: %+v %v", consumer, err)
	}
	// Begin must bind downstream files to the accepted replacement, never the rejected candidate.
	consumer["execution"] = map[string]any{"generation": 1, "instance_id": "instance-1", "process_id": "process-1", "state": "active"}
	putTask(store, "teams/alpha/shared/tasks/"+consumerID+"/meta.json", consumer)
	r := httptest.NewRequest(http.MethodPost, "/begin", nil)
	r.SetPathValue("id", consumerID)
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	begin := httptest.NewRecorder()
	h.BeginTask(begin, withCaller(r, &authpkg.CallerIdentity{Role: authpkg.RoleWorker, Username: "second-cr", Team: "alpha"}))
	if begin.Code != 200 || !strings.Contains(begin.Body.String(), newID) || strings.Contains(begin.Body.String(), oldID) {
		t.Fatalf("wrong corrected input: %d %s", begin.Code, begin.Body.String())
	}
	oldAfter, _ := store.GetObject(t.Context(), oldKey)
	if string(oldBefore) != string(oldAfter) {
		t.Fatal("rejected execution changed during replacement")
	}
	submitPlannedCandidate(t, h, store, consumerID, "second-cr", "Verified integration")
	completed := callTaskComplete(h, leader, "Corrected outputs accepted")
	var final struct {
		Task taskResource `json:"task"`
	}
	if completed.Code != 200 || json.Unmarshal(completed.Body.Bytes(), &final) != nil || len(final.Task.Artifacts) != 2 {
		t.Fatalf("corrected completion: %d %s", completed.Code, completed.Body.String())
	}
	for _, artifact := range final.Task.Artifacts {
		if artifact.TaskID == oldID {
			t.Fatal("rejected output included in final delivery")
		}
	}
}

func TestLeaderCanResumeReviewedBlockedExecution(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	if w := callPlan(h, leader, planRequest()); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := taskPlanExecutionID("alpha", "main", "A")
	submission := submitPlannedCandidate(t, h, store, id, "worker-cr", "Awaiting asset permission", "BLOCKED")
	first := rejectWithReplacement(h, leader, id, submission, "")
	if first.Code != 200 {
		t.Fatal(first.Body.String())
	}
	const context = "Admin approved the assets; continue the offline checks"
	w := rejectWithReplacement(h, leader, id, submission, context)
	var result struct {
		ReplacementTaskID string
		MainProjection    string
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.MainProjection != "applied" || result.ReplacementTaskID == "" {
		t.Fatalf("resume: %d %s", w.Code, w.Body.String())
	}
	next, err := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+result.ReplacementTaskID+"/meta.json")
	if err != nil || next["status"] != "assigned" || !strings.Contains(coordinationString(next, "spec"), context) {
		t.Fatalf("replacement: %+v %v", next, err)
	}
	retry := rejectWithReplacement(h, leader, id, submission, context)
	if retry.Code != 200 || !strings.Contains(retry.Body.String(), result.ReplacementTaskID) {
		t.Fatal(retry.Body.String())
	}
	if w := rejectWithReplacement(h, leader, id, submission, "Different continuation"); w.Code != 409 {
		t.Fatal(w.Body.String())
	}
}

func TestTaskReplacementRejectsFreezeAndActivatedConsumerBeforeSavingDecision(t *testing.T) {
	for _, scenario := range []string{"frozen", "activated-consumer"} {
		t.Run(scenario, func(t *testing.T) {
			h, store, leader, _ := taskPlanFixture(t)
			input := planRequest()
			input.Nodes = []taskPlanNode{input.Nodes[0], {ID: "C", AssignedTo: "second-cr", Spec: "Use A", DependsOn: []string{"A"}}}
			if w := callPlan(h, leader, input); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			id := taskPlanExecutionID("alpha", "main", "A")
			submission := submitPlannedCandidate(t, h, store, id, "worker-cr", "Bad", "REVISION_NEEDED")
			key := "teams/alpha/shared/tasks/main/meta.json"
			main, _ := h.coordination.read(t.Context(), key)
			if scenario == "frozen" {
				main["decision_status"] = "frozen"
			} else {
				main["tasks"].([]any)[1].(map[string]any)["status"] = "assigned"
			}
			putTask(store, key, main)
			w := rejectWithReplacement(h, leader, id, submission, "Fix")
			child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
			if w.Code < 400 || child["status"] != "submitted" || child["task_review"] != nil {
				t.Fatalf("invalid replacement saved: %d %s child=%+v", w.Code, w.Body.String(), child)
			}
		})
	}
}

func TestTaskReplacementKeepsR1AcceptanceGate(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	input := planRequest()
	input.Nodes = input.Nodes[:1]
	if w := callPlan(h, leader, input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := taskPlanExecutionID("alpha", "main", "A")
	submission := submitPlannedCandidate(t, h, store, id, "worker-cr", "Bad", "REVISION_NEEDED")
	key := "teams/alpha/shared/tasks/" + id + "/meta.json"
	child, _ := h.coordination.read(t.Context(), key)
	child["decision_class"] = "r1"
	putTask(store, key, child)
	w := rejectWithReplacement(h, leader, id, submission, "Fix")
	var result struct {
		ReplacementTaskID string `json:"replacementTaskId"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ReplacementTaskID == "" {
		t.Fatalf("replacement: %d %s", w.Code, w.Body.String())
	}
	newKey := "teams/alpha/shared/tasks/" + result.ReplacementTaskID + "/meta.json"
	replacement, _ := h.coordination.read(t.Context(), newKey)
	if replacement["decision_class"] != "r1" {
		t.Fatal("replacement downgraded r1")
	}
	newSubmission := submitPlannedCandidate(t, h, store, result.ReplacementTaskID, "worker-cr", "Corrected")
	body, _ := json.Marshal(taskReviewRequest{SubmissionID: newSubmission, Note: "Looks correct"})
	r := httptest.NewRequest(http.MethodPost, "/accept", strings.NewReader(string(body)))
	r.SetPathValue("id", result.ReplacementTaskID)
	accepted := httptest.NewRecorder()
	h.ReviewTask(true)(accepted, withCaller(r, leader))
	if accepted.Code != 409 || !strings.Contains(accepted.Body.String(), "human approval") {
		t.Fatalf("r1 bypass: %d %s", accepted.Code, accepted.Body.String())
	}
}

func TestTaskReplacementRecordLimitIsCheckedBeforeSavingRejection(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	input := planRequest()
	input.Nodes = input.Nodes[:1]
	input.Nodes[0].Spec = strings.Repeat("x", 350000)
	if w := callPlan(h, leader, input); w.Code != 200 {
		t.Fatalf("plan code %d", w.Code)
	}
	id := taskPlanExecutionID("alpha", "main", "A")
	submission := submitPlannedCandidate(t, h, store, id, "worker-cr", "Bad", "REVISION_NEEDED")
	w := rejectWithReplacement(h, leader, id, submission, strings.Repeat("c", 100000))
	child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/"+id+"/meta.json")
	if w.Code != 413 || child["status"] != "submitted" || child["task_review"] != nil {
		t.Fatalf("record overflow committed a decision: %d", w.Code)
	}
}

func TestTaskReplacementRecoversCommittedRejectionAfterParentCASFailure(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	input := planRequest()
	input.Nodes = input.Nodes[:1]
	if w := callPlan(h, leader, input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := taskPlanExecutionID("alpha", "main", "A")
	submission := submitPlannedCandidate(t, h, store, id, "worker-cr", "Bad output", "REVISION_NEEDED")
	fault := &reviewProjectionFailure{Memory: store, fail: true}
	h.oss = fault
	w := rejectWithReplacement(h, leader, id, submission, "Fix the result")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mainProjection":"pending"`) {
		t.Fatalf("pending recovery: %d %s", w.Code, w.Body.String())
	}
	fault.fail = false
	h.recoverTaskNotifications(t.Context())
	h.recoverTaskNotifications(t.Context())
	retry := rejectWithReplacement(h, leader, id, submission, "Fix the result")
	if retry.Code != 200 || !strings.Contains(retry.Body.String(), `"mainProjection":"applied"`) {
		t.Fatalf("recovery: %d %s", retry.Code, retry.Body.String())
	}
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	if len(coordinationMap(main, "task_attempts")) != 1 {
		t.Fatal("replacement history lost or duplicated")
	}
}

func TestTaskReviewPublishesDecisionBeforeRework(t *testing.T) {
	h, store, leader, _ := taskPlanFixture(t)
	var reviewTxns []string
	matrix := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var content map[string]any
		_ = json.NewDecoder(r.Body).Decode(&content)
		if content["body"] != "规划一个验证节点" {
			if content["body"] != "实测结果不符合要求，需要返工。" || coordinationMap(content, "org.agentteams.run")["run_id"] != "review-run" {
				t.Errorf("unexpected review message: %v", content)
			}
			reviewTxns = append(reviewTxns, r.URL.Path)
			if len(reviewTxns) == 1 {
				w.WriteHeader(503)
				return
			}
		}
		_, _ = w.Write([]byte(`{"event_id":"$published"}`))
	}))
	defer matrix.Close()
	h.coordination.deps.MatrixConfig.ServerURL = matrix.URL
	input := planRequest()
	input.Overview = "规划一个验证节点"
	input.Nodes = input.Nodes[:1]
	if w := callPlan(h, leader, input); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	id := taskPlanExecutionID("alpha", "main", "A")
	submission := submitPlannedCandidate(t, h, store, id, "worker-cr", "数值错误", "REVISION_NEEDED")
	reject := func() string {
		body, _ := json.Marshal(taskReviewRequest{SubmissionID: submission, Note: "实测结果不符合要求，需要返工。", ReplacementSpec: "修正数值"})
		r := httptest.NewRequest("POST", "/reject", strings.NewReader(string(body)))
		r.SetPathValue("id", id)
		r.Header.Set("X-AgentTeams-Run-ID", "review-run")
		w := httptest.NewRecorder()
		h.ReviewTask(false)(w, withCaller(r, leader))
		return w.Body.String()
	}
	if got := reject(); !strings.Contains(got, `"mainProjection":"pending"`) {
		t.Fatalf("replacement released before review was visible: %s", got)
	}
	if got := reject(); !strings.Contains(got, `"mainProjection":"applied"`) {
		t.Fatal(got)
	}
	_ = reject()
	if len(reviewTxns) != 2 || reviewTxns[0] != reviewTxns[1] {
		t.Fatalf("review publication was not idempotent: %v", reviewTxns)
	}
}
