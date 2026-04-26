package mocks

import (
	"context"
	"sync"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
)

// MockManagerDeployer implements service.ManagerDeployer for testing.
type MockManagerDeployer struct {
	mu sync.Mutex

	DeployPackageFn       func(ctx context.Context, name, uri string, isUpdate bool) error
	DeployManagerConfigFn func(ctx context.Context, req service.ManagerDeployRequest) error
	PushOnDemandSkillsFn  func(ctx context.Context, name string, skills []string, remoteSkills []v1beta1.RemoteSkillSource) error
	CleanupOSSDataFn      func(ctx context.Context, name string) error

	Calls struct {
		DeployPackage       []string
		DeployManagerConfig []service.ManagerDeployRequest
		PushOnDemandSkills  []string
		CleanupOSSData      []string
	}
}

func NewMockManagerDeployer() *MockManagerDeployer {
	return &MockManagerDeployer{}
}

func (m *MockManagerDeployer) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
	m.DeployPackageFn = nil
	m.DeployManagerConfigFn = nil
	m.PushOnDemandSkillsFn = nil
	m.CleanupOSSDataFn = nil
}

func (m *MockManagerDeployer) ClearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
}

func (m *MockManagerDeployer) clearCallsLocked() {
	m.Calls = struct {
		DeployPackage       []string
		DeployManagerConfig []service.ManagerDeployRequest
		PushOnDemandSkills  []string
		CleanupOSSData      []string
	}{}
}

func (m *MockManagerDeployer) DeployPackage(ctx context.Context, name, uri string, isUpdate bool) error {
	m.mu.Lock()
	m.Calls.DeployPackage = append(m.Calls.DeployPackage, name)
	fn := m.DeployPackageFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name, uri, isUpdate)
	}
	return nil
}

func (m *MockManagerDeployer) DeployManagerConfig(ctx context.Context, req service.ManagerDeployRequest) error {
	m.mu.Lock()
	m.Calls.DeployManagerConfig = append(m.Calls.DeployManagerConfig, req)
	fn := m.DeployManagerConfigFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return nil
}

func (m *MockManagerDeployer) PushOnDemandSkills(ctx context.Context, name string, skills []string, remoteSkills []v1beta1.RemoteSkillSource) error {
	m.mu.Lock()
	m.Calls.PushOnDemandSkills = append(m.Calls.PushOnDemandSkills, name)
	fn := m.PushOnDemandSkillsFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name, skills, remoteSkills)
	}
	return nil
}

func (m *MockManagerDeployer) CleanupOSSData(ctx context.Context, name string) error {
	m.mu.Lock()
	m.Calls.CleanupOSSData = append(m.Calls.CleanupOSSData, name)
	fn := m.CleanupOSSDataFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, name)
	}
	return nil
}

// CallCounts returns a snapshot of call counts safe for concurrent use.
func (m *MockManagerDeployer) CallCounts() (deployPkg, deployConfig, pushSkills, cleanup int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls.DeployPackage),
		len(m.Calls.DeployManagerConfig),
		len(m.Calls.PushOnDemandSkills),
		len(m.Calls.CleanupOSSData)
}

var _ service.ManagerDeployer = (*MockManagerDeployer)(nil)
