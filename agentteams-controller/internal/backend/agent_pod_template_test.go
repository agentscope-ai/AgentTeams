package backend

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
)

// ── fixtures ─────────────────────────────────────────────────────────────

func baseOverlay() PodOverlay {
	return PodOverlay{
		Name:               "agentteams-worker-alice",
		Namespace:          "hiclaw",
		Labels:             map[string]string{"app": "agentteams-worker", "agentteams.io/worker": "alice"},
		Annotations:        map[string]string{"agentteams.io/test-overlay": "controller"},
		ServiceAccountName: "agentteams-worker-alice",
		Container: corev1.Container{
			Name:            "worker",
			Image:           "hiclaw/worker:latest",
			ImagePullPolicy: corev1.PullIfNotPresent,
			Env:             []corev1.EnvVar{{Name: "AGENTTEAMS_RUNTIME", Value: "k8s"}},
			WorkingDir:      "/root/agentteams-fs/agents/alice",
		},
		DefaultResources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1000m"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		TokenVolume: corev1.Volume{
			Name: "agentteams-token",
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Audience: "agentteams-controller",
						Path:     "token",
					},
				}}},
			},
		},
		TokenVolumeMount: corev1.VolumeMount{
			Name:      "agentteams-token",
			MountPath: "/var/run/secrets/agentteams",
			ReadOnly:  true,
		},
	}
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

func findVolumeMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

// ── ApplyPodTemplate tests ───────────────────────────────────────────────

func TestApplyPodTemplate_EmptyTemplate(t *testing.T) {
	pod := ApplyPodTemplate(corev1.PodTemplateSpec{}, baseOverlay())
	if pod.Name != "agentteams-worker-alice" || pod.Namespace != "hiclaw" {
		t.Fatalf("name/ns: %+v", pod.ObjectMeta)
	}
	if pod.Spec.ServiceAccountName != "agentteams-worker-alice" {
		t.Fatalf("SA: %q", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("AutomountServiceAccountToken must be false, got %v", pod.Spec.AutomountServiceAccountToken)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Fatalf("restartPolicy default: %q", pod.Spec.RestartPolicy)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "worker" {
		t.Fatalf("containers: %+v", pod.Spec.Containers)
	}
	if pod.Spec.Containers[0].Image != "hiclaw/worker:latest" {
		t.Fatalf("image: %q", pod.Spec.Containers[0].Image)
	}
	if v := findVolume(pod.Spec.Volumes, "agentteams-token"); v == nil {
		t.Fatalf("token volume missing: %+v", pod.Spec.Volumes)
	}
	if m := findVolumeMount(pod.Spec.Containers[0].VolumeMounts, "agentteams-token"); m == nil {
		t.Fatalf("token volume mount missing: %+v", pod.Spec.Containers[0].VolumeMounts)
	}
}

func TestApplyPodTemplate_MetadataLabelsMerge(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"a": "x", "app": "ignored-by-overlay"},
		},
	}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if pod.Labels["a"] != "x" {
		t.Fatalf("template-only label missing: %+v", pod.Labels)
	}
	if pod.Labels["app"] != "agentteams-worker" {
		t.Fatalf("overlay must win on app: %q", pod.Labels["app"])
	}
	if pod.Labels["agentteams.io/worker"] != "alice" {
		t.Fatalf("overlay-only label missing: %+v", pod.Labels)
	}
}

func TestApplyPodTemplate_MetadataAnnotationsMerge(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"foo": "bar",
				// overlay should overwrite this key
				"agentteams.io/test-overlay": "should-be-overwritten",
			},
		},
	}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if pod.Annotations["foo"] != "bar" {
		t.Fatalf("template annotation dropped: %+v", pod.Annotations)
	}
	if pod.Annotations["agentteams.io/test-overlay"] != "controller" {
		t.Fatalf("overlay must win on annotation: %q", pod.Annotations["agentteams.io/test-overlay"])
	}
}

