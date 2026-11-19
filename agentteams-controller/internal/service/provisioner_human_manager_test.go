package service

import (
	"context"
	"reflect"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
	"sigs.k8s.io/yaml"
)

func TestPersonalManagerRoomUsesManagerCreatorAndHumanUID(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	matrixClient.tokenUsers["manager-token"] = "@manager:localhost"
	// This fake does not auto-join an explicit room creator as Matrix does.
	for _, room := range []string{"!manager:localhost", "!existing:localhost"} {
		matrixClient.members[room] = []matrix.RoomMember{{UserID: "@manager:localhost", Membership: "join"}}
	}
	matrixClient.roomCreators = map[string]string{"!manager:localhost": "@manager:localhost", "!existing:localhost": "@manager:localhost"}
	p := NewProvisioner(ProvisionerConfig{Matrix: matrixClient, Creds: fakeCredentialStore{"default": {MatrixToken: "manager-token"}}, AdminUser: "admin"})
	for _, uid := range []string{"first-human-uid", "recreated-human-uid"} {
		_, err := p.EnsureHumanManagerRoom(context.Background(), HumanManagerRoomRequest{ManagerName: "default", HumanName: "alice", HumanUID: uid, HumanMatrixID: "@alice:localhost"})
		if err != nil {
			t.Fatal(err)
		}
		req := matrixClient.createRooms[len(matrixClient.createRooms)-1]
		if req.CreatorToken == "" || !reflect.DeepEqual(req.Invite, []string{"@alice:localhost"}) || len(req.PowerLevels) != 2 || req.PowerLevels["@manager:localhost"] != 100 || !req.IsDirect || !strings.HasSuffix(req.RoomAliasName, uid) {
			t.Fatalf("private DM creation included wrong participants or identity: %+v", req)
		}
		meta := req.InitialState[0].Content
		if meta["managerName"] != "default" || meta["admin"].(map[string]interface{})["userId"] != "@alice:localhost" {
			t.Fatalf("wrong room metadata: %+v", meta)
		}
	}
	if matrixClient.createRooms[0].RoomAliasName == matrixClient.createRooms[1].RoomAliasName {
		t.Fatal("recreated Human inherited previous room alias")
	}
	_, err := p.EnsureHumanManagerRoom(context.Background(), HumanManagerRoomRequest{ManagerName: "default", HumanName: "alice", HumanMatrixID: "@alice:localhost", RoomID: "!existing:localhost"})
	if err != nil || len(matrixClient.createRooms) != 2 {
		t.Fatalf("existing mapping created another room: %v", err)
	}
}

func TestManagerHumanAllowFromSurvivesConfigRefreshAndRevokes(t *testing.T) {
	ctx := context.Background()
	store := ossfake.NewMemory()
	d := NewDeployer(DeployerConfig{OSS: store})
	req := ManagerDeployRequest{Name: "default", Spec: v1beta1.ManagerSpec{Runtime: "qwenpaw", Model: "test"}}
	if err := d.DeployManagerConfig(ctx, req); err != nil {
		t.Fatal(err)
	}
	config := NewManagerConfigStore(ManagerConfigStoreConfig{OSS: store})
	if err := config.UpdateManagerGroupAllowFrom("@leader:localhost", true); err != nil {
		t.Fatal(err)
	}
	for _, add := range []bool{true, false} {
		if err := config.UpdateManagerHumanAllowFrom("@alice:localhost", add); err != nil {
			t.Fatal(err)
		}
		if err := d.DeployManagerConfig(ctx, req); err != nil {
			t.Fatal(err)
		}
		data, _ := store.GetObject(ctx, "agents/manager/runtime/runtime.yaml")
		var doc memberRuntimeConfigDocument
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		policy := doc.Desired.ChannelPolicy
		if containsID(policy.GroupAllowExtra, "@alice:localhost") != add || containsID(policy.DmAllowExtra, "@alice:localhost") != add || !containsID(policy.GroupAllowExtra, "@leader:localhost") {
			t.Fatalf("lost/revocation drift: %+v", policy)
		}
	}
}

func containsID(ids []string, value string) bool {
	for _, id := range ids {
		if id == value {
			return true
		}
	}
	return false
}
