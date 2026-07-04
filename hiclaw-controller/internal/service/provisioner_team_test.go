package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/matrix"
)

type fakeTeamMatrix struct {
	createRooms  []matrix.CreateRoomRequest
	members      map[string][]matrix.RoomMember
	leaves       []string
	joins        []roomUserCall
	kicks        []roomUserCall
	tokenKicks   []roomUserCall
	kickErr      error
	adminCmds    []string
	tokenInvites []roomUserCall
	created      bool
}

type roomUserCall struct {
	roomID string
	userID string
}

func newFakeTeamMatrix() *fakeTeamMatrix {
	return &fakeTeamMatrix{
		members: make(map[string][]matrix.RoomMember),
		created: true,
	}
}

func (f *fakeTeamMatrix) EnsureUser(context.Context, matrix.EnsureUserRequest) (*matrix.UserCredentials, error) {
	return nil, nil
}

func (f *fakeTeamMatrix) CreateRoom(_ context.Context, req matrix.CreateRoomRequest) (*matrix.RoomInfo, error) {
	f.createRooms = append(f.createRooms, req)
	roomID := "!team:localhost"
	if strings.Contains(req.RoomAliasName, "leader-dm") {
		roomID = "!leader-dm:localhost"
	}
	if req.CreatorToken == "" {
		f.members[roomID] = []matrix.RoomMember{{UserID: "@admin:localhost", Membership: "join"}}
	}
	if f.created {
		for _, userID := range req.Invite {
			f.members[roomID] = append(f.members[roomID], matrix.RoomMember{UserID: userID, Membership: "invite"})
		}
	}
	return &matrix.RoomInfo{RoomID: roomID, Created: f.created}, nil
}

