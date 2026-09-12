package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss"
)

type fakeTeamMatrix struct {
	createRooms  []matrix.CreateRoomRequest
	members      map[string][]matrix.RoomMember
	listErrs     map[string]error
	tokenUsers   map[string]string
	leaves       []string
	joins        []roomUserCall
	kicks        []roomUserCall
	tokenKicks   []roomUserCall
	kickErr      error
	adminCmds    []string
	deletedAlias []string
	roomNames    []roomNameCall
	roomStates   []roomStateCall
	tokenInvites []roomUserCall
	created      bool

	// powerStates seeds m.room.power_levels reads per room; an absent room
	// reports (nil, nil) — the legacy "state never set" case.
	powerStates   map[string]map[string]interface{}
	powerStateErr error

	// --- Matrix room-auth model (spec v8 rules) — the fake enforces
	// authorization on power-level writes and kicks so that passing
	// tests prove the controller survives a real homeserver's
	// strict-greater rule, not just its own JSON round-trip.

	// adminUser is the user ID behind the "" (homeserver-admin) token.
	adminUser string
	// roomCreators maps roomID -> creator user ID. A creator that is not
	// explicitly listed in the users map still holds the implicit level
	// 100 (room versions 1-11); rooms without an entry are created by
	// the admin.
	roomCreators map[string]string
	// adminIsMember, when non-nil, makes the admin's membership explicit
	// per room (absent room = NOT a member) — the TeamAdmin-owned-room
	// case, where the homeserver admin is deliberately excluded. When
	// nil, the admin is a member of every room (legacy default).
	adminIsMember map[string]bool
}

type roomUserCall struct {
	roomID string
	userID string
}

type roomNameCall struct {
	roomID string
	name   string
	token  string
}

type roomStateCall struct {
	roomID    string
	eventType string
	stateKey  string
	content   map[string]interface{}
	token     string
}

func newFakeTeamMatrix() *fakeTeamMatrix {
	return &fakeTeamMatrix{
		members:    make(map[string][]matrix.RoomMember),
		listErrs:   make(map[string]error),
		tokenUsers: make(map[string]string),
		created:    true,
		adminUser:  "@admin:localhost",
	}
}

func (f *fakeTeamMatrix) EnsureUser(context.Context, matrix.EnsureUserRequest) (*matrix.UserCredentials, error) {
	return &matrix.UserCredentials{
		Password:    "matrix-password",
		AccessToken: "matrix-token",
	}, nil
}

