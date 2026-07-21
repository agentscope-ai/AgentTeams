package backend

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
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
	StatusSleeping WorkerStatus = "sleeping"
	StatusStarting WorkerStatus = "starting"
	StatusFailed   WorkerStatus = "failed"
	StatusNotFound WorkerStatus = "not_found"
	StatusUnknown  WorkerStatus = "unknown"
)

// Supported worker runtimes.
const (
	RuntimeOpenClaw  = "openclaw"
	RuntimeCopaw     = "copaw"
	RuntimeHermes    = "hermes"
	RuntimeOpenHuman = "openhuman"
	RuntimeQwenPaw   = "qwenpaw"
)

const (
	// BuiltinSandboxInstanceName is the fixed name for the shared SandboxSet,
	// built-in worker-deps PV, AgentIdentity resources, and AccessKey Secret.
	BuiltinSandboxInstanceName        = "agentteams"
	DefaultAuthTokenExpirationSeconds = int64(3600)
	MinAuthTokenExpirationSeconds     = int64(600)
)

func NormalizeAuthTokenExpirationSeconds(seconds int64) int64 {
	if seconds <= 0 {
		return DefaultAuthTokenExpirationSeconds
	}
	if seconds < MinAuthTokenExpirationSeconds {
		return MinAuthTokenExpirationSeconds
	}
	return seconds
}

// ValidRuntime reports whether r is a recognized runtime value.
// An empty string is valid — backends resolve it via ResolveRuntime.
func ValidRuntime(r string) bool {
	return r == "" || r == RuntimeOpenClaw || r == RuntimeCopaw || r == RuntimeHermes || r == RuntimeOpenHuman || r == RuntimeQwenPaw
}

// ResolveRuntime returns the effective runtime for a backend request.
// Resolution order:
//  1. The explicit runtime on the request (req.Runtime).
//  2. The caller-provided fallback (req.RuntimeFallback) — typically
//     AGENTTEAMS_MANAGER_RUNTIME for Manager pods, AGENTTEAMS_DEFAULT_WORKER_RUNTIME
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

// WorkerDepsSpec carries controller-derived workers-deps mount information.
// User CRs provide logical mount entries; reconcilers pass mountPath/subPath
// through to SandboxClaim dynamic volume mounts.
type WorkerDepsSpec struct {
	InplaceUpdateImage  string
	DynamicVolumeMounts []WorkerDepsDynamicVolumeMount
	PodVolume           *WorkerDepsPodVolume
}

type WorkerDepsDynamicVolumeMount struct {
	PVName     string
	MountPath  string
	SubPath    string
	ReadOnly   bool
	Attributes map[string]string
}

type WorkerDepsPodVolume struct {
	Name      string
	ClaimName string
	Mounts    []WorkerDepsPodVolumeMount
}