func (f *fakeTeamMatrix) ResolveRoomAlias(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (f *fakeTeamMatrix) DeleteRoomAlias(context.Context, string) error { return nil }

func (f *fakeTeamMatrix) SetRoomName(context.Context, string, string, string) error { return nil }

func (f *fakeTeamMatrix) SetRoomState(context.Context, string, string, string, map[string]interface{}, string) error {
	return nil
}

func (f *fakeTeamMatrix) JoinRoom(_ context.Context, roomID, token string) error {
	f.joins = append(f.joins, roomUserCall{roomID: roomID, userID: token})
	return nil
}

func (f *fakeTeamMatrix) LeaveRoom(_ context.Context, roomID, token string) error {
	f.leaves = append(f.leaves, roomID)
	return nil
}

func (f *fakeTeamMatrix) SendMessage(context.Context, string, string, string) error { return nil }

func (f *fakeTeamMatrix) SendMessageAsAdmin(context.Context, string, string) error { return nil }

func (f *fakeTeamMatrix) Login(context.Context, string, string) (string, error) { return "", nil }

func (f *fakeTeamMatrix) SetDisplayName(context.Context, string, string, string) error { return nil }

func (f *fakeTeamMatrix) AdminCommand(_ context.Context, cmd string) error {
	f.adminCmds = append(f.adminCmds, cmd)
	return nil
}

func (f *fakeTeamMatrix) ListJoinedRooms(context.Context, string) ([]string, error) { return nil, nil }

func (f *fakeTeamMatrix) ListRoomMembers(_ context.Context, roomID string) ([]matrix.RoomMember, error) {
	return f.members[roomID], nil
}

func (f *fakeTeamMatrix) ListRoomMembersWithToken(_ context.Context, roomID, _ string) ([]matrix.RoomMember, error) {
	return f.members[roomID], nil
}

func (f *fakeTeamMatrix) InviteToRoom(_ context.Context, roomID, userID string) error {
	f.members[roomID] = append(f.members[roomID], matrix.RoomMember{UserID: userID, Membership: "invite"})
	return nil
}

func (f *fakeTeamMatrix) InviteToRoomWithToken(_ context.Context, roomID, userID, _ string) error {
	f.tokenInvites = append(f.tokenInvites, roomUserCall{roomID: roomID, userID: userID})
	f.members[roomID] = append(f.members[roomID], matrix.RoomMember{UserID: userID, Membership: "invite"})
	return nil
}

func (f *fakeTeamMatrix) KickFromRoom(_ context.Context, roomID, userID, _ string) error {
	f.kicks = append(f.kicks, roomUserCall{roomID: roomID, userID: userID})
	if f.kickErr != nil {
		return f.kickErr
	}
	next := f.members[roomID][:0]
	for _, member := range f.members[roomID] {
		if member.UserID != userID {
			next = append(next, member)
		}
	}
	f.members[roomID] = next
	return nil
}

func (f *fakeTeamMatrix) KickFromRoomWithToken(_ context.Context, roomID, userID, _ string, _ string) error {
	f.tokenKicks = append(f.tokenKicks, roomUserCall{roomID: roomID, userID: userID})
	if f.kickErr != nil {
		return f.kickErr
	}
	next := f.members[roomID][:0]
	for _, member := range f.members[roomID] {
		if member.UserID != userID {
			next = append(next, member)
		}
	}
	f.members[roomID] = next
	return nil
}

func (f *fakeTeamMatrix) SyncMessages(context.Context, string, time.Duration) (*matrix.SyncMessagesResult, error) {
	return &matrix.SyncMessagesResult{}, nil
}

func (f *fakeTeamMatrix) UserID(localpart string) string {
	return "@" + localpart + ":localhost"
}

func (f *fakeTeamMatrix) EnsureAppServiceUser(_ context.Context, username string) (*matrix.UserCredentials, error) {
	return &matrix.UserCredentials{UserID: f.UserID(username), AccessToken: "as-token-" + username, Created: true}, nil
}

func (f *fakeTeamMatrix) LoginAppServiceUser(_ context.Context, username string) (string, error) {
	return "as-token-" + username, nil
}

func (f *fakeTeamMatrix) SetPasswordAsAdmin(_ context.Context, _, _ string) error { return nil }

func (f *fakeTeamMatrix) RegisterAppService(_ context.Context, _ matrix.AppServiceRegistration) error { return nil }
func (f *fakeTeamMatrix) UnregisterAppService(_ context.Context, _ string) error                     { return nil }
func (f *fakeTeamMatrix) AppServiceSmokeTest(_ context.Context) error { return nil }

func (f *fakeTeamMatrix) VerifyAccessToken(_ context.Context, _ string) error { return nil }

func TestProvisionTeamRoomsInvitesExplicitTeamAdminAndLeavesNewLeaderDM(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
	})

	_, err := p.ProvisionTeamRooms(context.Background(), TeamRoomRequest{
		TeamName:    "alpha",
		LeaderName:  "lead",
		WorkerNames: []string{"dev", "qa"},
		AdminSpec: &v1beta1.TeamAdminSpec{
			Name:         "alice",
			MatrixUserID: "@alice:example.com",
		},
		TeamAdminActorToken: "team-admin-token",
		TeamAdminActorName:  "alice",
	})
	if err != nil {
		t.Fatalf("ProvisionTeamRooms: %v", err)
	}
	if len(matrixClient.createRooms) != 2 {
		t.Fatalf("CreateRoom calls=%d, want 2", len(matrixClient.createRooms))
	}

	wantTeamInvites := []string{"@lead:localhost", "@dev:localhost", "@qa:localhost"}
	if got := matrixClient.createRooms[0].Invite; !reflect.DeepEqual(got, wantTeamInvites) {
		t.Fatalf("team room invites=%v, want %v", got, wantTeamInvites)
	}
	if got := matrixClient.createRooms[0].CreatorToken; got != "team-admin-token" {
		t.Fatalf("team room creator token=%q, want team-admin-token", got)
	}
	wantLeaderDMInvites := []string{"@lead:localhost"}
	if got := matrixClient.createRooms[1].Invite; !reflect.DeepEqual(got, wantLeaderDMInvites) {
		t.Fatalf("leader DM invites=%v, want %v", got, wantLeaderDMInvites)
	}
	if got := matrixClient.createRooms[1].CreatorToken; got != "team-admin-token" {
		t.Fatalf("leader DM creator token=%q, want team-admin-token", got)
	}
	if _, ok := matrixClient.createRooms[0].PowerLevels["@admin:localhost"]; ok {
		t.Fatalf("team room should not include global admin power level: %v", matrixClient.createRooms[0].PowerLevels)
	}
	if _, ok := matrixClient.createRooms[1].PowerLevels["@admin:localhost"]; ok {
		t.Fatalf("leader DM should not include global admin power level: %v", matrixClient.createRooms[1].PowerLevels)
	}
	for _, roomReq := range matrixClient.createRooms {
		if roomReq.PowerLevels["@alice:example.com"] != 100 {
			t.Fatalf("team admin power level=%d, want 100", roomReq.PowerLevels["@alice:example.com"])
		}
		if roomReq.PowerLevels["@lead:localhost"] != 100 {
			t.Fatalf("leader power level=%d, want 100", roomReq.PowerLevels["@lead:localhost"])
		}
	}
	if got, want := matrixClient.joins, []roomUserCall{
		{roomID: "!team:localhost", userID: "team-admin-token"},
		{roomID: "!leader-dm:localhost", userID: "team-admin-token"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("team admin joins=%v, want %v", got, want)
	}
	if len(matrixClient.leaves) != 0 {
		t.Fatalf("admin should not leave teamAdmin-created rooms, got %v", matrixClient.leaves)
	}
	if len(matrixClient.kicks) != 0 {
		t.Fatalf("global admin should leave explicitly, not be kicked: %+v", matrixClient.kicks)
	}
}

