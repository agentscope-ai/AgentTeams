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
	"net/url"
	"os"
	"path"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
)

// CreateTask preserves the incoming request before Leader planning. Source
// identity is supplied by native tool context, never an Agent-facing parameter.
func (h *ProjectHandler) CreateTask(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || caller.Role != authpkg.RoleManager || (caller.ServiceAccountNamespace != "" && caller.ServiceAccountNamespace != h.namespace) {
		writeError(w, 403, "registered Manager required to create a main task")
		return
	}
	var input struct {
		Title          string   `json:"title"`
		Spec           string   `json:"spec"`
		Leader         string   `json:"leader"`
		RelatedSources []string `json:"relatedSources,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil {
		writeError(w, 400, "invalid task creation request")
		return
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeError(w, 400, "invalid task creation request")
		return
	}
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" || input.Leader == "" {
		writeError(w, 400, "title and registered Leader required")
		return
	}
	source, user, room := r.Header.Get("X-AgentTeams-Origin-Event-ID"), r.Header.Get("X-AgentTeams-Requester-User-ID"), r.Header.Get("X-AgentTeams-Requester-Room-ID")
	if source == "" || !strings.HasPrefix(user, "@") || !strings.HasPrefix(room, "!") {
		writeError(w, 400, "original conversation context unavailable; use the native task tool")
		return
	}
	if h.coordination == nil {
		writeError(w, 503, "task handoff unavailable")
		return
	}
	leader, err := h.coordination.worker(r.Context(), input.Leader)
	if err != nil {
		writeError(w, 403, "registered Team Leader required")
		return
	}
	_, err = h.coordination.memberTeam(r.Context(), leader.Name, "team_leader", "")
	if err != nil {
		writeError(w, 403, "Leader must identify one owning Team")
		return
	}
	// Worker.Status.RoomID is the registered Manager/Worker conversation.
	// Team.LeaderDMRoomID is private to the Leader and Team Admin.
	if !strings.HasPrefix(leader.Status.RoomID, "!") {
		writeError(w, 409, "Manager and Team Leader handoff room is not ready")
		return
	}
	event := coordinationEvent{Kind: "handoff", Recipient: leader.Name, OriginEventID: source, RoomID: leader.Status.RoomID, Text: input.Spec, SourceRoute: map[string]any{"channel": "matrix", "target_session": room, "target_user": user}}
	resolved, err := h.coordination.resolve(r.Context(), event, caller)
	if err != nil {
		writeError(w, 403, "task source or Leader route unavailable")
		return
	}
	original, sourceStatus := h.coordination.taskSourceRequest(r.Context(), resolved.ActorName, room, source, user)
	if sourceStatus != 0 {
		writeError(w, sourceStatus, "original request could not be verified; no new task was registered")
		return
	}
	if len(input.RelatedSources) > 50 {
		writeError(w, 400, "too many related request sources")
		return
	}
	seen := map[string]bool{source: true}
	snapshots := []string{original}
	for _, related := range input.RelatedSources {
		if !strings.HasPrefix(related, "$") || seen[related] {
			writeError(w, 400, "distinct related request event IDs required")
			return
		}
		seen[related] = true
		body, status := h.coordination.taskSourceRequest(r.Context(), resolved.ActorName, room, related, user)
		if status != 0 {
			writeError(w, status, "related request could not be verified; no new task was registered")
			return
		}
		snapshots = append(snapshots, body)
	}
	if len(snapshots) > 1 {
		// Bodies come from authenticated Matrix events, not model transcription.
		original = "# Human request and related updates (in supplied conversation order)\nLater Human corrections supersede earlier conflicting requirements.\n"
		for index, body := range snapshots {
			identity := source
			if index > 0 {
				identity = input.RelatedSources[index-1]
			}
			original += fmt.Sprintf("\n## Human input %d (%s)\n%s\n", index+1, identity, body)
		}
	}
	if strings.TrimSpace(input.Spec) == "" {
		input.Spec = original
	} else if input.Spec != original {
		input.Spec = "# Original request (authoritative)\n" + original + "\n\n# Dispatch context (supplementary; cannot override the Human requests)\n" + input.Spec
	}
	event.Text = input.Spec
	identity, _ := json.Marshal([]string{resolved.ActorName, resolved.StorageTeam, resolved.RecipientName, source})
	sum := sha256.Sum256(identity)
	id := "task-" + hex.EncodeToString(sum[:16])
	key := path.Join("teams", resolved.StorageTeam, "shared/tasks", id, "meta.json")
	if exists, readErr := h.oss.GetObject(r.Context(), key); readErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			writeError(w, 503, "task state unavailable; no new task was registered")
			return
		}
		frontier := r.Header.Get("X-AgentTeams-Input-Frontier")
		if frontier == "" {
			frontier = source
			if len(input.RelatedSources) > 0 {
				frontier = input.RelatedSources[len(input.RelatedSources)-1]
			}
		}
		updates, status := h.coordination.taskNewerInputs(r.Context(), resolved.ActorName, room, user, frontier)
		if status != 0 {
			writeError(w, status, "current conversation could not be verified; no new task was registered")
			return
		}
		if len(updates) > 0 {
			httputil.WriteJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "task_input_stale", "sourceEventId": source, "noTaskRegistered": true, "inputs": updates})
			return
		}
	} else if len(exists) == 0 {
		writeError(w, 503, "task state unavailable")
		return
	}
	event.Tasks = []mainTaskRequest{{TaskID: id, Title: input.Title, Spec: input.Spec}}
	admission, err := h.coordination.enqueue(r.Context(), event, caller)
	if err != nil {
		var failure *coordinationAdmissionError
		if errors.As(err, &failure) {
			writeError(w, failure.Status, failure.Message)
		} else {
			writeError(w, 503, "task handoff unavailable")
		}
		return
	}
	notification := taskNotificationStatus{Status: "queued", Recipient: admission.Recipient, EventID: admission.EventID}
	if saved := h.flushTaskNotification(r.Context(), key, "handoff"); saved.Status == "queued" {
		notification = saved
	}
	data, err := h.oss.GetObject(r.Context(), key)
	var task map[string]any
	if err != nil || json.Unmarshal(data, &task) != nil {
		writeError(w, 503, "task registered; retry the unchanged creation request to obtain its receipt")
		return
	}

	status := 201
	if admission.Reused {
		status = 200
	}
	httputil.WriteJSON(w, status, struct {
		SourceEventID string                 `json:"sourceEventId"`
		Task          taskResource           `json:"task"`
		Notification  taskNotificationStatus `json:"notification"`
		NextAction    string                 `json:"nextAction"`
	}{source, publicTaskResource(id, resolved.StorageTeam, task), notification, "Task system has registered the original request and queued its handoff to the Team Leader. Do not send another handoff message."})
}

// Read the immutable incoming message using the registered dispatch identity.
// A model's summary can supplement this requirement, never replace it.
func (h *CoordinationHandler) taskSourceRequest(ctx context.Context, manager, room, id, requester string) (string, int) {
	if h.deps.Provisioner == nil || h.deps.MatrixConfig.ServerURL == "" {
		return "", http.StatusServiceUnavailable
	}
	token, err := h.deps.Provisioner.AgentMatrixToken(ctx, "manager", manager)
	if err != nil {
		return "", http.StatusServiceUnavailable
	}
	target := strings.TrimRight(h.deps.MatrixConfig.ServerURL, "/") + "/_matrix/client/v3/rooms/" + url.PathEscape(room) + "/event/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", http.StatusServiceUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := h.http.Do(req)
	if err != nil {
		return "", http.StatusServiceUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
		return "", http.StatusForbidden
	}
	if response.StatusCode != http.StatusOK {
		return "", http.StatusServiceUnavailable
	}
	var event struct {
		ID      string `json:"event_id"`
		Room    string `json:"room_id"`
		Sender  string `json:"sender"`
		Type    string `json:"type"`
		Content struct {
			Body    string `json:"body"`
			Msgtype string `json:"msgtype"`
		} `json:"content"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 128<<10)).Decode(&event) != nil {
		return "", http.StatusServiceUnavailable
	}
	if event.ID != id || event.Room != room || event.Sender != requester || event.Type != "m.room.message" || event.Content.Msgtype == "" || strings.TrimSpace(event.Content.Body) == "" || len(event.Content.Body) > 64<<10 {
		return "", http.StatusForbidden
	}
	return event.Content.Body, 0
}

