package mocks

import (
	"context"
	"sync"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// MockProvisioner implements service.WorkerProvisioner for testing.
type MockProvisioner struct {
	mu sync.Mutex

	ProvisionWorkerFn              func(ctx context.Context, req service.WorkerProvisionRequest) (*service.WorkerProvisionResult, error)
	DeprovisionWorkerFn            func(ctx context.Context, req service.WorkerDeprovisionRequest) error
	RefreshCredentialsFn           func(ctx context.Context, workerName string) (*service.RefreshResult, error)
	RefreshWorkerCredentialsFn     func(ctx context.Context, credentialName, workerName, teamName string) (*service.RefreshResult, error)
	EnsureWorkerGatewayAuthFn      func(ctx context.Context, workerName, gatewayKey string) error
	ReconcileExposeFn              func(ctx context.Context, workerName string, desired []v1beta1.ExposePort, current []v1beta1.ExposedPortStatus) ([]v1beta1.ExposedPortStatus, error)
	EnsureServiceAccountFn         func(ctx context.Context, workerName string) error
	DeleteServiceAccountFn         func(ctx context.Context, workerName string) error
	EnsureRemoteNamespaceFn        func(ctx context.Context, clusterID, namespace string) error
	EnsureRemoteServiceAccountFn   func(ctx context.Context, workerName, clusterID, namespace string) error
	DeleteRemoteServiceAccountFn   func(ctx context.Context, workerName, clusterID, namespace string) error
	DeleteCredentialsFn            func(ctx context.Context, workerName string) error
	DeleteWorkerCredentialsFn      func(ctx context.Context, credentialName string) error
	RequestSATokenFn               func(ctx context.Context, workerName string) (string, time.Time, error)
	RequestSATokenWithExpirationFn func(ctx context.Context, workerName string, expirationSeconds int64) (string, error)
	ProjectSATokenFn               func(ctx context.Context, workerName string, expirationSeconds int64) (*service.SATokenProjection, error)
	LeaveAllWorkerRoomsFn          func(ctx context.Context, workerName string) error
	DeleteWorkerRoomFn             func(ctx context.Context, roomID string) error
	MatrixUserIDFn                 func(name string) string
	LoginAsHumanFn                 func(ctx context.Context, username, password string) (string, error)
	EnsureHumanUserFn              func(ctx context.Context, name string) (*service.HumanCredentials, error)
	RegisterAppServiceUserFn       func(ctx context.Context, name string) (*service.HumanCredentials, error)
	RegisterLegacyUserFn           func(ctx context.Context, name string) (*service.HumanCredentials, error)
	SetUserPasswordFn              func(ctx context.Context, userID, password string) error
	LoginAppServiceUserFn          func(ctx context.Context, username string) (string, error)
	LoginWithPasswordFn            func(ctx context.Context, name, password string) (string, error)
	SetDisplayNameFn               func(ctx context.Context, userID, accessToken, displayName string) error
	InviteToRoomFn                 func(ctx context.Context, roomID, userID string) error
	JoinRoomAsFn                   func(ctx context.Context, roomID, userToken string) error
	KickFromRoomFn                 func(ctx context.Context, roomID, userID, reason string) error
	ForceLeaveRoomFn               func(ctx context.Context, userID, roomID string) error
	DeactivateHumanUserFn          func(ctx context.Context, userID string) error
	ProvisionTeamRoomsFn           func(ctx context.Context, req service.TeamRoomRequest) (*service.TeamRoomResult, error)
	ArchiveTeamRoomsFn             func(ctx context.Context, req service.TeamRoomArchiveRequest) error
	DeleteTeamRoomAliasesFn        func(ctx context.Context, teamName, leaderName string) error
	DeleteWorkerRoomAliasFn        func(ctx context.Context, workerName string) error

	// AppServiceEnabled controls the return value of MatrixAppServiceEnabled.
	// Defaults to false (password mode); set true to exercise AppService-mode
	// behaviour such as SSO Humans that have no initial password.
	AppServiceEnabled bool

	Calls struct {
		ProvisionWorker              []service.WorkerProvisionRequest
		DeprovisionWorker            []service.WorkerDeprovisionRequest
		RefreshCredentials           []string
		RefreshWorkerCredentials     []workerCredentialCall
		EnsureWorkerGatewayAuth      []gatewayAuthCall
		ReconcileExpose              []string
		EnsureServiceAccount         []string
		DeleteServiceAccount         []string
		EnsureRemoteNamespace        []remoteNamespaceCall
		EnsureRemoteSA               []string
		DeleteRemoteSA               []string
		DeleteCredentials            []string
		DeleteWorkerCredentials      []string
		RequestSAToken               []string
		RequestSATokenWithExpiration []tokenRequestCall
		LeaveAllWorkerRooms          []string
		DeleteWorkerRoom             []string
		LoginAsHuman                 []humanLoginCall
		EnsureHumanUser              []string
		RegisterAppServiceUser       []string
		RegisterLegacyUser           []string
		SetUserPassword              []userPasswordCall
		LoginAppServiceUser          []string
		LoginWithPassword            []humanLoginCall
		SetDisplayName               []displayNameCall
		InviteToRoom                 []roomMembershipCall
		JoinRoomAs                   []joinRoomAsCall
		KickFromRoom                 []kickFromRoomCall
		ForceLeaveRoom               []roomMembershipCall
		DeactivateHumanUser          []string
		ProvisionTeamRooms           []service.TeamRoomRequest
		ArchiveTeamRooms             []service.TeamRoomArchiveRequest
		DeleteTeamRoomAliases        []string
		DeleteWorkerRoomAlias        []string
	}
}

type workerCredentialCall struct {
	CredentialName string
	WorkerName     string
	TeamName       string
}

type gatewayAuthCall struct {
	Name       string
	GatewayKey string
}

type humanLoginCall struct {
	Username string
	Password string
}

type tokenRequestCall struct {
	WorkerName        string
	ExpirationSeconds int64
}

type remoteNamespaceCall struct {
	ClusterID string
	Namespace string
}

type userPasswordCall struct {
	UserID   string
	Password string
}

type displayNameCall struct {
	UserID      string
	AccessToken string
	DisplayName string
}

type roomMembershipCall struct {
	RoomID string
	UserID string
}

type joinRoomAsCall struct {
	RoomID    string
	UserToken string
}

type kickFromRoomCall struct {
	RoomID string
	UserID string
	Reason string
}

func NewMockProvisioner() *MockProvisioner {
	return &MockProvisioner{}
}

// Reset clears all Fn overrides and call records.
func (m *MockProvisioner) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
	m.ProvisionWorkerFn = nil
	m.DeprovisionWorkerFn = nil
	m.RefreshCredentialsFn = nil
	m.EnsureWorkerGatewayAuthFn = nil
	m.RefreshWorkerCredentialsFn = nil
	m.ReconcileExposeFn = nil
	m.EnsureServiceAccountFn = nil
	m.DeleteServiceAccountFn = nil
	m.EnsureRemoteNamespaceFn = nil
	m.EnsureRemoteServiceAccountFn = nil
	m.DeleteRemoteServiceAccountFn = nil
	m.DeleteCredentialsFn = nil
	m.DeleteWorkerCredentialsFn = nil
	m.RequestSATokenFn = nil
	m.RequestSATokenWithExpirationFn = nil
	m.ProjectSATokenFn = nil
	m.LeaveAllWorkerRoomsFn = nil
	m.DeleteWorkerRoomFn = nil
	m.MatrixUserIDFn = nil
	m.LoginAsHumanFn = nil
	m.EnsureHumanUserFn = nil
	m.RegisterAppServiceUserFn = nil
	m.RegisterLegacyUserFn = nil
	m.SetUserPasswordFn = nil
	m.LoginAppServiceUserFn = nil
	m.LoginWithPasswordFn = nil
	m.SetDisplayNameFn = nil
	m.InviteToRoomFn = nil
	m.JoinRoomAsFn = nil
	m.KickFromRoomFn = nil
	m.ForceLeaveRoomFn = nil
	m.DeactivateHumanUserFn = nil
	m.ProvisionTeamRoomsFn = nil
	m.ArchiveTeamRoomsFn = nil
	m.DeleteTeamRoomAliasesFn = nil
	m.DeleteWorkerRoomAliasFn = nil
	m.AppServiceEnabled = false
}

