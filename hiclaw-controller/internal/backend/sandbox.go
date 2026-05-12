package backend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hiclaw/hiclaw-controller/internal/backend/sandbox"
)

// SandboxConfig holds configuration for the SandboxBackend.
type SandboxConfig struct {
	Namespace         string
	ProviderType      string
	WorkerImage       string
	CopawWorkerImage  string
	HermesWorkerImage string
	WorkerCPU         string
	WorkerMemory      string
	ControllerName    string
	ResourcePrefix    string
}

// SandboxBackend manages worker lifecycle via sandbox providers (e.g. OpenKruise).
type SandboxBackend struct {
	plugin          sandbox.SandboxPlugin
	providerConfig  sandbox.ProviderConfig
	config          SandboxConfig
	containerPrefix string
	scheme          *runtime.Scheme
	k8sClient       K8sCoreClient
}

// NewSandboxBackend creates a SandboxBackend with the given plugin and config.
func NewSandboxBackend(
	plugin sandbox.SandboxPlugin,
	providerConfig sandbox.ProviderConfig,
	config SandboxConfig,
	containerPrefix string,
	scheme *runtime.Scheme,
	k8sClient K8sCoreClient,
) *SandboxBackend {
	if config.WorkerCPU == "" {
		config.WorkerCPU = "1000m"
	}
	if config.WorkerMemory == "" {
		config.WorkerMemory = "2Gi"
	}
	return &SandboxBackend{
		plugin:          plugin,
		providerConfig:  providerConfig,
		config:          config,
		containerPrefix: containerPrefix,
		scheme:          scheme,
		k8sClient:       k8sClient,
	}
}

func (s *SandboxBackend) Name() string                   { return "sandbox" }
func (s *SandboxBackend) DeploymentMode() string         { return DeployCloud }
func (s *SandboxBackend) NeedsCredentialInjection() bool { return true }

func (s *SandboxBackend) Available(ctx context.Context) bool {
	if err := s.plugin.HealthCheck(ctx, s.providerConfig); err != nil {
		return false
	}
	return true
}

// WithPrefix returns a shallow copy of the backend with a different container name prefix.
func (s *SandboxBackend) WithPrefix(prefix string) *SandboxBackend {
	cp := *s
	cp.containerPrefix = prefix
	return &cp
}

