package backend

import (
	"context"
	"fmt"
)

// DefaultContainerPrefix is the baked-in worker container/pod prefix.
// New constructors no longer force this fallback when prefix is empty;
// LoadConfig controls defaulting through AGENTTEAMS_RESOURCE_AUTOPREFIX and
// AGENTTEAMS_RESOURCE_PREFIX.
const DefaultContainerPrefix = "agentteams-worker-"

// Registry holds all available worker backends and provides auto-detection.
//
// Historically the registry also tracked a GatewayBackend slice, but
// gateway selection moved to a dedicated gateway.Client implementation
// (HigressClient / AIGatewayClient) wired directly in app/app.go.
type Registry struct {
	workerBackends []WorkerBackend
}

// NewRegistry creates a Registry with the given worker backends.
func NewRegistry(workers []WorkerBackend) *Registry {
	return &Registry{workerBackends: workers}
}

// DetectWorkerBackend returns the first available worker backend.
// Priority is determined by registration order (set in buildBackends):
//  1. Docker backend (socket available)
//  2. K8s backend (incluster mode)
//  3. nil
func (r *Registry) DetectWorkerBackend(ctx context.Context) WorkerBackend {
	for _, b := range r.workerBackends {
		if b.Available(ctx) {
			return b
		}
	}
	return nil
}

// FindServiceBackend returns the first available backend that implements
// ServiceBackend, or nil if none qualifies.
func (r *Registry) FindServiceBackend(ctx context.Context) ServiceBackend {
	for _, b := range r.workerBackends {
		if sb, ok := b.(ServiceBackend); ok && b.Available(ctx) {
			return sb
		}
	}
	return nil
}

// GetWorkerBackend returns a specific worker backend by name, or auto-detects if name is empty.
func (r *Registry) GetWorkerBackend(ctx context.Context, name string) (WorkerBackend, error) {
	if name == "" {
		b := r.DetectWorkerBackend(ctx)
		if b == nil {
			return nil, fmt.Errorf("no worker backend available")
		}
		return b, nil
	}
	for _, b := range r.workerBackends {
		if b.Name() == name {
			return b, nil
		}
	}
	return nil, fmt.Errorf("unknown worker backend: %q", name)
}

// GetBackendForType returns the backend for the given backendRuntime type.
// "pod" maps to the "k8s" backend; "sandbox" maps to the "sandbox" backend.
// Returns nil, error if the requested backend is not registered/available.
func (r *Registry) GetBackendForType(ctx context.Context, backendRuntime string) (WorkerBackend, error) {
	targetName := backendRuntime
	if backendRuntime == "pod" {
		targetName = "k8s"
	}
	for _, b := range r.workerBackends {
		if b.Name() == targetName && b.Available(ctx) {
			return b, nil
		}
	}
	return nil, fmt.Errorf("backend %q (backendRuntime=%q) not available", targetName, backendRuntime)
}
