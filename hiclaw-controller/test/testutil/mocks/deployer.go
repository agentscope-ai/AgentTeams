package mocks

import (
	"context"
	"sync"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
)

// MockDeployer implements service.WorkerDeployer for testing.
type MockDeployer struct {
	mu sync.Mutex

	DeployPackageFn             func(ctx context.Context, workerName string, pkg string, isUpdate bool) error
	WriteInlineConfigsFn        func(workerName string, spec v1beta1.WorkerSpec) error
	DeployWorkerConfigFn        func(ctx context.Context, req service.WorkerDeployRequest) error
	PushOnDemandSkillsFn        func(ctx context.Context, workerName string, skills []string) error
	CleanupOSSDataFn            func(ctx context.Context, workerName string) error
	InjectCoordinationContextFn func(ctx context.Context, req service.CoordinationDeployRequest) error
	EnsureTeamStorageFn         func(ctx context.Context, teamName string) error

	Calls struct {
		DeployPackage             []string
		WriteInlineConfigs        []string
		DeployWorkerConfig        []service.WorkerDeployRequest
		PushOnDemandSkills        []string
		CleanupOSSData            []string
		InjectCoordinationContext []service.CoordinationDeployRequest
		EnsureTeamStorage         []string
	}
}

func NewMockDeployer() *MockDeployer {
	return &MockDeployer{}
}

// Reset clears all Fn overrides and call records.
func (m *MockDeployer) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
	m.DeployPackageFn = nil
	m.WriteInlineConfigsFn = nil
	m.DeployWorkerConfigFn = nil
	m.PushOnDemandSkillsFn = nil
	m.CleanupOSSDataFn = nil
	m.InjectCoordinationContextFn = nil
	m.EnsureTeamStorageFn = nil
}

// ClearCalls resets call records only, preserving Fn overrides.
func (m *MockDeployer) ClearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
}

func (m *MockDeployer) clearCallsLocked() {
	m.Calls = struct {
		DeployPackage             []string
		WriteInlineConfigs        []string
		DeployWorkerConfig        []service.WorkerDeployRequest
		PushOnDemandSkills        []string
		CleanupOSSData            []string
		InjectCoordinationContext []service.CoordinationDeployRequest
		EnsureTeamStorage         []string
	}{}
}

func (m *MockDeployer) DeployPackage(ctx context.Context, workerName string, pkg string, isUpdate bool) error {
	m.mu.Lock()
	m.Calls.DeployPackage = append(m.Calls.DeployPackage, workerName)
	fn := m.DeployPackageFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName, pkg, isUpdate)
	}
	return nil
}

func (m *MockDeployer) WriteInlineConfigs(workerName string, spec v1beta1.WorkerSpec) error {
	m.mu.Lock()
	m.Calls.WriteInlineConfigs = append(m.Calls.WriteInlineConfigs, workerName)
	fn := m.WriteInlineConfigsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(workerName, spec)
	}
	return nil
}

func (m *MockDeployer) DeployWorkerConfig(ctx context.Context, req service.WorkerDeployRequest) error {
	m.mu.Lock()
	m.Calls.DeployWorkerConfig = append(m.Calls.DeployWorkerConfig, req)
	fn := m.DeployWorkerConfigFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (m *MockDeployer) PushOnDemandSkills(ctx context.Context, workerName string, skills []string) error {
	m.mu.Lock()
	m.Calls.PushOnDemandSkills = append(m.Calls.PushOnDemandSkills, workerName)
	fn := m.PushOnDemandSkillsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName, skills)
	}
	return nil
}

func (m *MockDeployer) CleanupOSSData(ctx context.Context, workerName string) error {
	m.mu.Lock()
	m.Calls.CleanupOSSData = append(m.Calls.CleanupOSSData, workerName)
	fn := m.CleanupOSSDataFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, workerName)
	}
	return nil
}

func (m *MockDeployer) InjectCoordinationContext(ctx context.Context, req service.CoordinationDeployRequest) error {
	m.mu.Lock()
	m.Calls.InjectCoordinationContext = append(m.Calls.InjectCoordinationContext, req)
	fn := m.InjectCoordinationContextFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (m *MockDeployer) EnsureTeamStorage(ctx context.Context, teamName string) error {
	m.mu.Lock()
	m.Calls.EnsureTeamStorage = append(m.Calls.EnsureTeamStorage, teamName)
	fn := m.EnsureTeamStorageFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, teamName)
	}
	return nil
}

// DeployWorkerConfigSnapshot returns a snapshot of DeployWorkerConfig calls
// safe for concurrent use with the reconciler goroutine.
func (m *MockDeployer) DeployWorkerConfigSnapshot() []service.WorkerDeployRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]service.WorkerDeployRequest, len(m.Calls.DeployWorkerConfig))
	copy(out, m.Calls.DeployWorkerConfig)
	return out
}

// CallCounts returns a snapshot of call counts safe for concurrent use.
func (m *MockDeployer) CallCounts() (deployPkg, writeInline, deployConfig, pushSkills, cleanup int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.DeployPackage),
		len(m.Calls.WriteInlineConfigs),
		len(m.Calls.DeployWorkerConfig),
		len(m.Calls.PushOnDemandSkills),
		len(m.Calls.CleanupOSSData)
}

var _ service.WorkerDeployer = (*MockDeployer)(nil)
