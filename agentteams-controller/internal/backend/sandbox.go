package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend/sandbox"
)

var sandboxSetGVR = schema.GroupVersionResource{
	Group:    "agents.kruise.io",
	Version:  "v1alpha1",
	Resource: "sandboxsets",
}

const defaultSandboxAgentRuntimeImage = "registry-cn-hangzhou-vpc.ack.aliyuncs.com/acs/agent-runtime:v0.0.9"

const DefaultSandboxPrewarmSize = 1

// SandboxConfig holds configuration for the SandboxBackend.
type SandboxConfig struct {
	Namespace                    string
	ProviderType                 string
	AgentRuntimeImage            string
	WorkerImage                  string
	CopawWorkerImage             string
	HermesWorkerImage            string
	OpenHumanWorkerImage         string
	QwenPawWorkerImage           string
	WorkerCPU                    string
	WorkerMemory                 string
	SandboxPrewarmSize           int
	SandboxPrewarmSizeConfigured bool
	ControllerName               string
	ResourcePrefix               string
}

// SandboxBackend manages worker lifecycle via sandbox providers (e.g. OpenKruise).
type SandboxBackend struct {
	plugin          sandbox.SandboxPlugin
	providerConfig  sandbox.ProviderConfig
	config          SandboxConfig
	containerPrefix string
	scheme          *runtime.Scheme
	k8sClient       K8sCoreClient
	remoteCache     RemoteClusterClientProvider
}

// NewSandboxBackend creates a SandboxBackend with the given plugin and config.
func NewSandboxBackend(
	plugin sandbox.SandboxPlugin,
	providerConfig sandbox.ProviderConfig,
	config SandboxConfig,
	containerPrefix string,
	scheme *runtime.Scheme,
	k8sClient K8sCoreClient,
	remoteCache RemoteClusterClientProvider,
) *SandboxBackend {
	if config.WorkerCPU == "" {
		config.WorkerCPU = "1000m"
	}
	if config.WorkerMemory == "" {
		config.WorkerMemory = "2Gi"
	}
	if config.AgentRuntimeImage == "" {
		config.AgentRuntimeImage = defaultSandboxAgentRuntimeImage
	}
	if !config.SandboxPrewarmSizeConfigured && config.SandboxPrewarmSize == 0 {
		config.SandboxPrewarmSize = DefaultSandboxPrewarmSize
	} else {
		config.SandboxPrewarmSize = NormalizeSandboxPrewarmSize(config.SandboxPrewarmSize)
	}
	return &SandboxBackend{
		plugin:          plugin,
		providerConfig:  providerConfig,
		config:          config,
		containerPrefix: containerPrefix,
		scheme:          scheme,
		k8sClient:       k8sClient,
		remoteCache:     remoteCache,
	}
}

func NormalizeSandboxPrewarmSize(size int) int {
	if size < 0 {
		return DefaultSandboxPrewarmSize
	}
	return size
}

func (s *SandboxBackend) Name() string                   { return "sandbox" }
func (s *SandboxBackend) DeploymentMode() string         { return DeployCloud }
func (s *SandboxBackend) NeedsCredentialInjection() bool { return true }

// Available reports whether this backend is configured and ready to accept
// operations. It only verifies the dynamic client is present — actual CRD
// existence is validated at operation time (Create/Status/Delete), so the
// controller does not need list/watch permissions on the CRD itself.
func (s *SandboxBackend) Available(_ context.Context) bool {
	return s.plugin != nil
}

// WithPrefix returns a shallow copy of the backend with a different container name prefix.
func (s *SandboxBackend) WithPrefix(prefix string) *SandboxBackend {
	cp := *s
	cp.containerPrefix = prefix
	return &cp
}

func (s *SandboxBackend) resolveProviderConfig(ctx context.Context) (sandbox.ProviderConfig, error) {
	return s.providerConfig, nil
}

func (s *SandboxBackend) ensureRemoteNamespace(ctx context.Context, namespace string) error {
	return nil
}

