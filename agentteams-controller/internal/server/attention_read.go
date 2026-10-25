package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// Project attention read surface (workbench decision modal).
//
// The worker records in-flight attention requests (approval / decision /
// escalation) into the per-task meta.json `attention` list (TeamHarness
// taskflow request_attention); the answer still goes to the Task room as a
// human Matrix message, never through the controller. This endpoint is the
// read-only aggregate that lets the workbench pop the unanswered questions:
// same team scope and 404/409/403 semantics as GetProjectSignalEvents,
// filtered to the project's graph tasks.

const (
	attentionDefaultLimit = 200
	attentionMaxLimit     = 500
)

// attentionRecord mirrors one stored entry of a task meta.json `attention`
// list (camelCase — the Python writer's shape; PR #1418 adds
// options/suggested/expiresAt). Parsed into map[string]any below, so the
// list shape itself is defined here for reference and tolerance:
//
//	[{kind, question, attempt, requestedAt, resolved,
//	  eventId?, options?:[{id,label}], suggested?, expiresAt?}]
type attentionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type attentionItem struct {
	TaskID      string            `json:"task_id"`
	TaskTitle   string            `json:"task_title"`
	TaskStatus  string            `json:"task_status"`
	Kind        string            `json:"kind"`
	Question    string            `json:"question"`
	Attempt     int               `json:"attempt"`
	RequestedAt string            `json:"requested_at"`
	Resolved    bool              `json:"resolved"`
	EventID     string            `json:"event_id,omitempty"`
	Options     []attentionOption `json:"options,omitempty"`
	Suggested   string            `json:"suggested,omitempty"`
	ExpiresAt   string            `json:"expires_at,omitempty"`
}

type attentionResponse struct {
	ProjectID string          `json:"project_id"`
	Items     []attentionItem `json:"items"`
	Count     int             `json:"count"`
}

// GetProjectAttention handles GET /api/v1/projects/{id}/attention.
//
// Query: include_resolved (1 = also return resolved records),
// limit (1..500, default 200), team (qualifier for ambiguous ids).
//
// Returns the attention requests across the project's graph tasks
// (project.tasks, or loop.tasks for loop plans), unresolved first (newest
// requestedAt first), capped at limit.
func (h *ProjectHandler) GetProjectAttention(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "project id is required")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	teamFilter := r.URL.Query().Get("team")

	prefixes, crToEffective, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "get attention: resolve prefixes", err)
		return
	}
	matches, err := h.resolveProjectMeta(r.Context(), projectID, prefixes, teamFilter, caller, crToEffective)
	if err != nil {
		writeK8sError(w, "get attention", err)
		return
	}
	match, ok := h.resolveSingleProjectMatch(w, matches)
	if !ok {
		return
	}
	if err := h.checkProjectAccess(caller, match.team, crToEffective); err != nil {
		if _, ok := err.(*accessDeniedError); ok {
			httputil.WriteError(w, http.StatusNotFound, "project not found")
			return
		}
		httputil.WriteError(w, http.StatusForbidden, err.Error())
		return
	}

	limit := attentionDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httputil.WriteError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > attentionMaxLimit {
		limit = attentionMaxLimit
	}
	includeResolved := r.URL.Query().Get("include_resolved") == "1"

	tasks := match.meta.Tasks
	if match.meta.Loop != nil && len(match.meta.Loop.Tasks) > 0 {
		tasks = match.meta.Loop.Tasks
	}

	items := make([]attentionItem, 0, len(tasks))
	seen := map[string]bool{}
	for _, node := range tasks {
		if node.TaskID == "" || seen[node.TaskID] {
			continue
		}
		seen[node.TaskID] = true
		items = append(items, h.readTaskAttention(r.Context(), node, match.team)...)
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].RequestedAt != items[j].RequestedAt {
			return items[i].RequestedAt > items[j].RequestedAt
		}
		if items[i].TaskID != items[j].TaskID {
			return items[i].TaskID < items[j].TaskID
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].Attempt < items[j].Attempt
	})

	out := items
	if !includeResolved {
		out = make([]attentionItem, 0, len(items))
		for _, it := range items {
			if !it.Resolved {
				out = append(out, it)
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	httputil.WriteJSON(w, http.StatusOK, attentionResponse{
		ProjectID: projectID,
		Items:     out,
		Count:     len(out),
	})
}

// readTaskAttention reads one task's attention list from its TaskMeta
// (shared/tasks/{id}/meta.json, the project's owning scope only —
// taskMetaKeys). A missing or malformed TaskMeta contributes nothing: the
// aggregate read never fails on a single task (same policy as
// collectProjectRooms).
func (h *ProjectHandler) readTaskAttention(ctx context.Context, node projectTaskMeta, team string) []attentionItem {
	for _, key := range taskMetaKeys(node.TaskID, team) {
		data, err := h.oss.GetObject(ctx, key)
		if err != nil {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			continue // truncated/legacy TaskMeta: skip, never fail the read
		}
		list, ok := raw["attention"].([]any)
		if !ok {
			continue
		}
		title, status := str(raw["title"]), str(raw["status"])
		if title == "" {
			title = node.Title
		}
		if status == "" {
			status = node.Status
		}
		var out []attentionItem
		for _, entry := range list {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			// TeamHarness writes the S0 deadline as `timeoutAt` (PR
			// #1418); `expiresAt` is accepted as the design-doc alias so
			// both spellings surface as the same `expires_at` wire field.
			expires := str(m["timeoutAt"])
			if expires == "" {
				expires = str(m["expiresAt"])
			}
			out = append(out, attentionItem{
				TaskID:      node.TaskID,
				TaskTitle:   title,
				TaskStatus:  status,
				Kind:        str(m["kind"]),
				Question:    str(m["question"]),
				Attempt:     intFromAny(m["attempt"]),
				RequestedAt: str(m["requestedAt"]),
				Resolved:    m["resolved"] == true,
				EventID:     str(m["eventId"]),
				Options:     attentionOptions(m["options"]),
				Suggested:   str(m["suggested"]),
				ExpiresAt:   expires,
			})
		}
		return out
	}
	return nil
}

// attentionOptions coerces the optional options list to the response shape
// (nil when absent, so omitempty drops it from the wire).
func attentionOptions(v any) []attentionOption {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	out := make([]attentionOption, 0, len(list))
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, attentionOption{ID: str(m["id"]), Label: str(m["label"])})
	}
	return out
}

// intFromAny coerces a JSON number to int (0 for absent or non-numeric).
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}
