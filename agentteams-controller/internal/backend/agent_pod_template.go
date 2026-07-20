package backend

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// AgentPodTemplateConfigMapKey is the data key inside the controller-scoped
// ConfigMap that carries the PodTemplateSpec YAML. The controller reads this
// and only this key; any other keys in the same ConfigMap are ignored.
const AgentPodTemplateConfigMapKey = "pod-template.yaml"

// AgentPodTemplateRemoteConfigMapKey is an optional data key inside the same
// ConfigMap. When the agent is being created in remote deploy mode
// (DeployMode=Remote) and this key is present and non-empty, the controller
// uses it instead of AgentPodTemplateConfigMapKey. This lets operators ship
// remote-cluster-specific scheduling fields (tolerations, nodeSelector,
// imagePullSecrets, etc.) without affecting in-cluster Pods. When the key is
// absent or empty in remote mode, the controller returns a zero-value
// PodTemplateSpec (no overlay) — it does NOT fall back to pod-template.yaml.
const AgentPodTemplateRemoteConfigMapKey = "pod-template-remote.yaml"

// PodOverlay carries every controller-computed field that ApplyPodTemplate
// must force onto the final Pod. Anything NOT in this struct is either copied
// verbatim from the PodTemplateSpec (the "user wins" side of the merge) or
// left at the zero value.
type PodOverlay struct {
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string

	ServiceAccountName string
	// Container is the agent-container base (Name="worker", Image, Env,
	// WorkingDir, ImagePullPolicy). Resources / VolumeMounts / SecurityContext
	// etc. are layered on top by ApplyPodTemplate itself.
	Container corev1.Container
	// ResourcesOverride, when non-nil, wins over template container.Resources
	// and DefaultResources. This is the per-CreateRequest resource override
	// path.
	ResourcesOverride *corev1.ResourceRequirements
	// DefaultResources is the backend-level fallback used only when neither
	// ResourcesOverride nor template-container.Resources provides a value.
	DefaultResources corev1.ResourceRequirements

	// TokenVolume + TokenVolumeMount are appended to Pod volumes and the
	// agent container's volumeMounts unless DisableTokenProjection is true.
	TokenVolume            corev1.Volume
	TokenVolumeMount       corev1.VolumeMount
	DisableTokenProjection bool

	// HostAliases from CreateRequest.ExtraHosts; appended to any host
	// aliases the template already declared.
	HostAliases []corev1.HostAlias

	// ExtraVolumes / ExtraVolumeMounts are controller-derived mounts such as
	// sandbox worker-deps token/env/data and custom OSS mounts. They are
	// appended after the token volume.
	ExtraVolumes      []corev1.Volume
	ExtraVolumeMounts []corev1.VolumeMount
}

