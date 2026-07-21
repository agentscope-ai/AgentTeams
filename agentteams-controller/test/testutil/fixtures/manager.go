package fixtures

import (
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewTestManager creates a minimal Manager CR for testing.
func NewTestManager(name string) *v1beta1.Manager {
	return &v1beta1.Manager{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
		},
		Spec: v1beta1.ManagerSpec{
			Model:   "gpt-4o",
			Runtime: "openclaw",
			Image:   "hiclaw/manager:test",
		},
	}
}

// NewTestManagerWithPhase creates a Manager CR with a pre-set status phase.
func NewTestManagerWithPhase(name, phase string) *v1beta1.Manager {
	m := NewTestManager(name)
	m.Status.Phase = phase
	return m
}

// NewTestManagerWithMCPServers creates a Manager CR with MCP servers configured.
func NewTestManagerWithMCPServers(name string, mcpServers []v1beta1.MCPServer) *v1beta1.Manager {
	m := NewTestManager(name)
	m.Spec.McpServers = mcpServers
	return m
}
