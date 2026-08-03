package mocks

import (
	"sync"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
)

// MockManagerEnvBuilder implements service.ManagerEnvBuilderI for testing.
type MockManagerEnvBuilder struct {
	mu sync.Mutex

	BuildManagerFn func(managerName string, prov *service.ManagerProvisionResult, spec v1beta1.ManagerSpec) map[string]string

	Calls struct {
		BuildManager []string
	}
}

func NewMockManagerEnvBuilder() *MockManagerEnvBuilder {
	return &MockManagerEnvBuilder{}
}

func (m *MockManagerEnvBuilder) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
	m.BuildManagerFn = nil
}

func (m *MockManagerEnvBuilder) ClearCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearCallsLocked()
}

func (m *MockManagerEnvBuilder) clearCallsLocked() {
	m.Calls = struct {
		BuildManager []string
	}{}
}

func (m *MockManagerEnvBuilder) BuildManager(managerName string, prov *service.ManagerProvisionResult, spec v1beta1.ManagerSpec) map[string]string {
	m.mu.Lock()
	m.Calls.BuildManager = append(m.Calls.BuildManager, managerName)
	fn := m.BuildManagerFn
	m.mu.Unlock()
	if fn != nil {
		return fn(managerName, prov, spec)
	}
	return map[string]string{
		"AGENTTEAMS_MANAGER_NAME": managerName,
		"MOCK_ENV":                "true",
	}
}

var _ service.ManagerEnvBuilderI = (*MockManagerEnvBuilder)(nil)
