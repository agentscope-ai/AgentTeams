package backend

import (
	"context"
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Typed errors for backend operations.
var (
	ErrConflict = errors.New("resource already exists")
	ErrNotFound = errors.New("resource not found")
)

// WorkerStatus represents normalized worker status across backends.
type WorkerStatus string

const (
	StatusRunning  WorkerStatus = "running"
	StatusReady    WorkerStatus = "ready"
	StatusStopped  WorkerStatus = "stopped"
	StatusStarting WorkerStatus = "starting"
	StatusNotFound WorkerStatus = "not_found"
	StatusUnknown  WorkerStatus = "unknown"
)

// Supported worker runtimes.
const (
	RuntimeOpenClaw  = "openclaw"
	RuntimeCopaw     = "copaw"
	RuntimeHermes    = "hermes"
	RuntimeOpenHuman = "openhuman"
)

// ValidRuntime reports whether r is a recognized runtime value.
// An empty string is valid — backends resolve it via ResolveRuntime.
func ValidRuntime(r string) bool {
	return r == "" || r == RuntimeOpenClaw || r == RuntimeCopaw || r == RuntimeHermes || r == RuntimeOpenHuman
}

// ResolveRuntime returns the effective runtime for a backend request.
// Resolution order:
//  1. The explicit runtime on the request (req.Runtime).
//  2. The caller-provided fallback (req.RuntimeFallback) — typically
//     HICLAW_MANAGER_RUNTIME for Manager pods, HICLAW_DEFAULT_WORKER_RUNTIME
//     for Worker pods. The caller (reconciler) is responsible for picking the
//     right env var since Backend.Create is shared between both.
//  3. RuntimeOpenClaw — the historical default.
//
// Backends call this once at the top of Create() so downstream image / working-
// dir / label resolution can rely on a non-empty, normalized runtime value.
//
// This indirection exists because the Worker / Manager CRDs intentionally do
// not pin a schema-level default — that would make the env-var fallback a
// no-op for any CR created with `spec.runtime` unset (the API server would
// silently fill it before the controller ever observes the empty value).
func ResolveRuntime(reqRuntime, fallback string) string {
	if reqRuntime != "" {
		return reqRuntime
	}
	if fallback != "" {
		return fallback
	}
	return RuntimeOpenClaw
}

// ResourceRequirements specifies CPU/memory requests and limits for a container.
// When nil on CreateRequest, backends use their configured defaults.
type ResourceRequirements struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// VolumeMount describes a host-to-container bind mount (Docker backend only;
// K8s backend ignores this — use standard Pod volume specs instead).
type VolumeMount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// PortMapping describes a host-to-container port binding (Docker backend only).
type PortMapping struct {
	HostIP        string // e.g. "127.0.0.1"; empty = all interfaces
	HostPort      string
	ContainerPort string
	Protocol      string // "tcp" (default) or "udp"
}

// CreateRequest holds parameters for creating a worker container/instance.
type CreateRequest struct {
	Name    string            `json:"name"`
	Image   string            `json:"image,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Runtime string            `json:"runtime,omitempty"` // "openclaw" | "copaw" | "hermes" | "openhuman"
	// RuntimeFallback is the value used by Backend.Create when Runtime is
	// empty, before falling back to RuntimeOpenClaw. Manager / Worker
	// reconcilers populate this from HICLAW_MANAGER_RUNTIME /
	// HICLAW_DEFAULT_WORKER_RUNTIME respectively, since Backend.Create is
	// shared between both and cannot tell which env var to consult on its own.
	RuntimeFallback string   `json:"-"`
	Network         string   `json:"network,omitempty"`
	ExtraHosts      []string `json:"extra_hosts,omitempty"`
	WorkingDir      string   `json:"working_dir,omitempty"`

	// Controller URL advertised to worker for callbacks.
	ControllerURL string `json:"-"`

	// SA-based auth — ServiceAccountName is set on K8s Pods (projected token).
	// AuthToken is the pre-issued SA token for Docker backend.
	// AuthAudience is the projected token audience (K8s backend only; defaults to "hiclaw-controller").
	ServiceAccountName string `json:"-"`
	AuthToken          string `json:"-"`
	AuthAudience       string `json:"-"`

	// Resources overrides default resource limits for this container.
	// nil = use backend defaults (e.g. K8sConfig.WorkerCPU/WorkerMemory).
	Resources *ResourceRequirements `json:"-"`

	// NamePrefix overrides the backend's default container/pod name prefix.
	// When set, pod name = NamePrefix + Name instead of containerPrefix + Name.
	NamePrefix string `json:"-"`

	// ContainerName overrides the computed container/pod name entirely.
	// When set, NamePrefix and containerPrefix are ignored for naming.
	ContainerName string `json:"-"`

	// Labels carries the full K8s label set for the Pod. Callers own the
	// identity labels (`app`, `hiclaw.io/worker` or `hiclaw.io/manager`,
	// `hiclaw.io/controller`, `hiclaw.io/role`, `hiclaw.io/team` when
	// applicable). The backend does NOT synthesize tenant/role defaults;
	// it only stamps `hiclaw.io/runtime` from the resolved runtime value
	// (the backend alone knows the post-resolution value after
	// `ResolveRuntime`).
	Labels map[string]string `json:"-"`

	// Volumes are host bind mounts (Docker backend only; ignored by K8s).
	Volumes []VolumeMount `json:"-"`

	// NetworkAliases are DNS names added to the container within the Docker network.
	NetworkAliases []string `json:"-"`

	// Ports are additional host-to-container port mappings (Docker backend only).
	Ports []PortMapping `json:"-"`

	// RestartPolicy for Docker containers (e.g. "unless-stopped", "always").
	// Empty means backend default (no restart).
	RestartPolicy string `json:"-"`

	// Owner is the Kubernetes parent object whose lifecycle the created Pod
	// should be bound to. K8sBackend stamps it as the Pod's controller
	// OwnerReference via controllerutil.SetControllerReference, so that
	// deletion of the owning CR (Worker / Team / Manager) cascades to the
	// Pod via native K8s garbage collection. Docker backend ignores this
	// field.
	Owner metav1.Object `json:"-"`
}

// Deployment modes returned by backends.
const (
	DeployLocal = "local"
	DeployCloud = "cloud"
)

// WorkerResult holds the result of a worker operation.
type WorkerResult struct {
	Name            string       `json:"name"`
	Backend         string       `json:"backend"`
	DeploymentMode  string       `json:"deployment_mode"`
	Status          WorkerStatus `json:"status"`
	ContainerID     string       `json:"container_id,omitempty"`
	AppID           string       `json:"app_id,omitempty"`
	RawStatus       string       `json:"raw_status,omitempty"`
	ConsoleHostPort string       `json:"console_host_port,omitempty"`
}

// WorkerBackend defines the interface for worker lifecycle operations.
// Implementations: DockerBackend (local), KubernetesBackend (incluster).
type WorkerBackend interface {
	// Name returns the backend identifier (e.g. "docker", "k8s").
	Name() string

	// DeploymentMode returns the user-facing deployment mode ("local" or "cloud").
	DeploymentMode() string

	// Available reports whether this backend is usable in the current environment.
	Available(ctx context.Context) bool

	// NeedsCredentialInjection reports whether this backend requires
	// controller-mediated credentials (API key + URL) injected into worker env.
	NeedsCredentialInjection() bool

	// Create creates and starts a new worker.
	Create(ctx context.Context, req CreateRequest) (*WorkerResult, error)

	// Delete removes a worker.
	Delete(ctx context.Context, name string) error

	// Start starts a stopped worker.
	Start(ctx context.Context, name string) error

	// Stop stops a running worker.
	Stop(ctx context.Context, name string) error

	// Status returns the current status of a worker.
	Status(ctx context.Context, name string) (*WorkerResult, error)
}
