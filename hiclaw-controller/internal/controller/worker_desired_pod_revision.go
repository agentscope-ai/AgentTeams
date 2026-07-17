package controller

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
)

// desiredPodRevision is the single entry point for pod-recreate hashing (C10.7).
// Callers pass the effective runtime and optional resource overlay; Hash()
// delegates to the QwenPaw subset or standard WorkerSpec hash paths.
type desiredPodRevision struct {
	Spec      v1beta1.WorkerSpec
	Runtime   string
	Resources *v1beta1.AgentResourceRequirements
}

func (d desiredPodRevision) Hash() string {
	runtime := d.Runtime
	if runtime == "" {
		runtime = d.Spec.Runtime
	}
	if runtime == backend.RuntimeQwenPaw {
		spec := d.Spec
		if spec.Runtime == "" {
			spec.Runtime = runtime
		}
		return hashQwenPawPodSpecWithResources(spec, d.Resources)
	}
	spec := workerSpecWithEffectiveRuntime(d.Spec, runtime)
	if d.Resources != nil {
		return hashAppliedWorkerSpecWithResources(spec, runtime, d.Resources)
	}
	return hashAppliedWorkerSpec(spec)
}