// LoadAgentPodTemplate fetches the agent PodTemplateSpec overlay from the
// ConfigMap named `name` (typically the controller's own name, i.e. the
// AGENTTEAMS_CONTROLLER_NAME env var) in `namespace`. The key
// AgentPodTemplateConfigMapKey ("pod-template.yaml") is expected to carry
// a YAML document with the two top-level fields of corev1.PodTemplateSpec
// directly (metadata:, spec:) — NOT a full apiVersion/kind-wrapped
// PodTemplate object.
//
// This function is called on every Create(), so it is a live lookup with
// no caching — ConfigMap edits take effect on the very next Pod creation.
//
// Every failure mode (nil client, empty name, missing ConfigMap, API error,
// RBAC denial, missing key, parse failure) collapses to a zero-value
// PodTemplateSpec. A broken overlay must never block Pod creation. Failures
// are surfaced via controller-runtime's logger at varying levels:
//
//   - NotFound: V(1) debug — a common "no overlay configured" state.
//   - Parse failure: Error — the user's YAML is almost certainly wrong.
//   - Other API errors: Info — likely transient; next Create retries.
//
// When deployMode == v1beta1.DeployModeRemote, the loader only tries the
// AgentPodTemplateRemoteConfigMapKey ("pod-template-remote.yaml") within the
// same ConfigMap. If that key is missing or empty, it returns a zero-value
// PodTemplateSpec (no overlay) without falling back to pod-template.yaml.
// Any other deployMode value (including the empty string) only consults the
// standard key, preserving the original behaviour.
func LoadAgentPodTemplate(ctx context.Context, client K8sCoreClient, namespace, name, deployMode string) corev1.PodTemplateSpec {
	logger := log.FromContext(ctx).WithName("agent-pod-template")
	if client == nil || namespace == "" || name == "" {
		return corev1.PodTemplateSpec{}
	}
	cm, err := client.ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("agent pod template ConfigMap not found; using empty overlay",
				"namespace", namespace, "name", name)
			return corev1.PodTemplateSpec{}
		}
		logger.Info("agent pod template ConfigMap fetch failed; using empty overlay",
			"namespace", namespace, "name", name, "err", err.Error())
		return corev1.PodTemplateSpec{}
	}

	// Resolve which key to consume. Remote deploy mode uses only the remote
	// key; if absent or empty, no overlay is applied (no fallback to local).
	var raw string
	var usedKey string
	if deployMode == v1beta1.DeployModeRemote {
		v, ok := cm.Data[AgentPodTemplateRemoteConfigMapKey]
		if !ok || v == "" {
			logger.V(1).Info("remote pod template key not found or empty; using empty overlay",
				"namespace", namespace, "name", name, "key", AgentPodTemplateRemoteConfigMapKey)
			return corev1.PodTemplateSpec{}
		}
		raw = v
		usedKey = AgentPodTemplateRemoteConfigMapKey
	} else {
		v, ok := cm.Data[AgentPodTemplateConfigMapKey]
		if !ok || v == "" {
			return corev1.PodTemplateSpec{}
		}
		raw = v
		usedKey = AgentPodTemplateConfigMapKey
	}

	var tmpl corev1.PodTemplateSpec
	if err := yaml.Unmarshal([]byte(raw), &tmpl); err != nil {
		logger.Error(err, "agent pod template YAML parse failed; using empty overlay",
			"namespace", namespace, "name", name, "key", usedKey)
		return corev1.PodTemplateSpec{}
	}
	return tmpl
}

// ApplyPodTemplate overlays controller-owned runtime fields from overlay onto
// a deep copy of tmpl, producing a ready-to-submit *corev1.Pod. This function
// is pure (no I/O, no K8s API calls) for ease of testing.
//
// Merge rules (see design doc section 1.2):
//
//   - metadata.Labels: template first, overlay labels overwrite on key collision.
//   - metadata.Annotations: template first, overlay annotations overwrite on key collision.
//   - metadata.OwnerReferences: template's ownerRefs are discarded (the
//     backend stamps its own controller OwnerReference on the returned Pod
//     via controllerutil.SetControllerReference in K8sBackend.Create).
//   - spec.Containers: template containers NOT named "worker" are preserved
//     as sidecars. If template has a container named "worker", its fields
//     serve as a base that overlay.Container's Name/Image/Env/WorkingDir/
//     ImagePullPolicy overwrite (empty overlay fields fall through to template).
//     overlay.TokenVolumeMount is always appended to the agent container's
//     volumeMounts. Resources: overlay.ResourcesOverride wins, else template
//     container.Resources if non-empty, else overlay.DefaultResources.
//   - spec.Volumes: template volumes + overlay.TokenVolume (appended).
//   - spec.ServiceAccountName: overlay wins.
//   - spec.AutomountServiceAccountToken: forced to false.
//   - spec.RestartPolicy: template if set, otherwise "Always".
//   - spec.HostAliases: template first, overlay.HostAliases appended.
//   - everything else in spec: template wins verbatim (nodeSelector,
//     tolerations, affinity, imagePullSecrets, securityContext, topology
//     spread constraints, runtimeClassName, schedulerName, priorityClassName,
//     dnsPolicy, dnsConfig, etc.).
func ApplyPodTemplate(tmpl corev1.PodTemplateSpec, overlay PodOverlay) *corev1.Pod {
	tmplCopy := tmpl.DeepCopy()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        overlay.Name,
			Namespace:   overlay.Namespace,
			Labels:      mergeStringMaps(tmplCopy.ObjectMeta.Labels, overlay.Labels),
			Annotations: mergeStringMaps(tmplCopy.ObjectMeta.Annotations, overlay.Annotations),
		},
		Spec: tmplCopy.Spec,
	}

	agentContainer, sidecars := splitAgentContainer(pod.Spec.Containers, overlay.Container.Name)
	agentContainer = overlayAgentContainer(agentContainer, overlay)
	pod.Spec.Containers = append([]corev1.Container{agentContainer}, sidecars...)

	if !overlay.DisableTokenProjection {
		pod.Spec.Volumes = append(pod.Spec.Volumes, overlay.TokenVolume)
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, overlay.ExtraVolumes...)

	pod.Spec.ServiceAccountName = overlay.ServiceAccountName
	pod.Spec.AutomountServiceAccountToken = boolPtr(false)

	if pod.Spec.RestartPolicy == "" {
		pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	}

	if len(overlay.HostAliases) > 0 {
		pod.Spec.HostAliases = append(pod.Spec.HostAliases, overlay.HostAliases...)
	}

	return pod
}

