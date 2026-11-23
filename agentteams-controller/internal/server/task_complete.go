package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type taskFinalArtifact struct {
	TaskID string `json:"taskId"`
	publishedTaskFile
}

type taskCompletion struct {
	RequestDigest string              `json:"request_digest"`
	Artifacts     []taskFinalArtifact `json:"artifacts,omitempty"`
	ID            string              `json:"id"`
	ReportSHA256  string              `json:"report_sha256"`
}

func (h *ProjectHandler) ownsMainTask(w http.ResponseWriter, r *http.Request, task map[string]any) bool {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || caller.Role != authpkg.RoleTeamLeader || caller.Team == "" || (caller.ServiceAccountNamespace != "" && caller.ServiceAccountNamespace != h.namespace) {
		writeError(w, 403, "assigned Team Leader required")
		return false
	}
	routing := NewCoordinationHandler(ServerDeps{Client: h.client, APIReader: h.configReader, Namespace: h.namespace})
	worker, err := routing.worker(r.Context(), caller.Username)
	if err != nil {
		writeError(w, 403, "registered Team Leader required")
		return false
	}
	team, err := routing.memberTeam(r.Context(), worker.Name, "team_leader", caller.Team)
	if err != nil || (coordinationString(task, "team_id") != team.Name && coordinationString(task, "team_id") != team.Spec.EffectiveTeamName(team.Name)) {
		writeError(w, 403, "main task does not belong to your Team")
		return false
	}
	assigned := coordinationString(task, "assigned_to")
	if assigned != worker.Name && assigned != worker.Spec.EffectiveWorkerName(worker.Name) && assigned != worker.Status.MatrixUserID {
		writeError(w, 403, "main task is assigned to another Leader")
		return false
	}
	return true
}

