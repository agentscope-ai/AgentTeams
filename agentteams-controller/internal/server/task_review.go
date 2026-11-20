package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type taskReviewRequest struct {
	ReplacementSpec string `json:"replacementSpec,omitempty"`
	SubmissionID    string `json:"submissionId"`
	Note            string `json:"note"`
}

type taskReview struct {
	ReplacementSpec string `json:"replacementSpec,omitempty"`
	ID              string `json:"id"`
	SubmissionID    string `json:"submissionId"`
	Note            string `json:"note"`
	Accepted        bool   `json:"accepted"`
	Status          string `json:"status"`
	Actor           string `json:"actor"`
}

// ReviewTask commits the Leader's business decision against an exact immutable
// candidate. Parent projection is retriable from this durable child decision.
func (h *ProjectHandler) ReviewTask(accepted bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isSafeTaskID(id) {
			writeError(w, 400, "invalid task id")
			return
		}
		var input taskReviewRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF {
			writeError(w, 400, "invalid review")
			return
		}
		input.Note = strings.TrimSpace(input.Note)
		input.ReplacementSpec = strings.TrimSpace(input.ReplacementSpec)
		if accepted && input.ReplacementSpec != "" {
			writeError(w, 400, "acceptance cannot request replacement work")
			return
		}
		if input.SubmissionID == "" || input.Note == "" {
			writeError(w, 400, "exact submission identity and business review note required")
			return
		}
		key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
		if !ok {
			return
		}
		for attempt := 0; attempt < 3; attempt++ {
			body, err := h.oss.GetObject(r.Context(), key)
			var task map[string]any
			var raw map[string]json.RawMessage
			if err != nil || json.Unmarshal(body, &task) != nil || json.Unmarshal(body, &raw) != nil {
				writeError(w, 503, "candidate snapshot unavailable")
				return
			}
			mainID := coordinationString(task, "main_task_id")
			if coordinationString(task, "task_id") != id || coordinationString(task, "task_kind") == "main" || !isSafeTaskID(mainID) {
				writeError(w, 409, "review requires an execution task in a coordinating main task")
				return
			}
			mainKey := path.Join(path.Dir(path.Dir(key)), mainID, "meta.json")
			mainBody, err := h.oss.GetObject(r.Context(), mainKey)
			var main map[string]any
			if err != nil || json.Unmarshal(mainBody, &main) != nil || coordinationString(main, "task_id") != mainID || coordinationString(main, "task_kind") != "main" {
				writeError(w, 503, "main task snapshot unavailable")
				return
			}
			if !h.ownsMainTask(w, r, main) {
				return
			}
			if !taskReviewMembership(main, task) && !taskReviewArchived(main, task) {
				writeError(w, 409, "candidate does not match the main task plan")
				return
			}
			caller := authpkg.CallerFromContext(r.Context())
			actor := "leader:" + caller.Username
			var saved taskReview
			if len(raw["task_review"]) > 0 {
				if json.Unmarshal(raw["task_review"], &saved) != nil || saved.ID == "" {
					writeError(w, 503, "review receipt unavailable")
					return
				}
				if saved.SubmissionID != input.SubmissionID || saved.Note != input.Note || saved.Accepted != accepted || saved.Actor != actor || saved.ReplacementSpec != input.ReplacementSpec {
					writeError(w, 409, "candidate already has a different review decision")
					return
				}
				h.writeTaskReview(w, r, key, id, team, task, saved)
				return
			}
			if coordinationString(task, "status") != "submitted" || coordinationString(task, "submission_id") != input.SubmissionID || coordinationString(main, "status") != "in_progress" {
				writeError(w, 409, "review requires the current submitted candidate and active main task")
				return
			}
			if input.ReplacementSpec != "" {
				if !h.taskExecutionAdmission(w, r, mainKey, main) || !h.taskExecutionAdmission(w, r, key, task) {
					return
				}
				if err := validateTaskReplacement(main, task); err != nil {
					writeError(w, 409, err.Error())
					return
				}
			}
			// Rejection remains possible while a decision/approval is outstanding.
			if accepted {
				if !h.taskExecutionAdmission(w, r, mainKey, main) || !h.taskExecutionAdmission(w, r, key, task) {
					return
				}
				if status := coordinationString(task, "result_status"); status != "SUCCESS" && status != "SUCCESS_WITH_NOTES" {
					writeError(w, 409, "non-success candidate cannot be accepted as completed")
					return
				}
				routing := NewCoordinationHandler(ServerDeps{Client: h.client, APIReader: h.configReader, Namespace: h.namespace})
				assignee, err := routing.worker(r.Context(), coordinationString(task, "assigned_to"))
				leader, leaderErr := routing.worker(r.Context(), caller.Username)
				if err != nil || leaderErr != nil || assignee.Name == leader.Name {
					writeError(w, 403, "a registered independent Leader must review this candidate")
					return
				}
				if !taskReviewApproval(raw, []string{assignee.Name, assignee.Spec.EffectiveWorkerName(assignee.Name), assignee.Status.MatrixUserID}) {
					writeError(w, 409, "r1 acceptance requires an independently resolved human approval")
					return
				}
				if err := h.verifyTaskReviewCandidate(r, key, id, raw); err != nil {
					writeError(w, 409, err.Error())
					return
				}
			}
			status := "revision"
			if accepted {
				status = "completed"
			} else if result := coordinationString(task, "result_status"); result == "BLOCKED" || result == "INTERRUPTED" {
				status = "blocked"
			}
			identity, _ := json.Marshal([]any{team, id, input, accepted, actor})
			sum := sha256.Sum256(identity)
			review := taskReview{ReplacementSpec: input.ReplacementSpec, ID: hex.EncodeToString(sum[:]), SubmissionID: input.SubmissionID, Note: input.Note, Accepted: accepted, Status: status, Actor: actor}
			if review.ReplacementSpec != "" {
				var preview map[string]any
				_ = json.Unmarshal(mainBody, &preview)
				if err := applyTaskReplacement(preview, task, review); err != nil {
					writeError(w, 409, err.Error())
					return
				}
				encoded, _ := json.Marshal(preview)
				if len(encoded) > taskMetaCommitMaxBytes {
					writeError(w, 413, "replacement exceeds main task record limit")
					return
				}
			}
			task["task_review"], task["status"] = review, status
			verdict := "fail"
			if accepted {
				verdict = "pass"
			}
			task["review"] = map[string]any{"verdict": verdict, "reviewer": review.Actor, "note": review.Note, "submission_id": review.SubmissionID, "review_id": review.ID}
			task["reviewed_at"] = time.Now().UTC().Format(time.RFC3339)
			appendTaskOperationHistory(task, "submitted", status, "review_task", review.Actor)
			next, _ := json.Marshal(task)
			strict, supported := h.oss.(conditionalWriter)
			if !supported {
				writeError(w, 503, "conditional task storage required")
				return
			}
			if len(next) > taskMetaCommitMaxBytes {
				writeError(w, 413, "review exceeds task record limit")
				return
			}
			if err := strict.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(body)); err != nil {
				if errors.Is(err, oss.ErrPreconditionFailed) {
					continue
				}
				writeError(w, 503, "review commit uncertain; retry unchanged decision")
				return
			}
			h.writeTaskReview(w, r, key, id, team, task, review)
			return
		}
		writeError(w, 409, "candidate changed during review; retry unchanged decision")
	}
}

