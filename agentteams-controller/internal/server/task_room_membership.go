package server

import (
	"net/http"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// JoinTaskRoomHumans accepts existing task-room invitations on behalf of
// registered team Humans. It never invites users or changes room access.
func (h *CoordinationHandler) JoinTaskRoomHumans(w http.ResponseWriter, r *http.Request) {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil || (caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleAdmin) {
		writeError(w, 403, "task room membership requires its team leader")
		return
	}
	if h.deps.Provisioner == nil || h.deps.MatrixClient == nil {
		writeError(w, 503, "Matrix membership unavailable")
		return
	}
	name, room := r.PathValue("name"), r.PathValue("roomId")
	if !strings.HasPrefix(room, "!") {
		writeError(w, 400, "task room ID is required")
		return
	}
	var team *v1beta1.Team
	if caller.Role == authpkg.RoleTeamLeader {
		actor, err := h.worker(r.Context(), caller.Username)
		if err != nil || caller.Team == "" {
			writeError(w, 403, "team leader unavailable")
			return
		}
		team, err = h.memberTeam(r.Context(), actor.Name, "team_leader", caller.Team)
		if err != nil || (name != team.Name && name != team.Spec.EffectiveTeamName(team.Name)) {
			writeError(w, 403, "task room outside caller team")
			return
		}
	} else {
		team = &v1beta1.Team{}
		if h.deps.Client.Get(r.Context(), client.ObjectKey{Namespace: h.deps.Namespace, Name: name}, team) != nil {
			writeError(w, 404, "team not found")
			return
		}
	}
	var leader *v1beta1.Worker
	for _, member := range team.Spec.WorkerMembers {
		if member.Role != "team_leader" {
			continue
		}
		worker, err := h.worker(r.Context(), member.Name)
		if err != nil || leader != nil {
			writeError(w, 503, "task room leader unavailable")
			return
		}
		leader = worker
	}
	if leader == nil {
		writeError(w, 503, "task room leader unavailable")
		return
	}
	token, err := h.deps.Provisioner.AgentMatrixToken(r.Context(), "worker", leader.Name)
	if err != nil {
		writeError(w, 503, "task room leader credentials unavailable")
		return
	}
	meta, err := h.deps.MatrixClient.GetRoomState(r.Context(), room, "room.meta", "", token)
	if err != nil || coordinationString(meta, "roomKind") != "task_room" ||
		(coordinationString(meta, "teamName") != team.Name && coordinationString(meta, "teamName") != team.Spec.EffectiveTeamName(team.Name)) ||
		coordinationString(coordinationMap(meta, "leaderWorker"), "userId") != leader.Status.MatrixUserID {
		writeError(w, 403, "room is not owned by this task team")
		return
	}
	members, err := h.deps.MatrixClient.ListRoomMembersWithToken(r.Context(), room, token)
	if err != nil {
		writeError(w, 503, "task room membership unavailable")
		return
	}
	invited := map[string]bool{}
	for _, m := range members {
		if m.Membership == "invite" {
			invited[m.UserID] = true
		}
	}
	var humans v1beta1.HumanList
	if h.deps.Client.List(r.Context(), &humans, client.InNamespace(h.deps.Namespace)) != nil {
		writeError(w, 503, "team Humans unavailable")
		return
	}
	joined, failed := []string{}, []string{}
	for _, human := range humans.Items {
		id := human.Status.MatrixUserID
		if !invited[id] || !human.DeletionTimestamp.IsZero() {
			continue
		}
		allowed := team.Spec.Admin != nil && team.Spec.Admin.Name == human.Name && (team.Spec.Admin.MatrixUserID == "" || team.Spec.Admin.MatrixUserID == id)
		for _, m := range team.Spec.HumanMembers {
			if m.Name == human.Name && (m.MatrixUserID == "" || m.MatrixUserID == id) {
				allowed = true
			}
		}
		for _, t := range human.Spec.AccessibleTeams {
			if t == team.Name || t == team.Spec.EffectiveTeamName(team.Name) {
				allowed = true
			}
		}
		if !allowed {
			continue
		}
		localpart := strings.SplitN(strings.TrimPrefix(id, "@"), ":", 2)[0]
		if h.deps.Provisioner.MatrixUserID(localpart) != id {
			failed = append(failed, id)
			continue
		}
		password := human.Status.InitialPassword
		if localpart == h.deps.MatrixConfig.AdminUser {
			password = h.deps.MatrixConfig.AdminPassword
		}
		userToken, loginErr := h.deps.Provisioner.LoginAsHuman(r.Context(), localpart, password)
		if loginErr != nil || userToken == "" {
			failed = append(failed, id)
			continue
		}
		if h.deps.Provisioner.JoinRoomAs(r.Context(), room, userToken) != nil {
			failed = append(failed, id)
			continue
		}
		joined = append(joined, id)
	}
	httputil.WriteJSON(w, 200, map[string]any{"joined": joined, "failed": failed})
}
