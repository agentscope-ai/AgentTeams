package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
			Resources: &v1beta1.ExecutionSandboxResourceRequirements{
				Requests: v1beta1.ExecutionSandboxResourceValues{CPU: "100m", Memory: "128Mi", EphemeralStorage: "512Mi"},
				Limits:   v1beta1.ExecutionSandboxResourceValues{CPU: "1", Memory: "1Gi", EphemeralStorage: "4Gi"},
			},
		},
	}
	allowed := []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{443}}}
	_, resources, emptyDirLimit, err := sandboxpolicy.Default().Resolve(sandbox.Spec.Resources)
	if err != nil {
		t.Fatalf("resolve sandbox resources: %v", err)
	}

	pod, service, policy, err := buildExecutionSandboxResources(sandbox, "runner-token", "ctl-a", allowed, resources, emptyDirLimit)
	if err != nil {
		t.Fatalf("buildExecutionSandboxResources: %v", err)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("sandbox must disable ServiceAccount token automount")
	}
	if pod.Spec.ServiceAccountName != "default" {
		t.Fatalf("sandbox ServiceAccountName=%q, want default to match API defaulting", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot ||
		pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context is not hardened: %#v", pod.Spec.SecurityContext)
	}
	container := pod.Spec.Containers[0]
	if got := container.Resources.Requests[corev1.ResourceEphemeralStorage]; got.String() != "512Mi" {
		t.Fatalf("ephemeral request=%q, want 512Mi", got.String())
	}
	if got := container.Resources.Limits[corev1.ResourceEphemeralStorage]; got.String() != "4Gi" {
		t.Fatalf("ephemeral limit=%q, want 4Gi", got.String())
	}
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
	for _, volume := range pod.Spec.Volumes {
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.String() != "4Gi" {
			t.Fatalf("volume %s sizeLimit=%v, want 4Gi", volume.Name, volume.EmptyDir)
		}
	}
	if service.Spec.Selector[v1beta1.LabelExecutionSandbox] != sandbox.Name || service.Spec.Ports[0].Port != 8080 {
		t.Fatalf("service does not target runner: %#v", service.Spec)
	}
	if len(policy.Spec.PolicyTypes) != 2 || len(policy.Spec.Ingress) != 1 || len(policy.Spec.Egress) != 2 {
		t.Fatalf("network policy must default-deny and add explicit rules: %#v", policy.Spec)
	}
	ingressPeer := policy.Spec.Ingress[0].From[0].PodSelector
	if ingressPeer == nil || ingressPeer.MatchLabels[v1beta1.LabelWorker] != "researcher" {
		t.Fatalf("runner ingress is not restricted to its worker: %#v", policy.Spec.Ingress)
	}
	if ingressPeer.MatchLabels[v1beta1.LabelController] != "ctl-a" {
		t.Fatalf("runner ingress is not restricted to its controller: %#v", policy.Spec.Ingress)
	}
	if got := policy.Spec.Egress[0].To[0].IPBlock.CIDR; got != "10.96.0.10/32" {
		t.Fatalf("egress CIDR=%q", got)
	}
	dnsPeer := policy.Spec.Egress[1].To[0]
	if dnsPeer.NamespaceSelector == nil || dnsPeer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" ||
		dnsPeer.PodSelector == nil || dnsPeer.PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Fatalf("DNS egress peer=%#v", dnsPeer)
	}
}

