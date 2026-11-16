package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/audit"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TaskRoomArchiveHandler applies current Controller identities and Team CR
// ownership before invoking the homeserver's privileged lifecycle operations.
type TaskRoomArchiveHandler struct {
	deps       ServerDeps
	http       *http.Client
	adminToken func(context.Context) (string, error)
	actorToken func(context.Context, *http.Request, *authpkg.CallerIdentity) (string, error)
	audit      *audit.Client
}

func NewTaskRoomArchiveHandler(deps ServerDeps) *TaskRoomArchiveHandler {
	h := &TaskRoomArchiveHandler{deps: deps, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, audit: audit.NewClient(deps.OSS)}
	h.adminToken = func(ctx context.Context) (string, error) {
		provider, ok := deps.MatrixClient.(interface {
			ControllerAdminToken(context.Context) (string, error)
		})
		if !ok {
			return "", fmt.Errorf("Matrix administration is unavailable")
		}
		return provider.ControllerAdminToken(ctx)
	}
	h.actorToken = func(ctx context.Context, r *http.Request, caller *authpkg.CallerIdentity) (string, error) {
		if caller.MatrixUserID != "" {
			fields := strings.Fields(r.Header.Get("Authorization"))
			if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
				return "", fmt.Errorf("Matrix caller credential is missing")
			}
			return fields[1], nil
		}
		if caller.Role == authpkg.RoleAdmin {
			return h.adminToken(ctx)
		}
		if deps.Provisioner == nil {
			return "", fmt.Errorf("agent Matrix credentials unavailable")
		}
		if caller.Role == authpkg.RoleManager {
			var managers v1beta1.ManagerList
			options := []client.ListOption{client.InNamespace(deps.Namespace)}
			if deps.ControllerName != "" {
				options = append(options, client.MatchingLabels{v1beta1.LabelController: deps.ControllerName})
			}
			if err := deps.Client.List(ctx, &managers, options...); err != nil {
				return "", err
			}
			if len(managers.Items) != 1 {
				return "", fmt.Errorf("one Manager credential owner is required")
			}
			return deps.Provisioner.AgentMatrixToken(ctx, "manager", managers.Items[0].Name)
		}
		return deps.Provisioner.AgentMatrixToken(ctx, "worker", caller.Username)
	}
	return h
}

func (h *TaskRoomArchiveHandler) request(ctx context.Context, token, method, path string) (int, []byte, error) {
	if token == "" || h.deps.MatrixConfig.ServerURL == "" {
		return 0, nil, fmt.Errorf("Matrix is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.deps.MatrixConfig.ServerURL, "/")+path, bytes.NewBufferString("{}"))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := h.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	return res.StatusCode, body, err
}

func taskRoomPath(room, action string) string {
	return "/_matrix/client/unstable/org.agentteams/rooms/" + url.PathEscape(room) + "/" + action
}

func (h *TaskRoomArchiveHandler) teams(ctx context.Context) ([]v1beta1.Team, error) {
	reader := h.deps.APIReader
	if reader == nil {
		reader = h.deps.Client
	}
	if reader == nil {
		return nil, nil
	}
	var teams v1beta1.TeamList
	if err := reader.List(ctx, &teams, client.InNamespace(h.deps.Namespace)); err != nil {
		return nil, err
	}
	return teams.Items, nil
}

func taskRoomRights(caller *authpkg.CallerIdentity, meta map[string]any, teams []v1beta1.Team) (read, archive, remove bool) {
	if caller == nil || meta["roomKind"] != "task_room" {
		return
	}
	if caller.Role == authpkg.RoleAdmin || caller.Role == authpkg.RoleManager {
		return true, true, true
	}
	teamName, _ := meta["teamName"].(string)
	for _, team := range teams {
		storageName := team.Spec.EffectiveTeamName(team.Name)
		if (team.Name != teamName && storageName != teamName) || (!caller.TeamMatches(team.Name) && !caller.TeamMatches(storageName)) {
			continue
		}
		switch caller.Role {
		case authpkg.RoleHuman:
			if caller.HumanPermissionLevel != 2 {
				return
			}
			admin := team.Spec.Admin != nil && team.Spec.Admin.Name == caller.Username
			return true, admin, admin
		case authpkg.RoleTeamLeader:
			for _, member := range team.Spec.WorkerMembers {
				if member.Role == "team_leader" && member.Name == caller.Username {
					return true, true, false
				}
			}
		}
	}
	return
}

type taskRoomArchiveStatus struct {
	IsTask     bool           `json:"is_task_room"`
	Archived   bool           `json:"archived"`
	Meta       map[string]any `json:"room_meta"`
	CanArchive bool           `json:"can_archive"`
	CanDelete  bool           `json:"can_delete"`
}

func (h *TaskRoomArchiveHandler) load(w http.ResponseWriter, r *http.Request) (*taskRoomArchiveStatus, string, bool) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, 401, "authentication required")
		return nil, "", false
	}
	room := r.PathValue("roomId")
	if !strings.HasPrefix(room, "!") || len(room) < 2 || len(room) > 255 {
		httputil.WriteError(w, 400, "invalid room id")
		return nil, "", false
	}
	token, err := h.adminToken(r.Context())
	if err != nil {
		httputil.WriteError(w, 503, "Matrix administration is unavailable")
		return nil, "", false
	}
	status, body, err := h.request(r.Context(), token, http.MethodGet, taskRoomPath(room, "archive"))
	if err != nil {
		httputil.WriteError(w, 502, "Matrix request failed")
		return nil, "", false
	}
	if status != 200 {
		writeTaskRoomResponse(w, status, body)
		return nil, "", false
	}
	var state taskRoomArchiveStatus
	if err := json.Unmarshal(body, &state); err != nil {
		httputil.WriteError(w, 502, "invalid Matrix archive response")
		return nil, "", false
	}
	teams, err := h.teams(r.Context())
	if err != nil {
		writeK8sError(w, "resolve task room team", err)
		return nil, "", false
	}
	read, archive, remove := taskRoomRights(caller, state.Meta, teams)
	if state.IsTask && !read {
		httputil.WriteError(w, 403, "task room is outside the caller's accessible teams")
		return nil, "", false
	}
	state.CanArchive = state.IsTask && archive
	state.CanDelete = state.IsTask && state.Archived && remove
	return &state, token, true
}

