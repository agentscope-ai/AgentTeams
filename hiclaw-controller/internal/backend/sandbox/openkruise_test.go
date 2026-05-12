package sandbox

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildSandboxSpec_BasicFields(t *testing.T) {
	p := &OpenKruisePlugin{}
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:       "worker",
					Image:      "hiclaw/worker:latest",
					WorkingDir: "/root/workspace",
					Env: []corev1.EnvVar{
						{Name: "FOO", Value: "bar"},
						{Name: "BAZ", Value: "qux"},
					},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				}},
				ServiceAccountName: "worker-sa",
				NodeSelector:       map[string]string{"node-type": "gpu"},
			},
		},
	}

	result := p.buildSandboxSpec(spec)

	// Verify nested template structure.
	tmpl, ok := result["template"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec must contain 'template' map, got %T", result["template"])
	}
	podSpec, ok := tmpl["spec"].(map[string]interface{})
	if !ok {
		t.Fatalf("template must contain 'spec' map, got %T", tmpl["spec"])
	}

	// Verify containers.
	containers, ok := podSpec["containers"].([]interface{})
	if !ok || len(containers) != 1 {
		t.Fatalf("podSpec.containers must have 1 entry, got %v", podSpec["containers"])
	}
	container, ok := containers[0].(map[string]interface{})
	if !ok {
		t.Fatalf("container must be map, got %T", containers[0])
	}
	if container["name"] != "worker" {
		t.Errorf("container name = %v, want 'worker'", container["name"])
	}
	if container["image"] != "hiclaw/worker:latest" {
		t.Errorf("container image = %v, want 'hiclaw/worker:latest'", container["image"])
	}
	if container["workingDir"] != "/root/workspace" {
		t.Errorf("container workingDir = %v, want '/root/workspace'", container["workingDir"])
	}

	// Verify env.
	envList, ok := container["env"].([]interface{})
	if !ok || len(envList) != 2 {
		t.Fatalf("container.env must have 2 entries, got %v", container["env"])
	}

	// Verify resources.
	res, ok := container["resources"].(map[string]interface{})
	if !ok {
		t.Fatalf("container.resources must be map, got %T", container["resources"])
	}
	limits := res["limits"].(map[string]interface{})
	if limits["cpu"] != "2" {
		t.Errorf("resources.limits.cpu = %v, want '2'", limits["cpu"])
	}

	// Verify pod-level fields.
	if podSpec["serviceAccountName"] != "worker-sa" {
		t.Errorf("podSpec.serviceAccountName = %v, want 'worker-sa'", podSpec["serviceAccountName"])
	}
	ns, ok := podSpec["nodeSelector"].(map[string]interface{})
	if !ok || ns["node-type"] != "gpu" {
		t.Errorf("podSpec.nodeSelector = %v, want {node-type: gpu}", podSpec["nodeSelector"])
	}
}

func TestBuildSandboxSpec_Tolerations(t *testing.T) {
	p := &OpenKruisePlugin{}
	tolerationSeconds := int64(300)
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: "test:latest"}},
				Tolerations: []corev1.Toleration{
					{
						Key:               "gpu",
						Operator:          corev1.TolerationOpEqual,
						Value:             "true",
						Effect:            corev1.TaintEffectNoSchedule,
						TolerationSeconds: &tolerationSeconds,
					},
					{
						Operator: corev1.TolerationOpExists,
					},
				},
			},
		},
	}

	result := p.buildSandboxSpec(spec)
	podSpec := result["template"].(map[string]interface{})["spec"].(map[string]interface{})

	tolRaw, ok := podSpec["tolerations"]
	if !ok {
		t.Fatal("podSpec must contain 'tolerations'")
	}
	tolJSON, _ := json.Marshal(tolRaw)
	var tolerations []corev1.Toleration
	if err := json.Unmarshal(tolJSON, &tolerations); err != nil {
		t.Fatalf("failed to unmarshal tolerations: %v", err)
	}
	if len(tolerations) != 2 {
		t.Fatalf("expected 2 tolerations, got %d", len(tolerations))
	}
	if tolerations[0].Key != "gpu" || tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("toleration[0] = %+v, want key=gpu effect=NoSchedule", tolerations[0])
	}
	if tolerations[0].TolerationSeconds == nil || *tolerations[0].TolerationSeconds != 300 {
		t.Errorf("toleration[0].TolerationSeconds = %v, want 300", tolerations[0].TolerationSeconds)
	}
}