func TestExecutionSandboxReconcilerConvergesInvalidResources(t *testing.T) {
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
			Model: "qwen-max",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
			}},
		},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "exec-invalid-resources", Namespace: "agentteams-system", UID: "sandbox-uid"},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: "researcher", UID: "worker-uid"},
			SessionID: "thread-hash",
			Resources: &v1beta1.ExecutionSandboxResourceRequirements{
				Limits: v1beta1.ExecutionSandboxResourceValues{EphemeralStorage: "9Gi"},
			},
		},
	}
	key := types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.ExecutionSandbox{}).
		WithObjects(worker, sandbox, secret, pod, service, policy).
		Build()
	r := &ExecutionSandboxReconciler{
		Client:         cl,
		RunnerImage:    "runner:v1",
		ControllerName: "ctl-a",
		DefaultRuntime: "deepagents",
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile invalid resources: %v", err)
	}
	for _, object := range []client.Object{&corev1.Secret{}, &corev1.Pod{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}} {
		object.SetName(key.Name)
		object.SetNamespace(key.Namespace)
		if err := cl.Get(context.Background(), key, object); !apierrors.IsNotFound(err) {
			t.Fatalf("invalid sandbox left %T behind: %v", object, err)
		}
	}
	var updated v1beta1.ExecutionSandbox
	if err := cl.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get invalid sandbox: %v", err)
	}
	if updated.Status.Phase != "Failed" || updated.Status.Endpoint != "" || updated.Status.PodName != "" {
		t.Fatalf("invalid sandbox status=%#v", updated.Status)
	}
	for _, condition := range updated.Status.Conditions {
		if condition.Type == "Ready" {
			if condition.Status != metav1.ConditionFalse || condition.Reason != "InvalidResources" {
				t.Fatalf("Ready condition=%#v, want false InvalidResources", condition)
			}
			break
		}
	}
	if sandboxReadyReason(updated.Status.Conditions) != "InvalidResources" {
		t.Fatalf("missing InvalidResources Ready condition: %#v", updated.Status.Conditions)
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("repeat Reconcile invalid resources: %v", err)
	}
	for _, object := range []client.Object{&corev1.Secret{}, &corev1.Pod{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}} {
		object.SetName(key.Name)
		object.SetNamespace(key.Namespace)
		if err := cl.Get(context.Background(), key, object); !apierrors.IsNotFound(err) {
			t.Fatalf("repeat invalid sandbox left %T behind: %v", object, err)
		}
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
			Model: "qwen-max",
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
		DefaultRuntime: "deepagents",
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
	if pod.Spec.Containers[0].ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("runner imagePullPolicy=%q, want IfNotPresent", pod.Spec.Containers[0].ImagePullPolicy)
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
	// Drift the live policy without touching its desired-state annotation. The
	// reconciler must inspect the real spec and repair it in place; deleting the
	// policy would leave the still-running Runner temporarily fail-open.
	policy.Spec.Egress = nil
	if err := cl.Update(context.Background(), &policy); err != nil {
		t.Fatalf("inject NetworkPolicy drift: %v", err)
	}
	result, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("drift reconcile result=%#v err=%v", result, err)
	}
	if err := cl.Get(context.Background(), key, &policy); err != nil {
		t.Fatalf("NetworkPolicy must remain present while drift is repaired: %v", err)
	}
	if len(policy.Spec.Egress) != 2 {
		t.Fatalf("NetworkPolicy egress was not restored in place: %#v", policy.Spec.Egress)
	}
}

func TestExecutionSandboxReconcilerRecreatesStaleImmutableResourcesAndUpdatesNetworkPolicy(t *testing.T) {
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
			Model: "qwen-max",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
			}},
		},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "exec-policy-refresh", Namespace: "agentteams-system", UID: "sandbox-uid"},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: worker.Name, UID: string(worker.UID)},
			SessionID: "thread-hash",
			Egress:    []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{443}}},
		},
	}
	key := types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.ExecutionSandbox{}).
		WithObjects(worker, sandbox).
		Build()
	r := &ExecutionSandboxReconciler{
		Client:         cl,
		RunnerImage:    "runner:v1",
		ControllerName: "ctl-a",
		DefaultRuntime: "deepagents",
		EgressCeilings: []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.0/12", Ports: []int32{443}}},
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("initial Reconcile: %v", err)
	}

	var pod corev1.Pod
	if err := cl.Get(context.Background(), key, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.Containers[0].Image = "runner:stale"
	if err := cl.Update(context.Background(), &pod); err != nil {
		t.Fatalf("inject Pod drift: %v", err)
	}
	var service corev1.Service
	if err := cl.Get(context.Background(), key, &service); err != nil {
		t.Fatal(err)
	}
	service.Spec.Ports[0].Port = 9090
	if err := cl.Update(context.Background(), &service); err != nil {
		t.Fatalf("inject Service drift: %v", err)
	}
	var policy networkingv1.NetworkPolicy
	if err := cl.Get(context.Background(), key, &policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.Egress = nil
	if err := cl.Update(context.Background(), &policy); err != nil {
		t.Fatalf("inject NetworkPolicy drift: %v", err)
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile stale Service: %v", err)
	}
	if result.RequeueAfter != executionSandboxRequeue {
		t.Fatalf("stale Service requeue=%s, want %s", result.RequeueAfter, executionSandboxRequeue)
	}
	if err := cl.Get(context.Background(), key, &service); !apierrors.IsNotFound(err) {
		t.Fatalf("stale immutable Service was not deleted: %v", err)
	}

	result, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile stale Pod: %v", err)
	}
	if result.RequeueAfter != executionSandboxRequeue {
		t.Fatalf("stale Pod requeue=%s, want %s", result.RequeueAfter, executionSandboxRequeue)
	}
	if err := cl.Get(context.Background(), key, &pod); !apierrors.IsNotFound(err) {
		t.Fatalf("stale immutable Pod was not deleted: %v", err)
	}
	if err := cl.Get(context.Background(), key, &policy); err != nil {
		t.Fatalf("NetworkPolicy must remain present while it is updated: %v", err)
	}
	if len(policy.Spec.Egress) != 2 {
		t.Fatalf("NetworkPolicy egress=%#v, want restored desired policy", policy.Spec.Egress)
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("recreate immutable resources: %v", err)
	}
	if err := cl.Get(context.Background(), key, &service); err != nil {
		t.Fatalf("recreated Service: %v", err)
	}
	if service.Spec.Ports[0].Port != 8080 {
		t.Fatalf("recreated Service port=%d, want 8080", service.Spec.Ports[0].Port)
	}
	if err := cl.Get(context.Background(), key, &pod); err != nil {
		t.Fatalf("recreated Pod: %v", err)
	}
	if pod.Spec.Containers[0].Image != "runner:v1" {
		t.Fatalf("recreated Pod image=%q, want runner:v1", pod.Spec.Containers[0].Image)
	}
}