func TestApplyPodTemplate_NodeSelectorFromTemplate(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		NodeSelector: map[string]string{"type": "virtual-kubelet"},
	}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if !reflect.DeepEqual(pod.Spec.NodeSelector, map[string]string{"type": "virtual-kubelet"}) {
		t.Fatalf("nodeSelector: %+v", pod.Spec.NodeSelector)
	}
}

func TestApplyPodTemplate_TolerationsFromTemplate(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Tolerations: []corev1.Toleration{
			{Key: "virtual-kubelet.io/provider", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			{Key: "virtual-kubelet.io/compute-type", Value: "acs", Effect: corev1.TaintEffectNoSchedule},
		},
	}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if len(pod.Spec.Tolerations) != 2 {
		t.Fatalf("tolerations: %+v", pod.Spec.Tolerations)
	}
}

func TestApplyPodTemplate_ImagePullSecretsFromTemplate(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regsecret"}},
	}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "regsecret" {
		t.Fatalf("imagePullSecrets: %+v", pod.Spec.ImagePullSecrets)
	}
}

func TestApplyPodTemplate_SysctlsFromTemplate(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{Sysctls: []corev1.Sysctl{
			{Name: "net.ipv4.fib_multipath_hash_policy", Value: "1"},
		}},
	}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if pod.Spec.SecurityContext == nil || len(pod.Spec.SecurityContext.Sysctls) != 1 {
		t.Fatalf("sysctls: %+v", pod.Spec.SecurityContext)
	}
	if pod.Spec.SecurityContext.Sysctls[0].Name != "net.ipv4.fib_multipath_hash_policy" {
		t.Fatalf("sysctl name: %+v", pod.Spec.SecurityContext.Sysctls[0])
	}
}

func TestApplyPodTemplate_AffinityFromTemplate(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: "k", Operator: corev1.NodeSelectorOpExists,
				}}}},
			},
		}},
	}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		t.Fatalf("affinity: %+v", pod.Spec.Affinity)
	}
}

// TestApplyPodTemplate_AliyunAnnotationsFromTemplate is the anchor test proving
// this refactor's thesis: the backend ITSELF has zero cloud-vendor knowledge,
// yet a template containing Alibaba Cloud annotations/labels/sysctls flows
// through unchanged onto the final Pod.
func TestApplyPodTemplate_AliyunAnnotationsFromTemplate(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"network.alibabacloud.com/security-group-ids": "sg-bp1xxx",
				"kubeone.ali/appinstance-name":                "magic-ctl",
			},
			Labels: map[string]string{
				"nsm.alibabacloud.com/inject-sidecar": "ansm-magic-xxx",
			},
		},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{Sysctls: []corev1.Sysctl{
				{Name: "net.ipv4.fib_multipath_hash_policy", Value: "1"},
			}},
		},
	}
	pod := ApplyPodTemplate(tmpl, baseOverlay())

	if pod.Annotations["network.alibabacloud.com/security-group-ids"] != "sg-bp1xxx" {
		t.Fatalf("SG annotation missing: %+v", pod.Annotations)
	}
	if pod.Annotations["kubeone.ali/appinstance-name"] != "magic-ctl" {
		t.Fatalf("appinstance annotation missing: %+v", pod.Annotations)
	}
	if pod.Labels["nsm.alibabacloud.com/inject-sidecar"] != "ansm-magic-xxx" {
		t.Fatalf("ANSM label missing: %+v", pod.Labels)
	}
	if pod.Spec.SecurityContext == nil || len(pod.Spec.SecurityContext.Sysctls) != 1 {
		t.Fatalf("sysctls: %+v", pod.Spec.SecurityContext)
	}
}

func TestApplyPodTemplate_WorkerContainerResourcesFromTemplate(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "worker",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		}},
	}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	got := pod.Spec.Containers[0].Resources.Limits.Cpu().String()
	if got != "4" {
		t.Fatalf("expected cpu=4 from template, got %q", got)
	}
}

