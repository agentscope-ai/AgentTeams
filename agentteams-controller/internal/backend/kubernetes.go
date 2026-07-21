package backend

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

const defaultK8sNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// K8sConfig holds Kubernetes backend configuration.
type K8sConfig struct {
	Namespace            string
	WorkerImage          string
	CopawWorkerImage     string
	HermesWorkerImage    string
	OpenHumanWorkerImage string
	QwenPawWorkerImage   string
	WorkerCPU            string
	WorkerMemory         string

	// ControllerName identifies this controller instance. The agent
	// PodTemplateSpec overlay (see LoadAgentPodTemplate) is looked up as the
	// ConfigMap named exactly ControllerName in the controller's own
	// Namespace, with key "pod-template.yaml". Empty ControllerName, a
	// missing ConfigMap, or any API / parse error all collapse to "no
	// overlay" (Pod creation proceeds unchanged).
	ControllerName string

	// ResourcePrefix is the tenant prefix used to derive worker "app" label
	// values and default SA names. Empty falls back to "hiclaw-" for tests
	// and out-of-cluster callers. See internal/auth.ResourcePrefix for
	// semantics.
	ResourcePrefix string
}

// K8sBackend manages worker lifecycle via Kubernetes Pods.
type K8sBackend struct {
	client          K8sCoreClient
	config          K8sConfig
	containerPrefix string

	// scheme is used to resolve GVK for CreateRequest.Owner when stamping
	// the child Pod's controller OwnerReference via
	// controllerutil.SetControllerReference. A nil scheme means "callers
	// never supply Owner" — typical for unit tests that don't exercise
	// ownerRef behaviour.
	scheme *runtime.Scheme

	// namespace is a convenience alias for config.Namespace used by
	// resolveClient to return the local namespace.
	namespace string
}

// K8sServiceAccountClient is the minimal ServiceAccount client surface needed.
type K8sServiceAccountClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ServiceAccount, error)
	Create(ctx context.Context, sa *corev1.ServiceAccount, opts metav1.CreateOptions) (*corev1.ServiceAccount, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// K8sTokenReviewClient is the minimal TokenReview client surface needed for authentication.
type K8sTokenReviewClient interface {
	Create(ctx context.Context, review *authenticationv1.TokenReview, opts metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

// K8sCoreClient is the minimal CoreV1 client surface needed by the backend.
type K8sCoreClient interface {
	Pods(namespace string) K8sPodClient
	ConfigMaps(namespace string) K8sConfigMapClient
	Services(namespace string) K8sServiceClient
	Namespaces() K8sNamespaceClient
	ServiceAccounts(namespace string) K8sServiceAccountClient
	TokenReviews() K8sTokenReviewClient
}

// K8sPodClient is the minimal Pod client surface needed by the backend.
type K8sPodClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Pod, error)
	Create(ctx context.Context, pod *corev1.Pod, opts metav1.CreateOptions) (*corev1.Pod, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// K8sConfigMapClient is the minimal ConfigMap client surface needed by the
// backend. Only Get is exposed — ConfigMaps are consumed read-only for the
// agent pod template.
type K8sConfigMapClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error)
}

// k8sCoreClientWrapper adapts *corev1client.CoreV1Client to K8sCoreClient.
type k8sCoreClientWrapper struct {
	client     *corev1client.CoreV1Client
	authClient *authenticationv1client.AuthenticationV1Client
}

func (w *k8sCoreClientWrapper) Pods(namespace string) K8sPodClient {
	return w.client.Pods(namespace)
}

func (w *k8sCoreClientWrapper) ConfigMaps(namespace string) K8sConfigMapClient {
	return w.client.ConfigMaps(namespace)
}

func (w *k8sCoreClientWrapper) Services(namespace string) K8sServiceClient {
	return w.client.Services(namespace)
}

func (w *k8sCoreClientWrapper) Namespaces() K8sNamespaceClient {
	return w.client.Namespaces()
}

func (w *k8sCoreClientWrapper) ServiceAccounts(namespace string) K8sServiceAccountClient {
	return w.client.ServiceAccounts(namespace)
}

func (w *k8sCoreClientWrapper) TokenReviews() K8sTokenReviewClient {
	return w.authClient.TokenReviews()
}

// NewK8sBackend creates a Kubernetes backend using in-cluster config or kubeconfig.
// scheme is used by Create to stamp CR-to-Pod controller OwnerReferences
// (see CreateRequest.Owner); it must have all CR kinds that might appear as
// Owner registered.
func NewK8sBackend(config K8sConfig, containerPrefix string, scheme *runtime.Scheme) (*K8sBackend, error) {
	return NewK8sBackendWithCache(config, containerPrefix, scheme, nil)
}

// NewK8sBackendWithCache creates a Kubernetes backend using in-cluster config
// or kubeconfig. The remoteCache argument is retained only for call-site
// compatibility; OSS controllers no longer route backend operations to target
// clusters.
func NewK8sBackendWithCache(config K8sConfig, containerPrefix string, scheme *runtime.Scheme, remoteCache RemoteClientProvider) (*K8sBackend, error) {
	restConfig, err := loadK8sRESTConfig()
	if err != nil {
		return nil, err
	}
	clientset, err := corev1client.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	authClient, err := authenticationv1client.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create authentication client: %w", err)
	}
	return NewK8sBackendWithClient(&k8sCoreClientWrapper{client: clientset, authClient: authClient}, config, containerPrefix, scheme), nil
}

