package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

// Execution is part of the task CAS, never the notification outbox lease.
// A replacement can take over only after the backend reports another start.
type taskExecution struct {
	Generation uint64          `json:"generation"`
	InstanceID string          `json:"instance_id"`
	ProcessID  string          `json:"process_id"`
	Runtime    string          `json:"runtime"`
	State      string          `json:"state"`
	Checkpoint *taskCheckpoint `json:"checkpoint,omitempty"`
}

type taskArtifact struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type taskOperation struct {
	ID       string `json:"id"`
	State    string `json:"state"` // started, unknown, verified
	Evidence string `json:"evidence,omitempty"`
}

type taskCheckpoint struct {
	Version        uint64         `json:"version"`
	CompletedSteps []string       `json:"completed_steps"`
	RemainingSteps []string       `json:"remaining_steps"`
	Operation      *taskOperation `json:"operation,omitempty"`
	Artifacts      []taskArtifact `json:"artifacts"`
	Result         *taskArtifact  `json:"result,omitempty"`
	Delivery       string         `json:"delivery,omitempty"` // pending, delivered
	ReplyTxn       string         `json:"reply_txn,omitempty"`
	ReplyEvent     string         `json:"reply_event,omitempty"`
	SavedAt        string         `json:"saved_at"`
}

type TaskRecoveryHandler struct {
	project  *ProjectHandler
	deps     ServerDeps
	instance func(context.Context, *authpkg.CallerIdentity) (string, string, error)
}

func NewTaskRecoveryHandler(project *ProjectHandler, deps ServerDeps) *TaskRecoveryHandler {
	h := &TaskRecoveryHandler{project: project, deps: deps}
	h.instance = h.currentInstance
	return h
}

func recoveryCaller(caller *authpkg.CallerIdentity) bool {
	return caller != nil && (caller.Role == authpkg.RoleWorker || caller.Role == authpkg.RoleTeamLeader) && caller.Team != ""
}

func (h *TaskRecoveryHandler) currentInstance(ctx context.Context, caller *authpkg.CallerIdentity) (string, string, error) {
	if h.deps.Backend == nil || !recoveryCaller(caller) {
		return "", "", fmt.Errorf("managed worker backend required")
	}
	w, err := NewCoordinationHandler(h.deps).worker(ctx, caller.Username)
	if err != nil {
		return "", "", err
	}
	recorded := w.Status.BackendRuntime
	if recorded == "" {
		recorded = w.Spec.GetBackendRuntime()
	}
	var b backend.WorkerBackend
	if recorded == "" {
		b = h.deps.Backend.DetectWorkerBackend(ctx)
	} else if recorded == "pod" {
		b, err = h.deps.Backend.GetWorkerBackend(ctx, "k8s")
		if err != nil {
			b, err = h.deps.Backend.GetWorkerBackend(ctx, "docker")
		}
	} else {
		b, err = h.deps.Backend.GetBackendForType(ctx, recorded)
	}
	if err != nil {
		return "", "", err
	}
	if b == nil {
		return "", "", fmt.Errorf("worker backend unavailable")
	}
	status, err := b.Status(ctx, w.Name)
	if err != nil {
		return "", "", err
	}
	if status == nil || status.InstanceID == "" || (status.Status != backend.StatusRunning && status.Status != backend.StatusStarting && status.Status != backend.StatusReady) {
		return "", "", fmt.Errorf("current worker start cannot be verified")
	}
	return b.Name() + "/" + status.InstanceID, backend.ResolveRuntime(w.Spec.Runtime, h.deps.DefaultWorkerRuntime), nil
}

func (h *TaskRecoveryHandler) owns(ctx context.Context, caller *authpkg.CallerIdentity, task map[string]json.RawMessage) bool {
	var assigned string
	_ = json.Unmarshal(task["assigned_to"], &assigned)
	for _, name := range []string{caller.Username, caller.WorkerName, caller.MatrixUserID} {
		if name != "" && assigned == name {
			return true
		}
	}
	if h.deps.Client != nil {
		w, err := NewCoordinationHandler(h.deps).worker(ctx, caller.Username)
		return err == nil && (assigned == w.Name || assigned == w.Spec.EffectiveWorkerName(w.Name) || (w.Status.MatrixUserID != "" && assigned == w.Status.MatrixUserID))
	}
	return false
}