// ClearCalls resets call records only, preserving Fn overrides.
func (m *MockProvisioner) ClearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
}

func (m *MockProvisioner) clearCallsLocked() {
	m.Calls = struct {
		ProvisionWorker              []service.WorkerProvisionRequest
		DeprovisionWorker            []service.WorkerDeprovisionRequest
		RefreshCredentials           []string
		RefreshWorkerCredentials     []workerCredentialCall
		EnsureWorkerGatewayAuth      []gatewayAuthCall
		ReconcileExpose              []string
		EnsureServiceAccount         []string
		DeleteServiceAccount         []string
		EnsureRemoteNamespace        []remoteNamespaceCall
		EnsureRemoteSA               []string
		DeleteRemoteSA               []string
		DeleteCredentials            []string
		DeleteWorkerCredentials      []string
		RequestSAToken               []string
		RequestSATokenWithExpiration []tokenRequestCall
		LeaveAllWorkerRooms          []string
		DeleteWorkerRoom             []string
		LoginAsHuman                 []humanLoginCall
		EnsureHumanUser              []string
		RegisterAppServiceUser       []string
		RegisterLegacyUser           []string
		SetUserPassword              []userPasswordCall
		LoginAppServiceUser          []string
		LoginWithPassword            []humanLoginCall
		SetDisplayName               []displayNameCall
		InviteToRoom                 []roomMembershipCall
		JoinRoomAs                   []joinRoomAsCall
		KickFromRoom                 []kickFromRoomCall
		ForceLeaveRoom               []roomMembershipCall
		DeactivateHumanUser          []string
		ProvisionTeamRooms           []service.TeamRoomRequest
		ArchiveTeamRooms             []service.TeamRoomArchiveRequest
		DeleteTeamRoomAliases        []string
		DeleteWorkerRoomAlias        []string
	}{}
}