func TestApplyPodTemplate_WorkerContainerResourcesCodeOverride(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "worker",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
			},
		}},
	}}
	ov := baseOverlay()
	override := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")},
	}
	ov.ResourcesOverride = &override
	pod := ApplyPodTemplate(tmpl, ov)
	got := pod.Spec.Containers[0].Resources.Limits.Cpu().String()
	if got != "8" {
		t.Fatalf("expected cpu=8 from override, got %q", got)
	}
}

func TestApplyPodTemplate_WorkerContainerResourcesFallback(t *testing.T) {
	pod := ApplyPodTemplate(corev1.PodTemplateSpec{}, baseOverlay())
	got := pod.Spec.Containers[0].Resources.Limits.Cpu().String()
	if got != "1" {
		t.Fatalf("expected default cpu=1 (1000m), got %q", got)
	}
	gotMem := pod.Spec.Containers[0].Resources.Requests.Memory().String()
	if gotMem != "256Mi" {
		t.Fatalf("expected default mem request=256Mi, got %q", gotMem)
	}
}

func TestApplyPodTemplate_TokenVolumeAppended(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{{
			Name:         "cache",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
	}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if len(pod.Spec.Volumes) != 2 {
		t.Fatalf("expected 2 volumes (template + token), got %+v", pod.Spec.Volumes)
	}
	if findVolume(pod.Spec.Volumes, "cache") == nil {
		t.Fatalf("template volume dropped")
	}
	if findVolume(pod.Spec.Volumes, "agentteams-token") == nil {
		t.Fatalf("token volume not appended")
	}
}

func TestApplyPodTemplate_SidecarContainersPreserved(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "worker", Image: "should-be-overwritten"},
			{Name: "logagent", Image: "logagent:1.0"},
		},
	}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("expected 2 containers, got %+v", pod.Spec.Containers)
	}
	if pod.Spec.Containers[0].Name != "worker" {
		t.Fatalf("agent container must be first, got %q", pod.Spec.Containers[0].Name)
	}
	if pod.Spec.Containers[0].Image != "hiclaw/worker:latest" {
		t.Fatalf("worker image must come from overlay, got %q", pod.Spec.Containers[0].Image)
	}
	log := findContainer(pod.Spec.Containers, "logagent")
	if log == nil || log.Image != "logagent:1.0" {
		t.Fatalf("logagent sidecar missing or mutated: %+v", log)
	}
}

func TestApplyPodTemplate_RestartPolicyDefault(t *testing.T) {
	pod := ApplyPodTemplate(corev1.PodTemplateSpec{}, baseOverlay())
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Fatalf("expected Always, got %q", pod.Spec.RestartPolicy)
	}
}

func TestApplyPodTemplate_RestartPolicyRespectTemplate(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyOnFailure}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if pod.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Fatalf("template restartPolicy should win, got %q", pod.Spec.RestartPolicy)
	}
}

func TestApplyPodTemplate_HostAliasesAppended(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		HostAliases: []corev1.HostAlias{{IP: "1.1.1.1", Hostnames: []string{"a"}}},
	}}
	ov := baseOverlay()
	ov.HostAliases = []corev1.HostAlias{{IP: "2.2.2.2", Hostnames: []string{"b"}}}
	pod := ApplyPodTemplate(tmpl, ov)
	if len(pod.Spec.HostAliases) != 2 {
		t.Fatalf("hostAliases: %+v", pod.Spec.HostAliases)
	}
}

func TestApplyPodTemplate_ServiceAccountNameCodeWins(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "foo"}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if pod.Spec.ServiceAccountName != "agentteams-worker-alice" {
		t.Fatalf("SA: %q", pod.Spec.ServiceAccountName)
	}
}

func TestApplyPodTemplate_AutomountDisabled(t *testing.T) {
	truePtr := true
	tmpl := corev1.PodTemplateSpec{Spec: corev1.PodSpec{AutomountServiceAccountToken: &truePtr}}
	pod := ApplyPodTemplate(tmpl, baseOverlay())
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("expected automount=false, got %v", pod.Spec.AutomountServiceAccountToken)
	}
}

