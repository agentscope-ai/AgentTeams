package sandbox

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

// SandboxPlugin defines the interface for sandbox provider implementations.
// Each plugin manages a specific sandbox technology (e.g. OpenKruise Agent Sandbox).
type SandboxPlugin interface {
	// Type returns the plugin identifier (e.g. "openkruise").
	Type() string

	// Capabilities returns the effective capabilities of this plugin,
	// computed as min(MaxCapabilities, config.Capabilities).
	Capabilities(config ProviderConfig) ProviderCapabilities

	// CreateSandboxClaim creates a SandboxClaim that claims one instance from
	// an existing SandboxSet.
	CreateSandboxClaim(ctx context.Context, spec SandboxClaimSpec, config ProviderConfig) (SandboxHandle, error)

	// DeleteSandboxClaim removes a SandboxClaim. NotFound is treated as success.
	DeleteSandboxClaim(ctx context.Context, claimID string, config ProviderConfig) error

	// DeleteSandbox removes an actual Sandbox. NotFound is treated as success.
	DeleteSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error

	// HibernateSandbox pauses a running sandbox.
	// Returns ErrCapabilityNotSupported if hibernate is not enabled.
	HibernateSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error

	// ResumeSandbox resumes a hibernated sandbox.
	// Returns ErrCapabilityNotSupported if hibernate is not enabled.
	ResumeSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error

	// GetSandboxClaimStatus returns the current status of a claimed sandbox.
	GetSandboxClaimStatus(ctx context.Context, claimID string, config ProviderConfig) (SandboxStatus, error)

	// GetSandboxStatus returns the current status of an actual Sandbox.
	GetSandboxStatus(ctx context.Context, sandboxID string, config ProviderConfig) (SandboxStatus, error)

	// ListSandboxes returns actual Sandboxes matching all labels.
	ListSandboxes(ctx context.Context, matchLabels map[string]string, config ProviderConfig) ([]SandboxStatus, error)

	// Validate checks that the provider configuration is valid and the
	// underlying CRDs are available.
	Validate(config ProviderConfig) error

	// HealthCheck performs a connectivity check against the sandbox provider.
	HealthCheck(ctx context.Context, config ProviderConfig) error
}

// ProviderCapabilities declares which optional features the provider supports
// in the current environment. Capabilities are configuration-driven: the same
// plugin may have different capabilities in different clusters.
//
// Only fields that are actually consumed by plugin methods belong here.
// When a new gated feature is introduced, add the corresponding field then —
// not speculatively.
type ProviderCapabilities struct {
	Hibernate bool
	Pool      bool
}

// SandboxClaimSpec defines the desired state for a SandboxClaim instance.
// The SandboxClaim binds a Worker to an available Sandbox from a SandboxSet.
// It only carries claim-time fields supported by the provider CRD; PodSpec
// fields remain owned by the pod backend path.
type SandboxClaimSpec struct {
	Name                string
	Namespace           string
	SandboxSetName      string
	Labels              map[string]string
	Annotations         map[string]string
	OwnerRef            *metav1.OwnerReference
	InplaceUpdate       *SandboxClaimInplaceUpdateSpec
	DynamicVolumesMount []SandboxClaimDynamicVolumeMount
}

type SandboxClaimInplaceUpdateSpec struct {
	Image string
}

type SandboxClaimDynamicVolumeMount struct {
	PVName     string
	MountPath  string
	SubPath    string
	ReadOnly   bool
	Attributes map[string]string
}

// ProviderConfig holds runtime configuration for a sandbox plugin.
type ProviderConfig struct {
	// Type identifies which plugin to use (e.g. "openkruise").
	Type string

	// Config is a free-form key-value map for provider-specific settings.
	Config map[string]string

	// DynamicClient is the Kubernetes dynamic client for CRD operations.
	DynamicClient dynamic.Interface

	// Capabilities declares which features are enabled in this environment.
	// When zero-value, the plugin falls back to its MaxCapabilities.
	Capabilities ProviderCapabilities

	// Namespace is the target namespace for sandbox resources.
	Namespace string
}

// SandboxHandle is returned after successful sandbox creation.
type SandboxHandle struct {
	// SandboxID is the unique identifier (typically CR name) of the sandbox.
	SandboxID string

	// Endpoint is the network endpoint where the sandbox is reachable (if applicable).
	Endpoint string
}

// SandboxStatus represents the current state of a sandbox instance.
type SandboxStatus struct {
	// SandboxID is the underlying Sandbox name for direct Sandbox status. Claim
	// status does not use this field as an authoritative runtime binding; the
	// backend resolves actual Sandboxes by identity labels.
	SandboxID string

	// Phase is the provider-reported lifecycle phase (e.g. "Running", "Hibernated").
	Phase string

	// Message is an optional human-readable status message.
	Message string

	// Raw carries the full unstructured status for debugging.
	Raw map[string]any

	// AppliedSpecHash is a legacy migration fallback read from the
	// AnnotationLastAppliedSpecHash annotation on the underlying CR at the
	// time GetStatus was called. Empty when the annotation is missing.
	AppliedSpecHash string

	// ReadyConditionStatus reflects the "Ready" type condition's status.
	// True when Ready condition is True or when no Ready condition exists
	// (backward compatible with CRs that don't report conditions).
	ReadyConditionStatus bool

	// ReadyConditionMessage holds the message from the Ready condition when
	// ReadyConditionStatus is false. Empty when ready or still starting.
	ReadyConditionMessage string

	// DesiredReplicas and ClaimedReplicas are populated for SandboxClaim status
	// and reflect spec.replicas / status.claimedReplicas.
	DesiredReplicas *int64
	ClaimedReplicas *int64
}