func (s *SandboxBackend) Create(ctx context.Context, req CreateRequest) (*WorkerResult, error) {
	req.Runtime = ResolveRuntime(req.Runtime, req.RuntimeFallback)

	sandboxName := s.sandboxName(req)

	// Build environment variables.
	if req.Env == nil {
		req.Env = make(map[string]string)
	}
	mergeOSSRegionFromProcessEnv(req.Env)
	if rt := os.Getenv("HICLAW_RUNTIME"); rt != "" {
		req.Env["HICLAW_RUNTIME"] = rt
	} else {
		req.Env["HICLAW_RUNTIME"] = "k8s"
	}
	if req.ControllerURL != "" {
		req.Env["HICLAW_CONTROLLER_URL"] = req.ControllerURL
	}
	req.Env["HICLAW_AUTH_TOKEN_FILE"] = "/var/run/secrets/hiclaw/token"

	// Resolve image.
	image := req.Image
	if image == "" {
		switch {
		case req.Runtime == RuntimeCopaw && s.config.CopawWorkerImage != "":
			image = s.config.CopawWorkerImage
		case req.Runtime == RuntimeHermes && s.config.HermesWorkerImage != "":
			image = s.config.HermesWorkerImage
		case s.config.WorkerImage != "":
			image = s.config.WorkerImage
		}
	}
	if image == "" {
		return nil, fmt.Errorf("no worker image configured for sandbox backend")
	}

	// Resolve working directory.
	workingDir := req.WorkingDir
	if workingDir == "" {
		switch {
		case req.Runtime == RuntimeCopaw:
			workingDir = "/root/.copaw-worker"
		default:
			if home := req.Env["HOME"]; home != "" {
				workingDir = home
			} else {
				workingDir = fmt.Sprintf("/root/hiclaw-fs/agents/%s", req.Name)
				req.Env["HOME"] = workingDir
			}
		}
	}

	// Build resource requirements (same 3-tier precedence as K8sBackend).
	defaultResources := buildDefaultResources(s.config.WorkerCPU, s.config.WorkerMemory)
	var resourcesOverride *corev1.ResourceRequirements
	if req.Resources != nil {
		merged, err := mergeResourceOverrides(defaultResources, req.Resources)
		if err != nil {
			return nil, err
		}
		resourcesOverride = &merged
	}

	// Build labels.
	sandboxLabels := map[string]string{
		"hiclaw.io/runtime": defaultRuntime(req.Runtime),
	}
	for k, v := range req.Labels {
		sandboxLabels[k] = v
	}

	// Build the agent container.
	agentContainer := corev1.Container{
		Name:            "worker",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             buildK8sEnvVars(req.Env),
		WorkingDir:      workingDir,
	}

	// Build projected SA token volume (same as K8sBackend).
	tokenAudience := req.AuthAudience
	if tokenAudience == "" {
		tokenAudience = "hiclaw-controller"
	}
	tokenExpSeconds := int64(3600)
	tokenVolume := corev1.Volume{
		Name: "hiclaw-token",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Audience:          tokenAudience,
						ExpirationSeconds: &tokenExpSeconds,
						Path:              "token",
					},
				}},
			},
		},
	}
	tokenVolumeMount := corev1.VolumeMount{
		Name:      "hiclaw-token",
		MountPath: "/var/run/secrets/hiclaw",
		ReadOnly:  true,
	}

	// ServiceAccount name.
	saName := req.ServiceAccountName

	// Load pod template and apply full merge (same logic as K8sBackend).
	tmpl := LoadAgentPodTemplate(ctx, s.k8sClient, s.config.Namespace, s.config.ControllerName)
	pod := ApplyPodTemplate(tmpl, PodOverlay{
		Name:               sandboxName,
		Namespace:          s.config.Namespace,
		Labels:             sandboxLabels,
		Annotations:        nil,
		ServiceAccountName: saName,
		Container:          agentContainer,
		ResourcesOverride:  resourcesOverride,
		DefaultResources:   defaultResources,
		TokenVolume:        tokenVolume,
		TokenVolumeMount:   tokenVolumeMount,
		HostAliases:        buildHostAliases(req.ExtraHosts),
	})

	// Derive OwnerReference from req.Owner (same semantics as K8sBackend).
	var ownerRef *metav1.OwnerReference
	if req.Owner != nil && s.scheme != nil {
		if rObj, ok := req.Owner.(runtime.Object); ok {
			gvks, _, _ := s.scheme.ObjectKinds(rObj)
			if len(gvks) > 0 {
				ownerRef = &metav1.OwnerReference{
					APIVersion:         gvks[0].GroupVersion().String(),
					Kind:               gvks[0].Kind,
					Name:               req.Owner.GetName(),
					UID:                req.Owner.GetUID(),
					Controller:         boolPtr(true),
					BlockOwnerDeletion: boolPtr(true),
				}
			}
		}
	}

	// Build SandboxSpec with full PodTemplateSpec.
	spec := sandbox.SandboxSpec{
		Name:        sandboxName,
		Namespace:   s.config.Namespace,
		Labels:      pod.Labels,
		Annotations: cloneStringMap(pod.Annotations),
		OwnerRef:    ownerRef,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      pod.Labels,
				Annotations: pod.Annotations,
			},
			Spec: pod.Spec,
		},
	}

	// Stamp the controller-computed spec hash on the CR so that future
	// reconciles can decide between Resume and Delete+Create by comparing
	// it against the current desired hash. We only stamp the CR-level
	// metadata, NOT the embedded PodTemplateSpec metadata, because this is
	// reconciler bookkeeping and must not propagate to the workload Pod.
	// NOTE: spec.Annotations is a separate map from spec.Template.ObjectMeta.
	// Annotations (ensured by cloneStringMap above) so writing here does not
	// pollute the Pod template.
	if req.AppliedSpecHash != "" {
		if spec.Annotations == nil {
			spec.Annotations = map[string]string{}
		}
		spec.Annotations[sandbox.AnnotationLastAppliedSpecHash] = req.AppliedSpecHash
	}

	handle, err := s.plugin.CreateSandbox(ctx, spec, s.providerConfig)
	if err != nil {
		// Translate the sandbox-layer "already exists or terminating"
		// sentinel into backend.ErrConflict so the reconciler takes the
		// same no-op-and-requeue path it already has for K8sBackend Pod
		// creates (see member_reconcile.go / manager_reconcile_container.go).
		if errors.Is(err, sandbox.ErrAlreadyExists) {
			return nil, fmt.Errorf("%w: sandbox %q", ErrConflict, sandboxName)
		}
		if errors.Is(err, sandbox.ErrCapabilityNotSupported) {
			return nil, fmt.Errorf("sandbox create %s: %w", sandboxName, err)
		}
		return nil, fmt.Errorf("sandbox create %s: %w", sandboxName, err)
	}

	return &WorkerResult{
		Name:    req.Name,
		Backend: "sandbox",
		Status:  StatusStarting,
		AppID:   handle.SandboxID,
	}, nil
}

