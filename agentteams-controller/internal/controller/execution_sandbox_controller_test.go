package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestIntersectSandboxEgressRestrictsCIDRsAndPorts(t *testing.T) {
	requested := []v1beta1.DeepAgentsEgressRule{
		{CIDR: "10.96.0.10/32", Ports: []int32{80, 443}},
		{CIDR: "192.168.1.0/24", Ports: []int32{443}},
	}
	ceilings := []v1beta1.DeepAgentsEgressRule{
		{CIDR: "10.96.0.0/12", Ports: []int32{443}},
	}

	got, err := intersectSandboxEgress(requested, ceilings)
	if err != nil {
		t.Fatalf("intersectSandboxEgress: %v", err)
	}
	want := []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{443}}}
	if len(got) != 1 || got[0].CIDR != want[0].CIDR || len(got[0].Ports) != 1 || got[0].Ports[0] != 443 {
		t.Fatalf("intersection=%#v, want %#v", got, want)
	}

	if _, err := intersectSandboxEgress(
		[]v1beta1.DeepAgentsEgressRule{{CIDR: "not-a-cidr", Ports: []int32{443}}}, ceilings,
	); err == nil {
		t.Fatal("invalid requested CIDR was accepted")
	}
	if _, err := intersectSandboxEgress(
		[]v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{70000}}}, ceilings,
	); err == nil {
		t.Fatal("invalid requested port was accepted")
	}
}

func TestExecutionSandboxReconcilerExpiresIdleSandbox(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	now := time.Date(2026, 8, 4, 18, 0, 0, 0, time.UTC)
	created := metav1.NewTime(now.Add(-2 * time.Hour))
	heartbeat := metav1.NewTime(now.Add(-31 * time.Minute))
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "researcher", Namespace: "default", UID: "worker-uid"},
		Spec: v1beta1.WorkerSpec{
			Model:   "qwen-max",
			Runtime: "deepagents",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
			}},
		},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "exec-expired", Namespace: "default", UID: "sandbox-uid", CreationTimestamp: created,
		},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef:   v1beta1.ExecutionSandboxWorkerRef{Name: "researcher", UID: "worker-uid"},
			SessionID:   "thread-hash",
			IdleTimeout: "30m",
			MaxLifetime: "8h",
		},
		Status: v1beta1.ExecutionSandboxStatus{LastHeartbeat: &heartbeat},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker, sandbox).Build()
	r := &ExecutionSandboxReconciler{Client: cl, RunnerImage: "runner:v1", Now: func() time.Time { return now }}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{
		Name: sandbox.Name, Namespace: sandbox.Namespace,
	}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var deleted v1beta1.ExecutionSandbox
	err = cl.Get(context.Background(), types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}, &deleted)
	if err == nil {
		t.Fatal("idle ExecutionSandbox was not deleted")
	}
}

func TestBuildExecutionSandboxResourcesAreHardenedAndSecretSafe(t *testing.T) {
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "exec-abc", Namespace: "agentteams-system", UID: "sandbox-uid"},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: "researcher", UID: "worker-uid"},
			SessionID: "thread-hash",
			Image:     "runner:v1",
			Resources: &v1beta1.AgentResourceRequirements{
				Requests: v1beta1.AgentResourceValues{CPU: "100m", Memory: "128Mi"},
				Limits:   v1beta1.AgentResourceValues{CPU: "1", Memory: "1Gi"},
			},
		},
	}
	allowed := []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{443}}}

	pod, service, policy, err := buildExecutionSandboxResources(sandbox, "runner-token", "ctl-a", allowed)
	if err != nil {
		t.Fatalf("buildExecutionSandboxResources: %v", err)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("sandbox must disable ServiceAccount token automount")
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot ||
		pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context is not hardened: %#v", pod.Spec.SecurityContext)
	}
	container := pod.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem ||
		container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("container security context is not hardened: %#v", container.SecurityContext)
	}
	if len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatalf("container capabilities=%#v", container.SecurityContext.Capabilities)
	}
	for _, env := range container.Env {
		if env.Name == "AGENTTEAMS_WORKER_MATRIX_TOKEN" || env.Name == "AGENTTEAMS_WORKER_GATEWAY_KEY" ||
			env.Name == "AGENTTEAMS_FS_SECRET_KEY" || env.Value != "" {
			t.Fatalf("sandbox received a forbidden or literal environment value: %#v", env)
		}
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil || env.ValueFrom.SecretKeyRef.Name != "runner-token" {
			t.Fatalf("runner token must use SecretKeyRef: %#v", env)
		}
	}
	if len(pod.Spec.Volumes) != 2 || len(container.VolumeMounts) != 2 {
		t.Fatalf("sandbox needs only workspace and tmp emptyDir volumes: volumes=%#v mounts=%#v", pod.Spec.Volumes, container.VolumeMounts)
	}
	if service.Spec.Selector[v1beta1.LabelExecutionSandbox] != sandbox.Name || service.Spec.Ports[0].Port != 8080 {
		t.Fatalf("service does not target runner: %#v", service.Spec)
	}
	if len(policy.Spec.PolicyTypes) != 2 || len(policy.Spec.Ingress) != 1 || len(policy.Spec.Egress) != 1 {
		t.Fatalf("network policy must default-deny and add explicit rules: %#v", policy.Spec)
	}
	ingressPeer := policy.Spec.Ingress[0].From[0].PodSelector
	if ingressPeer == nil || ingressPeer.MatchLabels[v1beta1.LabelWorker] != "researcher" {
		t.Fatalf("runner ingress is not restricted to its worker: %#v", policy.Spec.Ingress)
	}
	if got := policy.Spec.Egress[0].To[0].IPBlock.CIDR; got != "10.96.0.10/32" {
		t.Fatalf("egress CIDR=%q", got)
	}
}