func (f *fakeTeamMatrix) CreateRoom(_ context.Context, req matrix.CreateRoomRequest) (*matrix.RoomInfo, error) {
	f.createRooms = append(f.createRooms, req)
	roomID := "!team:localhost"
	if strings.Contains(req.RoomAliasName, "leader-dm") {
		roomID = "!leader-dm:localhost"
	} else if strings.Contains(req.RoomAliasName, "manager") {
		roomID = "!manager:localhost"
	} else if strings.Contains(req.RoomAliasName, "worker") {
		if f.created {
			roomID = "!worker-new:localhost"
		} else {
			roomID = "!worker-old:localhost"
		}
	}
	if req.CreatorToken == "" && f.created {
		f.members[roomID] = []matrix.RoomMember{{UserID: "@admin:localhost", Membership: "join"}}
	}
	if f.created && len(req.PowerLevels) > 0 {
		// Wire-faithful: the homeserver records the creator-time power
		// level overrides as the room's m.room.power_levels state.
		if f.powerStates == nil {
			f.powerStates = map[string]map[string]interface{}{}
		}
		users := map[string]interface{}{}
		for uid, lvl := range req.PowerLevels {
			users[uid] = float64(lvl)
		}
		f.powerStates[roomID] = map[string]interface{}{"users": users}
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

func (f *fakeTeamMatrix) DeleteRoomAlias(_ context.Context, alias string) error {
	f.deletedAlias = append(f.deletedAlias, alias)
	if strings.Contains(alias, "agentteams-worker-") {
		f.created = true
	}
	return nil
}

func (f *fakeTeamMatrix) SetRoomName(_ context.Context, roomID, name, token string) error {
	f.roomNames = append(f.roomNames, roomNameCall{roomID: roomID, name: name, token: token})
	return nil
}

func (f *fakeTeamMatrix) SetRoomState(_ context.Context, roomID, eventType, stateKey string, content map[string]interface{}, token string) error {
	f.roomStates = append(f.roomStates, roomStateCall{
		roomID:    roomID,
		eventType: eventType,
		stateKey:  stateKey,
		content:   content,
		token:     token,
	})
	if eventType == "m.room.power_levels" {
		// Authorization-aware: a rejected write stores nothing, exactly
		// like a real homeserver would refuse the event.
		if err := f.authorizePowerLevelWrite(roomID, token, content); err != nil {
			return err
		}
		// JSON round-trip so stored state matches what the homeserver
		// would return (numbers as float64 in map[string]interface{}).
		data, _ := json.Marshal(content)
		var rt map[string]interface{}
		if err := json.Unmarshal(data, &rt); err != nil {
			rt = content
		}
		if f.powerStates == nil {
			f.powerStates = map[string]map[string]interface{}{}
		}
		f.powerStates[roomID] = rt
	}
	return nil
}

func (f *fakeTeamMatrix) GetRoomState(_ context.Context, roomID, eventType, _ string, token string) (map[string]interface{}, error) {
	if eventType != "m.room.power_levels" {
		return nil, nil
	}
	// State reads are membership-scoped: a non-member reader (the
	// homeserver admin in a TeamAdmin-owned room) is rejected, exactly
	// like the real endpoint.
	if sender := f.senderFor(token); sender == "" {
		return nil, fakeUnknownToken()
	} else if !f.isMember(roomID, sender) {
		return nil, fakeForbidden("not a member of room " + roomID)
	}
	if f.powerStateErr != nil {
		return nil, f.powerStateErr
	}
	if f.powerStates == nil {
		return nil, nil // legacy room: state never set
	}
	st, ok := f.powerStates[roomID]
	if !ok {
		return nil, nil
	}
	// Wire-faithful round-trip: the real endpoint returns a fresh JSON
	// decode, never a shared reference. Returning the stored map directly
	// would let a caller's mutation leak into the "server" state before
	// the write is authorized.
	data, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (f *fakeTeamMatrix) JoinRoom(_ context.Context, roomID, token string) error {
	f.joins = append(f.joins, roomUserCall{roomID: roomID, userID: token})
	if userID := f.tokenUsers[token]; userID != "" {
		f.members[roomID] = append(f.members[roomID], matrix.RoomMember{UserID: userID, Membership: "join"})
	}
	return nil
}

func (f *fakeTeamMatrix) LeaveRoom(_ context.Context, roomID, token string) error {
	f.leaves = append(f.leaves, roomID)
	uid := f.senderFor(token)
	if uid == "" {
		return fakeUnknownToken()
	}
	if !f.hasMembership(roomID, uid) {
		return nil // already out — idempotent (client maps 404 -> nil)
	}
	f.removeMember(roomID, uid)
	return nil
}

func (f *fakeTeamMatrix) SendMessage(context.Context, string, string, string) error { return nil }

func (f *fakeTeamMatrix) SendMessageAsAdmin(context.Context, string, string) error { return nil }

func (f *fakeTeamMatrix) Login(context.Context, string, string) (string, error) { return "", nil }

// InvalidateUserToken is a no-op: the fake holds no login-token cache.
func (f *fakeTeamMatrix) InvalidateUserToken(string) {}

func (f *fakeTeamMatrix) SetDisplayName(context.Context, string, string, string) error { return nil }

func (f *fakeTeamMatrix) AdminCommand(_ context.Context, cmd string) error {
	f.adminCmds = append(f.adminCmds, cmd)
	return nil
}

func (f *fakeTeamMatrix) ListJoinedRooms(context.Context, string) ([]string, error) { return nil, nil }

func (f *fakeTeamMatrix) ListRoomMembers(_ context.Context, roomID string) ([]matrix.RoomMember, error) {
	if err := f.listErrs[roomID]; err != nil {
		return nil, err
	}
	return f.members[roomID], nil
}

func (f *fakeTeamMatrix) ListRoomMembersWithToken(_ context.Context, roomID, _ string) ([]matrix.RoomMember, error) {
	if err := f.listErrs[roomID]; err != nil {
		return nil, err
	}
	return f.members[roomID], nil
}

func (f *fakeTeamMatrix) InviteToRoom(_ context.Context, roomID, userID string) error {
	if err := f.authorizeInvite(roomID, "", userID); err != nil {
		return err
	}
	f.members[roomID] = append(f.members[roomID], matrix.RoomMember{UserID: userID, Membership: "invite"})
	return nil
}

func (f *fakeTeamMatrix) InviteToRoomWithToken(_ context.Context, roomID, userID, token string) error {
	f.tokenInvites = append(f.tokenInvites, roomUserCall{roomID: roomID, userID: userID})
	if err := f.authorizeInvite(roomID, token, userID); err != nil {
		return err
	}
	f.members[roomID] = append(f.members[roomID], matrix.RoomMember{UserID: userID, Membership: "invite"})
	return nil
}

func (f *fakeTeamMatrix) KickFromRoom(_ context.Context, roomID, userID, _ string) error {
	f.kicks = append(f.kicks, roomUserCall{roomID: roomID, userID: userID})
	if f.kickErr != nil {
		return f.kickErr
	}
	return f.enforceKick(roomID, "", userID)
}

func (f *fakeTeamMatrix) KickFromRoomWithToken(_ context.Context, roomID, userID, _ string, token string) error {
	f.tokenKicks = append(f.tokenKicks, roomUserCall{roomID: roomID, userID: userID})
	if f.kickErr != nil {
		return f.kickErr
	}
	return f.enforceKick(roomID, token, userID)
}

// enforceKick runs the spec room-auth rule 4.5.4 for a kick and performs
// it when authorized: the kicker must be a member with at least the kick
// level (default 50) and the target's level must be STRICTLY below the
// kicker's. A missing target maps to nil (the real client treats the 404
// as idempotent success).
func (f *fakeTeamMatrix) enforceKick(roomID, token, userID string) error {
	if err := f.authorizeKick(roomID, token, userID); err != nil {
		if apiErr, ok := err.(*matrix.APIError); ok && apiErr.StatusCode == 404 {
			return nil
		}
		return err
	}
	f.removeMember(roomID, userID)
	return nil
}

func (f *fakeTeamMatrix) SyncMessages(context.Context, string, time.Duration) (*matrix.SyncMessagesResult, error) {
	return &matrix.SyncMessagesResult{}, nil
}

func (f *fakeTeamMatrix) UserID(localpart string) string {
	return "@" + localpart + ":localhost"
}

type fakeCredentialStore map[string]*WorkerCredentials

func (f fakeCredentialStore) Load(_ context.Context, workerName string) (*WorkerCredentials, error) {
	return f[workerName], nil
}

func (f fakeCredentialStore) Save(_ context.Context, workerName string, creds *WorkerCredentials) error {
	f[workerName] = creds
	return nil
}

func (f fakeCredentialStore) Delete(_ context.Context, workerName string) error {
	delete(f, workerName)
	return nil
}

func (f fakeCredentialStore) List(_ context.Context) ([]string, error) {
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

type fakeGateway struct{}

func (fakeGateway) EnsureConsumer(context.Context, gateway.ConsumerRequest) (*gateway.ConsumerResult, error) {
	return &gateway.ConsumerResult{}, nil
}
func (fakeGateway) DeleteConsumer(context.Context, string) error                  { return nil }
func (fakeGateway) AuthorizeAIRoutes(context.Context, string, string) error       { return nil }
func (fakeGateway) DeauthorizeAIRoutes(context.Context, string, string) error     { return nil }
func (fakeGateway) ExposePort(context.Context, gateway.PortExposeRequest) error   { return nil }
func (fakeGateway) UnexposePort(context.Context, gateway.PortExposeRequest) error { return nil }
func (fakeGateway) EnsureServiceSource(context.Context, string, string, int, string) error {
	return nil
}
func (fakeGateway) EnsureStaticServiceSource(context.Context, string, string, int) error { return nil }
func (fakeGateway) EnsureRoute(context.Context, string, []string, string, int, string) error {
	return nil
}
func (fakeGateway) DeleteRoute(context.Context, string) error                         { return nil }
func (fakeGateway) EnsureAIProvider(context.Context, gateway.AIProviderRequest) error { return nil }
func (fakeGateway) EnsureStreamIdleTimeout(context.Context, int) error                { return nil }
func (fakeGateway) EnsureAIRoute(context.Context, gateway.AIRouteRequest) error       { return nil }
func (fakeGateway) ResolveModelProvider(context.Context, string) (*gateway.ModelProviderInfo, error) {
	return nil, gateway.ErrUnsupportedOp
}
func (fakeGateway) Healthy(context.Context) error { return nil }

type fakeStorageAdmin struct {
	users    []storageUserCall
	policies []oss.PolicyRequest
}

type storageUserCall struct {
	name     string
	password string
}

func (f *fakeStorageAdmin) EnsureUser(_ context.Context, username, password string) error {
	f.users = append(f.users, storageUserCall{name: username, password: password})
	return nil
}

func (f *fakeStorageAdmin) EnsurePolicy(_ context.Context, req oss.PolicyRequest) error {
	f.policies = append(f.policies, req)
	return nil
}

func (f *fakeStorageAdmin) DeleteUser(context.Context, string) error { return nil }

func TestRefreshWorkerCredentialsRestoresMinIOAccess(t *testing.T) {
	admin := &fakeStorageAdmin{}
	creds := fakeCredentialStore{
		"alpha-worker-lead": {
			MatrixToken:   "leader-token",
			MinIOPassword: "minio-secret",
			GatewayKey:    "gateway-key",
		},
	}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   newFakeTeamMatrix(),
		Creds:    creds,
		OSSAdmin: admin,
	})

	result, err := p.RefreshWorkerCredentials(context.Background(), "alpha-worker-lead", "leader", "alpha")
	if err != nil {
		t.Fatalf("RefreshWorkerCredentials: %v", err)
	}
	if result.MinIOPassword != "minio-secret" {
		t.Fatalf("MinIOPassword=%q, want minio-secret", result.MinIOPassword)
	}
	if len(admin.users) != 1 {
		t.Fatalf("EnsureUser calls=%d, want 1", len(admin.users))
	}
	if got := admin.users[0]; got.name != "leader" || got.password != "minio-secret" {
		t.Fatalf("EnsureUser=%+v, want leader/minio-secret", got)
	}
	if len(admin.policies) != 1 {
		t.Fatalf("EnsurePolicy calls=%d, want 1", len(admin.policies))
	}
	if got := admin.policies[0]; got.WorkerName != "leader" || got.TeamName != "alpha" {
		t.Fatalf("EnsurePolicy=%+v, want worker leader team alpha", got)
	}
}

func TestRefreshManagerCredentialsRestoresMinIOAccess(t *testing.T) {
	admin := &fakeStorageAdmin{}
	creds := fakeCredentialStore{
		"default": {
			MatrixToken:   "manager-token",
			MinIOPassword: "manager-minio-secret",
			GatewayKey:    "manager-gateway-key",
		},
	}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:   newFakeTeamMatrix(),
		Creds:    creds,
		OSSAdmin: admin,
	})

	result, err := p.RefreshManagerCredentials(context.Background(), "default")
	if err != nil {
		t.Fatalf("RefreshManagerCredentials: %v", err)
	}
	if result.MinIOPassword != "manager-minio-secret" {
		t.Fatalf("MinIOPassword=%q, want manager-minio-secret", result.MinIOPassword)
	}
	if len(admin.users) != 1 {
		t.Fatalf("EnsureUser calls=%d, want 1", len(admin.users))
	}
	if got := admin.users[0]; got.name != "default" || got.password != "manager-minio-secret" {
		t.Fatalf("EnsureUser=%+v, want default/manager-minio-secret", got)
	}
	if len(admin.policies) != 1 {
		t.Fatalf("EnsurePolicy calls=%d, want 1", len(admin.policies))
	}
	if got := admin.policies[0]; got.WorkerName != "default" || !got.IsManager {
		t.Fatalf("EnsurePolicy=%+v, want Manager policy for default", got)
	}
}