func (s *SandboxBackend) Create(ctx context.Context, req CreateRequest) (*WorkerResult, error) {
	req.Runtime = ResolveRuntime(req.Runtime, req.RuntimeFallback)

	claimName := s.sandboxClaimName(req)

	// Resolve provider config for the target cluster.
	targetCfg, err := s.resolveProviderConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve provider config for create: %w", err)
	}
	if err := s.ensureRemoteNamespace(ctx, targetCfg.Namespace); err != nil {
		return nil, fmt.Errorf("ensure remote namespace: %w", err)
	}

	// Resolve image.
	workerImage := req.Image
	if workerImage == "" {
		switch {
		case req.Runtime == RuntimeCopaw && s.config.CopawWorkerImage != "":
			workerImage = s.config.CopawWorkerImage
		case req.Runtime == RuntimeHermes && s.config.HermesWorkerImage != "":
			workerImage = s.config.HermesWorkerImage
		case req.Runtime == RuntimeOpenHuman && s.config.OpenHumanWorkerImage != "":
			workerImage = s.config.OpenHumanWorkerImage
		case req.Runtime == RuntimeQwenPaw && s.config.QwenPawWorkerImage != "":
			workerImage = s.config.QwenPawWorkerImage
		case s.config.WorkerImage != "":
			workerImage = s.config.WorkerImage
		}
	}
	if workerImage == "" {
		return nil, fmt.Errorf("no worker image configured for sandbox backend")
	}
	if req.WorkersDeps != nil && req.WorkersDeps.InplaceUpdateImage == "" {
		req.WorkersDeps.InplaceUpdateImage = workerImage
	}

	sandboxSetResources := buildSandboxSetResources(s.config.WorkerCPU, s.config.WorkerMemory)

	// Build labels.
	sandboxLabels := map[string]string{
		v1beta1.LabelRuntime: defaultRuntime(req.Runtime),
	}
	for k, v := range req.Labels {
		sandboxLabels[k] = v
	}

	// SandboxClaim does not support PodSpec template patches in ACS. Consume
	// only metadata that has a first-class SandboxClaim field; PodSpec fields
	// stay pod-backend-only.
	tmpl := LoadAgentPodTemplate(ctx, s.k8sClient, s.config.Namespace, s.config.ControllerName, req.DeployMode)
	claimLabels := sandboxClaimLabels(mergeStringMaps(tmpl.ObjectMeta.Labels, sandboxLabels))
	claimAnnotations := cloneStringMap(tmpl.ObjectMeta.Annotations)

	// Derive OwnerReference from req.Owner (same semantics as K8sBackend).
	// Skip for remote mode: cross-cluster ownerRef is not possible.
	var ownerRef *metav1.OwnerReference
	if req.Owner != nil && s.scheme != nil && req.DeployMode != v1beta1.DeployModeRemote {
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

	sandboxSetName := s.resolveSandboxSetName(req.SandboxSetName)
	if err := s.ensureUnifiedSandboxSet(ctx, targetCfg, sandboxSetName, s.config.AgentRuntimeImage, s.config.SandboxPrewarmSize, &sandboxSetResources); err != nil {
		return nil, fmt.Errorf("ensure SandboxSet %s/%s: %w", targetCfg.Namespace, sandboxSetName, err)
	}
	claimSpec := sandbox.SandboxClaimSpec{
		Name:                claimName,
		Namespace:           targetCfg.Namespace,
		SandboxSetName:      sandboxSetName,
		Labels:              claimLabels,
		Annotations:         claimAnnotations,
		OwnerRef:            ownerRef,
		InplaceUpdate:       claimInplaceUpdate(req.WorkersDeps, workerImage),
		DynamicVolumesMount: claimDynamicVolumes(req.WorkersDeps),
	}
	handle, err := s.plugin.CreateSandboxClaim(ctx, claimSpec, targetCfg)
	if err != nil {
		if errors.Is(err, sandbox.ErrAlreadyExists) {
			return nil, fmt.Errorf("%w: SandboxClaim %q", ErrConflict, claimName)
		}
		return nil, fmt.Errorf("SandboxClaim create %s: %w", claimName, err)
	}
	return &WorkerResult{
		Name:    req.Name,
		Backend: "sandbox",
		Status:  StatusStarting,
		AppID:   handle.SandboxID,
	}, nil
}

