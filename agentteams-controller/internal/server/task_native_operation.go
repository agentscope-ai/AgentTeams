package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// A separate framework ledger fences ambiguous canonical CLI calls without
// blocking their own Task transactions with a generic started operation.
type canonicalNativeOperation struct {
	Generation    uint64         `json:"generation,omitempty"`
	Action        string         `json:"action"`
	SourceEventID string         `json:"source_event_id,omitempty"`
	SubmissionID  string         `json:"submission_id,omitempty"`
	ID            string         `json:"id"`
	Intent        string         `json:"intent"`
	State         string         `json:"state"`
	TaskKey       string         `json:"task_key,omitempty"`
	TaskID        string         `json:"task_id,omitempty"`
	Team          string         `json:"team,omitempty"`
	Result        map[string]any `json:"result,omitempty"`
}
type canonicalNativeRequest struct {
	Mode                string            `json:"mode"`
	Arguments           map[string]any    `json:"arguments"`
	ContentFingerprints map[string]string `json:"contentFingerprints,omitempty"`
	OperationID         string            `json:"operationId,omitempty"`
	Result              map[string]any    `json:"result,omitempty"`
}

func canonicalNativeHash(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (h *ProjectHandler) nativeOperationScope(w http.ResponseWriter, r *http.Request, input canonicalNativeRequest) (string, canonicalNativeOperation, bool) {
	caller := authpkg.CallerFromContext(r.Context())
	scope := canonicalNativeOperation{Action: coordinationString(input.Arguments, "action"), SubmissionID: coordinationString(input.Arguments, "submissionId")}
	if caller == nil || (caller.Role != authpkg.RoleManager && caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleWorker) || (caller.ServiceAccountNamespace != "" && caller.ServiceAccountNamespace != h.namespace) {
		writeError(w, 403, "registered Task actor required")
		return "", scope, false
	}
	actor := []string{h.namespace, caller.Role, caller.Username}
	if caller.Role != authpkg.RoleManager {
		if h.coordination == nil {
			writeError(w, 503, "registered Task actor unavailable")
			return "", scope, false
		}
		worker, err := h.coordination.worker(r.Context(), caller.Username)
		if err != nil {
			writeError(w, 403, "registered Task actor required")
			return "", scope, false
		}
		actor[1], actor[2] = "worker", worker.Name
		state, _, err := (&service.WorkerMatrixStore{Storage: h.oss}).Load(r.Context(), worker.Spec.EffectiveWorkerName(worker.Name))
		process := r.Header.Get("X-AgentTeams-Process-ID")
		if err != nil {
			writeError(w, 503, "registered native start unavailable")
			return "", scope, false
		}
		if process == "" || state.ActiveStart.ProcessID != process || state.ActiveStart.InstanceID == "" {
			writeError(w, 409, "native process was replaced")
			return "", scope, false
		}

	}
	if coordinationString(input.Arguments, "action") == "create" {
		source := coordinationMap(input.Arguments, "_nativeSource")
		event, room, user := coordinationString(source, "eventId"), coordinationString(source, "roomId"), coordinationString(source, "userId")
		if caller.Role != authpkg.RoleManager || !strings.HasPrefix(event, "$") || !strings.HasPrefix(room, "!") || !strings.HasPrefix(user, "@") || h.coordination == nil {
			writeError(w, 403, "verified original request required")
			return "", scope, false
		}
		manager, err := h.coordination.manager(r.Context(), caller.Username)
		if err != nil {
			writeError(w, 403, "registered Manager required")
			return "", scope, false
		}
		if _, status := h.coordination.taskSourceRequest(r.Context(), manager.Name, room, event, user); status != 0 {
			writeError(w, status, "original request authority unavailable")
			return "", scope, false
		}
		actor[2] = manager.Name
		// Source scope deliberately excludes Leader/arguments: changing the model's
		// proposed plan or dispatch target cannot evade an unresolved source call.
		actor = append(actor, "source", room, user, event)
		scope.SourceEventID = event
		if input.Mode == "begin" {
			resolved, err := h.coordination.resolve(r.Context(), coordinationEvent{Kind: "handoff", Recipient: coordinationString(input.Arguments, "leader"), OriginEventID: event, RoomID: room, SourceRoute: map[string]any{"target_session": room, "target_user": user}}, caller)
			if err != nil {
				writeError(w, 403, "registered Leader required")
				return "", scope, false
			}
			identity, _ := json.Marshal([]string{resolved.ActorName, resolved.StorageTeam, resolved.RecipientName, event})
			sum := sha256.Sum256(identity)
			scope.TaskID = "task-" + hex.EncodeToString(sum[:16])
			scope.Team = resolved.StorageTeam
			scope.TaskKey = path.Join("teams", scope.Team, "shared/tasks", scope.TaskID, "meta.json")
		}
	} else {
		id := coordinationString(input.Arguments, "taskId")
		if !isSafeTaskID(id) {
			writeError(w, 400, "valid task identity required")
			return "", scope, false
		}
		if team := coordinationString(input.Arguments, "team"); team != "" {
			query := r.URL.Query()
			query.Set("team", team)
			r.URL.RawQuery = query.Encode()
		}
		key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
		if !ok {
			return "", scope, false
		}
		scope.TaskID, scope.Team, scope.TaskKey = id, team, key
		raw, err := h.oss.GetObject(r.Context(), key)
		var task struct {
			Execution struct {
				Generation uint64 `json:"generation"`
			} `json:"execution"`
		}
		if err != nil || json.Unmarshal(raw, &task) != nil {
			writeError(w, 503, "Task execution authority unavailable")
			return "", scope, false
		}
		scope.Generation = task.Execution.Generation
		actor = append(actor, "task", team, id)
	}
	return "control/task-native/" + canonicalNativeHash(actor) + ".json", scope, true
}

func (h *ProjectHandler) nativeOperationAuthority(r *http.Request, operation canonicalNativeOperation) (map[string]any, error) {
	if operation.TaskKey == "" {
		return map[string]any{"task": nil}, nil
	}
	data, err := h.oss.GetObject(r.Context(), operation.TaskKey)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"task": nil}, nil
	}
	var record map[string]any
	if err != nil || json.Unmarshal(data, &record) != nil {
		return nil, errors.New("Task authority unavailable")
	}
	return map[string]any{"task": publicTaskResource(operation.TaskID, operation.Team, record)}, nil
}

