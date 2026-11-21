package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

func taskReviewFixture(t *testing.T) (*ProjectHandler, *ossfake.Memory, *authpkg.CallerIdentity, string) {
	t.Helper()
	h, store, worker, artifact := taskSubmitFixture(t)
	body, _ := json.Marshal(map[string]any{"summary": "Verified candidate", "artifacts": []string{artifact}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/submit", strings.NewReader(string(body)))
	r.SetPathValue("id", "t1")
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w := httptest.NewRecorder()
	h.SubmitTask(w, withCaller(r, worker))
	if w.Code != 200 {
		t.Fatalf("submission fixture: %d %s", w.Code, w.Body.String())
	}
	var submitted struct {
		Task taskResource `json:"task"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &submitted)
	mainKey := "teams/alpha/shared/tasks/main/meta.json"
	data, _ := store.GetObject(t.Context(), mainKey)
	var main map[string]any
	_ = json.Unmarshal(data, &main)
	main["assigned_to"] = "@lead:test"
	main["reply_route"] = map[string]any{"target_user": "@manager:test"}
	main["requester_reply_route"] = map[string]any{"target_user": "@human:test", "target_session": "!source:test"}
	putTask(store, mainKey, main)
	return h, store, &authpkg.CallerIdentity{Role: authpkg.RoleTeamLeader, Username: "lead-cr", Team: "alpha"}, submitted.Task.SubmissionID
}

func callTaskReview(h *ProjectHandler, caller *authpkg.CallerIdentity, submission, note string, accepted bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(taskReviewRequest{SubmissionID: submission, Note: note})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/review", strings.NewReader(string(body)))
	r.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	h.ReviewTask(accepted)(w, withCaller(r, caller))
	return w
}

func TestTaskReviewBindsExactCandidateAndQueuesMainClosureOnce(t *testing.T) {
	h, store, caller, submission := taskReviewFixture(t)
	a := callTaskReview(h, caller, submission, "Validated original acceptance criteria and file content", true)
	b := callTaskReview(h, caller, submission, "Validated original acceptance criteria and file content", true)
	if a.Code != 200 || b.Code != 200 {
		t.Fatalf("accept/retry: %d %s / %d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
	var first, retry struct {
		Task           taskResource           `json:"task"`
		Review         taskReview             `json:"review"`
		MainProjection string                 `json:"mainProjection"`
		Notification   taskNotificationStatus `json:"notification"`
	}
	_ = json.Unmarshal(a.Body.Bytes(), &first)
	_ = json.Unmarshal(b.Body.Bytes(), &retry)
	if first.Task.Status != "completed" || first.Review.ID == "" || first.Review != retry.Review || first.MainProjection != "applied" || first.Notification.Status != "queued" || first.Notification.Recipient != "lead-cr" || first.Notification != retry.Notification {
		t.Fatalf("invalid acceptance/closure receipt: %s / %s", a.Body.String(), b.Body.String())
	}
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	node := main["tasks"].([]any)[0].(map[string]any)
	if node["status"] != "completed" || node["submission_id"] != submission || node["review_id"] != first.Review.ID || main["status"] != "in_progress" {
		t.Fatalf("invalid parent projection: %+v", main)
	}
	for _, change := range []struct {
		note     string
		accepted bool
	}{{"Changed decision", true}, {"Validated original acceptance criteria and file content", false}} {
		w := callTaskReview(h, caller, submission, change.note, change.accepted)
		if w.Code != 409 {
			t.Fatalf("changed decision accepted: %d %s", w.Code, w.Body.String())
		}
	}
	data, _ := store.GetObject(t.Context(), coordinationPending+first.Notification.EventID+".json")
	var delivery coordinationDelivery
	if json.Unmarshal(data, &delivery) != nil || delivery.Event.Kind != "main_task_ready" || delivery.Event.MainTaskID != "main" {
		t.Fatalf("missing durable closure event: %s", data)
	}
}

func TestTaskReviewFailsClosedOnWrongIdentityAuthorityAndCorruptContent(t *testing.T) {
	for _, scenario := range []string{"stale-submission", "worker", "other-team", "not-assigned-leader", "corrupt-file", "corrupt-report", "wrong-report-pointer", "changed-summary", "changed-submission", "r1-missing-approval", "candidate-blocked", "main-paused", "self-review"} {
		t.Run(scenario, func(t *testing.T) {
			h, store, caller, submission := taskReviewFixture(t)
			key := "teams/alpha/shared/tasks/t1/meta.json"
			body, _ := store.GetObject(t.Context(), key)
			var child map[string]any
			_ = json.Unmarshal(body, &child)
			want := 409
			switch scenario {
			case "stale-submission":
				submission = "old-submission"
			case "worker":
				caller.Role, caller.Username, want = authpkg.RoleWorker, "worker-cr", 403
			case "other-team":
				caller.Team, want = "other", 404
			case "not-assigned-leader":
				main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
				main["assigned_to"] = "another-leader"
				putTask(store, "teams/alpha/shared/tasks/main/meta.json", main)
				want = 403
			case "r1-missing-approval":
				child["decision_class"] = "r1"
				putTask(store, key, child)
			case "candidate-blocked":
				child["result_status"] = "BLOCKED"
				putTask(store, key, child)
			case "wrong-report-pointer":
				child["result_path"] = "shared/tasks/t1/result.md"
				putTask(store, key, child)
			case "changed-summary":
				child["summary"] = "Unsubmitted summary"
				putTask(store, key, child)
			case "changed-submission":
				child["task_submission"].(map[string]any)["request"].(map[string]any)["summary"] = "Changed immutable candidate"
				putTask(store, key, child)
			case "corrupt-report":
				ref := strings.TrimPrefix(child["result_path"].(string), "shared/tasks/t1/")
				_ = store.PutObject(t.Context(), "teams/alpha/shared/tasks/t1/"+ref, []byte("corrupt"))
			case "corrupt-file":
				var index taskFileIndex
				data, _ := json.Marshal(child["task_files"])
				_ = json.Unmarshal(data, &index)
				for _, file := range index.Artifacts {
					_ = store.PutObject(t.Context(), "teams/alpha/shared/tasks/t1/artifacts/"+file.SHA256, []byte("corrupt"))
				}
			case "main-paused":
				main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
				main["status"] = "cancelled"
				putTask(store, "teams/alpha/shared/tasks/main/meta.json", main)
			case "self-review":
				child["assigned_to"] = "@lead:test"
				putTask(store, key, child)
				main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
				main["tasks"].([]any)[0].(map[string]any)["assigned_to"] = "@lead:test"
				putTask(store, "teams/alpha/shared/tasks/main/meta.json", main)
				want = 403
			}
			w := callTaskReview(h, caller, submission, "Business validation", true)
			if w.Code != want {
				t.Fatalf("invalid acceptance: %d %s want %d", w.Code, w.Body.String(), want)
			}
			data, _ := store.GetObject(t.Context(), key)
			if strings.Contains(string(data), `"task_review"`) {
				t.Fatal("failed acceptance committed a decision")
			}
		})
	}
}

func TestTaskReviewRejectPreservesTerminalRevisionAndApprovalAuthority(t *testing.T) {
	h, store, caller, submission := taskReviewFixture(t)
	key := "teams/alpha/shared/tasks/t1/meta.json"
	child, _ := h.coordination.read(t.Context(), key)
	child["decision_class"], child["decision_status"] = "r1", "frozen"
	child["attention"] = []any{map[string]any{"kind": "approval", "resolved": false}}
	putTask(store, key, child)
	w := callTaskReview(h, caller, submission, "The delivered output does not meet the original criteria", false)
	if w.Code != 200 {
		t.Fatalf("rejection blocked by approval gate: %d %s", w.Code, w.Body.String())
	}
	child, _ = h.coordination.read(t.Context(), key)
	if child["status"] != "revision" || child["decision_status"] != "frozen" || child["attention"].([]any)[0].(map[string]any)["resolved"] != false {
		t.Fatalf("rejection reopened execution or forged approval: %+v", child)
	}
	if again := callTaskReview(h, caller, submission, "Accept instead", true); again.Code != 409 {
		t.Fatalf("terminal revision changed: %d %s", again.Code, again.Body.String())
	}
}

type reviewProjectionFailure struct {
	*ossfake.Memory
	fail bool
}

func (s *reviewProjectionFailure) PutObjectIfMatchStrict(ctx context.Context, key string, body []byte, etag string) error {
	if s.fail && key == "teams/alpha/shared/tasks/main/meta.json" {
		return errors.New("injected parent projection outage")
	}
	return s.Memory.PutObjectIfMatchStrict(ctx, key, body, etag)
}

func TestTaskReviewRecoversCommittedDecisionAfterParentWriteFailure(t *testing.T) {
	h, store, caller, submission := taskReviewFixture(t)
	fault := &reviewProjectionFailure{Memory: store, fail: true}
	h.oss = fault
	w := callTaskReview(h, caller, submission, "Passed independent business checks", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mainProjection":"pending"`) {
		t.Fatalf("decision recovery receipt missing: %d %s", w.Code, w.Body.String())
	}
	child, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	if child["status"] != "completed" {
		t.Fatal("decision was lost")
	}
	fault.fail = false
	h.recoverTaskNotifications(t.Context())
	h.recoverTaskNotifications(t.Context())
	main, _ := h.coordination.read(t.Context(), "teams/alpha/shared/tasks/main/meta.json")
	if main["tasks"].([]any)[0].(map[string]any)["status"] != "completed" {
		t.Fatal("recovery did not apply durable decision")
	}
	encoded, _ := json.Marshal(main)
	if !strings.Contains(string(encoded), `"status":"queued"`) {
		t.Fatalf("recovery did not queue closure: %s", encoded)
	}
}

func TestTaskReviewApprovalUsesTrustedResolvedFieldsAndRejectsSelfApproval(t *testing.T) {
	for _, scenario := range []string{"approved", "display-only", "self", "self-matrix-alias", "self-runtime-alias", "wrong-channel", "unresolved"} {
		t.Run(scenario, func(t *testing.T) {
			h, store, caller, submission := taskReviewFixture(t)
			key := "teams/alpha/shared/tasks/t1/meta.json"
			child, _ := h.coordination.read(t.Context(), key)
			record := map[string]any{"kind": "approval", "resolved": true, "resolvedRole": "human", "resolvedActor": "human:alice", "resolvedVia": "dashboard"}
			switch scenario {
			case "display-only":
				delete(record, "resolvedActor")
				record["resolvedBy"] = "human:alice"
			case "self":
				record["resolvedActor"] = "human:worker-cr"
			case "self-matrix-alias":
				record["resolvedActor"] = "human:@worker:test"
			case "self-runtime-alias":
				record["resolvedActor"] = "human:worker-runtime"
			case "wrong-channel":
				record["resolvedVia"] = "tool"
			case "unresolved":
				record["resolved"] = false
			}
			child["decisionClass"], child["attention"] = "r1", []any{record}
			putTask(store, key, child)
			w := callTaskReview(h, caller, submission, "Validated business criteria", true)
			want := 409
			if scenario == "approved" {
				want = 200
			}
			if w.Code != want {
				t.Fatalf("approval gate: %d %s want %d", w.Code, w.Body.String(), want)
			}
		})
	}
}

func TestTaskReviewReceiptCannotBeForgedThroughLegacyMeta(t *testing.T) {
	h, store, caller, submission := taskReviewFixture(t)
	w := callTaskReview(h, caller, submission, "Verified", true)
	if w.Code != 200 {
		t.Fatalf("fixture review: %d %s", w.Code, w.Body.String())
	}
	key := "teams/alpha/shared/tasks/t1/meta.json"
	before, _ := store.GetObject(t.Context(), key)
	var child map[string]any
	_ = json.Unmarshal(before, &child)
	delete(child, "task_review")
	body, _ := json.Marshal(child)
	r := httptest.NewRequest(http.MethodPut, "/api/v1/tasks/t1/meta", strings.NewReader(string(body)))
	r.SetPathValue("id", "t1")
	r.Header.Set("If-Match", contentETag(before))
	w = httptest.NewRecorder()
	h.PutTaskMeta(w, withCaller(r, caller))
	if w.Code != 403 {
		t.Fatalf("review deletion allowed: %d %s", w.Code, w.Body.String())
	}
	after, _ := store.GetObject(t.Context(), key)
	if string(before) != string(after) {
		t.Fatal("legacy write changed durable decision")
	}
}

func TestTaskReviewAcceptedCandidateCanCompleteWithOriginalArtifacts(t *testing.T) {
	h, _, caller, submission := taskReviewFixture(t)
	accepted := callTaskReview(h, caller, submission, "Passed independent business validation", true)
	if accepted.Code != 200 {
		t.Fatalf("acceptance: %d %s", accepted.Code, accepted.Body.String())
	}
	completed := callTaskComplete(h, caller, "Accepted output attached.")
	var response struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
	}
	_ = json.Unmarshal(completed.Body.Bytes(), &response)
	if completed.Code != 200 || response.Task.Status != "completed" || len(response.Task.Artifacts) != 1 || response.Task.Artifacts[0].TaskID != "t1" || response.Notification.Status != "queued" || response.Notification.Recipient != "default" {
		t.Fatalf("accepted result did not reach original-report completion: %d %s", completed.Code, completed.Body.String())
	}
}

type reviewProjectionRace struct {
	*ossfake.Memory
	raced bool
}

func (s *reviewProjectionRace) PutObjectIfMatchStrict(ctx context.Context, key string, body []byte, etag string) error {
	if key == "teams/alpha/shared/tasks/main/meta.json" && !s.raced {
		s.raced = true
		snapshot, _ := s.Memory.GetObject(ctx, key)
		var main map[string]any
		_ = json.Unmarshal(snapshot, &main)
		main["tasks"].([]any)[1].(map[string]any)["status"] = "in_progress"
		updated, _ := json.Marshal(main)
		_ = s.Memory.PutObject(ctx, key, updated)
	}
	return s.Memory.PutObjectIfMatchStrict(ctx, key, body, etag)
}

func TestTaskReviewParentCASPreservesAnotherConcurrentNode(t *testing.T) {
	h, store, caller, submission := taskReviewFixture(t)
	mainKey := "teams/alpha/shared/tasks/main/meta.json"
	main, _ := h.coordination.read(t.Context(), mainKey)
	main["tasks"] = append(main["tasks"].([]any), map[string]any{"task_id": "other", "assigned_to": "worker-cr", "status": "assigned"})
	putTask(store, mainKey, main)
	h.oss = &reviewProjectionRace{Memory: store}
	w := callTaskReview(h, caller, submission, "Verified", true)
	if w.Code != 200 {
		t.Fatalf("acceptance: %d %s", w.Code, w.Body.String())
	}
	main, _ = h.coordination.read(t.Context(), mainKey)
	nodes := main["tasks"].([]any)
	if nodes[0].(map[string]any)["status"] != "completed" || nodes[1].(map[string]any)["status"] != "in_progress" {
		t.Fatalf("concurrent node progress lost: %+v", nodes)
	}
}

func TestTaskReviewRecoveryRespectsFreezeBeforeProjectingAcceptedDecision(t *testing.T) {
	h, store, caller, submission := taskReviewFixture(t)
	fault := &reviewProjectionFailure{Memory: store, fail: true}
	h.oss = fault
	w := callTaskReview(h, caller, submission, "Validated", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mainProjection":"pending"`) {
		t.Fatalf("fixture pending decision: %d %s", w.Code, w.Body.String())
	}
	mainKey := "teams/alpha/shared/tasks/main/meta.json"
	main, _ := h.coordination.read(t.Context(), mainKey)
	main["decision_status"] = "frozen"
	putTask(store, mainKey, main)
	fault.fail = false
	h.recoverTaskNotifications(t.Context())
	main, _ = h.coordination.read(t.Context(), mainKey)
	if main["tasks"].([]any)[0].(map[string]any)["status"] == "completed" || main["task_notifications"] != nil {
		t.Fatalf("recovery bypassed main decision freeze: %+v", main)
	}
	main["decision_status"] = "open"
	putTask(store, mainKey, main)
	h.recoverTaskNotifications(t.Context())
	h.recoverTaskNotifications(t.Context())
	main, _ = h.coordination.read(t.Context(), mainKey)
	if main["tasks"].([]any)[0].(map[string]any)["status"] != "completed" {
		t.Fatalf("unfrozen accepted decision did not recover: %+v", main)
	}
}
