package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// S2 signal-events read surface (#1405 follow-up).
//
// #1405 writes one object per signal event (controller control endpoints +
// TeamHarness task/loop emitters) and gives agents a read path through the
// Python MCP action `list_events`, but deliberately leaves the controller's
// project event feed (GetProjectEvents, the task-transition stream)
// untouched — so a browser (workbench / dashboard) had no way to read
// SIG-* events. This endpoint is that read surface: same team scope and
// cursor semantics as `list_events`, filtered to one project.

const (
	signalEventsDefaultLimit = 50
	signalEventsMaxLimit     = 200
)

type signalEventsResponse struct {
	ProjectID  string        `json:"project_id"`
	Events     []signalEvent `json:"events"`
	Count      int           `json:"count"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// signalEventPrefix is the directory signalEventKey writes into.
func signalEventPrefix(team, teamID string) string {
	if team != "" {
		return fmt.Sprintf("teams/%s/shared/events/%s/", team, teamID)
	}
	return fmt.Sprintf("shared/events/%s/", teamID)
}

// encodeSignalCursor / decodeSignalCursor mirror the Python cursor shape
// (base64url of "<occurred_at>\x00<event_id>"): events sort by
// (occurred_at, event_id), and a page resumes strictly after the anchor.
func encodeSignalCursor(occurredAt, eventID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(occurredAt + "\x00" + eventID))
}

// decodeSignalCursor accepts both padded and unpadded base64url cursors:
// this endpoint emits unpadded (RawURLEncoding) while the Python side
// (base64.urlsafe_b64encode) emits padded, and a page must resume across
// the boundary in either direction.
func decodeSignalCursor(raw string) (string, string, bool) {
	unpadded := strings.TrimRight(raw, "=")
	if len(unpadded)%4 == 1 {
		return "", "", false // a base64 body can never be 1 mod 4
	}
	data, err := base64.URLEncoding.DecodeString(
		unpadded + strings.Repeat("=", (4-len(unpadded)%4)%4),
	)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(data), "\x00", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// GetProjectSignalEvents handles GET /api/v1/projects/{id}/signal-events.
//
// Query: type (repeatable / comma-separated), task (alias taskId),
// since (RFC3339 lower bound), limit (1..200, default 50), cursor.
//
// Project attribution: the 11-field event contract has no project_id, so
// project-level events match by ref (…/projects/{id}/meta.json) and
// task-level events resolve the task meta's project_id.
func (h *ProjectHandler) GetProjectSignalEvents(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "project id is required")
		return
	}
	caller := authpkg.CallerFromContext(r.Context())
	teamFilter := r.URL.Query().Get("team")

	prefixes, crToEffective, err := h.teamProjectPrefixes(r.Context())
	if err != nil {
		writeK8sError(w, "get signal events: resolve prefixes", err)
		return
	}
	matches, err := h.resolveProjectMeta(r.Context(), projectID, prefixes, teamFilter, caller, crToEffective)
	if err != nil {
		writeK8sError(w, "get signal events", err)
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

	limit := signalEventsDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httputil.WriteError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > signalEventsMaxLimit {
		limit = signalEventsMaxLimit
	}

	var afterTs, afterID string
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		var ok bool
		afterTs, afterID, ok = decodeSignalCursor(raw)
		if !ok {
			httputil.WriteError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}

	typeFilter := map[string]bool{}
	for _, raw := range r.URL.Query()["type"] {
		for _, value := range strings.Split(raw, ",") {
			if v := strings.TrimSpace(value); v != "" {
				typeFilter[v] = true
			}
		}
	}
	taskFilter := strings.TrimSpace(r.URL.Query().Get("task"))
	if taskFilter == "" {
		taskFilter = strings.TrimSpace(r.URL.Query().Get("taskId"))
	}
	since := strings.TrimSpace(r.URL.Query().Get("since"))
	var sinceTime time.Time
	if since != "" {
		sinceTime, err = time.Parse(time.RFC3339, since)
		if err != nil {
			// A literal '+' offset (e.g. 18:01:00+08:00) reaches the
			// handler as a space — the query parser decodes '+' as
			// whitespace; retry with it mapped back so unencoded
			// RFC3339 offsets are accepted.
			if alt := strings.ReplaceAll(since, " ", "+"); alt != since {
				sinceTime, err = time.Parse(time.RFC3339, alt)
			}
		}
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid since")
			return
		}
	}

	teamID := signalTeamID(match.meta, match.team)
	prefix := signalEventPrefix(match.team, teamID)
	infos, err := h.oss.ListObjectsDetailed(r.Context(), prefix)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list signal events: %v", err))
		return
	}

	projectOfTask := map[string]string{}
	events := make([]signalEvent, 0, len(infos))
	for _, info := range infos {
		if !strings.HasSuffix(info.Name, ".json") {
			continue
		}
		data, err := h.oss.GetObject(r.Context(), prefix+info.Name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // listed then deleted — skip, never fail the read
			}
			continue
		}
		var ev signalEvent
		if err := json.Unmarshal(data, &ev); err != nil || ev.EventID == "" {
			continue // tolerate unrelated objects under the events prefix
		}
		if !h.signalEventBelongsToProject(r.Context(), ev, projectID, match.team, projectOfTask) {
			continue
		}
		if len(typeFilter) > 0 && !typeFilter[ev.EventType] {
			continue
		}
		if taskFilter != "" && ev.TaskID != taskFilter {
			continue
		}
		if since != "" {
			// Instant comparison, not string comparison: offset zones and
			// fractional seconds must order chronologically.
			evTime, perr := time.Parse(time.RFC3339, ev.OccurredAt)
			if perr == nil && evTime.Before(sinceTime) {
				continue
			}
			// perr != nil: keep the event — silently dropping it would
			// lose data; emitters write normalized RFC3339, so this is
			// defensive.
		}
		events = append(events, ev)
	}

	// Lexicographic order is chronological on purpose: both emitters
	// (controller utcTimestamp; TeamHarness _utc_timestamp) write
	// occurred_at as fixed-width UTC 'Z' second-precision RFC3339, where
	// string order == time order (guarded by
	// TestSignalEventOccurredAtNormalized).
	sort.Slice(events, func(i, j int) bool {
		if events[i].OccurredAt != events[j].OccurredAt {
			return events[i].OccurredAt < events[j].OccurredAt
		}
		return events[i].EventID < events[j].EventID
	})
	// afterTs is itself an emitter-written occurred_at (same normalized
	// form as above), so lexicographic comparison resumes the page.
	if afterTs != "" || afterID != "" {
		idx := 0
		for idx < len(events) {
			e := events[idx]
			if e.OccurredAt > afterTs ||
				(e.OccurredAt == afterTs && e.EventID > afterID) {
				break
			}
			idx++
		}
		events = events[idx:]
	}

	resp := signalEventsResponse{ProjectID: projectID, Events: []signalEvent{}}
	if len(events) > 0 {
		end := limit
		if end > len(events) {
			end = len(events)
		}
		resp.Events = events[:end]
		if end < len(events) {
			last := events[end-1]
			resp.NextCursor = encodeSignalCursor(last.OccurredAt, last.EventID)
		}
	}
	resp.Count = len(resp.Events)
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// signalEventBelongsToProject resolves the event's project without a
// project_id field: project-level events by ref, task-level events by the
// task meta's project_id (cached per request).
func (h *ProjectHandler) signalEventBelongsToProject(
	ctx context.Context,
	ev signalEvent,
	projectID, team string,
	cache map[string]string,
) bool {
	if ev.TaskID == "" {
		return strings.Contains(ev.Ref, "/projects/"+projectID+"/")
	}
	pid, ok := cache[ev.TaskID]
	if !ok {
		pid = h.taskProjectID(ctx, ev.TaskID, team)
		cache[ev.TaskID] = pid
	}
	return pid == projectID
}

func (h *ProjectHandler) taskProjectID(ctx context.Context, taskID, team string) string {
	for _, key := range taskMetaKeys(taskID, team) {
		data, err := h.oss.GetObject(ctx, key)
		if err != nil {
			continue
		}
		var meta struct {
			ProjectID string `json:"project_id"`
		}
		if json.Unmarshal(data, &meta) == nil {
			return meta.ProjectID
		}
	}
	return ""
}