func TestBuildSandboxSpec_Affinity(t *testing.T) {
	p := &OpenKruisePlugin{}
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: "test:latest"}},
				Affinity: &corev1.Affinity{
					NodeAffinity: &corev1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{{
								MatchExpressions: []corev1.NodeSelectorRequirement{{
									Key:      "topology.kubernetes.io/zone",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{"us-east-1a"},
								}},
							}},
						},
					},
				},
			},
		},
	}

	result := p.buildSandboxSpec(spec)
	podSpec := result["template"].(map[string]interface{})["spec"].(map[string]interface{})

	affRaw, ok := podSpec["affinity"]
	if !ok {
		t.Fatal("podSpec must contain 'affinity'")
	}
	affJSON, _ := json.Marshal(affRaw)
	var affinity corev1.Affinity
	if err := json.Unmarshal(affJSON, &affinity); err != nil {
		t.Fatalf("failed to unmarshal affinity: %v", err)
	}
	if affinity.NodeAffinity == nil ||
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatal("nodeAffinity.required must be set")
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 {
		t.Fatalf("unexpected node selector terms: %+v", terms)
	}
	if terms[0].MatchExpressions[0].Key != "topology.kubernetes.io/zone" {
		t.Errorf("matchExpression key = %v, want topology.kubernetes.io/zone",
			terms[0].MatchExpressions[0].Key)
	}
}

func TestBuildSandboxSpec_Volumes(t *testing.T) {
	p := &OpenKruisePlugin{}
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "worker",
					Image: "test:latest",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data-vol", MountPath: "/data"},
						{Name: "config-vol", MountPath: "/etc/config", ReadOnly: true},
					},
				}},
				Volumes: []corev1.Volume{
					{
						Name: "data-vol",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
					{
						Name: "config-vol",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "my-config"},
							},
						},
					},
				},
			},
		},
	}

	result := p.buildSandboxSpec(spec)
	podSpec := result["template"].(map[string]interface{})["spec"].(map[string]interface{})
	container := podSpec["containers"].([]interface{})[0].(map[string]interface{})

	// Verify volumes at pod level.
	volRaw, ok := podSpec["volumes"]
	if !ok {
		t.Fatal("podSpec must contain 'volumes'")
	}
	volJSON, _ := json.Marshal(volRaw)
	var volumes []corev1.Volume
	if err := json.Unmarshal(volJSON, &volumes); err != nil {
		t.Fatalf("failed to unmarshal volumes: %v", err)
	}
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}
	if volumes[0].Name != "data-vol" || volumes[0].EmptyDir == nil {
		t.Errorf("volume[0] = %+v, want data-vol with emptyDir", volumes[0])
	}

	// Verify volumeMounts at container level.
	vmRaw, ok := container["volumeMounts"]
	if !ok {
		t.Fatal("container must contain 'volumeMounts'")
	}
	vmJSON, _ := json.Marshal(vmRaw)
	var mounts []corev1.VolumeMount
	if err := json.Unmarshal(vmJSON, &mounts); err != nil {
		t.Fatalf("failed to unmarshal volumeMounts: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("expected 2 volumeMounts, got %d", len(mounts))
	}
	if mounts[0].Name != "data-vol" || mounts[0].MountPath != "/data" {
		t.Errorf("mount[0] = %+v, want data-vol at /data", mounts[0])
	}
	if !mounts[1].ReadOnly {
		t.Errorf("mount[1].ReadOnly = false, want true")
	}
}

func TestBuildSandboxSpec_EmptyOptionalFields(t *testing.T) {
	p := &OpenKruisePlugin{}
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: "test:latest"}},
			},
		},
	}

	result := p.buildSandboxSpec(spec)
	podSpec := result["template"].(map[string]interface{})["spec"].(map[string]interface{})
	container := podSpec["containers"].([]interface{})[0].(map[string]interface{})

	// Verify omitted fields are absent (k8s types use omitempty).
	if _, ok := container["workingDir"]; ok {
		t.Error("empty workingDir should not be set")
	}
	if _, ok := container["env"]; ok {
		t.Error("empty env should not be set")
	}
	if _, ok := container["volumeMounts"]; ok {
		t.Error("empty volumeMounts should not be set")
	}
	if _, ok := podSpec["nodeSelector"]; ok {
		t.Error("empty nodeSelector should not be set")
	}
	if _, ok := podSpec["tolerations"]; ok {
		t.Error("empty tolerations should not be set")
	}
	if _, ok := podSpec["affinity"]; ok {
		t.Error("nil affinity should not be set")
	}
	if _, ok := podSpec["volumes"]; ok {
		t.Error("empty volumes should not be set")
	}
}