func (s *SandboxBackend) Delete(ctx context.Context, name string) error {
	sandboxID := s.containerPrefix + name
	err := s.plugin.DeleteSandbox(ctx, sandboxID, s.providerConfig)
	if err != nil {
		return fmt.Errorf("sandbox delete %s: %w", sandboxID, err)
	}
	return nil
}

// Start resumes a hibernated sandbox ("start" = "resume" for sandbox backend).
func (s *SandboxBackend) Start(ctx context.Context, name string) error {
	sandboxID := s.containerPrefix + name
	err := s.plugin.ResumeSandbox(ctx, sandboxID, s.providerConfig)
	if err != nil {
		return fmt.Errorf("sandbox resume %s: %w", sandboxID, err)
	}
	return nil
}

// Stop hibernates a running sandbox. If the provider does not support
// hibernate, Stop internally falls back to Delete so the reconciler does not
// need to be aware of capability differences.
func (s *SandboxBackend) Stop(ctx context.Context, name string) error {
	sandboxID := s.containerPrefix + name
	err := s.plugin.HibernateSandbox(ctx, sandboxID, s.providerConfig)
	if err != nil {
		if errors.Is(err, sandbox.ErrCapabilityNotSupported) {
			// Fallback: provider cannot hibernate, just delete the sandbox.
			return s.Delete(ctx, name)
		}
		return fmt.Errorf("sandbox hibernate %s: %w", sandboxID, err)
	}
	return nil
}

func (s *SandboxBackend) Status(ctx context.Context, name string) (*WorkerResult, error) {
	sandboxID := s.containerPrefix + name
	status, err := s.plugin.GetStatus(ctx, sandboxID, s.providerConfig)
	if err != nil {
		// Provider unreachable: return Unknown so the reconciler's default
		// branch requeues without taking destructive action.
		if errors.Is(err, sandbox.ErrProviderUnavailable) {
			return &WorkerResult{Name: name, Backend: "sandbox", Status: StatusUnknown}, nil
		}
		// CR really does not exist — the only case where "not found" is
		// a safe signal for the reconciler to create a new one.
		if errors.Is(err, sandbox.ErrNotFound) {
			return &WorkerResult{Name: name, Backend: "sandbox", Status: StatusNotFound}, nil
		}
		// Any other error (RBAC, API timeout, parse failure…) must NOT be
		// silently collapsed into NotFound. Previously this path returned
		// StatusNotFound and caused the reconciler to recreate the sandbox
		// on every transient API hiccup. Surface the error and let the
		// reconciler requeue.
		return nil, fmt.Errorf("sandbox status %s: %w", sandboxID, err)
	}

	workerStatus := mapSandboxPhaseToWorkerStatus(status.Phase)
	return &WorkerResult{
		Name:            name,
		Backend:         "sandbox",
		DeploymentMode:  DeployCloud,
		Status:          workerStatus,
		RawStatus:       status.Phase,
		AppliedSpecHash: status.AppliedSpecHash,
	}, nil
}