// NewK8sBackendWithClient creates a Kubernetes backend with a custom client.
// scheme may be nil in tests that don't set CreateRequest.Owner.
func NewK8sBackendWithClient(client K8sCoreClient, config K8sConfig, containerPrefix string, scheme *runtime.Scheme) *K8sBackend {
	if config.Namespace == "" {
		config.Namespace = detectK8sNamespace()
	}
	if config.WorkerCPU == "" {
		config.WorkerCPU = "1000m"
	}
	if config.WorkerMemory == "" {
		config.WorkerMemory = "2Gi"
	}
	return &K8sBackend{
		client:          client,
		config:          config,
		containerPrefix: containerPrefix,
		scheme:          scheme,
		namespace:       config.Namespace,
	}
}

// WithPrefix returns a shallow copy of the backend with a different container name prefix.
// The returned backend shares the same client (safe — K8sCoreClient is stateless).
// Use WithPrefix("") to disable prefix for containers that already have full names
// (e.g. Manager containers named "agentteams-manager" rather than "agentteams-worker-X").
func (k *K8sBackend) WithPrefix(prefix string) *K8sBackend {
	cp := *k
	cp.containerPrefix = prefix
	return &cp
}

func (k *K8sBackend) resolveClient(ctx context.Context) (K8sCoreClient, string, error) {
	return k.client, k.namespace, nil
}

// ServiceClient implements ServiceBackend.
func (k *K8sBackend) ServiceClient(ctx context.Context) (K8sServiceClient, string, error) {
	client, ns, err := k.resolveClient(ctx)
	if err != nil {
		return nil, "", err
	}
	return client.Services(ns), ns, nil
}

func (k *K8sBackend) Name() string                   { return "k8s" }
func (k *K8sBackend) DeploymentMode() string         { return DeployCloud }
func (k *K8sBackend) NeedsCredentialInjection() bool { return true }

func (k *K8sBackend) Available(_ context.Context) bool {
	return k.client != nil && k.config.Namespace != ""
}

