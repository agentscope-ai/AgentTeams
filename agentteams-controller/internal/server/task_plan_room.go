package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Room identity lives in the existing Matrix alias mechanism. An interrupted
// create before Task persistence resolves the same room and checks its owner.
func (h *CoordinationHandler) ensureTaskPlanRoom(ctx context.Context, main map[string]any, plan taskPlan) (string, error) {
	if h.deps.MatrixClient == nil || h.deps.Provisioner == nil {
		return "", fmt.Errorf("task room service unavailable")
	}
	leader, err := h.worker(ctx, plan.ActorName)
	if err != nil {
		return "", err
	}
	team, err := h.memberTeam(ctx, leader.Name, "team_leader", coordinationString(main, "team_id"))
	if err != nil {
		return "", err
	}
	token, err := h.deps.Provisioner.AgentMatrixToken(ctx, "worker", leader.Name)
	if err != nil || token == "" {
		return "", fmt.Errorf("task room creator unavailable")
	}
	inviteSet := map[string]bool{}
	workers := []map[string]any{}
	for _, node := range plan.Request.Nodes {
		worker, err := h.worker(ctx, node.AssignedTo)
		if err != nil {
			return "", err
		}
		if _, err := h.memberTeam(ctx, worker.Name, "worker", team.Name); err != nil {
			return "", err
		}
		if !strings.HasPrefix(worker.Status.MatrixUserID, "@") {
			return "", fmt.Errorf("task room member unavailable")
		}
		if !inviteSet[worker.Status.MatrixUserID] {
			inviteSet[worker.Status.MatrixUserID] = true
			workers = append(workers, map[string]any{"userId": worker.Status.MatrixUserID, "workerName": worker.Spec.EffectiveWorkerName(worker.Name)})
		}
	}
	var humans v1beta1.HumanList
	var reader client.Reader = h.deps.Client
	if h.deps.APIReader != nil {
		reader = h.deps.APIReader
	}
	if reader.List(ctx, &humans, client.InNamespace(h.deps.Namespace)) != nil {
		return "", fmt.Errorf("task room Humans unavailable")
	}
	humanMeta := []map[string]any{}
	for _, human := range humans.Items {
		if !human.DeletionTimestamp.IsZero() || !taskRoomHumanAllowed(team, &human) || !strings.HasPrefix(human.Status.MatrixUserID, "@") {
			continue
		}
		inviteSet[human.Status.MatrixUserID] = true
		humanMeta = append(humanMeta, map[string]any{"userId": human.Status.MatrixUserID, "name": human.Name})
	}
	meta := map[string]any{"schemaVersion": 1, "roomKind": "task_room", "lifecycle": "ephemeral", "createdBy": "teamharness",
		"taskId": main["task_id"], "planId": plan.ID, "teamName": team.Spec.EffectiveTeamName(team.Name),
		"leaderWorker":  map[string]any{"userId": leader.Status.MatrixUserID, "workerName": leader.Spec.EffectiveWorkerName(leader.Name)},
		"workerMembers": workers, "humanMembers": humanMeta}
	powers := map[string]int{leader.Status.MatrixUserID: 100}
	admin, adminName := "", ""
	explicitAdmin := team.Spec.Admin != nil && (team.Spec.Admin.Name != "" || team.Spec.Admin.MatrixUserID != "")
	if explicitAdmin {
		adminName, admin = team.Spec.Admin.Name, team.Spec.Admin.MatrixUserID
		if admin == "" {
			admin = h.deps.Provisioner.MatrixUserID(adminName)
		}
	} else if h.deps.MatrixConfig.AdminUser != "" {
		adminName = h.deps.MatrixConfig.AdminUser
		admin = h.deps.Provisioner.MatrixUserID(adminName)
	}
	if admin != "" {
		inviteSet[admin], powers[admin] = true, 100
		meta["teamAdmin"] = map[string]any{"userId": admin, "name": adminName}
	}
	delete(inviteSet, leader.Status.MatrixUserID)
	invite := []string{}
	for id := range inviteSet {
		invite = append(invite, id)
	}
	sort.Strings(invite)
	identity := sha256.Sum256([]byte(h.deps.Namespace + "\x00" + team.Name + "\x00" + coordinationString(main, "task_id")))
	room, err := h.deps.MatrixClient.CreateRoom(ctx, matrix.CreateRoomRequest{Name: "TASK：" + coordinationString(main, "title"), Topic: "Task " + coordinationString(main, "task_id"), CreatorToken: token,
		Invite: invite, PowerLevels: powers, RoomAliasName: "at-task-" + hex.EncodeToString(identity[:16]), InitialState: []matrix.StateEvent{{Type: "room.meta", Content: meta}}})
	if err != nil || room == nil || !strings.HasPrefix(room.RoomID, "!") {
		return "", fmt.Errorf("task room creation unavailable")
	}
	actual, err := h.deps.MatrixClient.GetRoomState(ctx, room.RoomID, "room.meta", "", token)
	if err != nil || actual["taskId"] != main["task_id"] || actual["planId"] != plan.ID || actual["teamName"] != team.Spec.EffectiveTeamName(team.Name) || coordinationString(coordinationMap(actual, "leaderWorker"), "userId") != leader.Status.MatrixUserID {
		return "", fmt.Errorf("task room ownership does not match the saved plan")
	}
	members, err := h.deps.MatrixClient.ListRoomMembersWithToken(ctx, room.RoomID, token)
	if err != nil {
		return "", err
	}
	membership := map[string]string{}
	for _, member := range members {
		membership[member.UserID] = member.Membership
	}
	for _, id := range invite {
		if membership[id] == "join" || membership[id] == "invite" {
			continue
		}
		if membership[id] == "ban" {
			return "", fmt.Errorf("task room member is banned")
		}
		if err := h.deps.MatrixClient.InviteToRoomWithToken(ctx, room.RoomID, id, token); err != nil {
			return "", err
		}
		membership[id] = "invite"
	}
	if !explicitAdmin && admin != "" && membership[admin] != "join" {
		adminToken, err := h.deps.Provisioner.LoginAsHuman(ctx, adminName, h.deps.MatrixConfig.AdminPassword)
		if err != nil || adminToken == "" || h.deps.Provisioner.JoinRoomAs(ctx, room.RoomID, adminToken) != nil {
			return "", fmt.Errorf("task room administrator membership pending")
		}
		membership[admin] = "join"
	}
	invited := map[string]bool{}
	for id, state := range membership {
		if state == "invite" {
			invited[id] = true
		}
	}
	_, failed, err := h.acceptTaskRoomHumanInvites(ctx, team, room.RoomID, invited)
	if err != nil || len(failed) > 0 {
		return "", fmt.Errorf("task room Human membership pending")
	}
	return room.RoomID, nil
}