func (m *MockProvisioner) ProvisionWorker(ctx context.Context, req service.WorkerProvisionRequest) (*service.WorkerProvisionResult, error) {
	m.mu.Lock()
	m.Calls.ProvisionWorker = append(m.Calls.ProvisionWorker, req)
	fn := m.ProvisionWorkerFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &service.WorkerProvisionResult{
		MatrixUserID:   "@" + req.Name + ":localhost",
		MatrixToken:    "mock-token-" + req.Name,
		RoomID:         "!room-" + req.Name + ":localhost",
		GatewayKey:     "mock-gw-key-" + req.Name,
		MinIOPassword:  "mock-minio-pw",
		MatrixPassword: "mock-matrix-pw",
	}, nil
}

func (m *MockProvisioner) DeprovisionWorker(ctx context.Context, req service.WorkerDeprovisionRequest) error {
	m.mu.Lock()
	m.Calls.DeprovisionWorker = append(m.Calls.DeprovisionWorker, req)
	fn := m.DeprovisionWorkerFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (m *MockProvisioner) RefreshCredentials(ctx context.Context, workerName string) (*service.RefreshResult, error) {
	m.mu.Lock()
	m.Calls.RefreshCredentials = append(m.Calls.RefreshCredentials, workerName)
	fn := m.RefreshCredentialsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName)
	}
	return &service.RefreshResult{
		MatrixToken:    "mock-token-" + workerName,
		GatewayKey:     "mock-gw-key-" + workerName,
		MinIOPassword:  "mock-minio-pw",
		MatrixPassword: "mock-matrix-pw",
	}, nil
}

func (m *MockProvisioner) RefreshWorkerCredentials(ctx context.Context, credentialName, workerName, teamName string) (*service.RefreshResult, error) {
	m.mu.Lock()
	m.Calls.RefreshWorkerCredentials = append(m.Calls.RefreshWorkerCredentials, workerCredentialCall{
		CredentialName: credentialName,
		WorkerName:     workerName,
		TeamName:       teamName,
	})
	fn := m.RefreshWorkerCredentialsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, credentialName, workerName, teamName)
	}
	return &service.RefreshResult{
		MatrixToken:    "mock-token-" + workerName,
		GatewayKey:     "mock-gw-key-" + workerName,
		MinIOPassword:  "mock-minio-pw",
		MatrixPassword: "mock-matrix-pw",
	}, nil
}

func (m *MockProvisioner) EnsureWorkerGatewayAuth(ctx context.Context, workerName, gatewayKey string) error {
	m.mu.Lock()
	m.Calls.EnsureWorkerGatewayAuth = append(m.Calls.EnsureWorkerGatewayAuth, gatewayAuthCall{
		Name:       workerName,
		GatewayKey: gatewayKey,
	})
	fn := m.EnsureWorkerGatewayAuthFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName, gatewayKey)
	}
	return nil
}

