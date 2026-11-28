package service

import (
	"context"
	"fmt"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
)

type HumanManagerRoomRequest struct {
	ManagerName, HumanName, HumanUID, HumanMatrixID, RoomID string
}

// EnsureHumanManagerRoom creates a DM as the Manager, keeping the system
// administrator out of other Humans' private conversations. The alias makes
// retries after a lost status patch safe; the Human UID prevents a recreated
// resource from inheriting a previous Human's room.
func (p *Provisioner) EnsureHumanManagerRoom(ctx context.Context, req HumanManagerRoomRequest) (string, error) {
	credentials, err := p.RefreshManagerCredentials(ctx, req.ManagerName)
	if err != nil {
		return "", err
	}
	token := credentials.MatrixToken
	roomID := req.RoomID
	if roomID == "" {
		managerID := p.matrix.UserID("manager")
		room, err := p.matrix.CreateRoom(ctx, matrix.CreateRoomRequest{
			Name: "AgentTeams助手", Topic: fmt.Sprintf("Private Manager conversation for %s", req.HumanName),
			CreatorToken: token, Invite: []string{req.HumanMatrixID}, IsDirect: true,
			PowerLevels:   map[string]int{managerID: 100, req.HumanMatrixID: 100},
			InitialState:  roomMetaState(managerDMRoomMeta(req.ManagerName, managerID, req.HumanMatrixID, req.HumanName)),
			RoomAliasName: roomAliasLocalpart("human-manager", req.ManagerName+"-"+req.HumanName+"-"+req.HumanUID),
		})
		if err != nil {
			return "", err
		}
		roomID = room.RoomID
	}
	if err := p.matrix.InviteToRoomWithToken(ctx, roomID, req.HumanMatrixID, token); err != nil {
		return "", err
	}
	return roomID, nil
}