func TestProvisionWorkerFreshCredentialsRecreatesStaleRoomAlias(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	matrixClient.created = false
	creds := fakeCredentialStore{}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:       matrixClient,
		Gateway:      fakeGateway{},
		Creds:        creds,
		MatrixDomain: "localhost",
		AdminUser:    "admin",
	})

	result, err := p.ProvisionWorker(context.Background(), WorkerProvisionRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("ProvisionWorker: %v", err)
	}
	if result.RoomID != "!worker-new:localhost" {
		t.Fatalf("RoomID=%q, want fresh worker room", result.RoomID)
	}
	if got, want := matrixClient.deletedAlias, []string{"#agentteams-worker-alice:localhost"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deleted aliases=%v, want %v", got, want)
	}
	if len(matrixClient.createRooms) != 2 {
		t.Fatalf("CreateRoom calls=%d, want 2", len(matrixClient.createRooms))
	}
	if got := matrixClient.joins; len(got) != 1 || got[0].roomID != "!worker-new:localhost" {
		t.Fatalf("joins=%v, want worker join only fresh room", got)
	}
	if len(matrixClient.roomStates) != 1 {
		t.Fatalf("room state calls=%d, want 1", len(matrixClient.roomStates))
	}
	state := matrixClient.roomStates[0]
	if state.roomID != "!worker-new:localhost" || state.eventType != "room.meta" || state.stateKey != "" {
		t.Fatalf("worker room state call=%+v", state)
	}
	if got := state.content["roomKind"]; got != "worker_room" {
		t.Fatalf("worker roomKind=%v, want worker_room", got)
	}
	if got := state.content["workerName"]; got != "alice" {
		t.Fatalf("workerName=%v, want alice", got)
	}
}

