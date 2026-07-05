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

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
)

const defaultK8sNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// K8sConfig holds Kubernetes backend configuration.
type K8sConfig struct {
	Namespace            string
	WorkerImage          string
	CopawWorkerImage     string
	HermesWorkerImage    string
	OpenHumanWorkerImage string
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
	remoteCache     RemoteClientProvider // remote cluster client cache (may be nil)
	config          K8sConfig
	containerPrefix string

	// Fields for remote routing (set via WithRemoteTarget).
	deployMode      string
	targetClusterID string
	targetNamespace string

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
// or kubeconfig, and wires in an optional remote-cluster client cache used
// to route operations against Workers/Managers deployed to remote clusters.
// remoteCache may be nil when no remote clusters are configured; in that
// case the backend behaves identically to NewK8sBackend.
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
	return NewK8sBackendWithRemote(&k8sCoreClientWrapper{client: clientset, authClient: authClient}, remoteCache, config, containerPrefix, scheme), nil
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

// NewK8sBackendWithRemote creates a Kubernetes backend with remote cluster support.
// remoteCache may be nil if no remote clusters are configured.
func NewK8sBackendWithRemote(client K8sCoreClient, remoteCache RemoteClientProvider, config K8sConfig, containerPrefix string, scheme *runtime.Scheme) *K8sBackend {
	b := NewK8sBackendWithClient(client, config, containerPrefix, scheme)
	b.remoteCache = remoteCache
	return b
}

// WithPrefix returns a shallow copy of the backend with a different container name prefix.
// The returned backend shares the same client (safe — K8sCoreClient is stateless).
// Use WithPrefix("") to disable prefix for containers that already have full names
// (e.g. Manager containers named "hiclaw-manager" rather than "hiclaw-worker-X").
func (k *K8sBackend) WithPrefix(prefix string) *K8sBackend {
	cp := *k
	cp.containerPrefix = prefix
	return &cp
}

// WithRemoteTarget returns a shallow copy of the backend configured to route
// Delete and Status operations to the specified remote cluster/namespace.
// Use this when the controller needs to operate on a pod in a remote cluster
// via the interface methods that only accept a name.
func (k *K8sBackend) WithRemoteTarget(deployMode, clusterID, namespace string) *K8sBackend {
	cp := *k
	cp.deployMode = deployMode
	cp.targetClusterID = clusterID
	cp.targetNamespace = namespace
	return &cp
}

// resolveClient returns the appropriate K8sCoreClient and namespace based on
// deploy mode. For Remote mode, it fetches the client from remoteCache.
func (k *K8sBackend) resolveClient(ctx context.Context, deployMode, targetClusterID, targetNamespace string) (K8sCoreClient, string, error) {
	if deployMode == v1beta1.DeployModeRemote {
		if k.remoteCache == nil {
			return nil, "", fmt.Errorf("remote client cache not configured")
		}
		client, err := k.remoteCache.ResolveClient(ctx, targetClusterID)
		if err != nil {
			return nil, "", fmt.Errorf("failed to get remote client for cluster %s: %w", targetClusterID, err)
		}
		ns := targetNamespace
		if ns == "" {
			ns = k.namespace
		}
		return client, ns, nil
	}
	return k.client, k.namespace, nil
}

// ServiceClient implements ServiceBackend. It returns a K8sServiceClient and
// resolved namespace for the appropriate cluster based on deploy mode.
func (k *K8sBackend) ServiceClient(ctx context.Context, deployMode, targetClusterID, targetNamespace string) (K8sServiceClient, string, error) {
	client, ns, err := k.resolveClient(ctx, deployMode, targetClusterID, targetNamespace)
	if err != nil {
		return nil, "", err
	}
	return client.Services(ns), ns, nil
}

// ensureRemoteNamespace ensures the target namespace exists in the remote cluster.
func (k *K8sBackend) ensureRemoteNamespace(ctx context.Context, client K8sCoreClient, namespace string) error {
	_, err := client.Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check namespace %s: %w", namespace, err)
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}
	_, err = client.Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create namespace %s: %w", namespace, err)
	}
	return nil
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
	// reconciler picks per-resource: HICLAW_MANAGER_RUNTIME for managers,
	// HICLAW_DEFAULT_WORKER_RUNTIME for workers).
	req.Runtime = ResolveRuntime(req.Runtime, req.RuntimeFallback)

	// Resolve the target client and namespace based on deploy mode.
	targetClient, targetNS, err := k.resolveClient(ctx, req.DeployMode, req.TargetClusterID, req.TargetNamespace)
	if err != nil {
		return nil, fmt.Errorf("resolve client for create: %w", err)
	}

	// Ensure the remote namespace exists before creating the Pod.
	if req.DeployMode == v1beta1.DeployModeRemote {
		if err := k.ensureRemoteNamespace(ctx, targetClient, targetNS); err != nil {
			return nil, fmt.Errorf("ensure remote namespace: %w", err)
		}
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
	if rt := os.Getenv("HICLAW_RUNTIME"); rt != "" {
		req.Env["HICLAW_RUNTIME"] = rt
	} else {
		req.Env["HICLAW_RUNTIME"] = "k8s"
	}
	if req.ControllerURL != "" {
		req.Env["HICLAW_CONTROLLER_URL"] = req.ControllerURL
	}
	// SA token is mounted via projected volume; tell the worker where to read it.
	req.Env["HICLAW_AUTH_TOKEN_FILE"] = "/var/run/secrets/hiclaw/token"

	// For OpenHuman runtime, map internal env var names to the aliases expected
	// by openhuman-worker-entrypoint.sh: MATRIX_ACCESS_TOKEN, MATRIX_HOME_ROOM_ID,
	// MATRIX_HOMESERVER_URL. These differ from the HICLAW_* names the controller
	// uses internally.
	if req.Runtime == RuntimeOpenHuman {
		if v := req.Env["HICLAW_WORKER_MATRIX_TOKEN"]; v != "" {
			req.Env["MATRIX_ACCESS_TOKEN"] = v
		}
		if v := req.Env["HICLAW_WORKER_ROOM_ID"]; v != "" {
			req.Env["MATRIX_HOME_ROOM_ID"] = v
		}
		if v := req.Env["HICLAW_MATRIX_URL"]; v != "" {
			req.Env["MATRIX_HOMESERVER_URL"] = v
		}

		// Build MATRIX_USER_ID for openhuman-worker-entrypoint.sh (fallback;
		// primary source is openclaw.json channels.matrix.userId).
		if domain := req.Env["HICLAW_MATRIX_DOMAIN"]; domain != "" && req.Env["HICLAW_WORKER_NAME"] != "" {
			req.Env["MATRIX_USER_ID"] = fmt.Sprintf("@%s:%s", req.Env["HICLAW_WORKER_NAME"], domain)
		}

		// Build MATRIX_ALLOWED_USERS for openhuman-worker-entrypoint.sh.
		// Primary source is openclaw.json (dm.allowFrom + groupAllowFrom)
		// read by the entrypoint's bridge layer; env var serves as fallback.
		var allowedUsers []string
		if domain := req.Env["HICLAW_MATRIX_DOMAIN"]; domain != "" {
			// Admin user — can DM and @mention the worker.
			if admin := os.Getenv("HICLAW_ADMIN_USER"); admin != "" {
				allowedUsers = append(allowedUsers, fmt.Sprintf("@%s:%s", admin, domain))
			}
			// Manager Matrix username is "manager" by convention (see agentconfig/generator.go).
			allowedUsers = append(allowedUsers, fmt.Sprintf("@manager:%s", domain))
		}
		if len(allowedUsers) > 0 {
			req.Env["MATRIX_ALLOWED_USERS"] = strings.Join(allowedUsers, ",")
		}
	}

	// Inject cluster ID so the worker CLI sends X-HiClaw-Cluster-ID header
	// for remote TokenReview routing.
	if req.DeployMode == v1beta1.DeployModeRemote && req.TargetClusterID != "" {
		req.Env["HICLAW_CLUSTER_ID"] = req.TargetClusterID
	}

	image := req.Image
	if image == "" {
		switch {
		case req.Runtime == RuntimeCopaw && k.config.CopawWorkerImage != "":
			image = k.config.CopawWorkerImage
		case req.Runtime == RuntimeHermes && k.config.HermesWorkerImage != "":
			image = k.config.HermesWorkerImage
		case req.Runtime == RuntimeOpenHuman && k.config.OpenHumanWorkerImage != "":
			image = k.config.OpenHumanWorkerImage
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
			req.WorkingDir = "/root/.copaw-worker"
		case req.Runtime == RuntimeOpenHuman:
			req.WorkingDir = "/home/openhuman/.openhuman"
		default:
			// Both openclaw and hermes use the same workspace layout:
			// HOME == WorkingDir == /root/hiclaw-fs/agents/<name> (== MinIO
			// mirror root). The hermes entrypoint anchors its install_dir to
			// the same location so workspace_dir == HOME and HERMES_HOME ==
			// $HOME/.hermes.
			if home := req.Env["HOME"]; home != "" {
				req.WorkingDir = home
			} else {
				req.WorkingDir = fmt.Sprintf("/root/hiclaw-fs/agents/%s", req.Name)
				req.Env["HOME"] = req.WorkingDir
			}
		}
	}

	defaultResources := buildDefaultResources(k.config.WorkerCPU, k.config.WorkerMemory)
	var resourcesOverride *corev1.ResourceRequirements
	if req.Resources != nil {
		merged, err := mergeResourceOverrides(defaultResources, req.Resources)
		if err != nil {
			return nil, fmt.Errorf("invalid resource override for %s: %w", req.Name, err)
		}
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

	saName := req.ServiceAccountName
	if saName == "" {
		saName = k.workerNamePrefix() + req.Name
	}

	// Callers own the full label set except agentteams.io/runtime, which the
	// backend stamps because it knows the resolved runtime value (after
	// CRD spec + operator-default fallback).
	podLabels := map[string]string{
		"agentteams.io/runtime": defaultRuntime(req.Runtime),
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
		HostAliases:        buildHostAliases(req.ExtraHosts),
	})

	// Skip controller OwnerReference for remote pods — cross-cluster ownerRef
	// is not possible. Local pods still get ownerRef for GC cascading.
	if req.Owner != nil && req.DeployMode != v1beta1.DeployModeRemote {
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
	targetClient, targetNS, err := k.resolveClient(ctx, k.deployMode, k.targetClusterID, k.targetNamespace)
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
	targetClient, targetNS, err := k.resolveClient(ctx, k.deployMode, k.targetClusterID, k.targetNamespace)
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
	targetClient, targetNS, err := k.resolveClient(ctx, k.deployMode, k.targetClusterID, k.targetNamespace)
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
	rawStatus := rawK8sPhase(pod.Status.Phase)
	var message string
	if containerStatus, containerMessage, containerRaw, ok := podContainerFailureStatus(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses); ok {
		status = containerStatus
		message = containerMessage
		rawStatus = containerRaw
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
// "hiclaw-worker-". Used only when a CreateRequest arrives without an
// explicit ServiceAccountName (production callers always set one).
func (k *K8sBackend) workerNamePrefix() string {
	if k.config.ResourcePrefix == "" {
		return "hiclaw-worker-"
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
func mergeResourceOverrides(defaults corev1.ResourceRequirements, override *ResourceRequirements) (corev1.ResourceRequirements, error) {
	out := *defaults.DeepCopy()
	if override == nil {
		return out, nil
	}
	if override.CPULimit != "" {
		q, err := resource.ParseQuantity(override.CPULimit)
		if err != nil {
			return out, fmt.Errorf("limits.cpu: %w", err)
		}
		out.Limits[corev1.ResourceCPU] = q
	}
	if override.MemoryLimit != "" {
		q, err := resource.ParseQuantity(override.MemoryLimit)
		if err != nil {
			return out, fmt.Errorf("limits.memory: %w", err)
		}
		out.Limits[corev1.ResourceMemory] = q
	}
	if override.CPURequest != "" {
		q, err := resource.ParseQuantity(override.CPURequest)
		if err != nil {
			return out, fmt.Errorf("requests.cpu: %w", err)
		}
		out.Requests[corev1.ResourceCPU] = q
	}
	if override.MemoryRequest != "" {
		q, err := resource.ParseQuantity(override.MemoryRequest)
		if err != nil {
			return out, fmt.Errorf("requests.memory: %w", err)
		}
		out.Requests[corev1.ResourceMemory] = q
	}
	return out, nil
}

// mergeOSSRegionFromProcessEnv sets HICLAW_FS_BUCKET and HICLAW_REGION when the client
// omitted them; the controller process should already have these from the same Secret as Manager (envFrom).
func mergeOSSRegionFromProcessEnv(env map[string]string) {
	if env == nil {
		return
	}
	bucket := firstNonEmptyTrimmed(
		env["HICLAW_FS_BUCKET"],
		os.Getenv("HICLAW_FS_BUCKET"),
	)
	if bucket != "" && strings.TrimSpace(env["HICLAW_FS_BUCKET"]) == "" {
		env["HICLAW_FS_BUCKET"] = bucket
	}
	if v := strings.TrimSpace(os.Getenv("HICLAW_REGION")); v != "" && strings.TrimSpace(env["HICLAW_REGION"]) == "" {
		env["HICLAW_REGION"] = v
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
	case RuntimeOpenHuman:
		return RuntimeOpenHuman
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
	if ns := strings.TrimSpace(os.Getenv("HICLAW_K8S_NAMESPACE")); ns != "" {
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