func (k *K8sBackend) Create(ctx context.Context, req CreateRequest) (*WorkerResult, error) {
	// Resolve effective runtime once: explicit > caller fallback > openclaw.
	// See ResolveRuntime godoc — the Worker / Manager CRDs intentionally have
	// no schema-level default, so the only place the operator-side env var can
	// take effect is here, via the caller-provided RuntimeFallback (which the
	// reconciler picks per-resource: AGENTTEAMS_MANAGER_RUNTIME for managers,
	// AGENTTEAMS_DEFAULT_WORKER_RUNTIME for workers).
	req.Runtime = ResolveRuntime(req.Runtime, req.RuntimeFallback)

	targetClient, targetNS, err := k.resolveClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve client for create: %w", err)
	}

	podName := req.ContainerName
	if podName == "" {
		podName = k.podName(req.NamePrefix, req.Name)
	}
	if _, err := targetClient.Pods(targetNS).Get(ctx, podName, metav1.GetOptions{}); err == nil {
		return nil, fmt.Errorf("%w: pod %q", ErrConflict, podName)
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("kubernetes get pod %s: %w", podName, err)
	}

	if req.Env == nil {
		req.Env = make(map[string]string)
	}
	mergeOSSRegionFromProcessEnv(req.Env)
	if rt := firstNonEmptyTrimmed(os.Getenv("AGENTTEAMS_RUNTIME")); rt != "" {
		req.Env["AGENTTEAMS_RUNTIME"] = rt
	} else {
		req.Env["AGENTTEAMS_RUNTIME"] = "k8s"
	}
	if req.ControllerURL != "" {
		req.Env["AGENTTEAMS_CONTROLLER_URL"] = req.ControllerURL
	}
	// SA token is mounted via projected volume; tell the worker where to read it.
	req.Env["AGENTTEAMS_AUTH_TOKEN_FILE"] = "/var/run/secrets/agentteams/token"

	image := req.Image
	if image == "" {
		switch {
		case req.Runtime == RuntimeCopaw && k.config.CopawWorkerImage != "":
			image = k.config.CopawWorkerImage
		case req.Runtime == RuntimeHermes && k.config.HermesWorkerImage != "":
			image = k.config.HermesWorkerImage
		case req.Runtime == RuntimeOpenHuman && k.config.OpenHumanWorkerImage != "":
			image = k.config.OpenHumanWorkerImage
		case req.Runtime == RuntimeQwenPaw && k.config.QwenPawWorkerImage != "":
			image = k.config.QwenPawWorkerImage
		case k.config.WorkerImage != "":
			image = k.config.WorkerImage
		}
	}
	if image == "" {
		return nil, fmt.Errorf("no worker image configured for kubernetes backend")
	}

	if req.WorkingDir == "" {
		switch {
		case req.Runtime == RuntimeCopaw:
			req.WorkingDir = fmt.Sprintf("/root/agentteams-fs/agents/%s", req.Name)
			if req.Env == nil {
				req.Env = map[string]string{}
			}
			req.Env["HOME"] = req.WorkingDir
		default:
			// Both openclaw and hermes use the same workspace layout:
			// HOME == WorkingDir == /root/agentteams-fs/agents/<name> (== MinIO
			// mirror root). The hermes entrypoint anchors its install_dir to
			// the same location so workspace_dir == HOME and HERMES_HOME ==
			// $HOME/.hermes.
			if home := req.Env["HOME"]; home != "" {
				req.WorkingDir = home
			} else {
				req.WorkingDir = fmt.Sprintf("/root/agentteams-fs/agents/%s", req.Name)
				req.Env["HOME"] = req.WorkingDir
			}
		}
	}

	defaultResources := buildDefaultResources(k.config.WorkerCPU, k.config.WorkerMemory)
	var resourcesOverride *corev1.ResourceRequirements
	if req.Resources != nil {
		merged := mergeResourceOverrides(defaultResources, req.Resources)
		resourcesOverride = &merged
	}

	agentContainer := corev1.Container{
		Name:            "worker",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             buildK8sEnvVars(req.Env),
		WorkingDir:      req.WorkingDir,
	}

	tokenAudience := req.AuthAudience
	if tokenAudience == "" {
		tokenAudience = "agentteams-controller"
	}
	tokenExpSeconds := NormalizeAuthTokenExpirationSeconds(req.AuthExpirationSeconds)
	tokenVolume := corev1.Volume{
		Name: "agentteams-token",
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
		Name:      "agentteams-token",
		MountPath: "/var/run/secrets/agentteams",
		ReadOnly:  true,
	}
	extraVolumes, extraVolumeMounts := podWorkerDepsVolumes(req.WorkersDeps)

	saName := req.ServiceAccountName
	if saName == "" {
		saName = k.workerNamePrefix() + req.Name
	}

	// Callers own the full label set except agentteams.io/runtime, which the
	// backend stamps because it knows the resolved runtime value (after
	// CRD spec + operator-default fallback).
	podLabels := map[string]string{
		v1beta1.LabelRuntime: defaultRuntime(req.Runtime),
	}
	for k, v := range req.Labels {
		podLabels[k] = v
	}

	tmpl := LoadAgentPodTemplate(ctx, k.client, k.config.Namespace, k.config.ControllerName, req.DeployMode)

	pod := ApplyPodTemplate(tmpl, PodOverlay{
		Name:               podName,
		Namespace:          targetNS,
		Labels:             podLabels,
		Annotations:        nil,
		ServiceAccountName: saName,
		Container:          agentContainer,
		ResourcesOverride:  resourcesOverride,
		DefaultResources:   defaultResources,
		TokenVolume:        tokenVolume,
		TokenVolumeMount:   tokenVolumeMount,
		ExtraVolumes:       extraVolumes,
		ExtraVolumeMounts:  extraVolumeMounts,
		HostAliases:        buildHostAliases(req.ExtraHosts),
	})

	if req.Owner != nil {
		if k.scheme == nil {
			return nil, fmt.Errorf("kubernetes backend: scheme is required when CreateRequest.Owner is set")
		}
		if err := controllerutil.SetControllerReference(req.Owner, pod, k.scheme); err != nil {
			return nil, fmt.Errorf("set owner reference on pod %s: %w", podName, err)
		}
	}

	created, err := targetClient.Pods(targetNS).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("%w: pod %q", ErrConflict, podName)
		}
		return nil, fmt.Errorf("kubernetes create pod %s: %w", podName, err)
	}

	return &WorkerResult{
		Name:      req.Name,
		Backend:   "k8s",
		Status:    StatusStarting,
		RawStatus: rawK8sPhase(created.Status.Phase),
	}, nil
}

