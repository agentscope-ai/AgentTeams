package mocks

import (
	"context"
	"sync"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
)

// MockProvisioner implements service.WorkerProvisioner for testing.
type MockProvisioner struct {
	mu sync.Mutex

	ProvisionWorkerFn          func(ctx context.Context, req service.WorkerProvisionRequest) (*service.WorkerProvisionResult, error)
	DeprovisionWorkerFn        func(ctx context.Context, req service.WorkerDeprovisionRequest) error
	RefreshCredentialsFn       func(ctx context.Context, workerName string) (*service.RefreshResult, error)
	RefreshWorkerCredentialsFn func(ctx context.Context, credentialName, workerName string) (*service.RefreshResult, error)
	EnsureWorkerGatewayAuthFn  func(ctx context.Context, workerName, gatewayKey, modelProviderID string) error
	ReconcileExposeFn          func(ctx context.Context, workerName string, desired []v1beta1.ExposePort, current []v1beta1.ExposedPortStatus) ([]v1beta1.ExposedPortStatus, error)
	EnsureServiceAccountFn     func(ctx context.Context, workerName string) error
	DeleteServiceAccountFn     func(ctx context.Context, workerName string) error
	DeleteCredentialsFn        func(ctx context.Context, workerName string) error
	DeleteWorkerCredentialsFn  func(ctx context.Context, credentialName string) error
	RequestSATokenFn           func(ctx context.Context, workerName string) (string, error)
	LeaveAllWorkerRoomsFn      func(ctx context.Context, workerName string) error
	DeleteWorkerRoomFn         func(ctx context.Context, roomID string) error
	MatrixUserIDFn             func(name string) string
	LoginAsHumanFn             func(ctx context.Context, username, password string) (string, error)
	ProvisionTeamRoomsFn       func(ctx context.Context, req service.TeamRoomRequest) (*service.TeamRoomResult, error)
	DeleteTeamRoomAliasesFn    func(ctx context.Context, teamName, leaderName string) error
	DeleteWorkerRoomAliasFn    func(ctx context.Context, workerName string) error

	Calls struct {
		ProvisionWorker          []service.WorkerProvisionRequest
		DeprovisionWorker        []service.WorkerDeprovisionRequest
		RefreshCredentials       []string
		RefreshWorkerCredentials []workerCredentialCall
		EnsureWorkerGatewayAuth  []gatewayAuthCall
		ReconcileExpose          []string
		EnsureServiceAccount     []string
		DeleteServiceAccount     []string
		DeleteCredentials        []string
		DeleteWorkerCredentials  []string
		RequestSAToken           []string
		LeaveAllWorkerRooms      []string
		DeleteWorkerRoom         []string
		LoginAsHuman             []humanLoginCall
		ProvisionTeamRooms       []service.TeamRoomRequest
		DeleteTeamRoomAliases    []string
		DeleteWorkerRoomAlias    []string
	}
}

type workerCredentialCall struct {
	CredentialName string
	WorkerName     string
}

type gatewayAuthCall struct {
	Name            string
	GatewayKey      string
	ModelProviderID string
}

type humanLoginCall struct {
	Username string
	Password string
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
	m.DeleteCredentialsFn = nil
	m.DeleteWorkerCredentialsFn = nil
	m.RequestSATokenFn = nil
	m.LeaveAllWorkerRoomsFn = nil
	m.DeleteWorkerRoomFn = nil
	m.MatrixUserIDFn = nil
	m.LoginAsHumanFn = nil
	m.ProvisionTeamRoomsFn = nil
	m.DeleteTeamRoomAliasesFn = nil
	m.DeleteWorkerRoomAliasFn = nil
}

// ClearCalls resets call records only, preserving Fn overrides.
func (m *MockProvisioner) ClearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
}

func (m *MockProvisioner) clearCallsLocked() {
	m.Calls = struct {
		ProvisionWorker          []service.WorkerProvisionRequest
		DeprovisionWorker        []service.WorkerDeprovisionRequest
		RefreshCredentials       []string
		RefreshWorkerCredentials []workerCredentialCall
		EnsureWorkerGatewayAuth  []gatewayAuthCall
		ReconcileExpose          []string
		EnsureServiceAccount     []string
		DeleteServiceAccount     []string
		DeleteCredentials        []string
		DeleteWorkerCredentials  []string
		RequestSAToken           []string
		LeaveAllWorkerRooms      []string
		DeleteWorkerRoom         []string
		LoginAsHuman             []humanLoginCall
		ProvisionTeamRooms       []service.TeamRoomRequest
		DeleteTeamRoomAliases    []string
		DeleteWorkerRoomAlias    []string
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

func (m *MockProvisioner) RefreshWorkerCredentials(ctx context.Context, credentialName, workerName string) (*service.RefreshResult, error) {
	m.mu.Lock()
	m.Calls.RefreshWorkerCredentials = append(m.Calls.RefreshWorkerCredentials, workerCredentialCall{
		CredentialName: credentialName,
		WorkerName:     workerName,
	})
	fn := m.RefreshWorkerCredentialsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, credentialName, workerName)
	}
	return &service.RefreshResult{
		MatrixToken:    "mock-token-" + workerName,
		GatewayKey:     "mock-gw-key-" + workerName,
		MinIOPassword:  "mock-minio-pw",
		MatrixPassword: "mock-matrix-pw",
	}, nil
}

func (m *MockProvisioner) EnsureWorkerGatewayAuth(ctx context.Context, workerName, gatewayKey, modelProviderID string) error {
	m.mu.Lock()
	m.Calls.EnsureWorkerGatewayAuth = append(m.Calls.EnsureWorkerGatewayAuth, gatewayAuthCall{
		Name:            workerName,
		GatewayKey:      gatewayKey,
		ModelProviderID: modelProviderID,
	})
	fn := m.EnsureWorkerGatewayAuthFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName, gatewayKey, modelProviderID)
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

func (m *MockProvisioner) RequestSAToken(ctx context.Context, workerName string) (string, error) {
	m.mu.Lock()
	m.Calls.RequestSAToken = append(m.Calls.RequestSAToken, workerName)
	fn := m.RequestSATokenFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName)
	}
	return "mock-sa-token-" + workerName, nil
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
	if m.MatrixUserIDFn != nil {
		return m.MatrixUserIDFn(name)
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
	return false
}

var _ service.WorkerProvisioner = (*MockProvisioner)(nil)