func TestProvisionWorkerTeamMemberRoomMeta(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	creds := fakeCredentialStore{}
	p := NewProvisioner(ProvisionerConfig{
		Matrix:       matrixClient,
		Gateway:      fakeGateway{},
		Creds:        creds,
		MatrixDomain: "localhost",
		AdminUser:    "admin",
	})

	_, err := p.ProvisionWorker(context.Background(), WorkerProvisionRequest{
		Name:           "dev",
		Role:           "worker",
		TeamName:       "alpha",
		TeamLeaderName: "lead",
	})
	if err != nil {
		t.Fatalf("ProvisionWorker: %v", err)
	}
	state := requireRoomState(t, matrixClient, "!worker-new:localhost")
	if got := state.content["teamName"]; got != "alpha" {
		t.Fatalf("teamName=%v, want alpha", got)
	}
	leader, ok := state.content["leaderWorker"].(map[string]interface{})
	if !ok {
		t.Fatalf("leaderWorker=%T, want map", state.content["leaderWorker"])
	}
	if got := leader["userId"]; got != "@lead:localhost" {
		t.Fatalf("leader userId=%v, want @lead:localhost", got)
	}
	if got := leader["workerName"]; got != "lead" {
		t.Fatalf("leader workerName=%v, want lead", got)
	}
}