// splitAgentContainer locates the agent container (by name) within tmpl and
// returns (base, sidecars). When no match exists, returns (zero, tmpl).
func splitAgentContainer(containers []corev1.Container, agentName string) (corev1.Container, []corev1.Container) {
	if agentName == "" {
		agentName = "worker"
	}
	sidecars := make([]corev1.Container, 0, len(containers))
	var base corev1.Container
	found := false
	for _, c := range containers {
		if !found && c.Name == agentName {
			base = c
			found = true
			continue
		}
		sidecars = append(sidecars, c)
	}
	return base, sidecars
}

// overlayAgentContainer merges overlay runtime fields onto base (which may be
// the zero Container when template defined no agent container) and returns
// the final agent container. Resources are resolved per the documented
// precedence (overlay override > template > backend default).
func overlayAgentContainer(base corev1.Container, overlay PodOverlay) corev1.Container {
	out := base
	if out.Name == "" {
		out.Name = overlay.Container.Name
	}
	if overlay.Container.Image != "" {
		out.Image = overlay.Container.Image
	}
	if overlay.Container.ImagePullPolicy != "" {
		out.ImagePullPolicy = overlay.Container.ImagePullPolicy
	} else if out.ImagePullPolicy == "" {
		out.ImagePullPolicy = corev1.PullIfNotPresent
	}
	if len(overlay.Container.Env) > 0 {
		out.Env = overlay.Container.Env
	}
	if overlay.Container.WorkingDir != "" {
		out.WorkingDir = overlay.Container.WorkingDir
	}
	if !overlay.DisableTokenProjection {
		out.VolumeMounts = append(out.VolumeMounts, overlay.TokenVolumeMount)
	}
	out.VolumeMounts = append(out.VolumeMounts, overlay.ExtraVolumeMounts...)

	switch {
	case overlay.ResourcesOverride != nil:
		out.Resources = *overlay.ResourcesOverride
	case isResourcesEmpty(out.Resources):
		out.Resources = overlay.DefaultResources
	}

	return out
}

func isResourcesEmpty(r corev1.ResourceRequirements) bool {
	return len(r.Limits) == 0 && len(r.Requests) == 0 && len(r.Claims) == 0
}

// mergeStringMaps returns base + overrides with overrides winning on
// key collision. A new map is always returned; inputs are not mutated.
func mergeStringMaps(base, overrides map[string]string) map[string]string {
	if len(base) == 0 && len(overrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// ExtractSchedulingFields extracts scheduling-related fields from a
// PodTemplateSpec.
// Returns zero values when the template does not specify these fields.
func ExtractSchedulingFields(tmpl corev1.PodTemplateSpec) (
	nodeSelector map[string]string,
	tolerations []corev1.Toleration,
	affinity *corev1.Affinity,
) {
	nodeSelector = tmpl.Spec.NodeSelector
	tolerations = tmpl.Spec.Tolerations
	affinity = tmpl.Spec.Affinity
	return
}

// ExtractVolumes extracts volumes and the "worker" container's volumeMounts
// from a PodTemplateSpec.
// If no "worker" container exists, volumeMounts is nil.
func ExtractVolumes(tmpl corev1.PodTemplateSpec) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := tmpl.Spec.Volumes
	var volumeMounts []corev1.VolumeMount
	for _, c := range tmpl.Spec.Containers {
		if c.Name == "worker" {
			volumeMounts = c.VolumeMounts
			break
		}
	}
	return volumes, volumeMounts
}

// ExtractEnv extracts environment variables from the "worker" container in a
// PodTemplateSpec.
// Returns nil if no "worker" container exists or it has no env vars.
func ExtractEnv(tmpl corev1.PodTemplateSpec) []corev1.EnvVar {
	for _, c := range tmpl.Spec.Containers {
		if c.Name == "worker" {
			return c.Env
		}
	}
	return nil
}