func (s *SandboxBackend) resolveSandboxSetName(_ string) string {
	return BuiltinSandboxInstanceName
}

const sandboxAgentNameLabel = "security.agents.kruise.io/agent-name"

func sandboxClaimLabels(labels map[string]string) map[string]string {
	out := cloneStringMap(labels)
	if out == nil {
		out = map[string]string{}
	}
	out[sandboxAgentNameLabel] = BuiltinSandboxInstanceName
	return out
}

func (s *SandboxBackend) ensureUnifiedSandboxSet(ctx context.Context, cfg sandbox.ProviderConfig, name, image string, prewarmSize int, resources *corev1.ResourceRequirements) error {
	if cfg.DynamicClient == nil {
		return fmt.Errorf("dynamic client is not configured")
	}
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = s.config.Namespace
	}
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	desired := buildUnifiedSandboxSetObject(name, namespace, image, NormalizeSandboxPrewarmSize(prewarmSize), resources, s.config.ControllerName)
	res := cfg.DynamicClient.Resource(sandboxSetGVR).Namespace(namespace)
	existing, err := res.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := res.Create(ctx, desired, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}

	labels := existing.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range desired.GetLabels() {
		labels[k] = v
	}
	existing.SetLabels(labels)
	spec, ok, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("desired SandboxSet spec is missing")
	}
	existing.Object["spec"] = spec
	if _, err := res.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

func buildUnifiedSandboxSetObject(name, namespace, image string, prewarmSize int, resources *corev1.ResourceRequirements, controllerName string) *unstructured.Unstructured {
	labels := map[string]interface{}{
		"agentteams.io/managed-by": "agentteams-controller",
		"agentteams.io/sandboxset": name,
	}
	if controllerName != "" {
		labels[v1beta1.LabelController] = controllerName
	}
	templateLabels := map[string]interface{}{
		"alibabacloud.com/acs":           "true",
		"alibabacloud.com/compute-class": "agent-sandbox",
		"alibabacloud.com/compute-qos":   "default",
		"agentteams.io/managed-by":       "agentteams-controller",
		"agentteams.io/sandboxset":       name,
	}
	if controllerName != "" {
		templateLabels[v1beta1.LabelController] = controllerName
	}

	container := map[string]interface{}{
		"name":            "worker",
		"image":           image,
		"imagePullPolicy": "IfNotPresent",
		"env": []interface{}{
			map[string]interface{}{"name": "AGENTTEAMS_WORKER_ENV_MOUNT_DIR", "value": "/mnt/agentteams/env"},
			map[string]interface{}{"name": "AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED", "value": "1"},
		},
	}
	if resourcesMap := sandboxSetResourcesMap(resources); len(resourcesMap) > 0 {
		container["resources"] = resourcesMap
	}

	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agents.kruise.io/v1alpha1",
		"kind":       "SandboxSet",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"labels":    labels,
		},
		"spec": map[string]interface{}{
			"replicas": int64(NormalizeSandboxPrewarmSize(prewarmSize)),
			"runtimes": []interface{}{
				map[string]interface{}{"name": "csi"},
				map[string]interface{}{"name": "agent-runtime"},
			},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"labels": templateLabels,
				},
				"spec": map[string]interface{}{
					"automountServiceAccountToken":  false,
					"terminationGracePeriodSeconds": int64(0),
					"containers": []interface{}{
						container,
					},
				},
			},
		},
	}}
}

func sandboxSetResourcesMap(resources *corev1.ResourceRequirements) map[string]interface{} {
	out := map[string]interface{}{}
	if resources == nil {
		return out
	}
	if len(resources.Requests) > 0 {
		requests := map[string]interface{}{}
		for name, quantity := range resources.Requests {
			requests[string(name)] = quantity.String()
		}
		out["requests"] = requests
	}
	if len(resources.Limits) > 0 {
		limits := map[string]interface{}{}
		for name, quantity := range resources.Limits {
			limits[string(name)] = quantity.String()
		}
		out["limits"] = limits
	}
	return out
}