func TestBuildSandboxSpec_TemplateMetadata(t *testing.T) {
	p := &OpenKruisePlugin{}
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app":               "hiclaw-worker",
					"hiclaw.io/runtime": "openclaw",
				},
				Annotations: map[string]string{
					"network.alibabacloud.com/security-group-ids": "sg-bp1xxx",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: "test:latest"}},
			},
		},
	}

	result := p.buildSandboxSpec(spec)
	tmpl := result["template"].(map[string]interface{})

	// Verify template.metadata.labels.
	meta, ok := tmpl["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("template must contain 'metadata' map, got %T", tmpl["metadata"])
	}
	labels, ok := meta["labels"].(map[string]interface{})
	if !ok {
		t.Fatalf("metadata.labels must be map, got %T", meta["labels"])
	}
	if labels["app"] != "hiclaw-worker" {
		t.Errorf("labels[app] = %v, want 'hiclaw-worker'", labels["app"])
	}

	// Verify template.metadata.annotations.
	annotations, ok := meta["annotations"].(map[string]interface{})
	if !ok {
		t.Fatalf("metadata.annotations must be map, got %T", meta["annotations"])
	}
	if annotations["network.alibabacloud.com/security-group-ids"] != "sg-bp1xxx" {
		t.Errorf("annotations[security-group-ids] = %v, want 'sg-bp1xxx'",
			annotations["network.alibabacloud.com/security-group-ids"])
	}
}

func TestBuildSandboxSpec_InitContainers(t *testing.T) {
	p := &OpenKruisePlugin{}
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{
					Name:    "init-setup",
					Image:   "busybox:latest",
					Command: []string{"sh", "-c", "echo setup"},
				}},
				Containers: []corev1.Container{{Name: "worker", Image: "test:latest"}},
			},
		},
	}

	result := p.buildSandboxSpec(spec)
	podSpec := result["template"].(map[string]interface{})["spec"].(map[string]interface{})

	initRaw, ok := podSpec["initContainers"]
	if !ok {
		t.Fatal("podSpec must contain 'initContainers'")
	}
	initJSON, _ := json.Marshal(initRaw)
	var initContainers []corev1.Container
	if err := json.Unmarshal(initJSON, &initContainers); err != nil {
		t.Fatalf("failed to unmarshal initContainers: %v", err)
	}
	if len(initContainers) != 1 {
		t.Fatalf("expected 1 initContainer, got %d", len(initContainers))
	}
	if initContainers[0].Name != "init-setup" {
		t.Errorf("initContainer name = %v, want 'init-setup'", initContainers[0].Name)
	}
}

func TestBuildSandboxSpec_SidecarContainers(t *testing.T) {
	p := &OpenKruisePlugin{}
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "worker", Image: "worker:latest"},
					{Name: "log-collector", Image: "fluentd:latest"},
				},
			},
		},
	}

	result := p.buildSandboxSpec(spec)
	podSpec := result["template"].(map[string]interface{})["spec"].(map[string]interface{})

	containers := podSpec["containers"].([]interface{})
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(containers))
	}
	c1 := containers[0].(map[string]interface{})
	c2 := containers[1].(map[string]interface{})
	if c1["name"] != "worker" {
		t.Errorf("container[0].name = %v, want 'worker'", c1["name"])
	}
	if c2["name"] != "log-collector" {
		t.Errorf("container[1].name = %v, want 'log-collector'", c2["name"])
	}
}

func TestBuildSandboxSpec_SecurityContext(t *testing.T) {
	p := &OpenKruisePlugin{}
	runAsUser := int64(1000)
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: "test:latest"}},
				SecurityContext: &corev1.PodSecurityContext{
					RunAsUser: &runAsUser,
				},
			},
		},
	}

	result := p.buildSandboxSpec(spec)
	podSpec := result["template"].(map[string]interface{})["spec"].(map[string]interface{})

	scRaw, ok := podSpec["securityContext"]
	if !ok {
		t.Fatal("podSpec must contain 'securityContext'")
	}
	scJSON, _ := json.Marshal(scRaw)
	var sc corev1.PodSecurityContext
	if err := json.Unmarshal(scJSON, &sc); err != nil {
		t.Fatalf("failed to unmarshal securityContext: %v", err)
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
		t.Errorf("securityContext.runAsUser = %v, want 1000", sc.RunAsUser)
	}
}

func TestBuildSandboxSpec_RuntimesField(t *testing.T) {
	p := &OpenKruisePlugin{}
	spec := SandboxSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Image: "test:latest"}},
			},
		},
	}

	result := p.buildSandboxSpec(spec)

	runtimesRaw, ok := result["runtimes"]
	if !ok {
		t.Fatal("spec must contain 'runtimes' field")
	}
	runtimes, ok := runtimesRaw.([]interface{})
	if !ok {
		t.Fatalf("runtimes must be a slice, got %T", runtimesRaw)
	}
	if len(runtimes) != 1 {
		t.Fatalf("runtimes must have 1 entry, got %d", len(runtimes))
	}
	rt, ok := runtimes[0].(map[string]interface{})
	if !ok {
		t.Fatalf("runtimes[0] must be a map, got %T", runtimes[0])
	}
	if rt["name"] != "agent-runtime" {
		t.Errorf("runtimes[0].name = %v, want 'agent-runtime'", rt["name"])
	}
}