// ── LoadAgentPodTemplate tests ───────────────────────────────────────────

const loaderTestNS = "hiclaw"
const loaderTestName = "hiclaw-ctl"

// injectLoaderCM is a test-local helper; the integration-style helper in
// kubernetes_test.go uses fixed names that don't match the parameterized
// namespace/name we want to exercise here.
func injectLoaderCM(fake *fakeK8sCoreClient, namespace, name, yaml string) {
	fake.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{AgentPodTemplateConfigMapKey: yaml},
	})
}

func TestLoadAgentPodTemplate_NilClient(t *testing.T) {
	got := LoadAgentPodTemplate(context.Background(), nil, loaderTestNS, loaderTestName, "")
	if !reflect.DeepEqual(got, corev1.PodTemplateSpec{}) {
		t.Fatalf("nil client: expected zero PodTemplateSpec, got %+v", got)
	}
}

func TestLoadAgentPodTemplate_EmptyNameOrNamespace(t *testing.T) {
	fake := newFakeK8sCoreClient()
	injectLoaderCM(fake, loaderTestNS, loaderTestName, `metadata: {labels: {x: "y"}}`)

	// Empty name → no lookup, empty template.
	if got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, "", ""); !reflect.DeepEqual(got, corev1.PodTemplateSpec{}) {
		t.Fatalf("empty name: expected zero, got %+v", got)
	}
	// Empty namespace → no lookup, empty template.
	if got := LoadAgentPodTemplate(context.Background(), fake, "", loaderTestName, ""); !reflect.DeepEqual(got, corev1.PodTemplateSpec{}) {
		t.Fatalf("empty namespace: expected zero, got %+v", got)
	}
}

func TestLoadAgentPodTemplate_ConfigMapNotFound(t *testing.T) {
	fake := newFakeK8sCoreClient()
	got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, "")
	if !reflect.DeepEqual(got, corev1.PodTemplateSpec{}) {
		t.Fatalf("NotFound: expected zero PodTemplateSpec, got %+v", got)
	}
}

func TestLoadAgentPodTemplate_ParseOK(t *testing.T) {
	fake := newFakeK8sCoreClient()
	injectLoaderCM(fake, loaderTestNS, loaderTestName, `metadata:
  labels:
    a: x
spec:
  nodeSelector:
    type: virtual-kubelet
  tolerations:
    - key: k
      operator: Exists
      effect: NoSchedule
`)
	got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, "")
	if got.Labels["a"] != "x" {
		t.Fatalf("labels: %+v", got.Labels)
	}
	if got.Spec.NodeSelector["type"] != "virtual-kubelet" {
		t.Fatalf("nodeSelector: %+v", got.Spec.NodeSelector)
	}
	if len(got.Spec.Tolerations) != 1 {
		t.Fatalf("tolerations: %+v", got.Spec.Tolerations)
	}
}

func TestLoadAgentPodTemplate_MissingOrEmptyDataKey(t *testing.T) {
	fake := newFakeK8sCoreClient()
	// ConfigMap exists but lacks the canonical key.
	fake.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: loaderTestName, Namespace: loaderTestNS},
		Data:       map[string]string{"something-else": "hello"},
	})
	if got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, ""); !reflect.DeepEqual(got, corev1.PodTemplateSpec{}) {
		t.Fatalf("missing key: expected zero, got %+v", got)
	}

	// ConfigMap has the key but the value is empty string.
	fake2 := newFakeK8sCoreClient()
	injectLoaderCM(fake2, loaderTestNS, loaderTestName, "")
	if got := LoadAgentPodTemplate(context.Background(), fake2, loaderTestNS, loaderTestName, ""); !reflect.DeepEqual(got, corev1.PodTemplateSpec{}) {
		t.Fatalf("empty value: expected zero, got %+v", got)
	}
}