func buildSandboxSetResources(workerCPU, workerMemory string) corev1.ResourceRequirements {
	resources := buildDefaultResources(workerCPU, workerMemory)
	requests := corev1.ResourceList{}
	for name, quantity := range resources.Limits {
		requests[name] = quantity.DeepCopy()
	}
	resources.Requests = requests
	return resources
}

func (s *SandboxBackend) Delete(ctx context.Context, name string) error {
	claimID := s.containerPrefix + name
	cfg, err := s.resolveProviderConfig(ctx)
	if err != nil {
		return fmt.Errorf("resolve config for delete: %w", err)
	}
	if err := s.plugin.DeleteSandboxClaim(ctx, claimID, cfg); err != nil {
		return fmt.Errorf("SandboxClaim delete %s: %w", claimID, err)
	}
	statuses, err := s.listActualSandboxes(ctx, name, cfg)
	if err != nil {
		return fmt.Errorf("list sandboxes for %s: %w", name, err)
	}
	for _, status := range statuses {
		if status.SandboxID == "" {
			continue
		}
		if err := s.plugin.DeleteSandbox(ctx, status.SandboxID, cfg); err != nil {
			return fmt.Errorf("Sandbox delete %s: %w", status.SandboxID, err)
		}
	}
	return nil
}

// Start resumes a hibernated sandbox ("start" = "resume" for sandbox backend).
func (s *SandboxBackend) Start(ctx context.Context, name string) error {
	cfg, err := s.resolveProviderConfig(ctx)
	if err != nil {
		return fmt.Errorf("resolve config for start: %w", err)
	}
	status, err := s.boundOrSingleActualSandbox(ctx, name, cfg)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if status.SandboxID == "" {
		return nil
	}
	err = s.plugin.ResumeSandbox(ctx, status.SandboxID, cfg)
	if err != nil {
		return fmt.Errorf("resume sandbox %s: %w", status.SandboxID, err)
	}
	return nil
}

// Stop hibernates a running sandbox. If the provider does not support
// hibernate, Stop internally falls back to Delete so the reconciler does not
// need to be aware of capability differences.
func (s *SandboxBackend) Stop(ctx context.Context, name string) error {
	cfg, err := s.resolveProviderConfig(ctx)
	if err != nil {
		return fmt.Errorf("resolve config for stop: %w", err)
	}
	status, err := s.boundOrSingleActualSandbox(ctx, name, cfg)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return nil
		}
		return err
	}
	return s.hibernateSandboxStatus(ctx, name, status, cfg)
}

func (s *SandboxBackend) hibernateSandboxStatus(ctx context.Context, name string, status sandbox.SandboxStatus, cfg sandbox.ProviderConfig) error {
	if status.SandboxID == "" {
		return nil
	}
	err := s.plugin.HibernateSandbox(ctx, status.SandboxID, cfg)
	if err != nil {
		if errors.Is(err, sandbox.ErrCapabilityNotSupported) {
			return s.Delete(ctx, name)
		}
		return fmt.Errorf("sandbox hibernate %s: %w", status.SandboxID, err)
	}
	return nil
}

func (s *SandboxBackend) Status(ctx context.Context, name string) (*WorkerResult, error) {
	cfg, err := s.resolveProviderConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve config for status: %w", err)
	}

	claimID := s.containerPrefix + name
	status, err := s.plugin.GetSandboxClaimStatus(ctx, claimID, cfg)
	if err == nil {
		return s.workerResultFromSandboxClaimStatus(ctx, name, claimID, status, cfg)
	}
	if !errors.Is(err, sandbox.ErrNotFound) {
		return s.workerResultFromSandboxStatus(name, status, err)
	}

	return s.workerResultFromActualSandboxes(
		ctx,
		name,
		cfg,
		func() (*WorkerResult, error) {
			return s.workerResultFromSandboxStatus(name, sandbox.SandboxStatus{}, sandbox.ErrNotFound)
		},
		fmt.Sprintf("after SandboxClaim %s not found", claimID),
	)
}