func recoveryStatus(task map[string]json.RawMessage) bool {
	var status string
	_ = json.Unmarshal(task["status"], &status)
	if status == "assigned" || status == "in_progress" {
		return true
	}
	var execution taskExecution
	return status == "submitted" && json.Unmarshal(task["execution"], &execution) == nil && execution.Generation > 0
}

// ListRecoverableTasks grants only self discovery; Project list stays closed.
// Namespace is fixed by this Controller and teams come from the bearer identity.
func (h *TaskRecoveryHandler) ListRecoverableTasks(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || (caller.Role != authpkg.RoleWorker && caller.Role != authpkg.RoleTeamLeader) {
		writeError(w, 403, "team worker identity required")
		return
	}
	if caller.Team == "" {
		httputil.WriteJSON(w, 200, map[string]any{"tasks": []any{}})
		return
	}
	instance, runtime, err := h.instance(r.Context(), caller)
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	prefixes, aliases, err := h.project.taskMetaPrefixes(r.Context())
	if err != nil {
		writeError(w, 503, "task scopes unavailable")
		return
	}
	tasks := []map[string]any{}
	for _, prefix := range prefixes {
		team := candidateTeam(prefix)
		if !callerMatchesStorageTeam(caller, team, aliases) {
			continue
		}
		children, err := h.project.oss.ListObjects(r.Context(), prefix)
		if err != nil {
			writeError(w, 503, "task discovery unavailable")
			return
		}
		for _, child := range children {
			key, ok := metaKeyFromListResult(prefix, child)
			if !ok {
				continue
			}
			data, err := h.project.oss.GetObject(r.Context(), key)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			var task map[string]json.RawMessage
			if err != nil || json.Unmarshal(data, &task) != nil {
				writeError(w, 503, "task snapshot unavailable")
				return
			}
			var taskID string
			_ = json.Unmarshal(task["task_id"], &taskID)
			if taskID != path.Base(path.Dir(key)) {
				writeError(w, 503, "task identity does not match storage scope")
				return
			}
			if !recoveryStatus(task) || !h.owns(r.Context(), caller, task) {
				continue
			}
			tasks = append(tasks, map[string]any{"team_id": team, "task": task, "etag": contentETag(data)})
		}
	}
	sort.Slice(tasks, func(i, j int) bool {
		a, _ := json.Marshal(tasks[i])
		b, _ := json.Marshal(tasks[j])
		return string(a) < string(b)
	})
	httputil.WriteJSON(w, 200, map[string]any{"tasks": tasks, "instance_id": instance, "runtime": runtime})
}

func unresolvedOperation(cp *taskCheckpoint) bool {
	return cp != nil && cp.Operation != nil && cp.Operation.State != "verified"
}

