package controller

import (
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type managerScope struct {
	manager           *v1beta1.Manager
	provResult        *service.ManagerProvisionResult
	patchBase         client.Patch
	modelProviderInfo *gateway.ModelProviderInfo
}

// computeManagerPhase determines the Manager status phase based on reconcile outcome.
// When reconcile succeeds, phase reflects the desired lifecycle state.
// When reconcile fails, phase depends on whether infrastructure was provisioned.
func computeManagerPhase(m *v1beta1.Manager, reconcileErr error) string {
	if reconcileErr != nil {
		if m.Status.MatrixUserID == "" {
			return "Failed"
		}
		if m.Status.Phase == "" {
			return "Pending"
		}
		return m.Status.Phase
	}
	return m.Spec.DesiredState()
}