func (s *SandboxBackend) workerResultFromSandboxClaimStatus(ctx context.Context, name, claimID string, status sandbox.SandboxStatus, cfg sandbox.ProviderConfig) (*WorkerResult, error) {
	if status.Phase != sandbox.PhaseCompleted {
		return &WorkerResult{
			Name:           name,
			Backend:        "sandbox",
			DeploymentMode: DeployCloud,
			Status:         StatusStarting,
			Message:        status.Message,
			RawStatus:      status.Phase,
		}, nil
	}

	if !sandboxClaimReplicasSatisfied(status) {
		detail := fmt.Sprintf("SandboxClaim replicas not satisfied (claimedReplicas=%s, spec.replicas=%s)",
			formatReplicaPtr(status.ClaimedReplicas), formatReplicaPtr(status.DesiredReplicas))
		return &WorkerResult{
			Name:           name,
			Backend:        "sandbox",
			DeploymentMode: DeployCloud,
			Status:         StatusFailed,
			Message:        appendStatusMessage(status.Message, detail),
			RawStatus:      status.Phase,
		}, nil
	}

	return s.workerResultFromActualSandboxes(
		ctx,
		name,
		cfg,
		func() (*WorkerResult, error) {
			return &WorkerResult{
				Name:           name,
				Backend:        "sandbox",
				DeploymentMode: DeployCloud,
				Status:         StatusStarting,
				Message:        appendStatusMessage(status.Message, "SandboxClaim completed but no matching sandbox found"),
				RawStatus:      status.Phase,
			}, nil
		},
		fmt.Sprintf("after SandboxClaim %s completed", claimID),
	)
}

func (s *SandboxBackend) workerResultFromActualSandboxes(ctx context.Context, name string, cfg sandbox.ProviderConfig, noMatch func() (*WorkerResult, error), multipleContext string) (*WorkerResult, error) {
	claimID := s.containerPrefix + name
	statuses, err := s.listActualSandboxes(ctx, name, cfg)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes for status %s: %w", name, err)
	}
	switch len(statuses) {
	case 0:
		return noMatch()
	case 1:
		return s.workerResultFromSandboxStatus(name, statuses[0], nil)
	default:
		if multipleContext == "" {
			multipleContext = fmt.Sprintf("after SandboxClaim %s lookup", claimID)
		}
		message := fmt.Sprintf("multiple sandboxes match %s %s: %s", name, multipleContext, sandboxStatusNames(statuses))
		return &WorkerResult{
			Name:           name,
			Backend:        "sandbox",
			DeploymentMode: DeployCloud,
			Status:         StatusUnknown,
			Message:        message,
			RawStatus:      "multiple_sandboxes",
		}, nil
	}
}

func (s *SandboxBackend) boundOrSingleActualSandbox(ctx context.Context, name string, cfg sandbox.ProviderConfig) (sandbox.SandboxStatus, error) {
	if _, _, err := s.boundSandboxStatus(ctx, name, cfg); err != nil {
		return sandbox.SandboxStatus{}, fmt.Errorf("SandboxClaim status %s: %w", s.containerPrefix+name, err)
	}
	statuses, err := s.listActualSandboxes(ctx, name, cfg)
	if err != nil {
		return sandbox.SandboxStatus{}, fmt.Errorf("list sandboxes for %s: %w", name, err)
	}
	switch len(statuses) {
	case 0:
		return sandbox.SandboxStatus{}, sandbox.ErrNotFound
	case 1:
		return statuses[0], nil
	default:
		return sandbox.SandboxStatus{}, fmt.Errorf("%w: multiple sandboxes match %s after SandboxClaim %s not found: %s", ErrConflict, name, s.containerPrefix+name, sandboxStatusNames(statuses))
	}
}

func (s *SandboxBackend) boundSandboxStatus(ctx context.Context, name string, cfg sandbox.ProviderConfig) (sandbox.SandboxStatus, bool, error) {
	claimID := s.containerPrefix + name
	status, err := s.plugin.GetSandboxClaimStatus(ctx, claimID, cfg)
	if err == nil {
		return status, true, nil
	}
	if errors.Is(err, sandbox.ErrNotFound) {
		return sandbox.SandboxStatus{}, false, nil
	}
	return sandbox.SandboxStatus{}, false, err
}