func (h *TaskRecoveryHandler) validateCheckpoint(ctx context.Context, key string, old, cp *taskCheckpoint) error {
	version := uint64(1)
	if old != nil {
		version = old.Version + 1
	}
	if cp == nil || cp.Version != version {
		return fmt.Errorf("checkpoint version must advance from the authoritative snapshot")
	}
	if unresolvedOperation(old) && (cp.Operation == nil || cp.Operation.ID != old.Operation.ID || cp.Operation.State != "verified" || strings.TrimSpace(cp.Operation.Evidence) == "") {
		return fmt.Errorf("verify the previous operation before continuing")
	}
	if op := cp.Operation; op != nil && (op.ID == "" || (op.State != "started" && op.State != "unknown" && op.State != "verified") || (op.State == "verified" && strings.TrimSpace(op.Evidence) == "")) {
		return fmt.Errorf("operation requires an identity, state and verification evidence")
	}
	completed := map[string]bool{}
	for _, step := range cp.CompletedSteps {
		if step == "" || completed[step] {
			return fmt.Errorf("completed steps must have unique identities")
		}
		completed[step] = true
	}
	if old != nil {
		for _, step := range old.CompletedSteps {
			if !completed[step] {
				return fmt.Errorf("a checkpoint cannot forget completed steps")
			}
		}
	}
	for _, step := range cp.RemainingSteps {
		if step == "" || completed[step] {
			return fmt.Errorf("remaining steps overlap completed work")
		}
	}
	artifacts := append([]taskArtifact{}, cp.Artifacts...)
	if cp.Result != nil {
		artifacts = append(artifacts, *cp.Result)
	}
	for _, artifact := range artifacts {
		if artifact.Size < 0 || len(artifact.SHA256) != 64 || path.Clean(artifact.Key) != artifact.Key || !strings.HasPrefix(artifact.Key, path.Dir(key)+"/") || artifact.Key == key {
			return fmt.Errorf("artifact must reference this task's persisted files")
		}
		data, err := h.project.oss.GetObject(ctx, artifact.Key)
		if err != nil {
			return fmt.Errorf("artifact is not durably readable: %s", artifact.Key)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != artifact.Size || hex.EncodeToString(sum[:]) != artifact.SHA256 {
			return fmt.Errorf("artifact digest mismatch: %s", artifact.Key)
		}
	}
	if cp.Delivery != "" && cp.Delivery != "pending" && cp.Delivery != "delivered" {
		return fmt.Errorf("invalid delivery state")
	}
	if cp.Delivery != "" && (cp.Result == nil || cp.ReplyTxn == "" || unresolvedOperation(cp)) {
		return fmt.Errorf("delivery requires a saved result, stable transaction and verified operations")
	}
	if cp.Delivery == "delivered" && cp.ReplyEvent == "" {
		return fmt.Errorf("delivered result requires a Matrix event")
	}
	cp.SavedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return nil
}

// UpdateExecution changes only the execution record in the versioned task.
// Project is intentionally absent from the durable identity.
func (h *TaskRecoveryHandler) UpdateExecution(w http.ResponseWriter, r *http.Request) {
	if !coordinationSafeID.MatchString(r.PathValue("id")) {
		writeError(w, 400, "invalid task identity")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	if !recoveryCaller(caller) {
		writeError(w, 403, "team worker identity required")
		return
	}
	if r.Header.Get("If-Match") == "" {
		writeError(w, 428, "read the authoritative task ETag first")
		return
	}
	var input struct {
		Action     string          `json:"action"`
		InstanceID string          `json:"instance_id"`
		ProcessID  string          `json:"process_id"`
		Checkpoint *taskCheckpoint `json:"checkpoint"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.ProcessID == "" || len(input.ProcessID) > 256 {
		writeError(w, 400, "invalid execution request")
		return
	}
	key, _, ok := h.project.resolveTaskMetaCommitKey(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	data, err := h.project.oss.GetObject(r.Context(), key)
	var task map[string]json.RawMessage
	if err != nil || json.Unmarshal(data, &task) != nil {
		writeError(w, 503, "task snapshot unavailable")
		return
	}
	if r.Header.Get("If-Match") != contentETag(data) {
		writeMetaConflict(w)
		return
	}
	var taskID string
	_ = json.Unmarshal(task["task_id"], &taskID)
	if taskID != r.PathValue("id") {
		writeError(w, 409, "task identity changed")
		return
	}
	if !h.owns(r.Context(), caller, task) {
		writeError(w, 403, "task is not assigned to you")
		return
	}
	if !recoveryStatus(task) {
		writeError(w, 409, "task is not recoverable")
		return
	}
	instance, runtime, err := h.instance(r.Context(), caller)
	if err != nil {
		writeError(w, 503, err.Error())
		return
	}
	if input.InstanceID != instance {
		writeError(w, 409, "worker instance changed")
		return
	}
	var execution taskExecution
	if raw := task["execution"]; len(raw) > 0 && json.Unmarshal(raw, &execution) != nil {
		writeError(w, 409, "invalid authoritative execution record")
		return
	}
	sameProcess := execution.InstanceID == instance && execution.ProcessID == input.ProcessID
	spec, err := h.project.oss.GetObject(r.Context(), path.Dir(key)+"/spec.md")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, 503, "task description unavailable")
		return
	}
	var savedResult string
	if cp := execution.Checkpoint; cp != nil && cp.Result != nil && cp.Delivery == "pending" {
		result, err := h.project.oss.GetObject(r.Context(), cp.Result.Key)
		sum := sha256.Sum256(result)
		if err != nil || int64(len(result)) != cp.Result.Size || hex.EncodeToString(sum[:]) != cp.Result.SHA256 {
			writeError(w, 409, "saved result is unavailable or changed")
			return
		}
		savedResult = string(result)
	}
	switch input.Action {
	case "claim":
		if !sameProcess {
			if execution.InstanceID == instance {
				writeError(w, 409, "another process owns this live instance")
				return
			}
			execution.Generation++
			execution.InstanceID, execution.ProcessID, execution.Runtime = instance, input.ProcessID, runtime
		}
		execution.State = "active"
		if unresolvedOperation(execution.Checkpoint) {
			execution.State = "needs_verification"
		}
	case "checkpoint", "release":
		if !sameProcess || execution.State == "released" {
			writeError(w, 409, "execution ownership changed")
			return
		}
		if err := h.validateCheckpoint(r.Context(), key, execution.Checkpoint, input.Checkpoint); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		execution.Checkpoint = input.Checkpoint
		execution.State = "active"
		if unresolvedOperation(input.Checkpoint) {
			execution.State = "needs_verification"
		}
		if input.Action == "release" {
			if unresolvedOperation(input.Checkpoint) {
				writeError(w, 409, "cannot release an unverified operation")
				return
			}
			execution.State = "released"
		}
	default:
		writeError(w, 400, "unsupported execution action")
		return
	}
	task["execution"], _ = json.Marshal(execution)
	body, _ := json.Marshal(task)
	strict, ok := h.project.oss.(conditionalWriter)
	if !ok {
		writeError(w, 503, "strict conditional storage required")
		return
	}
	if err := strict.PutObjectIfMatchStrict(r.Context(), key, body, contentETag(data)); err != nil {
		if errors.Is(err, oss.ErrPreconditionFailed) {
			writeMetaConflict(w)
		} else {
			writeError(w, 503, "execution commit failed")
		}
		return
	}
	w.Header().Set("ETag", contentETag(body))
	httputil.WriteJSON(w, 200, map[string]any{"task": task, "execution": execution, "etag": contentETag(body), "spec": string(spec), "saved_result": savedResult})
}

func (h *ProjectHandler) validateExecutionCommit(w http.ResponseWriter, r *http.Request, current, body []byte) bool {
	old, had := rawJSONField(current, "execution")
	next, has := rawJSONField(body, "execution")
	if had != has || (had && !sameJSONValue(old, next)) {
		writeError(w, 403, "execution record is owned by the execution endpoint")
		return false
	}
	caller := authpkg.CallerFromContext(r.Context())
	if !had || caller == nil || caller.Role != authpkg.RoleWorker {
		return true
	}
	var execution taskExecution
	var task map[string]json.RawMessage
	if h.recovery == nil || json.Unmarshal(old, &execution) != nil || json.Unmarshal(current, &task) != nil || !h.recovery.owns(r.Context(), caller, task) {
		writeError(w, 403, "task execution ownership unavailable")
		return false
	}
	instance, _, err := h.recovery.instance(r.Context(), caller)
	if err != nil || instance != execution.InstanceID || execution.ProcessID != r.Header.Get("X-AgentTeams-Process-ID") || execution.State != "active" {
		writeError(w, 409, "execution changed or requires operation verification")
		return false
	}
	return true
}
