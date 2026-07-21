package mocks

import (
	"context"
	"sync"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// MockManagerProvisioner implements service.ManagerProvisioner for testing.
type MockManagerProvisioner struct {
	mu sync.Mutex

	ProvisionManagerFn            func(ctx context.Context, req service.ManagerProvisionRequest) (*service.ManagerProvisionResult, error)
	DeprovisionManagerFn          func(ctx context.Context, name string) error
	RefreshCredentialsFn          func(ctx context.Context, name string) (*service.RefreshResult, error)
	RefreshManagerCredentialsFn   func(ctx context.Context, managerName string) (*service.RefreshResult, error)
	EnsureManagerGatewayAuthFn    func(ctx context.Context, managerName, gatewayKey string) error
	EnsureManagerServiceAccountFn func(ctx context.Context, managerName string) error
	DeleteManagerServiceAccountFn func(ctx context.Context, managerName string) error
	DeleteCredentialsFn           func(ctx context.Context, name string) error
	RequestManagerSATokenFn       func(ctx context.Context, managerName string) (string, error)
	LeaveAllManagerRoomsFn        func(ctx context.Context, managerName string) error
	DeleteManagerRoomFn           func(ctx context.Context, roomID string) error
	DeleteManagerRoomAliasFn      func(ctx context.Context, managerName string) error
	IsManagerJoinedDMFn           func(ctx context.Context, roomID string) (bool, error)
	IsManagerLLMAuthReadyFn       func(ctx context.Context, gatewayKey string) (bool, error)
	SendManagerWelcomeMessageFn   func(ctx context.Context, req service.ManagerWelcomeRequest) error

	Calls struct {
		ProvisionManager            []service.ManagerProvisionRequest
		DeprovisionManager          []string
		RefreshCredentials          []string
		RefreshManagerCredentials   []string
		EnsureManagerGatewayAuth    []managerGatewayAuthCall
		EnsureManagerServiceAccount []string
		DeleteManagerServiceAccount []string
		DeleteCredentials           []string
		RequestManagerSAToken       []string
		LeaveAllManagerRooms        []string
		DeleteManagerRoom           []string
		DeleteManagerRoomAlias      []string
		IsManagerJoinedDM           []string
		IsManagerLLMAuthReady       []string
		SendManagerWelcomeMessage   []service.ManagerWelcomeRequest
	}
}

func NewMockManagerProvisioner() *MockManagerProvisioner {
	return &MockManagerProvisioner{}
}

type managerGatewayAuthCall struct {
	Name       string
	GatewayKey string
}

func (m *MockManagerProvisioner) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
	m.ProvisionManagerFn = nil
	m.DeprovisionManagerFn = nil
	m.RefreshCredentialsFn = nil
	m.RefreshManagerCredentialsFn = nil
	m.EnsureManagerGatewayAuthFn = nil
	m.EnsureManagerServiceAccountFn = nil
	m.DeleteManagerServiceAccountFn = nil
	m.DeleteCredentialsFn = nil
	m.RequestManagerSATokenFn = nil
	m.LeaveAllManagerRoomsFn = nil
	m.DeleteManagerRoomFn = nil
	m.DeleteManagerRoomAliasFn = nil
	m.IsManagerJoinedDMFn = nil
	m.IsManagerLLMAuthReadyFn = nil
	m.SendManagerWelcomeMessageFn = nil
}

func (m *MockManagerProvisioner) ClearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
}

func (m *MockManagerProvisioner) clearCallsLocked() {
	m.Calls = struct {
		ProvisionManager            []service.ManagerProvisionRequest
		DeprovisionManager          []string
		RefreshCredentials          []string
		RefreshManagerCredentials   []string
		EnsureManagerGatewayAuth    []managerGatewayAuthCall
		EnsureManagerServiceAccount []string
		DeleteManagerServiceAccount []string
		DeleteCredentials           []string
		RequestManagerSAToken       []string
		LeaveAllManagerRooms        []string
		DeleteManagerRoom           []string
		DeleteManagerRoomAlias      []string
		IsManagerJoinedDM           []string
		IsManagerLLMAuthReady       []string
		SendManagerWelcomeMessage   []service.ManagerWelcomeRequest
	}{}
}

