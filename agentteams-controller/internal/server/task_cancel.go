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
	"os"
	"path"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type mainTaskCancellation struct {
	ID          string `json:"id"`
	Reason      string `json:"reason"`
	Actor       string `json:"actor"`
	CancelledAt string `json:"cancelledAt"`
}

// CancelMainTask commits the main-task fence before cancelling unfinished
// executions. Retrying, or the recovery scan, completes a partial projection.
func (h *ProjectHandler) CancelMainTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isSafeTaskID(id) {
		writeError(w, 400, "invalid main task id")
		return
	}
	var input struct {
		Reason string `json:"reason"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, taskMetaCommitMaxBytes))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF || strings.TrimSpace(input.Reason) == "" {
		writeError(w, 400, "cancellation reason required")
		return
	}
	input.Reason = strings.TrimSpace(input.Reason)
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || classifyCommitActor(caller) != actorDecider {
		writeError(w, 403, "task decision authority required")
		return
	}
	key, team, ok := h.resolveTaskMetaCommitKey(w, r, id)
	if !ok {
		return
	}
	strict, ok := h.oss.(conditionalWriter)
	if !ok {
		writeError(w, 503, "conditional task storage required")
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		body, err := h.oss.GetObject(r.Context(), key)
		var main map[string]any
		if err != nil || json.Unmarshal(body, &main) != nil {
			writeError(w, 503, "main task snapshot unavailable")
			return
		}
		if main["task_id"] != id || main["task_kind"] != "main" {
			writeError(w, 409, "cancel requires a coordinating main task")
			return
		}
		if caller.Role == authpkg.RoleTeamLeader && !h.ownsMainTask(w, r, main) {
			return
		}
		var cancellation mainTaskCancellation
		if saved := main["task_cancellation"]; saved != nil {
			encoded, _ := json.Marshal(saved)
			if json.Unmarshal(encoded, &cancellation) != nil || cancellation.ID == "" || main["status"] != "cancelled" {
				writeError(w, 503, "cancellation receipt unavailable")
				return
			}
			if cancellation.Reason != input.Reason {
				writeError(w, 409, "saved cancellation reason is immutable")
				return
			}
		} else {
			status := coordinationString(main, "status")
			if taskStatusIsTerminal(status) || main["task_completion"] != nil {
				writeError(w, 409, "cannot cancel a terminal main task")
				return
			}
			identity, _ := json.Marshal([]string{team, id, authzActor(caller), input.Reason})
			sum := sha256.Sum256(identity)
			cancellation = mainTaskCancellation{ID: hex.EncodeToString(sum[:]), Reason: input.Reason, Actor: authzActor(caller), CancelledAt: time.Now().UTC().Format(time.RFC3339)}
			main["task_cancellation"], main["status"] = cancellation, "cancelled"
			main["cancel_reason"], main["cancelled_at"] = cancellation.Reason, cancellation.CancelledAt
			appendTaskOperationHistory(main, status, "cancelled", "cancel_task", cancellation.Actor)
			next, err := json.Marshal(main)
			if err != nil || len(next) > taskMetaCommitMaxBytes {
				writeError(w, 413, "cancellation exceeds task record limit")
				return
			}
			if err := strict.PutObjectIfMatchStrict(r.Context(), key, next, contentETag(body)); err != nil {
				if errors.Is(err, oss.ErrPreconditionFailed) {
					continue
				}
				writeError(w, 503, "cancellation commit uncertain; retry unchanged reason")
				return
			}
		}
		projection := "applied"
		if err := h.reconcileTaskCancellation(r.Context(), key); err != nil {
			projection = "pending"
		}
		latest, err := h.oss.GetObject(r.Context(), key)
		if err != nil || json.Unmarshal(latest, &main) != nil {
			writeError(w, 503, "cancellation saved; retry unchanged reason for its receipt")
			return
		}
		httputil.WriteJSON(w, 200, struct {
			Task         taskResource         `json:"task"`
			Cancellation mainTaskCancellation `json:"cancellation"`
			Projection   string               `json:"projection"`
			NextAction   string               `json:"nextAction"`
		}{publicTaskResource(id, team, main), cancellation, projection, "Task system fenced this main task and is cancelling unfinished executions. Saved results and accepted artifacts are retained. Pending synchronization is retried; this receipt does not prove running tools have already stopped."})
		return
	}
	writeError(w, 409, "main task changed during cancellation; retry unchanged reason")
}

func (h *ProjectHandler) reconcileTaskCancellation(ctx context.Context, key string) error {
	strict, ok := h.oss.(conditionalWriter)
	if !ok {
		return fmt.Errorf("conditional task storage required")
	}
	for attempt := 0; attempt < 3; attempt++ {
		body, err := h.oss.GetObject(ctx, key)
		var main map[string]any
		if err != nil || json.Unmarshal(body, &main) != nil {
			return fmt.Errorf("main cancellation unavailable")
		}
		encoded, _ := json.Marshal(main["task_cancellation"])
		var cancellation mainTaskCancellation
		if json.Unmarshal(encoded, &cancellation) != nil || cancellation.ID == "" || main["status"] != "cancelled" || main["task_kind"] != "main" {
			return fmt.Errorf("invalid main cancellation")
		}
		if main["task_cancellation_applied"] == true {
			return nil
		}
		nodes, _ := main["tasks"].([]any)
		for _, item := range nodes {
			node, ok := item.(map[string]any)
			id := coordinationString(node, "task_id")
			if !ok || !isSafeTaskID(id) {
				return fmt.Errorf("invalid cancellation node")
			}
			childKey := path.Join(path.Dir(path.Dir(key)), id, "meta.json")
			settled := false
			for retry := 0; retry < 3; retry++ {
				data, err := h.oss.GetObject(ctx, childKey)
				if errors.Is(err, os.ErrNotExist) && (node["status"] == "planned" || node["status"] == "cancelled") {
					node["status"], settled = "cancelled", true
					break
				}
				var child map[string]any
				if err != nil || json.Unmarshal(data, &child) != nil || child["task_id"] != id || child["main_task_id"] != main["task_id"] || child["task_kind"] != "execution" || coordinationString(child, "project_id") != coordinationString(main, "project_id") || child["assigned_to"] != node["assigned_to"] {
					return fmt.Errorf("cancellation child unavailable or changed")
				}
				if planID := coordinationString(coordinationMap(main, "task_plan"), "id"); planID != "" && child["task_plan_id"] != planID {
					return fmt.Errorf("cancellation child belongs to another plan")
				}
				status := coordinationString(child, "status")
				if !taskStatusIsTerminal(status) {
					child["status"], child["cancel_reason"], child["cancelled_at"] = "cancelled", cancellation.Reason, cancellation.CancelledAt
					appendTaskOperationHistory(child, status, "cancelled", "cancel_task", cancellation.Actor)
					next, err := json.Marshal(child)
					if err != nil || len(next) > taskMetaCommitMaxBytes {
						return fmt.Errorf("cancellation child exceeds record limit")
					}
					if err := strict.PutObjectIfMatchStrict(ctx, childKey, next, contentETag(data)); err != nil {
						if errors.Is(err, oss.ErrPreconditionFailed) {
							continue
						}
						return err
					}
				}
				node["status"] = child["status"]
				if review := coordinationMap(child, "task_review"); review["id"] != nil {
					node["review_id"], node["submission_id"] = review["id"], review["submissionId"]
				}
				settled = true
				break
			}
			if !settled {
				return fmt.Errorf("cancellation child changed concurrently")
			}
		}
		main["task_cancellation_applied"] = true
		next, err := json.Marshal(main)
		if err != nil || len(next) > taskMetaCommitMaxBytes {
			return fmt.Errorf("cancellation projection exceeds record limit")
		}
		if string(next) == string(body) {
			return nil
		}
		if err := strict.PutObjectIfMatchStrict(ctx, key, next, contentETag(body)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("cancellation projection changed concurrently")
}

func validateTaskCancellationCommit(w http.ResponseWriter, current, next []byte) bool {
	for _, field := range []string{"task_cancellation", "task_cancellation_applied"} {
		a, had := rawJSONField(current, field)
		b, has := rawJSONField(next, field)
		if had != has || (had && !sameJSONValue(a, b)) {
			writeError(w, 403, "main cancellation receipts are server-owned; use task cancel")
			return false
		}
	}
	return true
}
