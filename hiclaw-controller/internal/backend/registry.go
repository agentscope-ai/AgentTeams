package backend

import (
	"context"
	"fmt"
)

// DefaultContainerPrefix is the legacy baked-in worker container/pod prefix.
// New constructors no longer force this fallback when prefix is empty;
// LoadConfig controls defaulting through HICLAW_RESOURCE_AUTOPREFIX and
// HICLAW_RESOURCE_PREFIX.
const DefaultContainerPrefix = "hiclaw-worker-"

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

// GetBackendForType resolves the API-level backendRuntime value to a concrete
// backend implementation. "pod" intentionally keeps the historical auto-detect
// behavior so embedded Docker and in-cluster K8s both continue to work.
func (r *Registry) GetBackendForType(ctx context.Context, backendRuntime string) (WorkerBackend, error) {
	switch backendRuntime {
	case "", "pod":
		return r.GetWorkerBackend(ctx, "")
	case "sandbox":
		return r.GetWorkerBackend(ctx, "sandbox")
	default:
		return nil, fmt.Errorf("unknown backendRuntime: %q", backendRuntime)
	}
}

// FindServiceBackend returns the first registered backend that can manage
// Kubernetes Services for the requested member placement.
func (r *Registry) FindServiceBackend(ctx context.Context, deployMode, targetClusterID, targetNamespace string) ServiceBackend {
	for _, b := range r.workerBackends {
		if !b.Available(ctx) {
			continue
		}
		sb, ok := b.(ServiceBackend)
		if !ok {
			continue
		}
		if _, _, err := sb.ServiceClient(ctx, deployMode, targetClusterID, targetNamespace); err == nil {
			return sb
		}
	}
	return nil
}