func (h *ProjectHandler) NativeTaskOperation(w http.ResponseWriter, r *http.Request) {
	var input canonicalNativeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Arguments == nil || (input.Mode != "begin" && input.Mode != "finish" && input.Mode != "unknown" && input.Mode != "admit") {
		writeError(w, 400, "valid native operation required")
		return
	}
	if input.Mode != "admit" {
		switch coordinationString(input.Arguments, "action") {
		case "create", "plan", "begin", "submit", "accept", "reject", "complete", "cancel", "files_put":
		default:
			writeError(w, 400, "canonical mutation required")
			return
		}
	}
	key, scope, ok := h.nativeOperationScope(w, r, input)
	if !ok {
		return
	}
	store, ok := h.oss.(interface {
		conditionalWriter
		createIfAbsentWriter
	})
	if !ok {
		writeError(w, 503, "conditional native operation storage required")
		return
	}
	data, err := h.oss.GetObject(r.Context(), key)
	absent := errors.Is(err, os.ErrNotExist)
	if err != nil && !absent {
		writeError(w, 503, "native operation ledger unavailable")
		return
	}
	var saved canonicalNativeOperation
	if !absent && (json.Unmarshal(data, &saved) != nil || saved.ID == "") {
		writeError(w, 503, "invalid native operation ledger")
		return
	}
	intent := canonicalNativeHash([]any{input.Arguments, input.ContentFingerprints, scope.Generation})
	if input.Mode == "admit" || input.Mode == "begin" {
		if !absent && saved.State != "verified" {
			authority, err := h.nativeOperationAuthority(r, saved)
			if err != nil {
				writeError(w, 503, "Task authority unavailable; keep verification fence")
				return
			}
			httputil.WriteJSON(w, 200, map[string]any{"mode": "verify", "operationId": saved.ID, "authority": authority})
			return
		}
		if input.Mode == "admit" {
			if saved.Generation != scope.Generation {
				saved.Result = nil
			}
			httputil.WriteJSON(w, 200, map[string]any{"mode": "admitted", "result": saved.Result, "action": saved.Action, "sourceEventId": saved.SourceEventID, "submissionId": saved.SubmissionID})
			return
		}
		if !absent && saved.Intent == intent && saved.Result != nil {
			httputil.WriteJSON(w, 200, map[string]any{"mode": "recovered", "operationId": saved.ID, "result": saved.Result})
			return
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			writeError(w, 503, "native operation identity unavailable")
			return
		}
		scope.ID, scope.Intent, scope.State = hex.EncodeToString(id[:]), intent, "started"
		next, _ := json.Marshal(scope)
		if absent {
			err = store.PutObjectIfNoneMatchStrict(r.Context(), key, next)
		} else {
			err = store.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(data))
		}
		if err != nil {
			writeError(w, 409, "native operation changed; reread before execution")
			return
		}
		httputil.WriteJSON(w, 200, map[string]any{"mode": "execute", "operationId": scope.ID})
		return
	}
	if absent || input.OperationID != saved.ID || intent != saved.Intent {
		writeError(w, 409, "native operation ownership changed")
		return
	}
	if input.Mode == "unknown" {
		// A lost finish response must not discard a receipt already durably saved.
		if saved.State != "verified" {
			saved.State = "unknown"
		}
	} else {
		task := coordinationMap(input.Result, "task")
		if input.Result == nil || input.Result["ok"] == false {
			writeError(w, 409, "verified canonical receipt required")
			return
		}
		authority, err := h.nativeOperationAuthority(r, saved)
		if err != nil {
			writeError(w, 503, "Task receipt authority unavailable")
			return
		}
		encoded, _ := json.Marshal(authority["task"])
		var actual map[string]any
		_ = json.Unmarshal(encoded, &actual)
		if actual == nil {
			writeError(w, 409, "Task receipt not durably present")
			return
		}
		if saved.Action == "files_put" {
			raw, err := h.oss.GetObject(r.Context(), saved.TaskKey)
			var record map[string]json.RawMessage
			if err != nil || json.Unmarshal(raw, &record) != nil {
				writeError(w, 503, "artifact authority unavailable")
				return
			}
			files, err := h.taskFileResources(r.Context(), saved.TaskKey, saved.TaskID, record)
			matched := false
			for _, file := range files {
				if file.ArtifactID != "" && file.ArtifactID == coordinationString(input.Result, "artifactId") && file.Version == coordinationString(input.Result, "version") {
					matched = true
				}
			}
			if err != nil || !matched {
				writeError(w, 409, "artifact receipt not durably present")
				return
			}
		} else {
			if coordinationString(task, "id") != saved.TaskID {
				writeError(w, 409, "Task receipt identity changed")
				return
			}
			field := map[string]string{"plan": "planId", "submit": "submissionId", "complete": "completionId", "begin": "assignmentId"}[saved.Action]
			if field != "" && (coordinationString(task, field) == "" || coordinationString(task, field) != coordinationString(actual, field)) {
				writeError(w, 409, "canonical receipt identity changed")
				return
			}
			if saved.Action == "accept" || saved.Action == "reject" {
				review := coordinationMap(input.Result, "review")
				current := coordinationMap(actual, "review")
				if coordinationString(review, "id") == "" || coordinationString(review, "id") != coordinationString(current, "id") || coordinationString(review, "submissionId") != saved.SubmissionID {
					writeError(w, 409, "candidate decision authority changed")
					return
				}
			}
			if saved.Action == "cancel" && coordinationString(actual, "status") != "cancelled" {
				writeError(w, 409, "cancellation is not durable")
				return
			}
			if saved.Action == "create" && coordinationString(actual, "originEventId") != saved.SourceEventID {
				writeError(w, 409, "original source authority changed")
				return
			}
		}
		saved.State, saved.Result = "verified", input.Result
	}
	next, _ := json.Marshal(saved)
	if err := store.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(data)); err != nil {
		writeError(w, 409, "native operation changed; retain verification fence")
		return
	}
	httputil.WriteJSON(w, 200, map[string]any{"mode": saved.State, "operationId": saved.ID})
}