func (m *MockProvisioner) ReconcileExpose(ctx context.Context, workerName string, desired []v1beta1.ExposePort, current []v1beta1.ExposedPortStatus) ([]v1beta1.ExposedPortStatus, error) {
	m.mu.Lock()
	m.Calls.ReconcileExpose = append(m.Calls.ReconcileExpose, workerName)
	fn := m.ReconcileExposeFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName, desired, current)
	}
	return nil, nil
}

func (m *MockProvisioner) EnsureServiceAccount(ctx context.Context, workerName string) error {
	m.mu.Lock()
	m.Calls.EnsureServiceAccount = append(m.Calls.EnsureServiceAccount, workerName)
	fn := m.EnsureServiceAccountFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName)
	}
	return nil
}

func (m *MockProvisioner) DeleteServiceAccount(ctx context.Context, workerName string) error {
	m.mu.Lock()
	m.Calls.DeleteServiceAccount = append(m.Calls.DeleteServiceAccount, workerName)
	fn := m.DeleteServiceAccountFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName)
	}
	return nil
}

func (m *MockProvisioner) EnsureRemoteNamespace(ctx context.Context, clusterID, namespace string) error {
	m.mu.Lock()
	m.Calls.EnsureRemoteNamespace = append(m.Calls.EnsureRemoteNamespace, remoteNamespaceCall{
		ClusterID: clusterID,
		Namespace: namespace,
	})
	fn := m.EnsureRemoteNamespaceFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, clusterID, namespace)
	}
	return nil
}

func (m *MockProvisioner) EnsureRemoteServiceAccount(ctx context.Context, workerName, clusterID, namespace string) error {
	m.mu.Lock()
	m.Calls.EnsureRemoteSA = append(m.Calls.EnsureRemoteSA, workerName)
	fn := m.EnsureRemoteServiceAccountFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName, clusterID, namespace)
	}
	return nil
}

func (m *MockProvisioner) DeleteRemoteServiceAccount(ctx context.Context, workerName, clusterID, namespace string) error {
	m.mu.Lock()
	m.Calls.DeleteRemoteSA = append(m.Calls.DeleteRemoteSA, workerName)
	fn := m.DeleteRemoteServiceAccountFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName, clusterID, namespace)
	}
	return nil
}

func (m *MockProvisioner) DeleteCredentials(ctx context.Context, workerName string) error {
	m.mu.Lock()
	m.Calls.DeleteCredentials = append(m.Calls.DeleteCredentials, workerName)
	fn := m.DeleteCredentialsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName)
	}
	return nil
}

func (m *MockProvisioner) DeleteWorkerCredentials(ctx context.Context, credentialName string) error {
	m.mu.Lock()
	m.Calls.DeleteWorkerCredentials = append(m.Calls.DeleteWorkerCredentials, credentialName)
	fn := m.DeleteWorkerCredentialsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, credentialName)
	}
	return nil
}

func (m *MockProvisioner) RequestSAToken(ctx context.Context, workerName string) (string, time.Time, error) {
	m.mu.Lock()
	m.Calls.RequestSAToken = append(m.Calls.RequestSAToken, workerName)
	fn := m.RequestSATokenFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName)
	}
	return "mock-sa-token-" + workerName, time.Now().Add(time.Hour), nil
}

func (m *MockProvisioner) RequestSATokenWithExpiration(ctx context.Context, workerName string, expirationSeconds int64) (string, error) {
	projection, err := m.ProjectSAToken(ctx, workerName, expirationSeconds)
	if err != nil || projection == nil {
		return "", err
	}
	return projection.Token, nil
}

func (m *MockProvisioner) ProjectSAToken(ctx context.Context, workerName string, expirationSeconds int64) (*service.SATokenProjection, error) {
	expirationSeconds = backend.NormalizeAuthTokenExpirationSeconds(expirationSeconds)
	m.mu.Lock()
	m.Calls.RequestSATokenWithExpiration = append(m.Calls.RequestSATokenWithExpiration, tokenRequestCall{
		WorkerName:        workerName,
		ExpirationSeconds: expirationSeconds,
	})
	projectFn := m.ProjectSATokenFn
	legacyFn := m.RequestSATokenWithExpirationFn
	m.mu.Unlock()
	if projectFn != nil {
		return projectFn(ctx, workerName, expirationSeconds)
	}
	if legacyFn != nil {
		token, err := legacyFn(ctx, workerName, expirationSeconds)
		if err != nil {
			return nil, err
		}
		return &service.SATokenProjection{
			Token:               token,
			IssuedAt:            time.Now(),
			ExpirationTimestamp: time.Now().Add(time.Duration(expirationSeconds) * time.Second),
			ExpirationSeconds:   expirationSeconds,
		}, nil
	}
	now := time.Now()
	return &service.SATokenProjection{
		Token:               "mock-sa-token-" + workerName,
		IssuedAt:            now,
		ExpirationTimestamp: now.Add(time.Duration(expirationSeconds) * time.Second),
		ExpirationSeconds:   expirationSeconds,
	}, nil
}

