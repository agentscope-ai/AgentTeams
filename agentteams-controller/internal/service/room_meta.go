package service

import (
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
)

const roomMetaEventType = "room.meta"

func roomMetaState(content map[string]interface{}) []matrix.StateEvent {
	return []matrix.StateEvent{{
		Type:     roomMetaEventType,
		StateKey: "",
		Content:  content,
	}}
}

func teamRoomMeta(req TeamRoomRequest, teamAdminID, leaderMatrixID string, userIDForName func(string) string) map[string]interface{} {
	meta := baseRoomMeta("team_room")
	if req.TeamName != "" {
		meta["teamName"] = req.TeamName
	}
	if req.AdminSpec != nil && teamAdminID != "" {
		meta["teamAdmin"] = namedUserMeta(teamAdminID, req.AdminSpec.Name)
	}
	if req.LeaderName != "" && leaderMatrixID != "" {
		meta["leaderWorker"] = workerUserMeta(leaderMatrixID, req.LeaderName)
	}
	if members := humanMemberMeta(req.HumanMembers, userIDForName); len(members) > 0 {
		meta["humanMembers"] = members
	}
	return meta
}

func leaderDMRoomMeta(req TeamRoomRequest, teamAdminID, leaderMatrixID string) map[string]interface{} {
	meta := baseRoomMeta("direct_room")
	if req.TeamName != "" {
		meta["teamName"] = req.TeamName
	}
	if req.AdminSpec != nil && teamAdminID != "" {
		meta["teamAdmin"] = namedUserMeta(teamAdminID, req.AdminSpec.Name)
	}
	if req.LeaderName != "" && leaderMatrixID != "" {
		meta["leaderWorker"] = workerUserMeta(leaderMatrixID, req.LeaderName)
	}
	return meta
}

func workerRoomMeta(req WorkerProvisionRequest, workerMatrixID, leaderMatrixID string) map[string]interface{} {
	meta := baseRoomMeta("worker_room")
	if req.TeamName != "" {
		meta["teamName"] = req.TeamName
	}
	if req.Name != "" {
		meta["workerName"] = req.Name
	}
	if req.Role == "team_leader" && workerMatrixID != "" {
		meta["leaderWorker"] = workerUserMeta(workerMatrixID, req.Name)
	} else if req.TeamLeaderName != "" && leaderMatrixID != "" {
		meta["leaderWorker"] = workerUserMeta(leaderMatrixID, req.TeamLeaderName)
	}
	return meta
}

func managerDMRoomMeta(managerName, managerMatrixID, adminMatrixID, adminName string) map[string]interface{} {
	meta := baseRoomMeta("direct_room")
	if managerName != "" {
		meta["managerName"] = managerName
	}
	if managerMatrixID != "" {
		meta["manager"] = namedUserMeta(managerMatrixID, "manager")
	}
	if adminMatrixID != "" {
		meta["admin"] = namedUserMeta(adminMatrixID, adminName)
	}
	return meta
}

func baseRoomMeta(kind string) map[string]interface{} {
	return map[string]interface{}{
		"schemaVersion": 1,
		"roomKind":      kind,
		"lifecycle":     "persistent",
		"createdBy":     "hiclaw",
	}
}

func namedUserMeta(userID, name string) map[string]interface{} {
	out := map[string]interface{}{"userId": userID}
	if name != "" {
		out["name"] = name
	}
	return out
}

func workerUserMeta(userID, workerName string) map[string]interface{} {
	out := map[string]interface{}{"userId": userID}
	if workerName != "" {
		out["workerName"] = workerName
	}
	return out
}

func humanMemberMeta(members []v1beta1.TeamMemberSpec, userIDForName func(string) string) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		userID := member.MatrixUserID
		if userID == "" && member.Name != "" && userIDForName != nil {
			userID = userIDForName(member.Name)
		}
		if userID == "" {
			continue
		}
		if _, ok := seen[userID]; ok {
			continue
		}
		seen[userID] = struct{}{}
		out = append(out, namedUserMeta(userID, member.Name))
	}
	return out
}
