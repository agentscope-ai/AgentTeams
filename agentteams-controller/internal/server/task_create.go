package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
		Title  string `json:"title"`
		Spec   string `json:"spec"`
		Leader string `json:"leader"`
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
	if input.Title == "" || strings.TrimSpace(input.Spec) == "" || input.Leader == "" {
		writeError(w, 400, "title, original spec and registered Leader required")
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
	team, err := h.coordination.memberTeam(r.Context(), leader.Name, "team_leader", "")
	if err != nil {
		writeError(w, 403, "Leader must identify one owning Team")
		return
	}
	if !strings.HasPrefix(team.Status.LeaderDMRoomID, "!") {
		writeError(w, 409, "Team Leader handoff room is not ready")
		return
	}
	event := coordinationEvent{Kind: "handoff", Recipient: leader.Name, OriginEventID: source, RoomID: team.Status.LeaderDMRoomID, Text: input.Spec, SourceRoute: map[string]any{"channel": "matrix", "target_session": room, "target_user": user}}
	resolved, err := h.coordination.resolve(r.Context(), event, caller)
	if err != nil {
		writeError(w, 403, "task source or Leader route unavailable")
		return
	}
	identity, _ := json.Marshal([]string{resolved.ActorName, resolved.StorageTeam, resolved.RecipientName, source})
	sum := sha256.Sum256(identity)
	id := "task-" + hex.EncodeToString(sum[:16])
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
	key := path.Join("teams", resolved.StorageTeam, "shared/tasks", id, "meta.json")
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