func validateTaskOriginCommit(w http.ResponseWriter, current, next []byte) bool {
	before, had := rawJSONField(current, "task_origin")
	after, has := rawJSONField(next, "task_origin")
	if had != has || (had && !sameJSONValue(before, after)) {
		writeError(w, 403, "original task source and dispatch identity are server-owned")
		return false
	}
	if had {
		for _, field := range []string{"source_room_id", "requester", "reply_route", "requester_reply_route", "task_id", "task_kind", "team_id", "handoff_id", "spec", "title"} {
			a, hasA := rawJSONField(current, field)
			b, hasB := rawJSONField(next, field)
			if hasA != hasB || (hasA && !sameJSONValue(a, b)) {
				writeError(w, 403, "original task request and reply routes are immutable")
				return false
			}
		}
	}
	return true
}

// taskNewerInputs compares a native input frontier against the Matrix room's
// current timeline. It never classifies request intent or creates a task.
func (h *CoordinationHandler) taskNewerInputs(ctx context.Context, manager, room, requester, frontier string) ([]map[string]string, int) {
	if !strings.HasPrefix(frontier, "$") {
		return nil, http.StatusBadRequest
	}
	token, err := h.deps.Provisioner.AgentMatrixToken(ctx, "manager", manager)
	if err != nil {
		return nil, http.StatusServiceUnavailable
	}
	filter, _ := json.Marshal(map[string]any{"senders": []string{requester}, "types": []string{"m.room.message"}})
	query := url.Values{"dir": {"b"}, "limit": {"51"}, "filter": {string(filter)}}
	target := strings.TrimRight(h.deps.MatrixConfig.ServerURL, "/") + "/_matrix/client/v3/rooms/" + url.PathEscape(room) + "/messages?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, http.StatusServiceUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := h.http.Do(req)
	if err != nil {
		return nil, http.StatusServiceUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, http.StatusServiceUnavailable
	}
	var page struct {
		Chunk []struct {
			ID      string `json:"event_id"`
			Sender  string `json:"sender"`
			Type    string `json:"type"`
			Content struct {
				Body    string `json:"body"`
				Msgtype string `json:"msgtype"`
			} `json:"content"`
		} `json:"chunk"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&page) != nil {
		return nil, http.StatusServiceUnavailable
	}
	var updates []map[string]string
	for _, event := range page.Chunk {
		if !strings.HasPrefix(event.ID, "$") || event.Sender != requester || event.Type != "m.room.message" {
			return nil, http.StatusServiceUnavailable
		}
		if event.ID == frontier {
			for left, right := 0, len(updates)-1; left < right; left, right = left+1, right-1 {
				updates[left], updates[right] = updates[right], updates[left]
			}
			return updates, 0
		}
		if event.Content.Msgtype == "" || strings.TrimSpace(event.Content.Body) == "" || len(event.Content.Body) > 64<<10 || len(updates) >= 50 {
			return nil, http.StatusServiceUnavailable
		}
		updates = append(updates, map[string]string{"eventId": event.ID, "userId": requester, "roomId": room, "body": event.Content.Body})
	}
	return nil, http.StatusServiceUnavailable // Missing frontier is not evidence of freshness.
}