func (h *TaskRoomArchiveHandler) Status(w http.ResponseWriter, r *http.Request) {
	state, _, ok := h.load(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httputil.WriteJSON(w, 200, map[string]bool{"is_task_room": state.IsTask, "archived": state.Archived, "can_archive": state.CanArchive, "can_delete": state.CanDelete})
}

func (h *TaskRoomArchiveHandler) Mutate(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state, token, ok := h.load(w, r)
		if !ok {
			return
		}
		allowed := state.CanArchive
		if action == "delete" {
			allowed = state.CanDelete
		}
		if !state.IsTask || !allowed {
			httputil.WriteError(w, 403, "operation requires an authorized role and an eligible task room")
			return
		}
		status, body, err := h.request(r.Context(), token, http.MethodPost, taskRoomPath(r.PathValue("roomId"), action))
		if err != nil {
			httputil.WriteError(w, 502, "Matrix request failed")
			return
		}
		if status >= 200 && status < 300 {
			caller := authpkg.CallerFromContext(r.Context())
			team, _ := state.Meta["teamName"].(string)
			h.audit.Record(r.Context(), audit.Event{Who: caller.Username, Role: caller.Role, Target: r.PathValue("roomId"), TargetTeam: team, Action: "task_room_" + action})
		}
		writeTaskRoomResponse(w, status, body)
	}
}

func (h *TaskRoomArchiveHandler) List(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, 401, "authentication required")
		return
	}
	if caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleManager && caller.Role != authpkg.RoleHuman && caller.Role != authpkg.RoleTeamLeader {
		httputil.WriteError(w, 403, "task room access denied")
		return
	}
	token, err := h.actorToken(r.Context(), r, caller)
	if err != nil {
		httputil.WriteError(w, 503, "Matrix identity unavailable")
		return
	}
	query := url.Values{}
	for _, key := range []string{"team", "team_admin", "human_member"} {
		if value := r.URL.Query().Get(key); value != "" {
			query.Set(key, value)
		}
	}
	status, body, err := h.request(r.Context(), token, http.MethodGet, "/_matrix/client/unstable/org.agentteams/rooms/archived?"+query.Encode())
	if err != nil {
		httputil.WriteError(w, 502, "Matrix request failed")
		return
	}
	if status != 200 {
		writeTaskRoomResponse(w, status, body)
		return
	}
	var response struct {
		Rooms []map[string]any `json:"rooms"`
	}
	if json.Unmarshal(body, &response) != nil {
		httputil.WriteError(w, 502, "invalid Matrix archive response")
		return
	}
	teams, err := h.teams(r.Context())
	if err != nil {
		writeK8sError(w, "resolve task room teams", err)
		return
	}
	rooms := make([]map[string]any, 0, len(response.Rooms))
	for _, room := range response.Rooms {
		meta, _ := room["room_meta"].(map[string]any)
		read, archive, remove := taskRoomRights(caller, meta, teams)
		if !read {
			continue
		}
		room["can_archive"] = archive
		room["can_delete"] = remove
		rooms = append(rooms, room)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httputil.WriteJSON(w, 200, map[string]any{"rooms": rooms})
}

func writeTaskRoomResponse(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