func TestProvisionManagerWritesDirectRoomMeta(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		Gateway:   fakeGateway{},
		Creds:     fakeCredentialStore{},
		AdminUser: "admin",
	})

	result, err := p.ProvisionManager(context.Background(), ManagerProvisionRequest{Name: "default"})
	if err != nil {
		t.Fatalf("ProvisionManager: %v", err)
	}
	if result.RoomID != "!manager:localhost" {
		t.Fatalf("manager room=%q, want !manager:localhost", result.RoomID)
	}
	state := requireRoomState(t, matrixClient, "!manager:localhost")
	if got := state.content["roomKind"]; got != "direct_room" {
		t.Fatalf("manager roomKind=%v, want direct_room", got)
	}
	if got := state.content["managerName"]; got != "default" {
		t.Fatalf("managerName=%v, want default", got)
	}
}

func TestArchiveTeamRoomsMarksRoomNamesDeleted(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
	})

	if err := p.ArchiveTeamRooms(context.Background(), TeamRoomArchiveRequest{
		TeamName:       "alpha",
		LeaderName:     "lead",
		TeamRoomID:     "!team:localhost",
		LeaderDMRoomID: "!leader-dm:localhost",
		ActorToken:     "team-admin-token",
	}); err != nil {
		t.Fatalf("ArchiveTeamRooms: %v", err)
	}

	want := []roomNameCall{
		{roomID: "!team:localhost", name: "Team: alpha [deleted]", token: "team-admin-token"},
		{roomID: "!leader-dm:localhost", name: "Leader DM: lead [deleted]", token: "team-admin-token"},
	}
	if !reflect.DeepEqual(matrixClient.roomNames, want) {
		t.Fatalf("room names=%v, want %v", matrixClient.roomNames, want)
	}
}
func (f *fakeTeamMatrix) EnsureAppServiceUser(_ context.Context, username string) (*matrix.UserCredentials, error) {
	return &matrix.UserCredentials{UserID: f.UserID(username), AccessToken: "as-token-" + username, Created: true}, nil
}

func (f *fakeTeamMatrix) LoginAppServiceUser(_ context.Context, username string) (string, error) {
	return "as-token-" + username, nil
}

func (f *fakeTeamMatrix) SetPasswordAsAdmin(_ context.Context, _, _ string) error { return nil }

func (f *fakeTeamMatrix) RegisterAppService(_ context.Context, _ matrix.AppServiceRegistration) error {
	return nil
}
func (f *fakeTeamMatrix) UnregisterAppService(_ context.Context, _ string) error { return nil }
func (f *fakeTeamMatrix) AppServiceSmokeTest(_ context.Context) error            { return nil }

func (f *fakeTeamMatrix) VerifyAccessToken(_ context.Context, _ string) error { return nil }
func (f *fakeTeamMatrix) Whoami(_ context.Context, _ string) (string, error)  { return "", nil }

func TestProvisionTeamRoomsInvitesExplicitTeamAdminAndLeavesNewLeaderDM(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	matrixClient.tokenUsers["team-admin-token"] = "@alice:example.com"
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
	teamState := requireRoomState(t, matrixClient, "!team:localhost")
	if got := teamState.content["roomKind"]; got != "team_room" {
		t.Fatalf("team roomKind=%v, want team_room", got)
	}
	if got := teamState.content["teamName"]; got != "alpha" {
		t.Fatalf("teamName=%v, want alpha", got)
	}
	admin, ok := teamState.content["teamAdmin"].(map[string]interface{})
	if !ok {
		t.Fatalf("teamAdmin=%T, want map", teamState.content["teamAdmin"])
	}
	if got := admin["userId"]; got != "@alice:example.com" {
		t.Fatalf("team admin userId=%v, want @alice:example.com", got)
	}
	leader, ok := teamState.content["leaderWorker"].(map[string]interface{})
	if !ok {
		t.Fatalf("leaderWorker=%T, want map", teamState.content["leaderWorker"])
	}
	if got := leader["userId"]; got != "@lead:localhost" {
		t.Fatalf("leader userId=%v, want @lead:localhost", got)
	}
	leaderDMState := requireRoomState(t, matrixClient, "!leader-dm:localhost")
	if got := leaderDMState.content["roomKind"]; got != "direct_room" {
		t.Fatalf("leader DM roomKind=%v, want direct_room", got)
	}
	if got := leaderDMState.token; got != "team-admin-token" {
		t.Fatalf("leader DM state token=%q, want team-admin-token", got)
	}
}