func (k *K8sBackend) Delete(ctx context.Context, name string) error {
	targetClient, targetNS, err := k.resolveClient(ctx)
	if err != nil {
		return fmt.Errorf("resolve client for delete: %w", err)
	}
	podName := k.workerPodName(name)
	err = targetClient.Pods(targetNS).Delete(ctx, podName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kubernetes delete pod %s: %w", podName, err)
	}
	return nil
}

func (k *K8sBackend) Start(ctx context.Context, name string) error {
	targetClient, targetNS, err := k.resolveClient(ctx)
	if err != nil {
		return fmt.Errorf("resolve client for start: %w", err)
	}
	pod, err := targetClient.Pods(targetNS).Get(ctx, k.workerPodName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: worker %q", ErrNotFound, name)
	}
	if err != nil {
		return fmt.Errorf("kubernetes get pod %s: %w", k.workerPodName(name), err)
	}

	switch pod.Status.Phase {
	case corev1.PodRunning, corev1.PodPending:
		return nil
	default:
		return fmt.Errorf("kubernetes worker %q cannot be started from phase %q; recreate it instead", name, pod.Status.Phase)
	}
}

func (k *K8sBackend) Stop(ctx context.Context, name string) error {
	return k.Delete(ctx, name)
}

func (k *K8sBackend) Status(ctx context.Context, name string) (*WorkerResult, error) {
	targetClient, targetNS, err := k.resolveClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve client for status: %w", err)
	}
	pod, err := targetClient.Pods(targetNS).Get(ctx, k.workerPodName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &WorkerResult{Name: name, Backend: "k8s", Status: StatusNotFound}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes get pod %s: %w", k.workerPodName(name), err)
	}
	status := normalizeK8sPodPhase(pod.Status.Phase)
	var message string
	rawStatus := rawK8sPhase(pod.Status.Phase)

	// Container waiting/terminated states carry the real failure reason for
	// cases such as ImagePullBackOff while the Pod phase is still Pending.
	if containerStatus, containerMessage, containerRaw, ok := podContainerFailureStatus(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses); ok {
		status = containerStatus
		message = containerMessage
		rawStatus = containerRaw
	} else if status == StatusRunning {
		// When phase maps to Running, additionally check the Ready condition.
		// A pod can have phase Running but Ready=False (e.g. CrashLoopBackOff).
		if msg, ready := podReadyCondition(pod.Status.Conditions); !ready {
			if msg != "" {
				// Ready=False + message: container has an actual error.
				status = StatusFailed
				message = msg
			} else {
				// Ready=False + no message: container still starting up.
				status = StatusStarting
			}
		}
	}

	return &WorkerResult{
		Name:           name,
		Backend:        "k8s",
		DeploymentMode: DeployCloud,
		Status:         status,
		Message:        message,
		RawStatus:      rawStatus,
	}, nil
}