func (m *MockProvisioner) LeaveAllWorkerRooms(ctx context.Context, workerName string) error {
	m.mu.Lock()
	m.Calls.LeaveAllWorkerRooms = append(m.Calls.LeaveAllWorkerRooms, workerName)
	fn := m.LeaveAllWorkerRoomsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName)
	}
	return nil
}

func (m *MockProvisioner) DeleteWorkerRoom(ctx context.Context, roomID string) error {
	m.mu.Lock()
	m.Calls.DeleteWorkerRoom = append(m.Calls.DeleteWorkerRoom, roomID)
	fn := m.DeleteWorkerRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID)
	}
	return nil
}

func (m *MockProvisioner) MatrixUserID(name string) string {
	m.mu.Lock()
	fn := m.MatrixUserIDFn
	m.mu.Unlock()
	if fn != nil {
		return fn(name)
	}
	return "@" + name + ":localhost"
}

func (m *MockProvisioner) LoginAsHuman(ctx context.Context, username, password string) (string, error) {
	m.mu.Lock()
	m.Calls.LoginAsHuman = append(m.Calls.LoginAsHuman, humanLoginCall{Username: username, Password: password})
	fn := m.LoginAsHumanFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, username, password)
	}
	return "mock-human-token-" + username, nil
}

func (m *MockProvisioner) EnsureHumanUser(ctx context.Context, name string) (*service.HumanCredentials, error) {
	m.mu.Lock()
	m.Calls.EnsureHumanUser = append(m.Calls.EnsureHumanUser, name)
	fn := m.EnsureHumanUserFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return &service.HumanCredentials{UserID: m.MatrixUserID(name), AccessToken: "mock-human-token-" + name, Password: "mock-human-pw-" + name, Created: true}, nil
}

func (m *MockProvisioner) RegisterAppServiceUser(ctx context.Context, name string) (*service.HumanCredentials, error) {
	m.mu.Lock()
	m.Calls.RegisterAppServiceUser = append(m.Calls.RegisterAppServiceUser, name)
	fn := m.RegisterAppServiceUserFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return &service.HumanCredentials{UserID: m.MatrixUserID(name), AccessToken: "mock-as-token-" + name, Created: true}, nil
}

func (m *MockProvisioner) RegisterLegacyUser(ctx context.Context, name string) (*service.HumanCredentials, error) {
	m.mu.Lock()
	m.Calls.RegisterLegacyUser = append(m.Calls.RegisterLegacyUser, name)
	fn := m.RegisterLegacyUserFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return &service.HumanCredentials{UserID: m.MatrixUserID(name), AccessToken: "mock-legacy-token-" + name, Password: "mock-human-pw-" + name, Created: true}, nil
}

func (m *MockProvisioner) SetUserPassword(ctx context.Context, userID, password string) error {
	m.mu.Lock()
	m.Calls.SetUserPassword = append(m.Calls.SetUserPassword, userPasswordCall{UserID: userID, Password: password})
	fn := m.SetUserPasswordFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, userID, password)
	}
	return nil
}

func (m *MockProvisioner) LoginAppServiceUser(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	m.Calls.LoginAppServiceUser = append(m.Calls.LoginAppServiceUser, name)
	fn := m.LoginAppServiceUserFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return "mock-as-token-" + name, nil
}

func (m *MockProvisioner) LoginWithPassword(ctx context.Context, name, password string) (string, error) {
	m.mu.Lock()
	m.Calls.LoginWithPassword = append(m.Calls.LoginWithPassword, humanLoginCall{Username: name, Password: password})
	fn := m.LoginWithPasswordFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name, password)
	}
	return "mock-pw-token-" + name, nil
}

func (m *MockProvisioner) SetDisplayName(ctx context.Context, userID, accessToken, displayName string) error {
	m.mu.Lock()
	m.Calls.SetDisplayName = append(m.Calls.SetDisplayName, displayNameCall{UserID: userID, AccessToken: accessToken, DisplayName: displayName})
	fn := m.SetDisplayNameFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, userID, accessToken, displayName)
	}
	return nil
}

