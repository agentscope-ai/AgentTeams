package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type taskSubmitRequest struct {
	Summary   string   `json:"summary"`
	Artifacts []string `json:"artifacts"`
	Report    string   `json:"report"`
	Status    string   `json:"status"`
}

type taskSubmission struct {
	ID         string            `json:"id"`
	Digest     string            `json:"digest"`
	Request    taskSubmitRequest `json:"request"`
	Generation uint64            `json:"generation"`
	ProcessID  string            `json:"process_id"`
	InstanceID string            `json:"instance_id"`
}

// SubmitTask commits a candidate and its notification intent atomically. Only
// Leader acceptance may complete the execution and release dependent work.
func (h *ProjectHandler) SubmitTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isSafeTaskID(id) {
		writeError(w, 400, "invalid task id")
		return
	}
	var input taskSubmitRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		writeError(w, 400, "invalid submission")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeError(w, 400, "invalid submission")
		return
	}
	input.Summary = strings.TrimSpace(input.Summary)
	if input.Status == "" {
		input.Status = "SUCCESS"
	}
	switch input.Status {
	case "SUCCESS", "SUCCESS_WITH_NOTES", "REVISION_NEEDED", "BLOCKED", "INTERRUPTED":
	default:
		writeError(w, 400, "invalid result status")
		return
	}
	if input.Summary == "" {
		writeError(w, 400, "submission summary required")
		return
	}
	sort.Strings(input.Artifacts)
	for i, artifact := range input.Artifacts {
		if artifact == "" || (i > 0 && input.Artifacts[i-1] == artifact) {
			writeError(w, 400, "invalid or duplicate artifact")
			return
		}
	}
	canonical, _ := json.Marshal(input)
	sum := sha256.Sum256(canonical)
	digest := hex.EncodeToString(sum[:])
	key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		current, err := h.oss.GetObject(r.Context(), key)
		var task map[string]any
		var raw map[string]json.RawMessage
		if err != nil || json.Unmarshal(current, &task) != nil || json.Unmarshal(current, &raw) != nil {
			writeError(w, 503, "task snapshot unavailable")
			return
		}
		execution, ok := h.ownedTaskExecution(w, r, raw)
		if !ok {
			return
		}
		if saved := raw["task_submission"]; len(saved) > 0 {
			var old taskSubmission
			if json.Unmarshal(saved, &old) != nil {
				writeError(w, 503, "submission receipt unavailable")
				return
			}
			if old.Digest != digest || old.Generation != execution.Generation || old.ProcessID != execution.ProcessID || old.InstanceID != execution.InstanceID {
				writeError(w, 409, "task already has a different submission")
				return
			}
			h.writeTaskSubmission(w, r, key, id, team, task)
			return
		}
		if coordinationString(task, "task_id") != id || coordinationString(task, "task_kind") == "main" || coordinationString(task, "status") != "in_progress" {
			writeError(w, 409, "task is not executing")
			return
		}
		if !h.taskExecutionAdmission(w, r, key, task) {
			return
		}
		if coordinationString(task, "main_task_id") == "" && coordinationString(task, "project_id") == "" {
			writeError(w, 409, "submission requires a registered parent task or project")
			return
		}
		if _, err := h.taskBeginInputs(r, key, task); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		var index taskFileIndex
		if len(input.Artifacts) > 0 && json.Unmarshal(raw["task_files"], &index) != nil {
			writeError(w, 404, "artifact not registered for this task")
			return
		}
		deliverables := []string{}
		names := map[string]bool{}
		for _, identity := range input.Artifacts {
			file, exists := index.Artifacts[identity]
			if !exists {
				writeError(w, 404, "artifact not registered for this task")
				return
			}
			if file.ArtifactID != identity || file.Kind != "output" || !validTaskFileName(file.Name) || !validArtifactDigest(file.SHA256) || file.Version != file.SHA256 || names[file.Name] {
				writeError(w, 409, "submission requires one immutable output per name")
				return
			}
			names[file.Name] = true
			if err := h.verifyTaskFileContent(r.Context(), key, file); err != nil {
				writeError(w, 503, "submission artifact verification failed")
				return
			}

			deliverables = append(deliverables, "shared/tasks/"+id+"/artifacts/"+file.SHA256)
		}
		strict, supported := h.oss.(conditionalWriter)
		create, canCreate := h.oss.(createIfAbsentWriter)
		if !supported || !canCreate {
			writeError(w, 503, "conditional task storage required")
			return
		}
		report := input.Report
		if report == "" {
			report = input.Summary
		}
		reportHash := sha256.Sum256([]byte(report))
		reportDigest := hex.EncodeToString(reportHash[:])
		blob := path.Dir(key) + "/artifacts/" + reportDigest
		if err := create.PutObjectIfNoneMatchStrict(r.Context(), blob, []byte(report)); err != nil {
			if !errors.Is(err, oss.ErrPreconditionFailed) {
				writeError(w, 503, "submission report unavailable")
				return
			}
			saved, readErr := h.oss.GetObject(r.Context(), blob)
			if readErr != nil || sha256.Sum256(saved) != reportHash {
				writeError(w, 503, "submission report verification failed")
				return
			}
		}
		identity, _ := json.Marshal([]any{team, id, execution.Generation, execution.ProcessID, execution.InstanceID, digest})
		submissionHash := sha256.Sum256(identity)
		submissionID := hex.EncodeToString(submissionHash[:])
		task["task_submission"] = taskSubmission{submissionID, digest, input, execution.Generation, execution.ProcessID, execution.InstanceID}
		task["status"], task["submission_id"], task["summary"], task["result_status"] = "submitted", submissionID, input.Summary, input.Status
		task["result_path"] = "shared/tasks/" + id + "/artifacts/" + reportDigest
		task["deliverables"], task["deliverable_refs"] = deliverables, input.Artifacts
		task["submitted_at"] = time.Now().UTC().Format(time.RFC3339)
		caller := authpkg.CallerFromContext(r.Context())
		event := coordinationEvent{Kind: "task_submitted", TaskID: id, MainTaskID: coordinationString(task, "main_task_id"), ProjectID: coordinationString(task, "project_id"), SubmissionID: submissionID}
		task["task_notifications"] = map[string]taskNotificationIntent{"submitted": {Event: event, ActorRole: caller.Role, ActorName: caller.Username, Team: caller.Team, Status: "pending"}}
		appendTaskOperationHistory(task, "in_progress", "submitted", "submit_task", authzActor(caller))
		next, err := json.Marshal(task)
		if err != nil || len(next) > taskMetaCommitMaxBytes {
			writeError(w, 413, "submission exceeds task record limit")
			return
		}
		if err := strict.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(current)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			writeError(w, 503, "submission commit uncertain; retry unchanged submission")
			return
		}
		h.writeTaskSubmission(w, r, key, id, team, task)
		return
	}
	writeError(w, 409, "task changed during submission; retry unchanged submission")
}