type WorkerDepsPodVolumeMount struct {
	MountPath string
	SubPath   string
	ReadOnly  bool
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
	Runtime string            `json:"runtime,omitempty"` // "openclaw" | "copaw" | "hermes" | "qwenpaw"
	// RuntimeFallback is the value used by Backend.Create when Runtime is
	// empty, before falling back to RuntimeOpenClaw. Manager / Worker
	// reconcilers populate this from AGENTTEAMS_MANAGER_RUNTIME /
	// AGENTTEAMS_DEFAULT_WORKER_RUNTIME respectively, since Backend.Create is
	// shared between both and cannot tell which env var to consult on its own.
	RuntimeFallback string   `json:"-"`
	Network         string   `json:"network,omitempty"`
	ExtraHosts      []string `json:"extra_hosts,omitempty"`
	WorkingDir      string   `json:"working_dir,omitempty"`

	// BackendRuntime is the desired infrastructure backend type that selected
	// this backend. Most backends ignore it.
	BackendRuntime string `json:"-"`
	// SandboxSetName is retained for SandboxClaim provider API shape. SandboxBackend
	// normalizes it to BuiltinSandboxInstanceName.
	SandboxSetName string `json:"-"`

	// Controller URL advertised to worker for callbacks.
	ControllerURL string `json:"-"`

	// SA-based auth — ServiceAccountName is set on K8s Pods (projected token).
	// AuthToken is the pre-issued SA token for Docker backend.
	// AuthAudience is the projected token audience (K8s backend only; defaults to "agentteams-controller").
	ServiceAccountName string `json:"-"`
	AuthToken          string `json:"-"`
	AuthAudience       string `json:"-"`
	// AuthExpirationSeconds controls the projected ServiceAccount token TTL.
	// Zero means DefaultAuthTokenExpirationSeconds; values below
	// MinAuthTokenExpirationSeconds are clamped because Kubernetes rejects
	// shorter TokenRequest expirations.
	AuthExpirationSeconds int64 `json:"-"`

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
	// identity labels (`app`, `agentteams.io/worker` or `agentteams.io/manager`,
	// `agentteams.io/controller`, `agentteams.io/role`, `agentteams.io/team` when
	// applicable). The backend does NOT synthesize tenant/role defaults;
	// it only stamps `agentteams.io/runtime` from the resolved runtime value
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

	// DeployMode selects local same-cluster Pod placement. Open-source
	// controllers reject Remote before reaching the backend.
	DeployMode string `json:"-"`

	// TargetNamespace overrides the namespace used for local K8s Pods.
	TargetNamespace string `json:"-"`

	// ServiceEnabled indicates whether a Service should be created for the Pod.
	ServiceEnabled bool `json:"-"`

	// WorkersDeps carries controller-derived sandbox worker-deps and custom
	// dynamic mounts.
	WorkersDeps *WorkerDepsSpec `json:"-"`
}

// Deployment modes returned by backends.
const (
	DeployLocal = "local"
	DeployCloud = "cloud"
)

// RemoteClientProvider abstracts access to remote cluster k8s clients.
// The remoteclient.Cache implements this interface.
type RemoteClientProvider interface {
	ResolveClient(ctx context.Context, clusterID string) (K8sCoreClient, error)
}

// RemoteDynamicClientProvider abstracts access to remote cluster dynamic clients.
// The remoteclient.Cache implements this interface.
type RemoteDynamicClientProvider interface {
	ResolveDynamicClient(ctx context.Context, clusterID string) (dynamic.Interface, error)
}

// RemoteClusterClientProvider exposes both typed and dynamic remote clients.
// SandboxBackend needs typed core clients for namespace convergence and dynamic
// clients for OpenKruise Sandbox CRDs.
type RemoteClusterClientProvider interface {
	RemoteClientProvider
	RemoteDynamicClientProvider
}

// K8sServiceClient is the minimal Service client surface needed by the backend.
type K8sServiceClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Service, error)
	Create(ctx context.Context, svc *corev1.Service, opts metav1.CreateOptions) (*corev1.Service, error)
	Update(ctx context.Context, svc *corev1.Service, opts metav1.UpdateOptions) (*corev1.Service, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.ServiceList, error)
}

// K8sNamespaceClient is the minimal Namespace client surface needed by the backend.
type K8sNamespaceClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Namespace, error)
	Create(ctx context.Context, ns *corev1.Namespace, opts metav1.CreateOptions) (*corev1.Namespace, error)
}

// ServiceBackend is an optional capability of a WorkerBackend that can
// provide Kubernetes Service lifecycle operations. Only K8sBackend implements
// this; Docker backend does not support Service management.
type ServiceBackend interface {
	// ServiceClient returns a K8sServiceClient and resolved namespace for
	// same-cluster Service management.
	ServiceClient(ctx context.Context) (K8sServiceClient, string, error)
}

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

	// Message carries a human-readable status detail from the backend.
	// Populated when Ready condition is False with a non-empty message
	// (e.g. container restart failure). Empty when the container is healthy
	// or still starting.
	Message string `json:"message,omitempty"`

	// AppliedSpecHash is a legacy migration fallback read from the underlying
	// sandbox resource's agentteams.io/last-applied-spec-hash annotation. New
	// resources no longer write it; owning reconcilers prefer status.specHash.
	AppliedSpecHash string `json:"applied_spec_hash,omitempty"`
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