func TestExecutionSandboxReconcilerCreatesIsolatedRunnerResources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid"},
		Spec: v1beta1.WorkerSpec{
			Model:   "qwen-max",
			Runtime: "deepagents",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
			}},
		},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "exec-abc", Namespace: "agentteams-system", UID: "sandbox-uid"},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: "researcher", UID: "worker-uid"},
			SessionID: "thread-hash",
			Egress:    []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{443}}},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.ExecutionSandbox{}).
		WithObjects(worker, sandbox).
		Build()
	r := &ExecutionSandboxReconciler{
		Client:         cl,
		RunnerImage:    "runner:v1",
		ControllerName: "ctl-a",
		EgressCeilings: []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.0/12", Ports: []int32{443}}},
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{
		Name: "exec-abc", Namespace: "agentteams-system",
	}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("pending sandbox should requeue, got %#v", result)
	}

	key := types.NamespacedName{Name: "exec-abc", Namespace: "agentteams-system"}
	var secret corev1.Secret
	if err := cl.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("runner token Secret: %v", err)
	}
	if len(secret.Data) != 1 || len(secret.Data["token"]) < 32 {
		t.Fatalf("runner Secret must contain only a strong token: %#v", secret.Data)
	}
	for _, forbidden := range []string{"matrix", "gateway", "storage", "checkpoint"} {
		for name := range secret.Data {
			if strings.Contains(strings.ToLower(name), forbidden) {
				t.Fatalf("runner Secret contains AgentTeams credential key %q", name)
			}
		}
	}
	var pod corev1.Pod
	if err := cl.Get(context.Background(), key, &pod); err != nil {
		t.Fatalf("runner Pod: %v", err)
	}
	if pod.Spec.Containers[0].Image != "runner:v1" {
		t.Fatalf("runner image=%q", pod.Spec.Containers[0].Image)
	}
	var service corev1.Service
	if err := cl.Get(context.Background(), key, &service); err != nil {
		t.Fatalf("runner Service: %v", err)
	}
	var policy networkingv1.NetworkPolicy
	if err := cl.Get(context.Background(), key, &policy); err != nil {
		t.Fatalf("runner NetworkPolicy: %v", err)
	}
	var updated v1beta1.ExecutionSandbox
	if err := cl.Get(context.Background(), key, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != "Pending" || updated.Status.Endpoint != "http://exec-abc.agentteams-system.svc:8080" {
		t.Fatalf("sandbox status=%#v", updated.Status)
	}
}

func TestExecutionSandboxReconcilerRejectsNonDeepAgentsWorker(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = networkingv1.AddToScheme(scheme)
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default", UID: "legacy-uid"},
		Spec:       v1beta1.WorkerSpec{Model: "qwen-max", Runtime: "openclaw"},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "exec-invalid", Namespace: "default"},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: "legacy", UID: "legacy-uid"},
			SessionID: "thread-hash",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(worker, sandbox).Build()
	r := &ExecutionSandboxReconciler{Client: cl, RunnerImage: "runner:v1"}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{
		Name: "exec-invalid", Namespace: "default",
	}})
	if err == nil || !strings.Contains(err.Error(), "deepagents") {
		t.Fatalf("Reconcile error=%v, want DeepAgents validation failure", err)
	}
}