func sandboxClaimReplicasSatisfied(status sandbox.SandboxStatus) bool {
	return status.DesiredReplicas != nil &&
		status.ClaimedReplicas != nil &&
		*status.ClaimedReplicas == *status.DesiredReplicas
}

func formatReplicaPtr(v *int64) string {
	if v == nil {
		return "missing"
	}
	return fmt.Sprintf("%d", *v)
}

func appendStatusMessage(message, detail string) string {
	if message == "" {
		return detail
	}
	if detail == "" {
		return message
	}
	return message + "; " + detail
}

func (s *SandboxBackend) listActualSandboxes(ctx context.Context, name string, cfg sandbox.ProviderConfig) ([]sandbox.SandboxStatus, error) {
	type query struct {
		labels map[string]string
	}
	withController := func(key, value string) map[string]string {
		out := map[string]string{key: value}
		if s.config.ControllerName != "" {
			out[v1beta1.LabelController] = s.config.ControllerName
		}
		return out
	}
	queries := []query{
		{labels: withController(v1beta1.LabelWorker, name)},
	}
	for _, managerName := range s.managerLabelCandidates(name) {
		queries = append(queries, query{labels: withController(v1beta1.LabelManager, managerName)})
	}

	seen := map[string]struct{}{}
	out := []sandbox.SandboxStatus{}
	for _, q := range queries {
		statuses, err := s.plugin.ListSandboxes(ctx, q.labels, cfg)
		if err != nil {
			return nil, err
		}
		for _, status := range statuses {
			if status.SandboxID == "" {
				continue
			}
			if _, ok := seen[status.SandboxID]; ok {
				continue
			}
			seen[status.SandboxID] = struct{}{}
			out = append(out, status)
		}
	}
	return out, nil
}

func (s *SandboxBackend) managerLabelCandidates(name string) []string {
	out := []string{name}
	prefix := s.config.ResourcePrefix
	if prefix == "" {
		prefix = "agentteams-"
	}
	defaultManagerName := prefix + "manager"
	if name == defaultManagerName {
		out = appendUniqueString(out, "default")
	}
	managerPrefix := prefix + "manager-"
	if strings.HasPrefix(name, managerPrefix) {
		managerName := strings.TrimPrefix(name, managerPrefix)
		if managerName != "" {
			out = appendUniqueString(out, managerName)
		}
	}
	return out
}

func appendUniqueString(values []string, candidate string) []string {
	for _, value := range values {
		if value == candidate {
			return values
		}
	}
	return append(values, candidate)
}

func sandboxStatusNames(statuses []sandbox.SandboxStatus) string {
	names := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if status.SandboxID != "" {
			names = append(names, status.SandboxID)
		}
	}
	return strings.Join(names, ",")
}

func (s *SandboxBackend) workerResultFromSandboxStatus(name string, status sandbox.SandboxStatus, err error) (*WorkerResult, error) {
	claimID := s.containerPrefix + name
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
		return nil, fmt.Errorf("sandbox status %s: %w", claimID, err)
	}

	workerStatus := mapSandboxPhaseToWorkerStatus(status.Phase)
	var message string
	// When phase maps to Running, additionally check the Ready condition.
	// Ready=False + message: container has crashed → StatusFailed.
	// Ready=False + no message: container still starting → StatusStarting.
	if workerStatus == StatusRunning && !status.ReadyConditionStatus {
		if status.ReadyConditionMessage != "" {
			workerStatus = StatusFailed
			message = status.ReadyConditionMessage
		} else {
			workerStatus = StatusStarting
		}
	}
	return &WorkerResult{
		Name:            name,
		Backend:         "sandbox",
		DeploymentMode:  DeployCloud,
		Status:          workerStatus,
		Message:         message,
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
		return StatusSleeping
	case sandbox.PhaseFailed:
		return StatusUnknown
	case sandbox.PhaseTerminated:
		return StatusNotFound
	default:
		return StatusUnknown
	}
}