func TestLoadAgentPodTemplate_ParseFailure(t *testing.T) {
	fake := newFakeK8sCoreClient()
	// Invalid YAML that fails strict PodTemplateSpec unmarshal.
	injectLoaderCM(fake, loaderTestNS, loaderTestName, "not-a-podtemplate-at-all: : : :")
	got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, "")
	if !reflect.DeepEqual(got, corev1.PodTemplateSpec{}) {
		t.Fatalf("parse failure must produce zero PodTemplateSpec, got %+v", got)
	}
}

// TestLoadAgentPodTemplate_LiveRead validates the "read on every Create"
// contract: editing the ConfigMap between calls must immediately surface,
// with no caching.
func TestLoadAgentPodTemplate_LiveRead(t *testing.T) {
	fake := newFakeK8sCoreClient()
	injectLoaderCM(fake, loaderTestNS, loaderTestName, `metadata: {labels: {v: "1"}}`)
	v1 := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, "")
	if v1.Labels["v"] != "1" {
		t.Fatalf("v1 labels: %+v", v1.Labels)
	}

	injectLoaderCM(fake, loaderTestNS, loaderTestName, `metadata: {labels: {v: "2"}}`)
	v2 := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, "")
	if v2.Labels["v"] != "2" {
		t.Fatalf("v2 labels (live-read broken): %+v", v2.Labels)
	}
}

// TestLoadAgentPodTemplate_GetError ensures non-NotFound API errors (e.g.
// transient unavailability, RBAC denial) degrade to empty overlay without
// blocking the caller.
func TestLoadAgentPodTemplate_GetError(t *testing.T) {
	fake := newFakeK8sCoreClient()
	fake.cmGetErr = errors.New("boom: API server unavailable")
	got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, "")
	if !reflect.DeepEqual(got, corev1.PodTemplateSpec{}) {
		t.Fatalf("API error must produce zero PodTemplateSpec, got %+v", got)
	}
}

// TestLoadAgentPodTemplate_RemoteKeyPreferred validates that when deployMode
// is Remote and pod-template-remote.yaml is present, that key wins over
// pod-template.yaml.
func TestLoadAgentPodTemplate_RemoteKeyPreferred(t *testing.T) {
	fake := newFakeK8sCoreClient()
	fake.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: loaderTestName, Namespace: loaderTestNS},
		Data: map[string]string{
			AgentPodTemplateConfigMapKey:       `metadata: {labels: {pick: "local"}}`,
			AgentPodTemplateRemoteConfigMapKey: `metadata: {labels: {pick: "remote"}}`,
		},
	})
	got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, v1beta1.DeployModeRemote)
	if got.Labels["pick"] != "remote" {
		t.Fatalf("remote mode must consume remote key, got %+v", got.Labels)
	}
}

// TestLoadAgentPodTemplate_RemoteKeyMissing_ReturnsEmpty validates that when
// deployMode is Remote but pod-template-remote.yaml is missing or empty, the
// loader returns a zero PodTemplateSpec (no fallback to pod-template.yaml).
func TestLoadAgentPodTemplate_RemoteKeyMissing_ReturnsEmpty(t *testing.T) {
	// Remote key absent.
	fake := newFakeK8sCoreClient()
	fake.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: loaderTestName, Namespace: loaderTestNS},
		Data: map[string]string{
			AgentPodTemplateConfigMapKey: `metadata: {labels: {pick: "local"}}`,
		},
	})
	got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, v1beta1.DeployModeRemote)
	if !reflect.DeepEqual(got, corev1.PodTemplateSpec{}) {
		t.Fatalf("missing remote key must return zero PodTemplateSpec, got %+v", got)
	}

	// Remote key present but empty.
	fake2 := newFakeK8sCoreClient()
	fake2.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: loaderTestName, Namespace: loaderTestNS},
		Data: map[string]string{
			AgentPodTemplateConfigMapKey:       `metadata: {labels: {pick: "local"}}`,
			AgentPodTemplateRemoteConfigMapKey: "",
		},
	})
	got2 := LoadAgentPodTemplate(context.Background(), fake2, loaderTestNS, loaderTestName, v1beta1.DeployModeRemote)
	if !reflect.DeepEqual(got2, corev1.PodTemplateSpec{}) {
		t.Fatalf("empty remote key must return zero PodTemplateSpec, got %+v", got2)
	}
}

