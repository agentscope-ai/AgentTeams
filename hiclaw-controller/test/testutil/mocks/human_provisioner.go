package mocks

import (
	"context"
	"sync"

	"github.com/hiclaw/hiclaw-controller/internal/service"
)

// MockHumanProvisioner implements service.HumanProvisioner for testing.
// It follows the same "default-behavior + Fn-override + Calls-record"
// pattern as MockProvisioner, so the HumanReconciler can be driven without
// a live Matrix homeserver while still letting individual tests assert on
// which provisioner methods were called and with what arguments.
type MockHumanProvisioner struct {
	mu sync.Mutex

	EnsureHumanUserFn func(ctx context.Context, name string) (*service.HumanCredentials, error)
	LoginAsHumanFn    func(ctx context.Context, name, password string) (string, error)
	MatrixUserIDFn    func(name string) string
	InviteToRoomFn    func(ctx context.Context, roomID, userID string) error
	JoinRoomAsFn      func(ctx context.Context, roomID, userToken string) error
	KickFromRoomFn    func(ctx context.Context, roomID, userID, reason string) error
	ForceLeaveRoomFn  func(ctx context.Context, userID, roomID string) error
	SetDisplayNameFn  func(ctx context.Context, userID, accessToken, displayName string) error

	Calls struct {
		EnsureHumanUser []string
		LoginAsHuman    []LoginAsHumanCall
		SetDisplayName  []SetDisplayNameCall
		InviteToRoom    []RoomMembershipCall
		JoinRoomAs      []JoinRoomAsCall
		KickFromRoom    []KickFromRoomCall
		ForceLeaveRoom  []ForceLeaveRoomCall
	}
}

// LoginAsHumanCall records the (name, password) pair passed to LoginAsHuman.
type LoginAsHumanCall struct {
	Name     string
	Password string
}

// SetDisplayNameCall records SetDisplayName input arguments.
type SetDisplayNameCall struct {
	UserID      string
	AccessToken string
	DisplayName string
}

// RoomMembershipCall records the (RoomID, UserID) pair passed to
// InviteToRoom. Named generically so it can be reused by future
// membership-mutation assertions if needed.
type RoomMembershipCall struct {
	RoomID string
	UserID string
}

// JoinRoomAsCall records (RoomID, UserToken) passed to JoinRoomAs. Token
// is captured as-is so tests can assert that the reconciler used the
// user-scoped token from EnsureHumanUser / LoginAsHuman rather than the
// admin token.
type JoinRoomAsCall struct {
	RoomID    string
	UserToken string
}

// KickFromRoomCall records all three parameters so tests can assert on
// the "reason" string the reconciler supplied (useful when debugging why
// a user was ejected).
type KickFromRoomCall struct {
	RoomID string
	UserID string
	Reason string
}

// ForceLeaveRoomCall records the (UserID, RoomID) pair passed to
// ForceLeaveRoom. Order matches the admin command: user first, room
// second, since that's the shape "!admin users force-leave-room" expects.
type ForceLeaveRoomCall struct {
	UserID string
	RoomID string
}

func NewMockHumanProvisioner() *MockHumanProvisioner {
	return &MockHumanProvisioner{}
}

// Reset clears all Fn overrides and call records. Useful at the top of
// subtests that share a parent mock instance.
func (m *MockHumanProvisioner) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
	m.EnsureHumanUserFn = nil
	m.LoginAsHumanFn = nil
	m.MatrixUserIDFn = nil
	m.InviteToRoomFn = nil
	m.JoinRoomAsFn = nil
	m.KickFromRoomFn = nil
	m.ForceLeaveRoomFn = nil
	m.SetDisplayNameFn = nil
}

// ClearCalls resets call records only, preserving Fn overrides.
func (m *MockHumanProvisioner) ClearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
}

func (m *MockHumanProvisioner) clearCallsLocked() {
	m.Calls = struct {
		EnsureHumanUser []string
		LoginAsHuman    []LoginAsHumanCall
		SetDisplayName  []SetDisplayNameCall
		InviteToRoom    []RoomMembershipCall
		JoinRoomAs      []JoinRoomAsCall
		KickFromRoom    []KickFromRoomCall
		ForceLeaveRoom  []ForceLeaveRoomCall
	}{}
}

func (m *MockHumanProvisioner) EnsureHumanUser(ctx context.Context, name string) (*service.HumanCredentials, error) {
	m.mu.Lock()
	m.Calls.EnsureHumanUser = append(m.Calls.EnsureHumanUser, name)
	fn := m.EnsureHumanUserFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return &service.HumanCredentials{
		UserID:      "@" + name + ":localhost",
		AccessToken: "mock-human-token-" + name,
		Password:    "mock-human-pw-" + name,
	}, nil
}

func (m *MockHumanProvisioner) LoginAsHuman(ctx context.Context, name, password string) (string, error) {
	m.mu.Lock()
	m.Calls.LoginAsHuman = append(m.Calls.LoginAsHuman, LoginAsHumanCall{Name: name, Password: password})
	fn := m.LoginAsHumanFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name, password)
	}
	return "mock-human-token-" + name, nil
}

func (m *MockHumanProvisioner) MatrixUserID(name string) string {
	m.mu.Lock()
	fn := m.MatrixUserIDFn
	m.mu.Unlock()
	if fn != nil {
		return fn(name)
	}
	return "@" + name + ":localhost"
}

func (m *MockHumanProvisioner) SetDisplayName(ctx context.Context, userID, accessToken, displayName string) error {
	m.mu.Lock()
	m.Calls.SetDisplayName = append(m.Calls.SetDisplayName, SetDisplayNameCall{UserID: userID, AccessToken: accessToken, DisplayName: displayName})
	fn := m.SetDisplayNameFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, userID, accessToken, displayName)
	}
	return nil
}

func (m *MockHumanProvisioner) InviteToRoom(ctx context.Context, roomID, userID string) error {
	m.mu.Lock()
	m.Calls.InviteToRoom = append(m.Calls.InviteToRoom, RoomMembershipCall{RoomID: roomID, UserID: userID})
	fn := m.InviteToRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, userID)
	}
	return nil
}

func (m *MockHumanProvisioner) JoinRoomAs(ctx context.Context, roomID, userToken string) error {
	m.mu.Lock()
	m.Calls.JoinRoomAs = append(m.Calls.JoinRoomAs, JoinRoomAsCall{RoomID: roomID, UserToken: userToken})
	fn := m.JoinRoomAsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, userToken)
	}
	return nil
}

func (m *MockHumanProvisioner) KickFromRoom(ctx context.Context, roomID, userID, reason string) error {
	m.mu.Lock()
	m.Calls.KickFromRoom = append(m.Calls.KickFromRoom, KickFromRoomCall{RoomID: roomID, UserID: userID, Reason: reason})
	fn := m.KickFromRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, roomID, userID, reason)
	}
	return nil
}

func (m *MockHumanProvisioner) ForceLeaveRoom(ctx context.Context, userID, roomID string) error {
	m.mu.Lock()
	m.Calls.ForceLeaveRoom = append(m.Calls.ForceLeaveRoom, ForceLeaveRoomCall{UserID: userID, RoomID: roomID})
	fn := m.ForceLeaveRoomFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, userID, roomID)
	}
	return nil
}

func (m *MockHumanProvisioner) MatrixAppServiceEnabled() bool {
	return false
}

// Compile-time interface satisfaction check.
var _ service.HumanProvisioner = (*MockHumanProvisioner)(nil)