// CompleteTask saves the immutable final report and the original-conversation
// notification intent in one Task CAS. Native Agents never send this reply.
func (h *ProjectHandler) CompleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isSafeTaskID(id) {
		writeError(w, 400, "invalid task id")
		return
	}
	var input struct {
		Report    string   `json:"report"`
		Artifacts []string `json:"artifacts,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		writeError(w, 400, "invalid completion")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeError(w, 400, "invalid completion")
		return
	}
	input.Report = strings.TrimSpace(input.Report)
	if input.Report == "" {
		writeError(w, 400, "verified final report required")
		return
	}
	sort.Strings(input.Artifacts)
	for i, ref := range input.Artifacts {
		if ref == "" || (i > 0 && ref == input.Artifacts[i-1]) {
			writeError(w, 400, "invalid or duplicate final artifact reference")
			return
		}
	}
	if input.Artifacts == nil {
		input.Artifacts = []string{}
	}
	request, _ := json.Marshal(input)
	requestSum := sha256.Sum256(request)
	requestDigest := hex.EncodeToString(requestSum[:])
	key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	sum := sha256.Sum256([]byte(input.Report))
	digest := hex.EncodeToString(sum[:])
	for attempt := 0; attempt < 3; attempt++ {
		current, err := h.oss.GetObject(r.Context(), key)
		var task map[string]any
		var raw map[string]json.RawMessage
		if err != nil || json.Unmarshal(current, &task) != nil || json.Unmarshal(current, &raw) != nil {
			writeError(w, 503, "main task snapshot unavailable")
			return
		}
		if coordinationString(task, "task_id") != id || coordinationString(task, "task_kind") != "main" {
			writeError(w, 409, "completion requires a coordinating main task")
			return
		}
		if !h.ownsMainTask(w, r, task) {
			return
		}
		if saved := raw["task_completion"]; len(saved) > 0 {
			var receipt taskCompletion
			if json.Unmarshal(saved, &receipt) != nil {
				writeError(w, 503, "completion receipt unavailable")
				return
			}
			if receipt.ReportSHA256 != digest || receipt.RequestDigest != requestDigest {
				writeError(w, 409, "completed task report is immutable")
				return
			}
			h.writeTaskCompletion(w, r, key, id, team, task)
			return
		}
		if !coordinationReady(task) {
			writeError(w, 409, "every execution subtask must be accepted before completion")
			return
		}
		if !h.taskExecutionAdmission(w, r, key, task) {
			return
		}
		available := map[string]taskFinalArtifact{}
		nodes, _ := task["tasks"].([]any)
		for _, item := range nodes {
			node, _ := item.(map[string]any)
			childID := coordinationString(node, "task_id")
			if !isSafeTaskID(childID) || coordinationString(node, "status") != "completed" {
				writeError(w, 409, "every task node must be accepted before completion")
				return
			}
			childKey := path.Join(path.Dir(path.Dir(key)), childID, "meta.json")
			data, err := h.oss.GetObject(r.Context(), childKey)
			var child map[string]any
			var childRaw map[string]json.RawMessage
			if err != nil || json.Unmarshal(data, &child) != nil || json.Unmarshal(data, &childRaw) != nil || coordinationString(child, "task_id") != childID || coordinationString(child, "main_task_id") != id || coordinationString(child, "project_id") != coordinationString(task, "project_id") || coordinationString(child, "status") != "completed" || coordinationString(child, "assigned_to") != coordinationString(node, "assigned_to") {
				writeError(w, 409, "accepted child snapshot unavailable or changed")
				return
			}
			if len(childRaw["task_submission"]) == 0 {
				writeError(w, 409, "child requires a registered immutable submission")
				return
			}
			files, err := h.acceptedTaskFileResources(r, childKey, childID, childRaw)
			if err != nil {
				writeError(w, 409, err.Error())
				return
			}
			var index taskFileIndex
			_ = json.Unmarshal(childRaw["task_files"], &index)
			for _, file := range files {
				available[file.ArtifactID] = taskFinalArtifact{TaskID: childID, publishedTaskFile: index.Artifacts[file.ArtifactID]}
				if err := h.verifyTaskFileContent(r.Context(), childKey, index.Artifacts[file.ArtifactID]); err != nil {
					writeError(w, 409, "accepted child artifact unavailable or changed")
					return
				}
			}
			reportPath := coordinationString(child, "result_path")
			prefix := "shared/tasks/" + childID + "/artifacts/"
			reportDigest := strings.TrimPrefix(reportPath, prefix)
			if reportPath == reportDigest || !validArtifactDigest(reportDigest) {
				writeError(w, 409, "accepted child report is not registered")
				return
			}
			report, err := h.oss.GetObject(r.Context(), path.Dir(childKey)+"/artifacts/"+reportDigest)
			actual := sha256.Sum256(report)
			if err != nil || len(report) == 0 || hex.EncodeToString(actual[:]) != reportDigest {
				writeError(w, 409, "accepted child report unavailable or changed")
				return
			}
		}
		finalFiles := []taskFinalArtifact{}
		if len(input.Artifacts) == 0 {
			for _, file := range available {
				finalFiles = append(finalFiles, file)
			}
		} else {
			for _, ref := range input.Artifacts {
				file, exists := available[ref]
				if !exists {
					writeError(w, 404, "final artifact is not part of an accepted submission of this main task")
					return
				}
				finalFiles = append(finalFiles, file)
			}
		}
		sort.Slice(finalFiles, func(i, j int) bool {
			if finalFiles[i].TaskID != finalFiles[j].TaskID {
				return finalFiles[i].TaskID < finalFiles[j].TaskID
			}
			return finalFiles[i].Name < finalFiles[j].Name
		})
		for _, file := range finalFiles {
			data, err := h.oss.GetObject(r.Context(), path.Join(path.Dir(path.Dir(key)), file.TaskID, "artifacts", file.SHA256))
			if err != nil {
				writeError(w, 503, "final artifact publication check unavailable")
				return
			}
			if artifactSensitive("shared/tasks/"+file.TaskID+"/"+file.Name, artifactMediaType(file.Name), data) {
				writeError(w, 409, "final artifact did not pass publication checks; select appropriate outputs before completing")
				return
			}
		}
		strict, canCommit := h.oss.(conditionalWriter)
		create, canCreate := h.oss.(createIfAbsentWriter)
		if !canCommit || !canCreate {
			writeError(w, 503, "conditional task storage required")
			return
		}
		blob := path.Dir(key) + "/artifacts/" + digest
		if err := create.PutObjectIfNoneMatchStrict(r.Context(), blob, []byte(input.Report)); err != nil {
			if !errors.Is(err, oss.ErrPreconditionFailed) {
				writeError(w, 503, "final report upload unavailable")
				return
			}
			saved, readErr := h.oss.GetObject(r.Context(), blob)
			if readErr != nil || sha256.Sum256(saved) != sum {
				writeError(w, 503, "final report verification failed")
				return
			}
		}
		identity, _ := json.Marshal([]string{team, id, "result.md", "output", digest})
		hash := sha256.Sum256(identity)
		artifactID := hex.EncodeToString(hash[:])
		index := taskFileIndex{Artifacts: map[string]publishedTaskFile{}, Heads: map[string]string{}}
		if len(raw["task_files"]) > 0 && (json.Unmarshal(raw["task_files"], &index) != nil || index.Artifacts == nil || index.Heads == nil) {
			writeError(w, 503, "invalid main task file registry")
			return
		}
		index.Artifacts[artifactID] = publishedTaskFile{ArtifactID: artifactID, Name: "result.md", Kind: "output", Version: digest, SHA256: digest, Size: int64(len(input.Report))}
		index.Heads["result.md"] = artifactID
		task["task_files"] = index
		completionIdentity, _ := json.Marshal([]string{team, id, requestDigest})
		completionHash := sha256.Sum256(completionIdentity)
		task["task_completion"] = taskCompletion{ID: hex.EncodeToString(completionHash[:]), ReportSHA256: digest, RequestDigest: requestDigest, Artifacts: finalFiles}
		task["status"], task["summary"], task["result_status"] = "completed", input.Report, "SUCCESS"
		task["result_path"] = "shared/tasks/" + id + "/artifacts/" + digest
		task["deliverables"] = []string{"shared/tasks/" + id + "/artifacts/" + digest}
		task["deliverable_refs"] = []string{artifactID}
		task["completed_at"] = time.Now().UTC().Format(time.RFC3339)
		caller := authpkg.CallerFromContext(r.Context())
		event := coordinationEvent{Kind: "main_task_completed", MainTaskID: id, ProjectID: coordinationString(task, "project_id")}
		intents := map[string]taskNotificationIntent{}
		if len(raw["task_notifications"]) > 0 && json.Unmarshal(raw["task_notifications"], &intents) != nil {
			writeError(w, 503, "task notification intents unavailable")
			return
		}
		intents["completed"] = taskNotificationIntent{Event: event, ActorRole: caller.Role, ActorName: caller.Username, Team: caller.Team, Status: "pending"}
		task["task_notifications"] = intents
		var previousStatus string
		_ = json.Unmarshal(raw["status"], &previousStatus)
		appendTaskOperationHistory(task, previousStatus, "completed", "complete_task", authzActor(caller))
		next, err := json.Marshal(task)
		if err != nil || len(next) > taskMetaCommitMaxBytes {
			writeError(w, 413, "completion exceeds task record limit")
			return
		}
		if err := strict.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(current)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			writeError(w, 503, "completion commit uncertain; retry unchanged report")
			return
		}
		h.writeTaskCompletion(w, r, key, id, team, task)
		return
	}
	writeError(w, 409, "main task changed during completion; retry unchanged report")
}

func (h *ProjectHandler) writeTaskCompletion(w http.ResponseWriter, r *http.Request, key, id, team string, task map[string]any) {
	notification := h.flushTaskNotification(r.Context(), key, "completed")
	httputil.WriteJSON(w, 200, struct {
		Task         taskResource           `json:"task"`
		Notification taskNotificationStatus `json:"notification"`
		NextAction   string                 `json:"nextAction"`
	}{publicTaskResource(id, team, task), notification, "Task system has taken responsibility for returning the saved final report to the original conversation. Do not send another completion message."})
}

func validateTaskCompletionCommit(w http.ResponseWriter, current, next []byte) bool {
	before, had := rawJSONField(current, "task_completion")
	after, has := rawJSONField(next, "task_completion")
	if had != has || (had && !sameJSONValue(before, after)) {
		writeError(w, 403, "task completion is server-owned; use the task complete API")
		return false
	}
	if had {
		for _, field := range []string{"status", "summary", "result_status", "result_path", "deliverables", "deliverable_refs", "completed_at", "reply_route", "requester_reply_route", "source_room_id", "requester", "team_id", "assigned_to", "tasks", "room_id", "project_id"} {
			a, hasA := rawJSONField(current, field)
			b, hasB := rawJSONField(next, field)
			if hasA != hasB || (hasA && !sameJSONValue(a, b)) {
				writeError(w, 403, "completed task projections and original reply route are immutable")
				return false
			}
		}
	}
	return true
}