// mapSandboxPhaseToWorkerStatus translates a provider-reported sandbox phase
// to the normalized WorkerStatus. Design decisions:
//   - An empty phase string means the provider controller has not yet written
//     status.phase (common right after Create). Map to StatusStarting so the
//     reconciler treats it as "converging", NOT as "unknown" (which would
//     trigger the delete+recreate path).
//   - PhaseTerminating (our synthetic phase for CRs with non-zero
//     deletionTimestamp) also maps to StatusStarting: the reconciler must
//     wait for GC before it can legitimately create a replacement.
//   - PhaseHibernated maps to StatusStopped (resumable), matching the Docker
//     backend's "stopped container can be started" pattern.
//   - PhaseTerminated (provider-reported final state) maps to StatusNotFound
//     so the reconciler can create a fresh CR. This is safe because, by
//     definition, a CR in this phase has no live workload.
func mapSandboxPhaseToWorkerStatus(phase string) WorkerStatus {
	switch phase {
	case "":
		return StatusStarting
	case sandbox.PhaseRunning:
		return StatusRunning
	case sandbox.PhaseStarting, sandbox.PhaseResuming, sandbox.PhasePending, sandbox.PhaseHibernating, sandbox.PhaseTerminating:
		return StatusStarting
	case sandbox.PhaseHibernated:
		return StatusStopped
	case sandbox.PhaseFailed:
		return StatusUnknown
	case sandbox.PhaseTerminated:
		return StatusNotFound
	default:
		return StatusUnknown
	}
}

// WatchObject returns an unstructured object with the GVK set for the
// sandbox provider's CRD, suitable for controller-runtime Watch().
func (s *SandboxBackend) WatchObject() client.Object {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "agents.kruise.io",
		Version: "v1alpha1",
		Kind:    "Sandbox",
	})
	return obj
}

// ProviderGVR returns the GroupVersionResource of the sandbox provider's CRD.
func (s *SandboxBackend) ProviderGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "agents.kruise.io",
		Version:  "v1alpha1",
		Resource: "sandboxes",
	}
}

func (s *SandboxBackend) sandboxName(req CreateRequest) string {
	if req.ContainerName != "" {
		return req.ContainerName
	}
	if req.NamePrefix != "" {
		return req.NamePrefix + req.Name
	}
	return s.containerPrefix + req.Name
}

// NewSandboxBackendFromConfig creates a fully wired SandboxBackend using the
// standard in-cluster/kubeconfig K8s config path. This is the entry point
// called by buildWorkerBackends in app.go.
func NewSandboxBackendFromConfig(cfg SandboxConfig, containerPrefix string, scheme *runtime.Scheme, capabilities string) (*SandboxBackend, error) {
	restConfig, err := loadK8sRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("sandbox backend: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("sandbox backend: create dynamic client: %w", err)
	}

	coreClient, err := corev1client.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("sandbox backend: create core client: %w", err)
	}
	k8sClient := &k8sCoreClientWrapper{client: coreClient}

	registry := sandbox.NewPluginRegistry()
	plugin := sandbox.NewOpenKruisePlugin(dynClient)
	registry.Register("openkruise", plugin)

	providerType := cfg.ProviderType
	if providerType == "" {
		providerType = "openkruise"
	}
	selectedPlugin, err := registry.Get(providerType)
	if err != nil {
		return nil, fmt.Errorf("sandbox backend: %w", err)
	}

	providerConfig := sandbox.ProviderConfig{
		Type:          providerType,
		Config:        make(map[string]string),
		DynamicClient: dynClient,
		Capabilities:  ParseCapabilities(capabilities),
		Namespace:     cfg.Namespace,
	}

	if err := selectedPlugin.Validate(providerConfig); err != nil {
		return nil, fmt.Errorf("sandbox backend: validate provider %q: %w", providerType, err)
	}

	return NewSandboxBackend(selectedPlugin, providerConfig, cfg, containerPrefix, scheme, k8sClient), nil
}

// ParseCapabilities parses a comma-separated capabilities string into
// ProviderCapabilities. Every recognized token enables one capability.
// Unknown tokens and the empty string both yield the zero value (all
// capabilities disabled), so administrators can opt out cleanly by not
// setting HICLAW_SANDBOX_CAPABILITIES. To enable Hibernate, set the env
// to "hibernate".
func ParseCapabilities(s string) sandbox.ProviderCapabilities {
	caps := sandbox.ProviderCapabilities{}
	for _, part := range strings.Split(s, ",") {
		switch strings.TrimSpace(strings.ToLower(part)) {
		case "hibernate":
			caps.Hibernate = true
		}
	}
	return caps
}

// cloneStringMap returns a shallow copy of the given map so that mutations
// to the returned map do not affect the original (and vice versa).
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
