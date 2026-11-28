package server

import (
	"context"
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
	joinedTeam, err := h.joinedTeamHumans(r.Context(), team, token)
	if err != nil {
		writeError(w, 503, "team room membership unavailable")
		return
	}
	joined, failed, err := h.acceptTaskRoomHumanInvites(r.Context(), team, room, invited, joinedTeam)
	if err != nil {
		writeError(w, 503, "team Humans unavailable")
		return
	}
	httputil.WriteJSON(w, 200, map[string]any{"joined": joined, "failed": failed})
}

func (h *CoordinationHandler) acceptTaskRoomHumanInvites(ctx context.Context, team *v1beta1.Team, room string, invited, joinedTeam map[string]bool) ([]string, []string, error) {
	var humans v1beta1.HumanList
	var reader client.Reader = h.deps.Client
	if h.deps.APIReader != nil {
		reader = h.deps.APIReader
	}
	if err := reader.List(ctx, &humans, client.InNamespace(h.deps.Namespace)); err != nil {
		return nil, nil, err
	}
	joined, failed := []string{}, []string{}
	for _, human := range humans.Items {
		id := human.Status.MatrixUserID
		if !invited[id] || !human.DeletionTimestamp.IsZero() {
			continue
		}
		if !taskRoomHumanAllowed(team, &human) || (!teamAdminHuman(team, &human) && !joinedTeam[id]) {
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
		userToken, loginErr := h.deps.Provisioner.LoginAsHuman(ctx, localpart, password)
		if loginErr != nil || userToken == "" {
			failed = append(failed, id)
			continue
		}
		if h.deps.Provisioner.JoinRoomAs(ctx, room, userToken) != nil {
			failed = append(failed, id)
			continue
		}
		joined = append(joined, id)
	}
	return joined, failed, nil
}

func (h *CoordinationHandler) joinedTeamHumans(ctx context.Context, team *v1beta1.Team, leaderToken string) (map[string]bool, error) {
	joined := map[string]bool{}
	if team.Status.TeamRoomID == "" {
		return joined, nil
	}
	members, err := h.deps.MatrixClient.ListRoomMembersWithToken(ctx, team.Status.TeamRoomID, leaderToken)
	for _, member := range members {
		if member.Membership == "join" {
			joined[member.UserID] = true
		}
	}
	return joined, err
}

func teamAdminHuman(team *v1beta1.Team, human *v1beta1.Human) bool {
	return team.Spec.Admin != nil && team.Spec.Admin.Name == human.Name && (team.Spec.Admin.MatrixUserID == "" || team.Spec.Admin.MatrixUserID == human.Status.MatrixUserID)
}

func taskRoomHumanAllowed(team *v1beta1.Team, human *v1beta1.Human) bool {
	id := human.Status.MatrixUserID
	if teamAdminHuman(team, human) {
		return true
	}
	for _, member := range team.Spec.HumanMembers {
		if member.Name == human.Name && (member.MatrixUserID == "" || member.MatrixUserID == id) {
			return true
		}
	}
	for _, name := range human.Spec.AccessibleTeams {
		if name == team.Name || name == team.Spec.EffectiveTeamName(team.Name) {
			return true
		}
	}
	return false
}