func taskReviewMembership(main, child map[string]any) bool {
	if coordinationString(child, "project_id") != coordinationString(main, "project_id") {
		return false
	}
	nodes, _ := main["tasks"].([]any)
	for _, item := range nodes {
		node, _ := item.(map[string]any)
		if node["task_id"] == child["task_id"] && node["assigned_to"] == child["assigned_to"] {
			return true
		}
	}
	return false
}

func taskReviewApproval(raw map[string]json.RawMessage, assigneeAliases []string) bool {
	var class string
	_ = json.Unmarshal(raw["decision_class"], &class)
	if class == "" {
		_ = json.Unmarshal(raw["decisionClass"], &class)
	}
	if class != "r1" {
		return true
	}
	records, _ := attentionRecords(raw)
	assignees := assigneeAccounts(raw)
	for _, name := range assigneeAliases {
		if name != "" {
			assignees[canonicalAccount(name)] = struct{}{}
		}
	}
	for _, record := range records {
		kind, _ := recordStringField(record, "kind")
		role, _ := recordStringField(record, "resolvedRole")
		via, _ := recordStringField(record, "resolvedVia")
		actor, _ := recordStringField(record, "resolvedActor")
		_, account := parseActor(actor)
		_, self := assignees[canonicalAccount(account)]
		_, humanChannel := humanResolveChannels[via]
		if kind == "approval" && jsonBoolTrue(record["resolved"]) && role == "human" && humanChannel && account != "" && !self {
			return true
		}
	}
	return false
}

