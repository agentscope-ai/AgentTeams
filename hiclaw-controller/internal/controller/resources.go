package controller

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
)

func agentResourcesToBackend(in *v1beta1.AgentResourceRequirements) *backend.ResourceRequirements {
	if in == nil {
		return nil
	}
	return &backend.ResourceRequirements{
		CPURequest:    in.Requests.CPU,
		MemoryRequest: in.Requests.Memory,
		CPULimit:      in.Limits.CPU,
		MemoryLimit:   in.Limits.Memory,
	}
}