func podContainerFailureStatus(statusGroups ...[]corev1.ContainerStatus) (WorkerStatus, string, string, bool) {
	for _, statuses := range statusGroups {
		for i := range statuses {
			cs := statuses[i]
			if waiting := cs.State.Waiting; waiting != nil {
				reason := strings.TrimSpace(waiting.Reason)
				if isK8sContainerFailureReason(reason) {
					return StatusFailed, formatK8sContainerStateMessage(cs.Name, reason, waiting.Message), reason, true
				}
			}
			if terminated := cs.State.Terminated; terminated != nil && terminated.ExitCode != 0 {
				reason := strings.TrimSpace(terminated.Reason)
				if reason == "" {
					reason = fmt.Sprintf("ExitCode%d", terminated.ExitCode)
				}
				return StatusFailed, formatK8sContainerStateMessage(cs.Name, reason, terminated.Message), reason, true
			}
		}
	}
	return "", "", "", false
}

func isK8sContainerFailureReason(reason string) bool {
	switch reason {
	case "CrashLoopBackOff",
		"CreateContainerConfigError",
		"CreateContainerError",
		"ErrImageNeverPull",
		"ErrImagePull",
		"ImageInspectError",
		"ImagePullBackOff",
		"InvalidImageName",
		"RegistryUnavailable",
		"RunContainerError":
		return true
	default:
		return false
	}
}

func formatK8sContainerStateMessage(containerName, reason, message string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "container failed"
	}
	if containerName != "" {
		reason = fmt.Sprintf("container %s: %s", containerName, reason)
	}
	if msg := strings.TrimSpace(message); msg != "" {
		return reason + ": " + msg
	}
	return reason
}

// podReadyCondition finds the Ready condition and returns (message, ready).
//   - No Ready condition found → ("", true) — conditions not yet populated.
//   - Ready.Status == True    → ("", true) — container is healthy.
//   - Ready.Status != True    → (Ready.Message, false) — container not ready;
//     message may be empty (still starting) or non-empty (actual error).
func podReadyCondition(conditions []corev1.PodCondition) (string, bool) {
	for i := range conditions {
		if conditions[i].Type == corev1.PodReady {
			if conditions[i].Status == corev1.ConditionTrue {
				return "", true
			}
			return conditions[i].Message, false
		}
	}
	// No Ready condition yet — treat as healthy (backward compat).
	return "", true
}

func (k *K8sBackend) podName(prefix, name string) string {
	if prefix != "" {
		return prefix + name
	}
	return k.containerPrefix + name
}

func (k *K8sBackend) workerPodName(name string) string {
	return k.containerPrefix + name
}

// workerNamePrefix returns the default worker SA name prefix, e.g.
// "agentteams-worker-". Used only when a CreateRequest arrives without an
// explicit ServiceAccountName (production callers always set one).
func (k *K8sBackend) workerNamePrefix() string {
	if k.config.ResourcePrefix == "" {
		return "agentteams-worker-"
	}
	return k.config.ResourcePrefix + "worker-"
}