func (h *ProjectHandler) verifyTaskReviewCandidate(r *http.Request, key, id string, raw map[string]json.RawMessage) error {
	var submission taskSubmission
	var identity string
	_ = json.Unmarshal(raw["submission_id"], &identity)
	if json.Unmarshal(raw["task_submission"], &submission) != nil || submission.ID == "" || submission.ID != identity {
		return fmt.Errorf("registered immutable candidate required")
	}
	canonical, _ := json.Marshal(submission.Request)
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != submission.Digest {
		return fmt.Errorf("candidate digest mismatch")
	}
	files, err := h.acceptedTaskFileResources(r, key, id, raw)
	if err != nil {
		return err
	}
	var index taskFileIndex
	_ = json.Unmarshal(raw["task_files"], &index)
	for _, file := range files {
		if err := h.verifyTaskFileContent(r.Context(), key, index.Artifacts[file.ArtifactID]); err != nil {
			return fmt.Errorf("candidate artifact unavailable or changed")
		}
	}
	report := submission.Request.Report
	if report == "" {
		report = submission.Request.Summary
	}
	reportSum := sha256.Sum256([]byte(report))
	reportDigest := hex.EncodeToString(reportSum[:])
	var resultPath, summary, resultStatus string
	_ = json.Unmarshal(raw["result_path"], &resultPath)
	_ = json.Unmarshal(raw["summary"], &summary)
	_ = json.Unmarshal(raw["result_status"], &resultStatus)
	if resultPath != "shared/tasks/"+id+"/artifacts/"+reportDigest || summary != submission.Request.Summary || resultStatus != submission.Request.Status {
		return fmt.Errorf("candidate projections do not match the immutable submission")
	}
	stored, err := h.oss.GetObject(r.Context(), path.Dir(key)+"/artifacts/"+reportDigest)
	if err != nil || sha256.Sum256(stored) != reportSum {
		return fmt.Errorf("candidate report unavailable or changed")
	}
	return nil
}

func (h *ProjectHandler) projectTaskReview(ctx context.Context, key string, child map[string]any, review taskReview) (map[string]any, error) {
	mainID := coordinationString(child, "main_task_id")
	if !isSafeTaskID(mainID) || coordinationString(child, "status") != review.Status || coordinationString(child, "submission_id") != review.SubmissionID {
		return nil, fmt.Errorf("review snapshot no longer matches candidate")
	}
	mainKey := path.Join(path.Dir(path.Dir(key)), mainID, "meta.json")
	strict, supported := h.oss.(conditionalWriter)
	if !supported {
		return nil, oss.ErrConditionalUnsupported
	}
	for attempt := 0; attempt < 3; attempt++ {
		body, err := h.oss.GetObject(ctx, mainKey)
		var main map[string]any
		if err != nil || json.Unmarshal(body, &main) != nil {
			return nil, fmt.Errorf("main task snapshot unavailable")
		}
		if main["task_id"] == mainID && main["task_kind"] == "main" && taskReviewArchived(main, child) {
			return main, nil
		}
		if main["task_id"] != mainID || main["task_kind"] != "main" || !taskReviewMembership(main, child) {
			return nil, fmt.Errorf("main task plan changed")
		}
		if review.Accepted || review.ReplacementSpec != "" {
			if status, message := h.taskExecutionAdmissionFailure(ctx, mainKey, main); status != 0 {
				return nil, fmt.Errorf("%s", message)
			}
		}
		nodes, _ := main["tasks"].([]any)
		for _, item := range nodes {
			node, _ := item.(map[string]any)
			if node["task_id"] != child["task_id"] {
				continue
			}
			if node["review_id"] == review.ID && node["status"] == review.Status {
				return main, nil
			}
			if main["status"] != "in_progress" || taskStatusIsTerminal(coordinationString(node, "status")) {
				return nil, fmt.Errorf("main task or node cannot accept this decision")
			}
			node["status"], node["review_id"], node["submission_id"] = review.Status, review.ID, review.SubmissionID
		}
		if review.ReplacementSpec != "" {
			if err := applyTaskReplacement(main, child, review); err != nil {
				return nil, err
			}
		}
		if coordinationReady(main) {
			var notices map[string]taskNotificationIntent
			encoded, _ := json.Marshal(main["task_notifications"])
			_ = json.Unmarshal(encoded, &notices)
			if notices == nil {
				notices = map[string]taskNotificationIntent{}
			}
			_, name := parseActor(review.Actor)
			notices["ready"] = taskNotificationIntent{Event: coordinationEvent{Kind: "main_task_ready", MainTaskID: mainID, ProjectID: coordinationString(main, "project_id")}, ActorRole: authpkg.RoleTeamLeader, ActorName: name, Team: coordinationString(main, "team_id"), Status: "pending"}
			main["task_notifications"] = notices
		}
		next, _ := json.Marshal(main)
		if len(next) > taskMetaCommitMaxBytes {
			return nil, fmt.Errorf("main task record limit exceeded")
		}
		if err := strict.PutObjectIfMatchStrict(ctx, mainKey, next, contentETag(body)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			return nil, err
		}
		return main, nil
	}
	return nil, oss.ErrPreconditionFailed
}