// WatchObject returns an unstructured Sandbox watch object.
func (s *SandboxBackend) WatchObject() client.Object {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "agents.kruise.io",
		Version: "v1alpha1",
		Kind:    "Sandbox",
	})
	return obj
}

// ClaimWatchObject returns an unstructured SandboxClaim watch object.
func (s *SandboxBackend) ClaimWatchObject() client.Object {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "agents.kruise.io",
		Version: "v1alpha1",
		Kind:    "SandboxClaim",
	})
	return obj
}

// ProviderGVR returns the GroupVersionResource of the Sandbox CRD.
func (s *SandboxBackend) ProviderGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "agents.kruise.io",
		Version:  "v1alpha1",
		Resource: "sandboxes",
	}
}

// ClaimProviderGVR returns the GroupVersionResource of the SandboxClaim CRD.
func (s *SandboxBackend) ClaimProviderGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "agents.kruise.io",
		Version:  "v1alpha1",
		Resource: "sandboxclaims",
	}
}

func (s *SandboxBackend) sandboxClaimName(req CreateRequest) string {
	if req.ContainerName != "" {
		return req.ContainerName
	}
	if req.NamePrefix != "" {
		return req.NamePrefix + req.Name
	}
	return s.containerPrefix + req.Name
}

// workerNamePrefix returns the default worker SA name prefix, e.g.
// "agentteams-worker-". Used only when a CreateRequest arrives without an
// explicit ServiceAccountName (production callers always set one).
func (s *SandboxBackend) workerNamePrefix() string {
	if s.config.ResourcePrefix == "" {
		return "agentteams-worker-"
	}
	return s.config.ResourcePrefix + "worker-"
}

// NewSandboxBackendFromConfig creates a fully wired SandboxBackend using the
// standard in-cluster/kubeconfig K8s config path. This is the entry point
// called by buildWorkerBackends in app.go.
func NewSandboxBackendFromConfig(cfg SandboxConfig, containerPrefix string, scheme *runtime.Scheme, capabilities string, remoteCache RemoteClusterClientProvider) (*SandboxBackend, error) {
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

	return NewSandboxBackend(selectedPlugin, providerConfig, cfg, containerPrefix, scheme, k8sClient, remoteCache), nil
}

// ParseCapabilities parses a comma-separated capabilities string into
// ProviderCapabilities. Every recognized token enables one capability.
// Unknown tokens and the empty string both yield the zero value (all
// capabilities disabled), so administrators can opt out cleanly by not
// setting AGENTTEAMS_SANDBOX_CAPABILITIES. To enable Hibernate, set the env
// to "hibernate".
func ParseCapabilities(s string) sandbox.ProviderCapabilities {
	caps := sandbox.ProviderCapabilities{}
	for _, part := range strings.Split(s, ",") {
		switch strings.TrimSpace(strings.ToLower(part)) {
		case "hibernate":
			caps.Hibernate = true
		case "pool":
			caps.Pool = true
		}
	}
	return caps
}

func claimInplaceUpdate(deps *WorkerDepsSpec, fallbackImage string) *sandbox.SandboxClaimInplaceUpdateSpec {
	image := fallbackImage
	if deps != nil && deps.InplaceUpdateImage != "" {
		image = deps.InplaceUpdateImage
	}
	if image == "" {
		return nil
	}
	return &sandbox.SandboxClaimInplaceUpdateSpec{Image: image}
}

func claimDynamicVolumes(deps *WorkerDepsSpec) []sandbox.SandboxClaimDynamicVolumeMount {
	if deps == nil || len(deps.DynamicVolumeMounts) == 0 {
		return nil
	}
	out := make([]sandbox.SandboxClaimDynamicVolumeMount, 0, len(deps.DynamicVolumeMounts))
	for _, mount := range deps.DynamicVolumeMounts {
		out = append(out, sandbox.SandboxClaimDynamicVolumeMount{
			PVName:     mount.PVName,
			MountPath:  mount.MountPath,
			SubPath:    mount.SubPath,
			ReadOnly:   mount.ReadOnly,
			Attributes: cloneStringMap(mount.Attributes),
		})
	}
	return out
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
