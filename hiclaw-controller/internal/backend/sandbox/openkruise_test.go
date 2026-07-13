package sandbox

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestBuildSandboxClaimSpec_BasicFields(t *testing.T) {
	p := &OpenKruisePlugin{}
	result := p.buildSandboxClaimSpec(SandboxClaimSpec{
		SandboxSetName: "warm",
		Labels: map[string]string{
			"agentteams.io/worker":                 "alice",
			"agentteams.io/manager":                "default",
			"security.agents.kruise.io/agent-name": "agentteams",
		},
		Annotations: map[string]string{
			"network.alibabacloud.com/security-group-ids": "sg-bp1xxx",
		},
	})

	if result["templateName"] != "warm" {
		t.Fatalf("templateName=%v, want warm", result["templateName"])
	}
	if result["replicas"] != int64(1) {
		t.Fatalf("replicas=%v, want 1", result["replicas"])
	}
	if result["waitReadyTimeout"] != "2m" {
		t.Fatalf("waitReadyTimeout=%v, want 2m", result["waitReadyTimeout"])
	}
	if _, ok := result["skipInitRuntime"]; ok {
		t.Fatalf("skipInitRuntime should be omitted: %v", result["skipInitRuntime"])
	}
	claimLabels, ok := result["labels"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec.labels=%v, want labels map", result["labels"])
	}
	for key, want := range map[string]string{
		"agentteams.io/worker":                 "alice",
		"agentteams.io/manager":                "default",
		"security.agents.kruise.io/agent-name": "agentteams",
	} {
		if claimLabels[key] != want {
			t.Fatalf("spec.labels[%s]=%v, want %s (all=%v)", key, claimLabels[key], want, claimLabels)
		}
	}
	claimAnnotations, ok := result["annotations"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec.annotations=%v, want annotations map", result["annotations"])
	}
	if claimAnnotations["network.alibabacloud.com/security-group-ids"] != "sg-bp1xxx" {
		t.Fatalf("spec.annotations=%v, want security group annotation", claimAnnotations)
	}
	if _, ok := result["template"]; ok {
		t.Fatalf("spec.template should be omitted: %v", result["template"])
	}
}

func TestBuildSandboxClaimSpec_InplaceUpdateAndDynamicVolumes(t *testing.T) {
	p := &OpenKruisePlugin{}
	result := p.buildSandboxClaimSpec(SandboxClaimSpec{
		SandboxSetName: "warm",
		InplaceUpdate:  &SandboxClaimInplaceUpdateSpec{Image: "hiclaw/worker:v2"},
		DynamicVolumesMount: []SandboxClaimDynamicVolumeMount{
			{
				PVName:     "agentteams",
				MountPath:  "/var/run/secrets/agentteams",
				SubPath:    "workers-deps/alice/token",
				ReadOnly:   true,
				Attributes: map[string]string{"credentialProviderName": "agentteams-token"},
			},
			{
				PVName:    "agentteams",
				MountPath: "/mnt/agentteams/env",
				SubPath:   "workers-deps/alice/env",
				ReadOnly:  true,
			},
			{
				PVName:    "agentteams",
				MountPath: "/mnt/agentteams/data",
				SubPath:   "workers-deps/alice/data",
				ReadOnly:  false,
			},
		},
	})

	inplace, ok := result["inplaceUpdate"].(map[string]interface{})
	if !ok {
		t.Fatalf("inplaceUpdate missing: %v", result)
	}
	if inplace["image"] != "hiclaw/worker:v2" {
		t.Fatalf("inplaceUpdate.image=%v", inplace["image"])
	}
	mounts, ok := result["dynamicVolumesMount"].([]interface{})
	if !ok || len(mounts) != 3 {
		t.Fatalf("dynamicVolumesMount=%v", result["dynamicVolumesMount"])
	}
	tokenMount := mounts[0].(map[string]interface{})
	if tokenMount["mountPath"] != "/var/run/secrets/agentteams" || tokenMount["readOnly"] != true {
		t.Fatalf("token dynamic volume mount=%v", tokenMount)
	}
	tokenAttributes, ok := tokenMount["attributes"].(map[string]interface{})
	if !ok || tokenAttributes["credentialProviderName"] != "agentteams-token" {
		t.Fatalf("token dynamic volume attributes=%v", tokenMount["attributes"])
	}
	if _, ok := tokenAttributes["credProviderName"]; ok {
		t.Fatalf("legacy credProviderName should be omitted: %v", tokenAttributes)
	}
	envMount := mounts[1].(map[string]interface{})
	if envMount["mountPath"] != "/mnt/agentteams/env" || envMount["readOnly"] != true {
		t.Fatalf("env dynamic volume mount=%v", envMount)
	}
	dataMount := mounts[2].(map[string]interface{})
	if dataMount["mountPath"] != "/mnt/agentteams/data" || dataMount["readOnly"] != false {
		t.Fatalf("data dynamic volume mount=%v", dataMount)
	}
	if _, ok := result["template"]; ok {
		t.Fatalf("identityless claim should omit template when no override is provided: %v", result["template"])
	}
}