func (h *ProjectHandler) writeTaskReview(w http.ResponseWriter, r *http.Request, key, id, team string, child map[string]any, review taskReview) {
	main, err := h.projectTaskReview(r.Context(), key, child, review)
	projection := "applied"
	if err != nil {
		projection = "pending"
	}
	dispatch := ""
	var assignments []taskPlanAssignment
	if err == nil && (review.Accepted || review.ReplacementSpec != "") && main["task_plan"] != nil {
		mainKey := path.Join(path.Dir(path.Dir(key)), coordinationString(child, "main_task_id"), "meta.json")
		dispatch = "ready"
		if dispatchErr := h.reconcileTaskPlan(r.Context(), mainKey); dispatchErr != nil {
			dispatch = "pending"
		}
		if latest, _, _, readErr := h.readTaskPlan(r.Context(), mainKey, ""); readErr == nil {
			var receiptErr error
			assignments, receiptErr = h.taskPlanAssignments(r.Context(), mainKey, latest)
			if receiptErr != nil {
				dispatch = "pending"
			}
		} else {
			dispatch = "pending"
		}
	}
	var notification *taskNotificationStatus
	if err == nil && coordinationReady(main) {
		notice := h.flushTaskNotification(r.Context(), path.Join(path.Dir(path.Dir(key)), coordinationString(child, "main_task_id"), "meta.json"), "ready")
		notification = &notice
	}
	replacementID := ""
	if review.ReplacementSpec != "" {
		replacementID = taskReplacementID(child, review)
	}
	httputil.WriteJSON(w, 200, struct {
		Task              taskResource            `json:"task"`
		Review            taskReview              `json:"review"`
		MainProjection    string                  `json:"mainProjection"`
		Notification      *taskNotificationStatus `json:"notification,omitempty"`
		NextAction        string                  `json:"nextAction"`
		ReplacementTaskID string                  `json:"replacementTaskId,omitempty"`
		DispatchStatus    string                  `json:"dispatchStatus,omitempty"`
		Assignments       []taskPlanAssignment    `json:"assignments,omitempty"`
	}{publicTaskResource(id, team, child), review, projection, notification, "Task system saved your exact submission decision. Do not repeat the review or reopen this execution; main-task synchronization is retriable.", replacementID, dispatch, assignments})
}

func validateTaskReviewCommit(w http.ResponseWriter, current, next []byte) bool {
	before, had := rawJSONField(current, "task_review")
	after, has := rawJSONField(next, "task_review")
	if had != has || (had && !sameJSONValue(before, after)) {
		writeError(w, 403, "task reviews are server-owned; use task accept or reject")
		return false
	}
	if had {
		for _, field := range []string{"status", "review", "reviewed_at"} {
			a, okA := rawJSONField(current, field)
			b, okB := rawJSONField(next, field)
			if okA != okB || !sameJSONValue(a, b) {
				writeError(w, 403, "reviewed candidate projections are server-owned")
				return false
			}
		}
	}
	return true
}