func (m *MockProvisioner) InviteToRoom(ctx context.Context, roomID, userID string) error {
	m.mu.Lock()
	m.Calls.InviteToRoom = append(m.Calls.InviteToRoom, roomMembershipCall{RoomID: roomID, UserID: userID})
	fn := m.InviteToRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, userID)
	}
	return nil
}

func (m *MockProvisioner) JoinRoomAs(ctx context.Context, roomID, userToken string) error {
	m.mu.Lock()
	m.Calls.JoinRoomAs = append(m.Calls.JoinRoomAs, joinRoomAsCall{RoomID: roomID, UserToken: userToken})
	fn := m.JoinRoomAsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, userToken)
	}
	return nil
}

func (m *MockProvisioner) KickFromRoom(ctx context.Context, roomID, userID, reason string) error {
	m.mu.Lock()
	m.Calls.KickFromRoom = append(m.Calls.KickFromRoom, kickFromRoomCall{RoomID: roomID, UserID: userID, Reason: reason})
	fn := m.KickFromRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, userID, reason)
	}
	return nil
}

func (m *MockProvisioner) ForceLeaveRoom(ctx context.Context, userID, roomID string) error {
	m.mu.Lock()
	m.Calls.ForceLeaveRoom = append(m.Calls.ForceLeaveRoom, roomMembershipCall{RoomID: roomID, UserID: userID})
	fn := m.ForceLeaveRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, userID, roomID)
	}
	return nil
}

func (m *MockProvisioner) DeactivateHumanUser(ctx context.Context, userID string) error {
	m.mu.Lock()
	m.Calls.DeactivateHumanUser = append(m.Calls.DeactivateHumanUser, userID)
	fn := m.DeactivateHumanUserFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, userID)
	}
	return nil
}

func (m *MockProvisioner) ProvisionTeamRooms(ctx context.Context, req service.TeamRoomRequest) (*service.TeamRoomResult, error) {
	m.mu.Lock()
	m.Calls.ProvisionTeamRooms = append(m.Calls.ProvisionTeamRooms, req)
	fn := m.ProvisionTeamRoomsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &service.TeamRoomResult{
		TeamRoomID:     "!team-" + req.TeamName + ":localhost",
		LeaderDMRoomID: "!leader-dm-" + req.TeamName + ":localhost",
	}, nil
}

func (m *MockProvisioner) ArchiveTeamRooms(ctx context.Context, req service.TeamRoomArchiveRequest) error {
	m.mu.Lock()
	m.Calls.ArchiveTeamRooms = append(m.Calls.ArchiveTeamRooms, req)
	fn := m.ArchiveTeamRoomsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (m *MockProvisioner) DeleteTeamRoomAliases(ctx context.Context, teamName, leaderName string) error {
	m.mu.Lock()
	m.Calls.DeleteTeamRoomAliases = append(m.Calls.DeleteTeamRoomAliases, teamName+"/"+leaderName)
	fn := m.DeleteTeamRoomAliasesFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, teamName, leaderName)
	}
	return nil
}

func (m *MockProvisioner) DeleteWorkerRoomAlias(ctx context.Context, workerName string) error {
	m.mu.Lock()
	m.Calls.DeleteWorkerRoomAlias = append(m.Calls.DeleteWorkerRoomAlias, workerName)
	fn := m.DeleteWorkerRoomAliasFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName)
	}
	return nil
}

// CallCounts returns a snapshot of call counts safe for concurrent use.
// The last slot reports LeaveAllWorkerRooms calls (which replaced the
// legacy DeactivateMatrixUser accounting).
func (m *MockProvisioner) CallCounts() (provision, deprovision, refresh, leaveAllRooms int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.ProvisionWorker),
		len(m.Calls.DeprovisionWorker),
		len(m.Calls.RefreshCredentials) + len(m.Calls.RefreshWorkerCredentials),
		len(m.Calls.LeaveAllWorkerRooms)
}

// ServiceAccountCallCounts returns EnsureServiceAccount and DeleteServiceAccount counts.
func (m *MockProvisioner) ServiceAccountCallCounts() (ensure, delete int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.EnsureServiceAccount), len(m.Calls.DeleteServiceAccount)
}

func (m *MockProvisioner) MatrixAppServiceEnabled() bool {
	return m.AppServiceEnabled
}

var _ service.WorkerProvisioner = (*MockProvisioner)(nil)
var _ service.HumanProvisioner = (*MockProvisioner)(nil)
