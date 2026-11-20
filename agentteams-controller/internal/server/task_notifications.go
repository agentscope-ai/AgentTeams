package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type taskNotificationIntent struct {
	Event     coordinationEvent `json:"event"`
	ActorRole string            `json:"actor_role"`
	ActorName string            `json:"actor_name"`
	Team      string            `json:"team"`
	Status    string            `json:"status"`
	EventID   string            `json:"event_id,omitempty"`
	Recipient string            `json:"recipient,omitempty"`
}

type taskNotificationStatus struct {
	Status    string `json:"status"`
	Recipient string `json:"recipient,omitempty"`
	EventID   string `json:"eventId,omitempty"`
	Error     string `json:"error,omitempty"`
}

// flushTaskNotification can be retried after either queue admission or the Task
// receipt update becomes uncertain. The existing outbox deduplicates admission.
func (h *ProjectHandler) flushTaskNotification(ctx context.Context, key, kind string) taskNotificationStatus {
	if h.coordination == nil {
		return taskNotificationStatus{Status: "pending", Error: "automatic notification is pending; the task system will retry"}
	}
	for attempt := 0; attempt < 3; attempt++ {
		body, err := h.oss.GetObject(ctx, key)
		var task map[string]json.RawMessage
		var intents map[string]taskNotificationIntent
		if err != nil || json.Unmarshal(body, &task) != nil || json.Unmarshal(task["task_notifications"], &intents) != nil {
			return taskNotificationStatus{Status: "pending", Error: "notification intent unavailable"}
		}
		intent, exists := intents[kind]
		if !exists {
			return taskNotificationStatus{Status: "pending", Error: "notification intent missing"}
		}
		if intent.Status == "queued" {
			return taskNotificationStatus{Status: "queued", EventID: intent.EventID, Recipient: intent.Recipient}
		}
		admitted, err := h.coordination.enqueue(ctx, intent.Event, &authpkg.CallerIdentity{Role: intent.ActorRole, Username: intent.ActorName, Team: intent.Team})
		if err != nil {
			return taskNotificationStatus{Status: "pending", Error: err.Error()}
		}
		if !admitted.Accepted || admitted.EventID == "" {
			return taskNotificationStatus{Status: "pending", Error: "native notification not admitted"}
		}
		result := taskNotificationStatus{Status: "queued", EventID: admitted.EventID, Recipient: admitted.Recipient}
		intent.Status, intent.EventID, intent.Recipient = "queued", admitted.EventID, admitted.Recipient
		intents[kind] = intent
		task["task_notifications"], _ = json.Marshal(intents)
		next, _ := json.Marshal(task)
		strict, ok := h.oss.(conditionalWriter)
		if !ok {
			return result
		}
		if err := strict.PutObjectIfMatchStrict(ctx, key, next, contentETag(body)); err != nil {
			if errors.Is(err, oss.ErrPreconditionFailed) {
				continue
			}
			return result // Queue admission is durable even if this marker lags.
		}
		return result
	}
	return taskNotificationStatus{Status: "pending", Error: "notification receipt update raced; recovery will retry"}
}

// Task state and pending intents are committed in one CAS. This scan closes the
// crash gap between that commit and the separate, existing notification outbox.
func (h *ProjectHandler) recoverTaskNotifications(ctx context.Context) {
	if h.oss == nil || h.configReader == nil {
		return
	}
	prefixes, _, err := h.taskMetaPrefixes(ctx)
	if err != nil {
		return
	}
	for _, prefix := range prefixes {
		cursor := ""
		for ctx.Err() == nil {
			entries, next, err := h.oss.ListObjectsRecursive(ctx, prefix, cursor, 200)
			if err != nil {
				break
			}
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name, "/meta.json") || strings.Count(entry.Name, "/") != 1 {
					continue
				}
				key := path.Join(prefix, entry.Name)
				body, err := h.oss.GetObject(ctx, key)
				if err != nil {
					continue
				}
				var task map[string]json.RawMessage
				var intents map[string]taskNotificationIntent
				if json.Unmarshal(body, &task) != nil {
					continue
				}
				if len(task["task_plan"]) > 0 {
					_ = h.reconcileTaskPlan(ctx, key)
				}
				var review taskReview
				if json.Unmarshal(task["task_review"], &review) == nil && review.ID != "" {
					var child map[string]any
					if json.Unmarshal(body, &child) == nil {
						main, err := h.projectTaskReview(ctx, key, child, review)
						if err == nil && main["task_plan"] != nil {
							_ = h.reconcileTaskPlan(ctx, path.Join(path.Dir(path.Dir(key)), coordinationString(child, "main_task_id"), "meta.json"))
						}
					}
				}
				if json.Unmarshal(task["task_notifications"], &intents) != nil {
					continue
				}
				for kind, intent := range intents {
					if intent.Status == "pending" {
						h.flushTaskNotification(ctx, key, kind)
					}
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
}

func validateTaskNotificationsCommit(w http.ResponseWriter, current, next []byte) bool {
	before, had := rawJSONField(current, "task_notifications")
	after, has := rawJSONField(next, "task_notifications")
	if had != has || (had && !sameJSONValue(before, after)) {
		writeError(w, 403, "task notifications are server-owned")
		return false
	}
	return true
}