// buildDefaultResources constructs the backend-level default ResourceRequirements
// that apply when neither the CreateRequest nor the agent pod template
// specifies resources. Request side is fixed at "100m" / "256Mi" to match
// historical behavior; limits come from K8sConfig.WorkerCPU / WorkerMemory.
func buildDefaultResources(workerCPU, workerMemory string) corev1.ResourceRequirements {
	if workerCPU == "" {
		workerCPU = "1000m"
	}
	if workerMemory == "" {
		workerMemory = "2Gi"
	}
	return corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(workerCPU),
			corev1.ResourceMemory: resource.MustParse(workerMemory),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

// mergeResourceOverrides layers a ResourceRequirements override (from
// CreateRequest.Resources) on top of defaults, field by field.
func mergeResourceOverrides(defaults corev1.ResourceRequirements, override *ResourceRequirements) corev1.ResourceRequirements {
	out := *defaults.DeepCopy()
	if override == nil {
		return out
	}
	if override.CPULimit != "" {
		out.Limits[corev1.ResourceCPU] = resource.MustParse(override.CPULimit)
	}
	if override.MemoryLimit != "" {
		out.Limits[corev1.ResourceMemory] = resource.MustParse(override.MemoryLimit)
	}
	if override.CPURequest != "" {
		out.Requests[corev1.ResourceCPU] = resource.MustParse(override.CPURequest)
	}
	if override.MemoryRequest != "" {
		out.Requests[corev1.ResourceMemory] = resource.MustParse(override.MemoryRequest)
	}
	return out
}

// mergeOSSRegionFromProcessEnv sets AGENTTEAMS_FS_BUCKET and AGENTTEAMS_REGION when the client
// omitted them; the controller process should already have these from the same Secret as Manager (envFrom).
func mergeOSSRegionFromProcessEnv(env map[string]string) {
	if env == nil {
		return
	}
	bucket := firstNonEmptyTrimmed(
		env["AGENTTEAMS_FS_BUCKET"],
		os.Getenv("AGENTTEAMS_FS_BUCKET"),
	)
	if bucket != "" && strings.TrimSpace(env["AGENTTEAMS_FS_BUCKET"]) == "" {
		env["AGENTTEAMS_FS_BUCKET"] = bucket
	}
	if v := firstNonEmptyTrimmed(os.Getenv("AGENTTEAMS_REGION")); v != "" && strings.TrimSpace(env["AGENTTEAMS_REGION"]) == "" {
		env["AGENTTEAMS_REGION"] = v
	}
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func buildK8sEnvVars(env map[string]string) []corev1.EnvVar {
	keys := make([]string, 0, len(env))
	for k := range env {
		if env[k] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var out []corev1.EnvVar
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}
	return out
}

func podWorkerDepsVolumes(deps *WorkerDepsSpec) ([]corev1.Volume, []corev1.VolumeMount) {
	if deps == nil || deps.PodVolume == nil || len(deps.PodVolume.Mounts) == 0 {
		return nil, nil
	}
	vol := corev1.Volume{
		Name: deps.PodVolume.Name,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: deps.PodVolume.ClaimName,
			},
		},
	}
	mounts := make([]corev1.VolumeMount, 0, len(deps.PodVolume.Mounts))
	for _, mount := range deps.PodVolume.Mounts {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      deps.PodVolume.Name,
			MountPath: mount.MountPath,
			SubPath:   mount.SubPath,
			ReadOnly:  mount.ReadOnly,
		})
	}
	return []corev1.Volume{vol}, mounts
}

func buildHostAliases(extraHosts []string) []corev1.HostAlias {
	byIP := map[string][]string{}
	for _, entry := range extraHosts {
		host, ip, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || host == "" || ip == "" {
			continue
		}
		byIP[ip] = append(byIP[ip], host)
	}
	if len(byIP) == 0 {
		return nil
	}

	ips := make([]string, 0, len(byIP))
	for ip := range byIP {
		ips = append(ips, ip)
	}
	sort.Strings(ips)

	aliases := make([]corev1.HostAlias, 0, len(ips))
	for _, ip := range ips {
		hosts := byIP[ip]
		sort.Strings(hosts)
		aliases = append(aliases, corev1.HostAlias{
			IP:        ip,
			Hostnames: hosts,
		})
	}
	return aliases
}

func normalizeK8sPodPhase(phase corev1.PodPhase) WorkerStatus {
	switch phase {
	case corev1.PodRunning:
		return StatusRunning
	case corev1.PodPending:
		return StatusStarting
	case corev1.PodSucceeded, corev1.PodFailed:
		return StatusStopped
	default:
		return StatusUnknown
	}
}

func rawK8sPhase(phase corev1.PodPhase) string {
	if phase == "" {
		return "Pending"
	}
	return string(phase)
}

func defaultRuntime(runtime string) string {
	switch runtime {
	case RuntimeCopaw:
		return RuntimeCopaw
	case RuntimeHermes:
		return RuntimeHermes
	case RuntimeQwenPaw:
		return RuntimeQwenPaw
	default:
		return RuntimeOpenClaw
	}
}

func loadK8sRESTConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = clientcmd.RecommendedHomeFile
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		return nil, fmt.Errorf("load kubernetes config: no in-cluster config and kubeconfig %q not found", kubeconfig)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubernetes kubeconfig %q: %w", kubeconfig, err)
	}
	return cfg, nil
}

func detectK8sNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("AGENTTEAMS_K8S_NAMESPACE")); ns != "" {
		return ns
	}
	if data, err := os.ReadFile(defaultK8sNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return ""
}

func boolPtr(v bool) *bool {
	return &v
}
