package service

import (
	"context"
	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"testing"
)

func TestTeamAssistantReusesRoomAcrossLeaderReplacement(t *testing.T) {
	m := newFakeTeamMatrix()
	m.tokenUsers["alice-token"] = "@alice:localhost"
	m.seedOwnedRoom("!assistant:localhost", "@alice:localhost")
	m.members["!assistant:localhost"] = []matrix.RoomMember{{UserID: "@alice:localhost", Membership: "join"}}
	m.powerStates = map[string]map[string]interface{}{"!assistant:localhost": {"users": map[string]interface{}{"@alice:localhost": 100.0}}}
	m.tokenUsers["first-token"] = "@first:localhost"
	m.tokenUsers["replacement-token"] = "@replacement:localhost"
	p := NewProvisioner(ProvisionerConfig{Matrix: m, AdminUser: "admin", Creds: fakeCredentialStore{
		"first": {MatrixToken: "first-token"}, "replacement": {MatrixToken: "replacement-token"},
	}})
	for _, leader := range []string{"first", "replacement"} {
		result, err := p.ProvisionTeamRooms(context.Background(), TeamRoomRequest{
			TeamName: "research", LeaderName: leader, LeaderDMRoomID: "!assistant:localhost",
			AdminSpec:           &v1.TeamAdminSpec{Name: "alice", MatrixUserID: "@alice:localhost"},
			TeamAdminActorToken: "alice-token", TeamAdminActorName: "alice",
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.LeaderDMRoomID != "!assistant:localhost" {
			t.Fatal("assistant room changed during handoff")
		}
	}
	for _, request := range m.createRooms {
		if request.IsDirect {
			t.Fatal("handoff created another private room")
		}
	}
	if len(m.roomNames) != 2 || m.roomNames[1].name != "research助手" {
		t.Fatal("room was not named after its Team")
	}
}