func (h *ProjectHandler) writeTaskSubmission(w http.ResponseWriter, r *http.Request, key, id, team string, task map[string]any) {
	notification := h.flushTaskNotification(r.Context(), key, "submitted")
	nextAction := "Task system has taken responsibility for automatically notifying your Team Leader for review. Do not resend the notification; wait for acceptance or revision."
	if notification.Status == "queued" {
		nextAction = fmt.Sprintf("Task system has queued an automatic notification to %q for review. Do not resend the notification; wait for acceptance or revision.", notification.Recipient)
	}
	httputil.WriteJSON(w, 200, struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
		NextAction   string                 `json:"nextAction"`
	}{publicTaskResource(id, team, task), notification, nextAction})
}

func validateTaskSubmissionCommit(w http.ResponseWriter, current, next []byte) bool {
	before, had := rawJSONField(current, "task_submission")
	after, has := rawJSONField(next, "task_submission")
	if had != has || (had && !sameJSONValue(before, after)) {
		writeError(w, 403, "task submission is server-owned; use the task submit API")
		return false
	}
	if had {
		for _, field := range []string{"status", "submission_id", "summary", "result_status", "result_path", "deliverables", "deliverable_refs", "submitted_at"} {
			a, hasA := rawJSONField(current, field)
			b, hasB := rawJSONField(next, field)
			if hasA != hasB || (hasA && !sameJSONValue(a, b)) {
				writeError(w, 403, "submitted task projections are server-owned")
				return false
			}
		}
	}
	return true
}