func (m *MockManagerProvisioner) ProvisionManager(ctx context.Context, req service.ManagerProvisionRequest) (*service.ManagerProvisionResult, error) {
	m.mu.Lock()
	m.Calls.ProvisionManager = append(m.Calls.ProvisionManager, req)
	fn := m.ProvisionManagerFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &service.ManagerProvisionResult{
		MatrixUserID:   "@manager:localhost",
		MatrixToken:    "mock-token-manager",
		RoomID:         "!room-manager:localhost",
		GatewayKey:     "mock-gw-key-manager",
		MinIOPassword:  "mock-minio-pw",
		MatrixPassword: "mock-matrix-pw",
	}, nil
}

func (m *MockManagerProvisioner) DeprovisionManager(ctx context.Context, name string) error {
	m.mu.Lock()
	m.Calls.DeprovisionManager = append(m.Calls.DeprovisionManager, name)
	fn := m.DeprovisionManagerFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return nil
}

func (m *MockManagerProvisioner) RefreshCredentials(ctx context.Context, name string) (*service.RefreshResult, error) {
	m.mu.Lock()
	m.Calls.RefreshCredentials = append(m.Calls.RefreshCredentials, name)
	fn := m.RefreshCredentialsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return &service.RefreshResult{
		MatrixToken:    "mock-token-manager",
		GatewayKey:     "mock-gw-key-manager",
		MinIOPassword:  "mock-minio-pw",
		MatrixPassword: "mock-matrix-pw",
	}, nil
}

func (m *MockManagerProvisioner) RefreshManagerCredentials(ctx context.Context, managerName string) (*service.RefreshResult, error) {
	m.mu.Lock()
	m.Calls.RefreshManagerCredentials = append(m.Calls.RefreshManagerCredentials, managerName)
	fn := m.RefreshManagerCredentialsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, managerName)
	}
	return &service.RefreshResult{
		MatrixToken:    "mock-token-manager",
		GatewayKey:     "mock-gw-key-manager",
		MinIOPassword:  "mock-minio-pw",
		MatrixPassword: "mock-matrix-pw",
	}, nil
}

func (m *MockManagerProvisioner) EnsureManagerGatewayAuth(ctx context.Context, managerName, gatewayKey string) error {
	m.mu.Lock()
	m.Calls.EnsureManagerGatewayAuth = append(m.Calls.EnsureManagerGatewayAuth, managerGatewayAuthCall{
		Name:       managerName,
		GatewayKey: gatewayKey,
	})
	fn := m.EnsureManagerGatewayAuthFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, managerName, gatewayKey)
	}
	return nil
}

func (m *MockManagerProvisioner) EnsureManagerServiceAccount(ctx context.Context, managerName string) error {
	m.mu.Lock()
	m.Calls.EnsureManagerServiceAccount = append(m.Calls.EnsureManagerServiceAccount, managerName)
	fn := m.EnsureManagerServiceAccountFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, managerName)
	}
	return nil
}

func (m *MockManagerProvisioner) DeleteManagerServiceAccount(ctx context.Context, managerName string) error {
	m.mu.Lock()
	m.Calls.DeleteManagerServiceAccount = append(m.Calls.DeleteManagerServiceAccount, managerName)
	fn := m.DeleteManagerServiceAccountFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, managerName)
	}
	return nil
}

func (m *MockManagerProvisioner) DeleteCredentials(ctx context.Context, name string) error {
	m.mu.Lock()
	m.Calls.DeleteCredentials = append(m.Calls.DeleteCredentials, name)
	fn := m.DeleteCredentialsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return nil
}

func (m *MockManagerProvisioner) RequestManagerSAToken(ctx context.Context, managerName string) (string, error) {
	m.mu.Lock()
	m.Calls.RequestManagerSAToken = append(m.Calls.RequestManagerSAToken, managerName)
	fn := m.RequestManagerSATokenFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, managerName)
	}
	return "mock-sa-token-manager", nil
}

func (m *MockManagerProvisioner) LeaveAllManagerRooms(ctx context.Context, managerName string) error {
	m.mu.Lock()
	m.Calls.LeaveAllManagerRooms = append(m.Calls.LeaveAllManagerRooms, managerName)
	fn := m.LeaveAllManagerRoomsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, managerName)
	}
	return nil
}