// TestLoadAgentPodTemplate_NonRemoteIgnoresRemoteKey validates that any
// non-Remote deployMode (including the empty string used by SandboxBackend
// and the explicit Local mode) ignores pod-template-remote.yaml even when
// it is present.
func TestLoadAgentPodTemplate_NonRemoteIgnoresRemoteKey(t *testing.T) {
	fake := newFakeK8sCoreClient()
	fake.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: loaderTestName, Namespace: loaderTestNS},
		Data: map[string]string{
			AgentPodTemplateConfigMapKey:       `metadata: {labels: {pick: "local"}}`,
			AgentPodTemplateRemoteConfigMapKey: `metadata: {labels: {pick: "remote"}}`,
		},
	})
	for _, mode := range []string{"", v1beta1.DeployModeLocal} {
		got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, mode)
		if got.Labels["pick"] != "local" {
			t.Fatalf("deployMode=%q must ignore remote key, got %+v", mode, got.Labels)
		}
	}
}

// TestLoadAgentPodTemplate_RemoteKeyOnly validates the case where the
// ConfigMap only carries the remote key (no pod-template.yaml) and the
// caller is in Remote mode.
func TestLoadAgentPodTemplate_RemoteKeyOnly(t *testing.T) {
	fake := newFakeK8sCoreClient()
	fake.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: loaderTestName, Namespace: loaderTestNS},
		Data: map[string]string{
			AgentPodTemplateRemoteConfigMapKey: `metadata: {labels: {pick: "remote"}}`,
		},
	})
	got := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, v1beta1.DeployModeRemote)
	if got.Labels["pick"] != "remote" {
		t.Fatalf("remote-only ConfigMap must surface remote key, got %+v", got.Labels)
	}

	// Same ConfigMap, but caller is non-remote: no usable key, expect zero.
	got2 := LoadAgentPodTemplate(context.Background(), fake, loaderTestNS, loaderTestName, "")
	if !reflect.DeepEqual(got2, corev1.PodTemplateSpec{}) {
		t.Fatalf("non-remote with only remote key must produce zero PodTemplateSpec, got %+v", got2)
	}
}

// ── ExtractEnv tests ─────────────────────────────────────────────────────

func TestExtractEnv_EmptyTemplate(t *testing.T) {
	got := ExtractEnv(corev1.PodTemplateSpec{})
	if got != nil {
		t.Fatalf("empty template must return nil, got %v", got)
	}
}

func TestExtractEnv_WorkerContainerWithEnv(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "sidecar",
					Env:  []corev1.EnvVar{{Name: "SIDECAR_VAR", Value: "ignored"}},
				},
				{
					Name: "worker",
					Env: []corev1.EnvVar{
						{Name: "FOO", Value: "bar"},
						{Name: "BAZ", Value: "qux"},
					},
				},
			},
		},
	}
	got := ExtractEnv(tmpl)
	if len(got) != 2 {
		t.Fatalf("expected 2 env vars, got %d", len(got))
	}
	if got[0].Name != "FOO" || got[0].Value != "bar" {
		t.Errorf("env[0] = %v, want FOO=bar", got[0])
	}
	if got[1].Name != "BAZ" || got[1].Value != "qux" {
		t.Errorf("env[1] = %v, want BAZ=qux", got[1])
	}
}

func TestExtractEnv_NoWorkerContainer(t *testing.T) {
	tmpl := corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "other", Env: []corev1.EnvVar{{Name: "X", Value: "y"}}},
			},
		},
	}
	got := ExtractEnv(tmpl)
	if got != nil {
		t.Fatalf("no worker container must return nil, got %v", got)
	}
}