func TestProvisionTeamRoomsInvitesCoordinatorMembersLikeTeamAdmin(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	matrixClient.tokenUsers["team-admin-token"] = "@alice:example.com"
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
	teamState := requireRoomState(t, matrixClient, "!team:localhost")
	members, ok := teamState.content["humanMembers"].([]map[string]interface{})
	if !ok {
		t.Fatalf("humanMembers=%T, want []map[string]interface{}", teamState.content["humanMembers"])
	}
	wantMembers := []map[string]interface{}{
		{"userId": "@bob:example.com", "name": "bob"},
		{"userId": "@carol:localhost", "name": "carol"},
	}
	if !reflect.DeepEqual(members, wantMembers) {
		t.Fatalf("humanMembers=%v, want %v", members, wantMembers)
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

func TestProvisionTeamRoomsSkipsNewFallbackLeaderDMReconcileWithoutJoinedActor(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	matrixClient.listErrs["!leader-dm:localhost"] = errors.New("M_FORBIDDEN: not a room member")
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
	if got, want := matrixClient.createRooms[1].Invite, []string{"@lead:localhost", "@admin:localhost"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("leader DM invites=%v, want %v", got, want)
	}
}

func TestProvisionTeamRoomsDerivesTeamAdminMatrixIDFromName(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	matrixClient.tokenUsers["team-admin-token"] = "@alice:localhost"
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
	matrixClient.tokenUsers["team-admin-token"] = "@alice:localhost"
	// Existing rooms: alice (the team admin) created and owns both.
	matrixClient.seedOwnedRoom("!team:localhost", "@alice:localhost")
	matrixClient.seedOwnedRoom("!leader-dm:localhost", "@alice:localhost")
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

func TestProvisionTeamRoomsLeaderJoinsExistingFallbackLeaderDMBeforeReconcile(t *testing.T) {
	matrixClient := newFakeTeamMatrix()
	// Existing leader DM was created with the fallback power levels.
	matrixClient.powerStates = map[string]map[string]interface{}{
		"!leader-dm:localhost": {"users": map[string]interface{}{
			"@lead:localhost":    100.0,
			"@admin:localhost":   100.0,
			"@manager:localhost": 100.0,
		}},
	}
	matrixClient.created = false
	matrixClient.tokenUsers["leader-token"] = "@lead:localhost"
	p := NewProvisioner(ProvisionerConfig{
		Matrix:    matrixClient,
		AdminUser: "admin",
		Creds: fakeCredentialStore{
			"lead-cr": {MatrixToken: "leader-token"},
		},
	})

	_, err := p.ProvisionTeamRooms(context.Background(), TeamRoomRequest{
		TeamName:             "alpha",
		LeaderName:           "lead",
		LeaderCredentialName: "lead-cr",
	})
	if err != nil {
		t.Fatalf("ProvisionTeamRooms: %v", err)
	}
	wantJoins := []roomUserCall{{roomID: "!leader-dm:localhost", userID: "leader-token"}}
	if !reflect.DeepEqual(matrixClient.joins, wantJoins) {
		t.Fatalf("joins=%v, want %v", matrixClient.joins, wantJoins)
	}
	wantInvites := []roomUserCall{{roomID: "!leader-dm:localhost", userID: "@admin:localhost"}}
	if !reflect.DeepEqual(matrixClient.tokenInvites, wantInvites) {
		t.Fatalf("token invites=%v, want %v", matrixClient.tokenInvites, wantInvites)
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
	matrixClient.tokenUsers["team-admin-token"] = "@alice:localhost"
	// Existing rooms: alice (the team admin) created and owns both.
	matrixClient.seedOwnedRoom("!team:localhost", "@alice:localhost")
	matrixClient.seedOwnedRoom("!leader-dm:localhost", "@alice:localhost")
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
	// alice already owns (is a member of) both rooms, so the reconcile
	// only invites the MISSING members — and does so with the team-admin
	// token, never an admin kick.
	wantInvites := []roomUserCall{
		{roomID: "!team:localhost", userID: "@lead:localhost"},
		{roomID: "!team:localhost", userID: "@dev:localhost"},
		{roomID: "!leader-dm:localhost", userID: "@lead:localhost"},
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

func requireRoomState(t *testing.T, matrixClient *fakeTeamMatrix, roomID string) roomStateCall {
	t.Helper()
	for _, call := range matrixClient.roomStates {
		if call.roomID == roomID && call.eventType == "room.meta" && call.stateKey == "" {
			return call
		}
	}
	t.Fatalf("room.meta state for %s not found in %+v", roomID, matrixClient.roomStates)
	return roomStateCall{}
}

// --- Authorization-aware fake helpers (spec v8 room-auth rules) ---

func fakeForbidden(msg string) *matrix.APIError {
	return &matrix.APIError{StatusCode: 403, ErrCode: "M_FORBIDDEN", Message: msg}
}

func fakeUnknownToken() *matrix.APIError {
	return &matrix.APIError{StatusCode: 401, ErrCode: "M_UNKNOWN_TOKEN", Message: "unknown token"}
}

// senderFor resolves the Matrix user ID behind a client token ("" = the
// homeserver admin). Returns "" for unknown tokens.
func (f *fakeTeamMatrix) senderFor(token string) string {
	if token == "" {
		return f.adminUser
	}
	return f.tokenUsers[token]
}

// isMember reports whether uid is a joined/invited member of roomID. The
// homeserver admin is a member of every room by default; when
// adminIsMember is set, its membership becomes explicit per room (absent
// room = not a member) — the TeamAdmin-owned-room case.
func (f *fakeTeamMatrix) isMember(roomID, uid string) bool {
	if uid == "" {
		return false
	}
	// The creator is always a joined member of their room.
	if uid == f.creatorIn(roomID) {
		return true
	}
	if uid == f.adminUser {
		if f.adminIsMember != nil {
			return f.adminIsMember[roomID]
		}
		return true
	}
	return f.hasMembership(roomID, uid)
}

func (f *fakeTeamMatrix) hasMembership(roomID, uid string) bool {
	for _, m := range f.members[roomID] {
		if m.UserID == uid && (m.Membership == "join" || m.Membership == "invite") {
			return true
		}
	}
	return false
}

func (f *fakeTeamMatrix) removeMember(roomID, uid string) {
	next := f.members[roomID][:0]
	for _, m := range f.members[roomID] {
		if m.UserID != uid {
			next = append(next, m)
		}
	}
	f.members[roomID] = next
}

// creatorIn returns the room's creator — an implicit level 100 in room
// versions 1-11 when not explicitly listed in the users map.
func (f *fakeTeamMatrix) creatorIn(roomID string) string {
	if c := f.roomCreators[roomID]; c != "" {
		return c
	}
	return f.adminUser
}

// levelIn resolves a user's current power level: explicit users-map
// entry, else the implicit creator level 100, else users_default, else 0.
func (f *fakeTeamMatrix) levelIn(roomID, uid string) int {
	if st, ok := f.powerStates[roomID]; ok {
		if users, ok := st["users"].(map[string]interface{}); ok {
			if v, ok := users[uid]; ok {
				if n, ok := v.(float64); ok {
					return int(n)
				}
			}
		}
	}
	if uid == f.creatorIn(roomID) {
		return 100
	}
	if st, ok := f.powerStates[roomID]; ok {
		if d, ok := st["users_default"].(float64); ok {
			return int(d)
		}
	}
	return 0
}

// authorizePowerLevelWrite enforces the spec v8 room-auth rule 9 for an
// m.room.power_levels write by the sender behind token:
//   - the sender must be a member with at least the event's required
//     level (default 100);
//   - 9.3: scalar fields may not move above the sender's level;
//   - 9.4/9.5: events/notifications entries likewise;
//   - 9.6: no users entry OTHER than the sender's own may be changed or
//     removed unless the sender's level is strictly greater than the
//     entry's current level — an equal-level demotion (admin 100 vs
//     human 100) is rejected: this is the deadlock the controller's
//     self-demotion fallback exists for;
//   - 9.7: any users entry may not be raised above the sender's level.
func (f *fakeTeamMatrix) authorizePowerLevelWrite(roomID, token string, content map[string]interface{}) error {
	sender := f.senderFor(token)
	if sender == "" {
		return fakeUnknownToken()
	}
	if !f.isMember(roomID, sender) {
		return fakeForbidden("not a member of room " + roomID)
	}
	prev := f.powerStates[roomID]
	senderLevel := f.levelIn(roomID, sender)

	required := 100
	if prev != nil {
		if ev, ok := prev["events"].(map[string]interface{}); ok {
			if v, ok := ev["m.room.power_levels"].(float64); ok {
				required = int(v)
			}
		}
	}
	if senderLevel < required {
		return fakeForbidden("insufficient power level to send m.room.power_levels")
	}

	var curUsers, newUsers map[string]interface{}
	if prev != nil {
		curUsers, _ = prev["users"].(map[string]interface{})
	}
	newUsers, _ = content["users"].(map[string]interface{})

	// Rule 9.3 — scalar fields.
	for _, field := range []string{"users_default", "events_default", "state_default", "ban", "redact", "kick", "invite"} {
		_, present := content[field]
		if !present {
			continue
		}
		cur := 0
		if prev != nil {
			if v, ok := prev[field].(float64); ok {
				cur = int(v)
			}
		}
		if cur > senderLevel {
			return fakeForbidden("cannot alter " + field + " above the sender's level")
		}
		if nv, ok := content[field].(float64); ok && int(nv) > senderLevel {
			return fakeForbidden("cannot raise " + field + " above the sender's level")
		}
	}

	// Rule 9.4/9.5 — events / notifications entries.
	for _, field := range []string{"events", "notifications"} {
		var cur, nw map[string]interface{}
		if prev != nil {
			cur, _ = prev[field].(map[string]interface{})
		}
		nw, _ = content[field].(map[string]interface{})
		for k, v := range nw {
			nv, ok := v.(float64)
			if !ok {
				continue
			}
			cv, existed := cur[k]
			cvi := 0
			if cf, ok := cv.(float64); ok {
				cvi = int(cf)
			}
			if existed && cvi > senderLevel {
				return fakeForbidden("cannot alter " + field + " " + k + " above the sender's level")
			}
			if int(nv) > senderLevel {
				return fakeForbidden("cannot set " + field + " " + k + " above the sender's level")
			}
		}
		for k, v := range cur {
			if _, kept := nw[k]; kept {
				continue
			}
			if cf, ok := v.(float64); ok && int(cf) > senderLevel {
				return fakeForbidden("cannot remove " + field + " " + k + " above the sender's level")
			}
		}
	}

	// Rule 9.6 — other users' entries: strict-greater on the CURRENT
	// level (equal is rejected). The sender's own entry is exempt.
	for k, cv := range curUsers {
		cvi, ok := cv.(float64)
		if !ok {
			continue
		}
		nv, kept := newUsers[k]
		nvi := 0
		if cf, ok := nv.(float64); ok {
			nvi = int(cf)
		}
		changed := !kept || nvi != int(cvi)
		if !changed {
			continue
		}
		if k != sender && int(cvi) >= senderLevel {
			return fakeForbidden("cannot change " + k + " from " + strconv.Itoa(int(cvi)) +
				" to " + strconv.Itoa(nvi) + ": the target's level is not below the sender's " +
				strconv.Itoa(senderLevel))
		}
	}

	// Rule 9.7 — no entry (own or other) may be raised above the sender.
	for k, v := range newUsers {
		nv, ok := v.(float64)
		if !ok {
			continue
		}
		cv, existed := curUsers[k]
		changedOrAdded := !existed
		if !changedOrAdded {
			if cf, ok := cv.(float64); ok && int(cf) != int(nv) {
				changedOrAdded = true
			}
		}
		if changedOrAdded && int(nv) > senderLevel {
			return fakeForbidden("cannot grant " + k + " level " + strconv.Itoa(int(nv)) +
				" above the sender's " + strconv.Itoa(senderLevel))
		}
	}
	return nil
}

// authorizeKick enforces spec room-auth rule 4.5.4 for a kick: the kicker
// must be a member with at least the kick level (default 50), and the
// target's level must be STRICTLY below the kicker's. A missing target
// yields a 404 (the caller maps it to idempotent success).
func (f *fakeTeamMatrix) authorizeKick(roomID, token, targetID string) error {
	kicker := f.senderFor(token)
	if kicker == "" {
		return fakeUnknownToken()
	}
	if !f.isMember(roomID, kicker) {
		return fakeForbidden("not in room")
	}
	if !f.hasMembership(roomID, targetID) {
		return &matrix.APIError{StatusCode: 404, ErrCode: "M_NOT_FOUND", Message: "not a member of that room"}
	}
	kickerLevel := f.levelIn(roomID, kicker)
	targetLevel := f.levelIn(roomID, targetID)
	kickLevel := 50
	if st := f.powerStates[roomID]; st != nil {
		if k, ok := st["kick"].(float64); ok {
			kickLevel = int(k)
		}
	}
	if kickerLevel < kickLevel {
		return fakeForbidden("insufficient power level to kick")
	}
	if targetLevel >= kickerLevel {
		return fakeForbidden("cannot kick " + targetID + ": their power level " +
			strconv.Itoa(targetLevel) + " is not below the kicker's " + strconv.Itoa(kickerLevel))
	}
	return nil
}

// authorizeInvite enforces spec room-auth rule 4.4 for an invite: the
// inviter must be a joined member at least at the invite level (default
// 50), and the target must not already be a joined/banned member.
func (f *fakeTeamMatrix) authorizeInvite(roomID, token, targetID string) error {
	inviter := f.senderFor(token)
	if inviter == "" {
		return fakeUnknownToken()
	}
	joined := false
	for _, m := range f.members[roomID] {
		if m.UserID == inviter && m.Membership == "join" {
			joined = true
			break
		}
	}
	if !joined && !f.isMember(roomID, inviter) {
		return fakeForbidden("not a member of room " + roomID)
	}
	inviteLevel := 50
	if st := f.powerStates[roomID]; st != nil {
		if k, ok := st["invite"].(float64); ok {
			inviteLevel = int(k)
		}
	}
	if f.levelIn(roomID, inviter) < inviteLevel {
		return fakeForbidden("insufficient power level to invite")
	}
	for _, m := range f.members[roomID] {
		if m.UserID == targetID && (m.Membership == "join" || m.Membership == "ban") {
			return fakeForbidden("target is already a member")
		}
	}
	return nil
}

// seedOwnedRoom models an EXISTING room that uid created: uid is the
// creator (implicit level 100 in room versions 1-11) and a joined member.
// Used by tests for rooms created before this reconcile ran.
func (f *fakeTeamMatrix) seedOwnedRoom(roomID, uid string) {
	if f.roomCreators == nil {
		f.roomCreators = map[string]string{}
	}
	f.roomCreators[roomID] = uid
	f.members[roomID] = append(f.members[roomID], matrix.RoomMember{UserID: uid, Membership: "join"})
}
