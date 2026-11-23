package server

import (
	"context"
	"fmt"
	"net/http"
	"path"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type assistantRoute struct {
	prefix, rolePrefix, owner, room, user, human, name, kind, label, agent string
}

func (h *AssistantHandler) reader() client.Reader {
	if h.deps.APIReader != nil {
		return h.deps.APIReader
	}
	return h.deps.Client
}

func (h *AssistantHandler) route(r *http.Request) (assistantRoute, error) {
	q := r.URL.Query()
	return h.routeForRecipient(r.Context(), authpkg.CallerFromContext(r.Context()), q.Get("team"), q.Get("manager"), q.Get("user"))
}

func (h *AssistantHandler) routeFor(ctx context.Context, caller *authpkg.CallerIdentity, team string) (assistantRoute, error) {
	return h.routeForRecipient(ctx, caller, team, "", "")
}

// Recipient identifiers select registered Human resources, never caller-supplied
// Matrix rooms. Re-read role and Human authority on every operation and retry.
func (h *AssistantHandler) routeForRecipient(ctx context.Context, caller *authpkg.CallerIdentity, teamName, managerName, humanName string) (assistantRoute, error) {
	var out assistantRoute
	if caller == nil || h.deps.OSS == nil || h.reader() == nil || (caller.ServiceAccountNamespace != "" && caller.ServiceAccountNamespace != h.deps.Namespace) {
		return out, fmt.Errorf("assistant unavailable")
	}
	coord := NewCoordinationHandler(h.deps)
	reader := h.reader()
	userKey := ""
	roomRecipient := ""
	if teamName != "" {
		if managerName != "" || humanName != "" || (caller.Role != authpkg.RoleTeamLeader && caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleHuman) {
			return out, fmt.Errorf("current Leader or Team Admin required")
		}
		var team v1.Team
		if err := reader.Get(ctx, client.ObjectKey{Namespace: h.deps.Namespace, Name: teamName}, &team); err != nil {
			return out, err
		}
		if !team.DeletionTimestamp.IsZero() || team.Spec.Admin == nil {
			return out, fmt.Errorf("Team Admin unavailable")
		}
		leader := ""
		for _, member := range team.Spec.WorkerMembers {
			if member.Role == "team_leader" {
				if leader != "" {
					return out, fmt.Errorf("ambiguous Leader")
				}
				leader = member.Name
			}
		}
		worker, err := coord.worker(ctx, leader)
		if err != nil || !worker.DeletionTimestamp.IsZero() {
			return out, fmt.Errorf("current Leader unavailable")
		}
		out.user = team.Spec.Admin.MatrixUserID
		var admin v1.Human
		if err := reader.Get(ctx, client.ObjectKey{Namespace: h.deps.Namespace, Name: team.Spec.Admin.Name}, &admin); err == nil {
			if !admin.DeletionTimestamp.IsZero() || admin.Status.MatrixUserID == "" {
				return out, fmt.Errorf("Team Admin identity unavailable")
			}
			out.user, userKey = admin.Status.MatrixUserID, string(admin.UID)
		} else if !apierrors.IsNotFound(err) {
			return out, err
		}
		roomRecipient = team.Status.AssistantRecipient
		if out.user == "" && h.deps.MatrixClient != nil {
			out.user = h.deps.MatrixClient.UserID(team.Spec.Admin.Name)
		}
		if caller.Role == authpkg.RoleTeamLeader {
			if caller.Username != worker.Name && caller.WorkerName != worker.Name {
				return out, fmt.Errorf("former Leader cannot access role context")
			}
		} else if caller.Username != team.Spec.Admin.Name || caller.MatrixUserID == "" || caller.MatrixUserID != out.user {
			return out, fmt.Errorf("only this Team Admin can access the private context")
		}
		out.rolePrefix, out.owner = path.Join("teams", team.Name, "roles/leader"), string(worker.UID)+":"+worker.Name
		out.room, out.name, out.kind, out.agent = team.Status.LeaderDMRoomID, worker.Name, "worker", worker.Status.MatrixUserID
		out.label, out.human = team.Spec.EffectiveTeamName(team.Name)+"助手", team.Spec.Admin.Name
		if h.deps.Backend != nil {
			instance, _, err := NewTaskRecoveryHandler(nil, h.deps).runningWorkerInstance(ctx, worker)
			if err != nil {
				return out, err
			}
			out.owner += ":" + instance
		}
	} else {
		if caller.Role == authpkg.RoleManager {
			if managerName == "" {
				managerName = caller.Username
			}
		} else {
			if caller.MatrixUserID == "" || (caller.Role != authpkg.RoleAdmin && caller.Role != authpkg.RoleHuman) {
				return out, fmt.Errorf("registered Manager or Human required")
			}
			if humanName != "" && humanName != caller.Username {
				return out, fmt.Errorf("another user's private context is unavailable")
			}
			humanName = caller.Username
		}
		if managerName == "" {
			managerName = "default"
		}
		if managerName == "manager" {
			managerName = "default"
		}
		var manager v1.Manager
		err := reader.Get(ctx, client.ObjectKey{Namespace: h.deps.Namespace, Name: managerName}, &manager)
		if err != nil || !manager.DeletionTimestamp.IsZero() {
			return out, fmt.Errorf("current Manager unavailable")
		}
		if caller.Role == authpkg.RoleManager && caller.Username != manager.Name && !(caller.Username == "manager" && manager.Name == "default") {
			return out, fmt.Errorf("another Manager cannot access role context")
		}
		out.rolePrefix = path.Join("manager/roles", manager.Name)
		out.owner = fmt.Sprintf("%s:%s:%d", manager.UID, manager.Name, manager.Generation)
		out.room, out.name, out.kind, out.label, out.agent = manager.Status.RoomID, manager.Name, "manager", "AT助手", manager.Status.MatrixUserID
		if humanName == "" {
			humanName = h.deps.MatrixConfig.AdminUser
		}
		if humanName == "" {
			return out, fmt.Errorf("registered recipient required")
		}
		var human v1.Human
		if err := reader.Get(ctx, client.ObjectKey{Namespace: h.deps.Namespace, Name: humanName}, &human); err != nil {
			return out, err
		}
		if !human.DeletionTimestamp.IsZero() || human.Spec.PermissionLevel != 1 || human.Status.MatrixUserID == "" {
			return out, fmt.Errorf("current L1 administrator required")
		}
		out.user, out.human = human.Status.MatrixUserID, human.Name
		if human.Labels[v1.LabelSystemAdmin] != "true" {
			out.room = human.Status.ManagerRooms[manager.Name]
		}
		if caller.Role != authpkg.RoleManager && caller.MatrixUserID != out.user {
			return out, fmt.Errorf("Human identity changed")
		}
		userKey = string(human.UID)
	}
	if out.room == "" || out.user == "" {
		return out, fmt.Errorf("assistant recipient room unavailable")
	}
	if userKey == "" {
		// Team Admins may be external Matrix users. Managed Humans additionally
		// include their UID so a recreated account cannot inherit private notes.
		var human v1.Human
		if reader.Get(ctx, client.ObjectKey{Namespace: h.deps.Namespace, Name: out.human}, &human) == nil && human.Status.MatrixUserID == out.user {
			if !human.DeletionTimestamp.IsZero() {
				return out, fmt.Errorf("recipient deleted")
			}
			userKey = string(human.UID)
		}
	}
	userKey = service.TeamAssistantRecipient(out.user, userKey)
	if teamName != "" && (roomRecipient != "" && roomRecipient != userKey || roomRecipient == "" && h.deps.Provisioner != nil) {
		return out, fmt.Errorf("assistant room recipient has not been reconciled")
	}
	out.prefix = path.Join(out.rolePrefix, "users", userKey)
	out.owner += ":" + userKey
	return out, nil
}