func TestExecutionSandboxReconcilerSchedulesReadySandboxExpiry(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	r, cl, key := newExecutionSandboxLifecycleTestRig(t, now)

	setExecutionSandboxPodPhase(t, cl, key, corev1.PodRunning, true)
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile ready sandbox: %v", err)
	}
	if result.RequeueAfter != 10*time.Minute {
		t.Fatalf("ready sandbox requeue=%s, want max-lifetime deadline in 10m", result.RequeueAfter)
	}

	var sandbox v1beta1.ExecutionSandbox
	if err := cl.Get(context.Background(), key, &sandbox); err != nil {
		t.Fatal(err)
	}
	if sandbox.Status.Phase != "Ready" || sandboxReadyReason(sandbox.Status.Conditions) != "PodReady" {
		t.Fatalf("ready sandbox status=%#v", sandbox.Status)
	}
	resourceVersion := sandbox.ResourceVersion
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("repeat ready Reconcile: %v", err)
	}
	if err := cl.Get(context.Background(), key, &sandbox); err != nil {
		t.Fatal(err)
	}
	if sandbox.ResourceVersion != resourceVersion {
		t.Fatalf(
			"unchanged ready status was rewritten: resourceVersion %q -> %q",
			resourceVersion,
			sandbox.ResourceVersion,
		)
	}
}

func TestExecutionSandboxReconcilerReportsTerminalRunnerPodFailed(t *testing.T) {
	tests := []struct {
		name       string
		podPhase   corev1.PodPhase
		wantReason string
	}{
		{name: "failed", podPhase: corev1.PodFailed, wantReason: "PodFailed"},
		{name: "unexpected successful exit", podPhase: corev1.PodSucceeded, wantReason: "PodCompleted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
			r, cl, key := newExecutionSandboxLifecycleTestRig(t, now)
			setExecutionSandboxPodPhase(t, cl, key, tt.podPhase, false)

			result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("Reconcile terminal sandbox: %v", err)
			}
			if result.RequeueAfter != 10*time.Minute {
				t.Fatalf("terminal sandbox requeue=%s, want max-lifetime deadline in 10m", result.RequeueAfter)
			}

			var sandbox v1beta1.ExecutionSandbox
			if err := cl.Get(context.Background(), key, &sandbox); err != nil {
				t.Fatal(err)
			}
			if sandbox.Status.Phase != "Failed" || sandboxReadyReason(sandbox.Status.Conditions) != tt.wantReason {
				t.Fatalf("terminal sandbox status=%#v", sandbox.Status)
			}
			var pod corev1.Pod
			if err := cl.Get(context.Background(), key, &pod); err != nil {
				t.Fatalf("terminal runner Pod was recreated or deleted: %v", err)
			}
			if pod.Status.Phase != tt.podPhase {
				t.Fatalf("terminal runner Pod phase=%q, want preserved %q", pod.Status.Phase, tt.podPhase)
			}
		})
	}
}