func TestProvisionTeamRoomsInvitesCoordinatorMembersLikeTeamAdmin(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
	})

	_, err := p.ProvisionTeamRooms(context.Background(), TeamRoomRequest{
		TeamName:    "alpha",
		LeaderName:  "lead",
		WorkerNames: []string{"dev"},
		AdminSpec: &v1beta1.TeamAdminSpec{
			Name:         "alice",
			MatrixUserID: "@alice:example.com",
		},
		HumanMembers: []v1beta1.TeamMemberSpec{
			{Name: "bob", MatrixUserID: "@bob:example.com", Role: "coordinator"},
			{Name: "carol", Role: "coordinator"},
		},
		TeamAdminActorToken: "team-admin-token",
		TeamAdminActorName:  "alice",
	})
	if err != nil {
		t.Fatalf("ProvisionTeamRooms: %v", err)
	}

	wantTeamInvites := []string{"@lead:localhost", "@bob:example.com", "@carol:localhost", "@dev:localhost"}
	if got := matrixClient.createRooms[0].Invite; !reflect.DeepEqual(got, wantTeamInvites) {
		t.Fatalf("team room invites=%v, want %v", got, wantTeamInvites)
	}
	wantLeaderDMInvites := []string{"@lead:localhost"}
	if got := matrixClient.createRooms[1].Invite; !reflect.DeepEqual(got, wantLeaderDMInvites) {
		t.Fatalf("leader DM invites=%v, want %v", got, wantLeaderDMInvites)
	}
	if got := matrixClient.createRooms[0].PowerLevels["@alice:example.com"]; got != 100 {
		t.Fatalf("team admin power level=%d, want 100", got)
	}
	if got := matrixClient.createRooms[1].PowerLevels["@alice:example.com"]; got != 100 {
		t.Fatalf("team admin leader DM power level=%d, want 100", got)
	}
	for _, id := range []string{"@bob:example.com", "@carol:localhost"} {
		if got := matrixClient.createRooms[0].PowerLevels[id]; got != 0 {
			t.Fatalf("team room coordinator power level for %s=%d, want 0", id, got)
		}
		if _, ok := matrixClient.createRooms[1].PowerLevels[id]; ok {
			t.Fatalf("leader DM should not include coordinator power level for %s: %v", id, matrixClient.createRooms[1].PowerLevels)
		}
	}
}

func TestProvisionTeamRoomsKeepsFallbackGlobalAdmin(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
	})

	_, err := p.ProvisionTeamRooms(context.Background(), TeamRoomRequest{
		TeamName:   "alpha",
		LeaderName: "lead",
	})
	if err != nil {
		t.Fatalf("ProvisionTeamRooms: %v", err)
	}
	if len(matrixClient.createRooms) != 2 {
		t.Fatalf("CreateRoom calls=%d, want 2", len(matrixClient.createRooms))
	}
	if got, want := matrixClient.createRooms[0].Invite, []string{"@admin:localhost", "@lead:localhost"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("team room invites=%v, want %v", got, want)
	}
	if got, want := matrixClient.createRooms[1].Invite, []string{"@lead:localhost", "@admin:localhost"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("leader DM invites=%v, want %v", got, want)
	}
	if len(matrixClient.leaves) != 0 {
		t.Fatalf("admin should not leave fallback rooms, got %v", matrixClient.leaves)
	}
}