func TestCreateSandboxClaim_CreatesSandboxClaimCR(t *testing.T) {
	scheme := runtime.NewScheme()
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		sandboxClaimGVR: "SandboxClaimList",
	})
	plugin := NewOpenKruisePlugin(fakeClient)

	_, err := plugin.CreateSandboxClaim(context.Background(), SandboxClaimSpec{
		Name:           "claim-alice",
		Namespace:      "test-ns",
		SandboxSetName: "warm",
		Labels: map[string]string{
			"agentteams.io/worker":                 "alice",
			"agentteams.io/manager":                "default",
			"security.agents.kruise.io/agent-name": "agentteams",
		},
		Annotations: map[string]string{
			"network.alibabacloud.com/security-group-ids": "sg-bp1xxx",
		},
	}, ProviderConfig{Namespace: "test-ns"})
	if err != nil {
		t.Fatalf("CreateSandboxClaim: %v", err)
	}

	obj, err := fakeClient.Resource(sandboxClaimGVR).Namespace("test-ns").Get(context.Background(), "claim-alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claim: %v", err)
	}
	if obj.GetKind() != "SandboxClaim" {
		t.Fatalf("kind=%q, want SandboxClaim", obj.GetKind())
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "templateName"); got != "warm" {
		t.Fatalf("spec.templateName=%q, want warm", got)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "waitReadyTimeout"); got != "2m" {
		t.Fatalf("spec.waitReadyTimeout=%q, want 2m", got)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "skipInitRuntime"); ok {
		t.Fatalf("spec.skipInitRuntime should be omitted: %v", obj.Object["spec"])
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "labels", "security.agents.kruise.io/agent-name"); got != "agentteams" {
		t.Fatalf("spec.labels[agent-name]=%q, want agentteams", got)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "labels", "agentteams.io/worker"); got != "alice" {
		t.Fatalf("spec.labels[worker]=%q, want alice", got)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "labels", "agentteams.io/manager"); got != "default" {
		t.Fatalf("spec.labels[manager]=%q, want default", got)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "spec", "annotations", "network.alibabacloud.com/security-group-ids"); got != "sg-bp1xxx" {
		t.Fatalf("spec.annotations[security-group-ids]=%q, want sg-bp1xxx", got)
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template"); ok {
		t.Fatalf("spec.template should be omitted: %v", obj.Object["spec"])
	}
}

func TestListSandboxes_FiltersByLabels(t *testing.T) {
	scheme := runtime.NewScheme()
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		sandboxGVR: "SandboxList",
	})
	matching := newFakeSandboxObject("test-ns", "sandbox-alice", "Running", nil)
	matching.SetLabels(map[string]string{"agentteams.io/worker": "alice", "agentteams.io/controller": "ctl-x"})
	other := newFakeSandboxObject("test-ns", "sandbox-bob", "Running", nil)
	other.SetLabels(map[string]string{"agentteams.io/worker": "bob", "agentteams.io/controller": "ctl-x"})
	for _, obj := range []*unstructured.Unstructured{matching, other} {
		if _, err := fakeClient.Resource(sandboxGVR).Namespace("test-ns").Create(
			context.Background(), obj, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed sandbox %s: %v", obj.GetName(), err)
		}
	}

	plugin := NewOpenKruisePlugin(fakeClient)
	statuses, err := plugin.ListSandboxes(context.Background(), map[string]string{
		"agentteams.io/worker":     "alice",
		"agentteams.io/controller": "ctl-x",
	}, ProviderConfig{Namespace: "test-ns"})
	if err != nil {
		t.Fatalf("ListSandboxes() error: %v", err)
	}
	if len(statuses) != 1 || statuses[0].SandboxID != "sandbox-alice" {
		t.Fatalf("ListSandboxes()=%+v, want only sandbox-alice", statuses)
	}
}

func TestDeleteSandbox_NotFoundIsSuccess(t *testing.T) {
	scheme := runtime.NewScheme()
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		sandboxGVR: "SandboxList",
	})
	plugin := NewOpenKruisePlugin(fakeClient)

	if err := plugin.DeleteSandbox(context.Background(), "missing", ProviderConfig{Namespace: "test-ns"}); err != nil {
		t.Fatalf("DeleteSandbox missing: %v", err)
	}
}

func newFakeSandboxObject(ns, name, phase string, conditions []interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "agents.kruise.io/v1alpha1",
			"kind":       "Sandbox",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"status": map[string]interface{}{
				"phase": phase,
			},
		},
	}
	if conditions != nil {
		_ = unstructured.SetNestedSlice(obj.Object, conditions, "status", "conditions")
	}
	return obj
}

func newFakeSandboxClaimObject(ns, name, phase, message string, desiredReplicas, claimedReplicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "agents.kruise.io/v1alpha1",
			"kind":       "SandboxClaim",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"replicas": desiredReplicas,
			},
			"status": map[string]interface{}{
				"phase":           phase,
				"message":         message,
				"claimedReplicas": claimedReplicas,
			},
		},
	}
}

func TestGetSandboxClaimStatus_ParsesClaimProgress(t *testing.T) {
	scheme := runtime.NewScheme()
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		sandboxClaimGVR: "SandboxClaimList",
	})
	claimObj := newFakeSandboxClaimObject("test-ns", "claim-alice", PhaseCompleted, "Successfully claimed 1/1 sandboxes", 1, 1)
	if _, err := fakeClient.Resource(sandboxClaimGVR).Namespace("test-ns").Create(
		context.Background(), claimObj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	plugin := NewOpenKruisePlugin(fakeClient)
	status, err := plugin.GetSandboxClaimStatus(context.Background(), "claim-alice", ProviderConfig{Namespace: "test-ns"})
	if err != nil {
		t.Fatalf("GetSandboxClaimStatus() error: %v", err)
	}
	if status.Phase != PhaseCompleted {
		t.Fatalf("Phase=%q, want %q", status.Phase, PhaseCompleted)
	}
	if status.Message != "Successfully claimed 1/1 sandboxes" {
		t.Fatalf("Message=%q", status.Message)
	}
	if status.DesiredReplicas == nil || *status.DesiredReplicas != 1 {
		t.Fatalf("DesiredReplicas=%v, want 1", status.DesiredReplicas)
	}
	if status.ClaimedReplicas == nil || *status.ClaimedReplicas != 1 {
		t.Fatalf("ClaimedReplicas=%v, want 1", status.ClaimedReplicas)
	}
}