func TestExecutionSandboxReconcilerMovesIdleDeadlineWithHeartbeatAndDeletesAtExpiry(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	r, cl, key := newExecutionSandboxLifecycleTestRig(t, now)
	var sandbox v1beta1.ExecutionSandbox
	if err := cl.Get(context.Background(), key, &sandbox); err != nil {
		t.Fatal(err)
	}
	sandbox.Spec.MaxLifetime = "8h"
	if err := cl.Update(context.Background(), &sandbox); err != nil {
		t.Fatalf("extend max lifetime: %v", err)
	}
	setExecutionSandboxPodPhase(t, cl, key, corev1.PodRunning, true)
	if err := cl.Get(context.Background(), key, &sandbox); err != nil {
		t.Fatal(err)
	}
	staleHeartbeat := metav1.NewTime(now.Add(-5 * time.Minute))
	sandbox.Status.LastHeartbeat = &staleHeartbeat
	if err := cl.Status().Update(context.Background(), &sandbox); err != nil {
		t.Fatalf("set stale heartbeat: %v", err)
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile stale heartbeat: %v", err)
	}
	if result.RequeueAfter != 25*time.Minute {
		t.Fatalf("stale-heartbeat requeue=%s, want 25m idle deadline", result.RequeueAfter)
	}

	if err := cl.Get(context.Background(), key, &sandbox); err != nil {
		t.Fatal(err)
	}
	freshHeartbeat := metav1.NewTime(now)
	sandbox.Status.LastHeartbeat = &freshHeartbeat
	if err := cl.Status().Update(context.Background(), &sandbox); err != nil {
		t.Fatalf("refresh heartbeat: %v", err)
	}
	result, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile fresh heartbeat: %v", err)
	}
	if result.RequeueAfter != 30*time.Minute {
		t.Fatalf("fresh-heartbeat requeue=%s, want moved 30m idle deadline", result.RequeueAfter)
	}

	r.Now = func() time.Time { return now.Add(30 * time.Minute) }
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile idle expiry: %v", err)
	}
	if err := cl.Get(context.Background(), key, &sandbox); !apierrors.IsNotFound(err) {
		t.Fatalf("idle sandbox was not deleted at moved deadline: %v", err)
	}
}

func newExecutionSandboxLifecycleTestRig(
	t *testing.T,
	now time.Time,
) (*ExecutionSandboxReconciler, client.Client, types.NamespacedName) {
	t.Helper()
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
			Model: "qwen-max",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
			}},
		},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "exec-lifecycle",
			Namespace:         "agentteams-system",
			UID:               "sandbox-uid",
			CreationTimestamp: metav1.NewTime(now),
		},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef:   v1beta1.ExecutionSandboxWorkerRef{Name: "researcher", UID: "worker-uid"},
			SessionID:   "thread-hash",
			IdleTimeout: "30m",
			MaxLifetime: "10m",
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.ExecutionSandbox{}, &corev1.Pod{}).
		WithObjects(worker, sandbox).
		Build()
	r := &ExecutionSandboxReconciler{
		Client:         cl,
		RunnerImage:    "runner:v1",
		ControllerName: "ctl-a",
		DefaultRuntime: "deepagents",
		Now:            func() time.Time { return now },
	}
	key := types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("initial Reconcile: %v", err)
	}
	return r, cl, key
}

func setExecutionSandboxPodPhase(
	t *testing.T,
	cl client.Client,
	key types.NamespacedName,
	phase corev1.PodPhase,
	ready bool,
) {
	t.Helper()
	var pod corev1.Pod
	if err := cl.Get(context.Background(), key, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = phase
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if err := cl.Status().Update(context.Background(), &pod); err != nil {
		t.Fatalf("set runner Pod status: %v", err)
	}
}

func sandboxReadyReason(conditions []metav1.Condition) string {
	for _, condition := range conditions {
		if condition.Type == "Ready" {
			return condition.Reason
		}
	}
	return ""
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