func TestProvisionTeamRoomsDerivesTeamAdminMatrixIDFromName(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
	})

	_, err := p.ProvisionTeamRooms(context.Background(), TeamRoomRequest{
		TeamName:            "alpha",
		LeaderName:          "lead",
		AdminSpec:           &v1beta1.TeamAdminSpec{Name: "alice"},
		TeamAdminActorToken: "team-admin-token",
		TeamAdminActorName:  "alice",
	})
	if err != nil {
		t.Fatalf("ProvisionTeamRooms: %v", err)
	}
	if got, want := matrixClient.createRooms[0].Invite, []string{"@lead:localhost"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("team room invites=%v, want %v", got, want)
	}
	if got, want := matrixClient.createRooms[1].Invite, []string{"@lead:localhost"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("leader DM invites=%v, want %v", got, want)
	}
	if len(matrixClient.leaves) != 0 {
		t.Fatalf("admin should not leave teamAdmin-created rooms, got %v", matrixClient.leaves)
	}
}

func TestProvisionTeamRoomsDoesNotLeaveExistingLeaderDM(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	matrixClient.created = false
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
	})

	_, err := p.ProvisionTeamRooms(context.Background(), TeamRoomRequest{
		TeamName:            "alpha",
		LeaderName:          "lead",
		AdminSpec:           &v1beta1.TeamAdminSpec{Name: "alice"},
		TeamAdminActorToken: "team-admin-token",
		TeamAdminActorName:  "alice",
	})
	if err != nil {
		t.Fatalf("ProvisionTeamRooms: %v", err)
	}
	if len(matrixClient.leaves) != 0 {
		t.Fatalf("admin should not leave existing rooms when not a member, got %v", matrixClient.leaves)
	}
}

func TestProvisionTeamRoomsRequiresTeamAdminActorToken(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
	})

	_, err := p.ProvisionTeamRooms(context.Background(), TeamRoomRequest{
		TeamName:   "alpha",
		LeaderName: "lead",
		AdminSpec:  &v1beta1.TeamAdminSpec{Name: "alice"},
	})
	if err == nil {
		t.Fatal("ProvisionTeamRooms should fail when team admin is configured without actor token")
	}
}

func TestProvisionTeamRoomsUsesTeamAdminTokenForExistingTeamRoom(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	matrixClient.created = false
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
	})

	_, err := p.ProvisionTeamRooms(context.Background(), TeamRoomRequest{
		TeamName:             "alpha",
		LeaderName:           "lead",
		WorkerNames:          []string{"dev"},
		AdminSpec:            &v1beta1.TeamAdminSpec{Name: "alice"},
		TeamAdminActorToken:  "team-admin-token",
		TeamAdminActorName:   "alice",
		LeaderCredentialName: "lead-cr",
	})
	if err != nil {
		t.Fatalf("ProvisionTeamRooms: %v", err)
	}
	wantInvites := []roomUserCall{
		{roomID: "!team:localhost", userID: "@alice:localhost"},
		{roomID: "!team:localhost", userID: "@lead:localhost"},
		{roomID: "!team:localhost", userID: "@dev:localhost"},
		{roomID: "!leader-dm:localhost", userID: "@lead:localhost"},
		{roomID: "!leader-dm:localhost", userID: "@alice:localhost"},
	}
	if got := matrixClient.tokenInvites; !reflect.DeepEqual(got, wantInvites) {
		t.Fatalf("team room token invites=%v, want %v", got, wantInvites)
	}
	if len(matrixClient.kicks) != 0 {
		t.Fatalf("team room should not use admin kicks, got %v", matrixClient.kicks)
	}
}

func TestReconcileRoomMembershipForceLeavesWhenKickPowerDenied(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	matrixClient.members["!team:localhost"] = []matrix.RoomMember{
		{UserID: "@lead:localhost", Membership: "join"},
		{UserID: "@nov11:localhost", Membership: "join"},
	}
	matrixClient.kickErr = errors.New("HTTP 403 M_FORBIDDEN: sender does not have enough power to kick target user")
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
	})

	if err := p.ReconcileRoomMembership(context.Background(), "!team:localhost", []string{"@lead:localhost"}); err != nil {
		t.Fatalf("ReconcileRoomMembership: %v", err)
	}

	if got, want := matrixClient.kicks, []roomUserCall{{roomID: "!team:localhost", userID: "@nov11:localhost"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kick calls=%v, want %v", got, want)
	}
	if got, want := matrixClient.adminCmds, []string{"!admin users force-leave-room @nov11:localhost !team:localhost"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("admin commands=%v, want %v", got, want)
	}
}