func (m *MockManagerProvisioner) DeleteManagerRoom(ctx context.Context, roomID string) error {
	m.mu.Lock()
	m.Calls.DeleteManagerRoom = append(m.Calls.DeleteManagerRoom, roomID)
	fn := m.DeleteManagerRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID)
	}
	return nil
}

func (m *MockManagerProvisioner) DeleteManagerRoomAlias(ctx context.Context, managerName string) error {
	m.mu.Lock()
	m.Calls.DeleteManagerRoomAlias = append(m.Calls.DeleteManagerRoomAlias, managerName)
	fn := m.DeleteManagerRoomAliasFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, managerName)
	}
	return nil
}

func (m *MockManagerProvisioner) IsManagerJoinedDM(ctx context.Context, roomID string) (bool, error) {
	m.mu.Lock()
	m.Calls.IsManagerJoinedDM = append(m.Calls.IsManagerJoinedDM, roomID)
	fn := m.IsManagerJoinedDMFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID)
	}
	return true, nil
}

func (m *MockManagerProvisioner) IsManagerLLMAuthReady(ctx context.Context, gatewayKey string) (bool, error) {
	m.mu.Lock()
	m.Calls.IsManagerLLMAuthReady = append(m.Calls.IsManagerLLMAuthReady, gatewayKey)
	fn := m.IsManagerLLMAuthReadyFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, gatewayKey)
	}
	return true, nil
}

func (m *MockManagerProvisioner) SendManagerWelcomeMessage(ctx context.Context, req service.ManagerWelcomeRequest) error {
	m.mu.Lock()
	m.Calls.SendManagerWelcomeMessage = append(m.Calls.SendManagerWelcomeMessage, req)
	fn := m.SendManagerWelcomeMessageFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

// CallCounts returns a snapshot of call counts safe for concurrent use.
// The last slot reports LeaveAllManagerRooms calls (which replaced the
// legacy DeactivateMatrixUser accounting).
func (m *MockManagerProvisioner) CallCounts() (provision, deprovision, refreshManager, leaveAllRooms int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.ProvisionManager),
		len(m.Calls.DeprovisionManager),
		len(m.Calls.RefreshManagerCredentials),
		len(m.Calls.LeaveAllManagerRooms)
}

// ServiceAccountCallCounts returns EnsureManagerServiceAccount and DeleteManagerServiceAccount counts.
func (m *MockManagerProvisioner) ServiceAccountCallCounts() (ensure, delete int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.EnsureManagerServiceAccount), len(m.Calls.DeleteManagerServiceAccount)
}

// CredentialCallCounts returns DeleteCredentials and RequestManagerSAToken counts.
func (m *MockManagerProvisioner) CredentialCallCounts() (deleteCredentials, requestSAToken int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.DeleteCredentials), len(m.Calls.RequestManagerSAToken)
}

// WelcomeCallCount returns the number of SendManagerWelcomeMessage
// invocations recorded so far. Safe for concurrent use.
func (m *MockManagerProvisioner) WelcomeCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.SendManagerWelcomeMessage)
}

// WelcomeCallsSnapshot returns a defensive copy of the recorded
// SendManagerWelcomeMessage requests so test assertions can read fields
// without holding the mock's lock.
func (m *MockManagerProvisioner) WelcomeCallsSnapshot() []service.ManagerWelcomeRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]service.ManagerWelcomeRequest, len(m.Calls.SendManagerWelcomeMessage))
	copy(out, m.Calls.SendManagerWelcomeMessage)
	return out
}

// ClearWelcomeCalls drops the recorded SendManagerWelcomeMessage requests
// without resetting other state. Used by tests that want to assert
// "no further welcome calls happen" after some controller event.
func (m *MockManagerProvisioner) ClearWelcomeCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls.SendManagerWelcomeMessage = nil
}

// JoinedDMCallCount returns the number of IsManagerJoinedDM invocations
// recorded so far.
func (m *MockManagerProvisioner) JoinedDMCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.IsManagerJoinedDM)
}

// LLMAuthReadyCallCount returns the number of IsManagerLLMAuthReady
// invocations recorded so far.
func (m *MockManagerProvisioner) LLMAuthReadyCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.IsManagerLLMAuthReady)
}

var _ service.ManagerProvisioner = (*MockManagerProvisioner)(nil)
