package sandbox

import (
	"context"

	corev1 "k8s.io/api/core/v1"
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

	// CreateSandbox creates a new sandbox instance and returns a handle.
	CreateSandbox(ctx context.Context, spec SandboxSpec, config ProviderConfig) (SandboxHandle, error)

	// DeleteSandbox removes a sandbox instance. NotFound is treated as success.
	DeleteSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error

	// HibernateSandbox pauses a running sandbox.
	// Returns ErrCapabilityNotSupported if hibernate is not enabled.
	HibernateSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error

	// ResumeSandbox resumes a hibernated sandbox.
	// Returns ErrCapabilityNotSupported if hibernate is not enabled.
	ResumeSandbox(ctx context.Context, sandboxID string, config ProviderConfig) error

	// GetStatus returns the current status of a sandbox.
	GetStatus(ctx context.Context, sandboxID string, config ProviderConfig) (SandboxStatus, error)

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
}

// SandboxSpec defines the desired state for a new sandbox instance.
// Template carries a full PodTemplateSpec that is embedded as-is into the
// Sandbox CR's spec.template field. This ensures complete fidelity with
// K8sBackend: all PodSpec fields (sidecars, initContainers, securityContext,
// dnsPolicy, hostAliases, imagePullSecrets, etc.) are preserved.
type SandboxSpec struct {
	// Sandbox CR-level metadata.
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	OwnerRef    *metav1.OwnerReference

	// Template is the full PodTemplateSpec embedded in spec.template.
	// The Sandbox controller propagates template.metadata to the created Pod
	// and uses template.spec as the Pod's spec.
	Template corev1.PodTemplateSpec
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
	// Phase is the provider-reported lifecycle phase (e.g. "Running", "Hibernated").
	Phase string

	// Message is an optional human-readable status message.
	Message string

	// Raw carries the full unstructured status for debugging.
	Raw map[string]any

	// AppliedSpecHash is the value of the AnnotationLastAppliedSpecHash annotation
	// on the underlying CR at the time GetStatus was called. Empty when
	// the annotation is missing (e.g. legacy CRs created before the
	// annotation support landed).
	AppliedSpecHash string
}
