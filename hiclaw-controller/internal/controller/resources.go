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

func mergeAgentResourcesWithBackendDefaults(defaults *backend.ResourceRequirements, override *v1beta1.AgentResourceRequirements) *backend.ResourceRequirements {
	if override == nil {
		return defaults
	}

	merged := &backend.ResourceRequirements{}
	if defaults != nil {
		*merged = *defaults
	}
	if override.Requests.CPU != "" {
		merged.CPURequest = override.Requests.CPU
	}
	if override.Requests.Memory != "" {
		merged.MemoryRequest = override.Requests.Memory
	}
	if override.Limits.CPU != "" {
		merged.CPULimit = override.Limits.CPU
	}
	if override.Limits.Memory != "" {
		merged.MemoryLimit = override.Limits.Memory
	}
	return merged
}
