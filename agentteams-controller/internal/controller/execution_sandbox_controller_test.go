package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
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

	got, err := sandboxpolicy.IntersectEgress(requested, ceilings)
	if err != nil {
		t.Fatalf("IntersectEgress: %v", err)
	}
	want := []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{443}}}
	if len(got) != 1 || got[0].CIDR != want[0].CIDR || len(got[0].Ports) != 1 || got[0].Ports[0] != 443 {
		t.Fatalf("intersection=%#v, want %#v", got, want)
	}

	if _, err := sandboxpolicy.IntersectEgress(
		[]v1beta1.DeepAgentsEgressRule{{CIDR: "not-a-cidr", Ports: []int32{443}}}, ceilings,
	); err == nil {
		t.Fatal("invalid requested CIDR was accepted")
	}
	if _, err := sandboxpolicy.IntersectEgress(
		[]v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{70000}}}, ceilings,
	); err == nil {
		t.Fatal("invalid requested port was accepted")
	}
}

func TestExecutionSandboxOwnershipPredicatesRejectForeignEvents(t *testing.T) {
	mine := &v1beta1.ExecutionSandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "mine", Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
	}}
	foreign := &v1beta1.ExecutionSandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign", Labels: map[string]string{v1beta1.LabelController: "ctl-b"},
	}}
	manual := &v1beta1.ExecutionSandbox{ObjectMeta: metav1.ObjectMeta{Name: "manual"}}
	predicate := ExecutionSandboxLifecyclePredicates("ctl-a")
	if !predicate.Create(event.CreateEvent{Object: mine}) ||
		!predicate.Update(event.UpdateEvent{ObjectOld: mine.DeepCopy(), ObjectNew: mine}) ||
		!predicate.Delete(event.DeleteEvent{Object: mine}) {
		t.Fatal("sandbox predicate rejected an owned event")
	}
	for _, object := range []client.Object{foreign, manual} {
		if predicate.Create(event.CreateEvent{Object: object}) ||
			predicate.Update(event.UpdateEvent{ObjectOld: object.DeepCopyObject().(client.Object), ObjectNew: object}) ||
			predicate.Delete(event.DeleteEvent{Object: object}) {
			t.Fatalf("sandbox predicate accepted foreign object %#v", object.GetLabels())
		}
	}

	childPredicate := ExecutionSandboxChildPredicates("ctl-a")
	ownedService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		v1beta1.LabelController: "ctl-a", v1beta1.LabelExecutionSandbox: "mine",
	}}}
	foreignPolicy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		v1beta1.LabelController: "ctl-b", v1beta1.LabelExecutionSandbox: "foreign",
	}}}
	if !childPredicate.Create(event.CreateEvent{Object: ownedService}) {
		t.Fatal("child predicate rejected owned Service")
	}
	if childPredicate.Create(event.CreateEvent{Object: foreignPolicy}) ||
		childPredicate.Create(event.CreateEvent{Object: &corev1.Service{}}) {
		t.Fatal("child predicate accepted foreign or unlabelled child")
	}
}

func TestExecutionSandboxReconcilerIgnoresForeignAndManualSandboxes(t *testing.T) {
	for _, labels := range []map[string]string{
		{v1beta1.LabelController: "ctl-b"},
		nil,
	} {
		t.Run(fmt.Sprintf("labels-%v", labels), func(t *testing.T) {
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
			sandbox := &v1beta1.ExecutionSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "agentteams-system", Labels: labels},
				Spec: v1beta1.ExecutionSandboxSpec{
					WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: "missing", UID: "missing"},
					SessionID: "thread", IdleTimeout: "500ms", MaxLifetime: "8h",
				},
			}
			before := sandbox.DeepCopy()
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1beta1.ExecutionSandbox{}).WithObjects(sandbox).Build()
			r := &ExecutionSandboxReconciler{Client: cl, RunnerImage: "runner:v1", ControllerName: "ctl-a", DefaultRuntime: "deepagents"}

			result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)})
			if err != nil || result != (reconcile.Result{}) {
				t.Fatalf("Reconcile foreign sandbox=(%#v, %v), want zero result", result, err)
			}
			var stored v1beta1.ExecutionSandbox
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(sandbox), &stored); err != nil {
				t.Fatal(err)
			}
			if !apiequality.Semantic.DeepEqual(stored.Spec, before.Spec) || !apiequality.Semantic.DeepEqual(stored.Status, before.Status) {
				t.Fatalf("foreign sandbox was mutated: %#v", stored)
			}
			for _, child := range []client.Object{&corev1.Secret{}, &corev1.Pod{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}} {
				if err := cl.Get(context.Background(), client.ObjectKeyFromObject(sandbox), child); !apierrors.IsNotFound(err) {
					t.Fatalf("foreign sandbox created %T: %v", child, err)
				}
			}
		})
	}
}

func TestExecutionSandboxReconcilerRevokesStaleWorkerBindings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1beta1.Worker, *v1beta1.ExecutionSandbox) bool
	}{
		{
			name: "Worker NotFound",
			mutate: func(*v1beta1.Worker, *v1beta1.ExecutionSandbox) bool {
				return false
			},
		},
		{
			name: "Worker controller changed",
			mutate: func(worker *v1beta1.Worker, _ *v1beta1.ExecutionSandbox) bool {
				worker.Labels[v1beta1.LabelController] = "ctl-b"
				return true
			},
		},
		{
			name: "Worker UID changed",
			mutate: func(worker *v1beta1.Worker, _ *v1beta1.ExecutionSandbox) bool {
				worker.UID = "replacement-worker-uid"
				return true
			},
		},
		{
			name: "effective runtime changed",
			mutate: func(worker *v1beta1.Worker, _ *v1beta1.ExecutionSandbox) bool {
				worker.Spec.Runtime = "openclaw"
				return true
			},
		},
		{
			name: "DeepAgents config removed",
			mutate: func(worker *v1beta1.Worker, _ *v1beta1.ExecutionSandbox) bool {
				worker.Spec.RuntimeConfig = nil
				return true
			},
		},
		{
			name: "execution disabled",
			mutate: func(worker *v1beta1.Worker, _ *v1beta1.ExecutionSandbox) bool {
				worker.Spec.RuntimeConfig.DeepAgents.Execution.Mode = "disabled"
				return true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme, worker, sandbox, children := newReadyExecutionSandboxRevokeFixture(t)
			objects := []client.Object{sandbox}
			if tt.mutate(worker, sandbox) {
				objects = append(objects, worker)
			}
			objects = append(objects, children...)
			base := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&v1beta1.ExecutionSandbox{}).
				WithObjects(objects...).
				Build()
			allowPodDisappearance := false
			creates := 0
			var deleteOrder []string
			podDeleteRequested := false
			cl := interceptor.NewClient(base, interceptor.Funcs{
				Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					creates++
					return underlying.Create(ctx, object, opts...)
				},
				Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					switch object.(type) {
					case *corev1.Service:
						deleteOrder = append(deleteOrder, "Service")
					case *corev1.Secret:
						deleteOrder = append(deleteOrder, "Secret")
					case *corev1.Pod:
						deleteOrder = append(deleteOrder, "Pod")
						podDeleteRequested = true
						if !allowPodDisappearance {
							return nil
						}
					case *networkingv1.NetworkPolicy:
						deleteOrder = append(deleteOrder, "NetworkPolicy")
					case *v1beta1.ExecutionSandbox:
						deleteOrder = append(deleteOrder, "ExecutionSandbox")
					}
					return underlying.Delete(ctx, object, opts...)
				},
				Get: func(ctx context.Context, underlying client.WithWatch, objectKey client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					err := underlying.Get(ctx, objectKey, object, opts...)
					if livePod, ok := object.(*corev1.Pod); ok && err == nil && podDeleteRequested && !allowPodDisappearance {
						terminating := metav1.NewTime(time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC))
						livePod.DeletionTimestamp = &terminating
					}
					return err
				},
			})
			r := &ExecutionSandboxReconciler{
				Client: cl, RunnerImage: "runner:v1", ControllerName: "ctl-a", DefaultRuntime: "deepagents",
			}
			key := client.ObjectKeyFromObject(sandbox)

			result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			if err != nil || result.RequeueAfter != executionSandboxRequeue {
				t.Fatalf("first revoke result=%#v err=%v, want five-second Pod drain requeue", result, err)
			}
			if got, want := strings.Join(deleteOrder, ","), "Service,Secret,Pod"; got != want {
				t.Fatalf("first revoke delete order=%q, want %q", got, want)
			}
			for _, object := range []client.Object{&corev1.Service{}, &corev1.Secret{}} {
				if err := base.Get(context.Background(), key, object); !apierrors.IsNotFound(err) {
					t.Fatalf("first revoke retained %T: %v", object, err)
				}
			}
			for _, object := range []client.Object{&corev1.Pod{}, &networkingv1.NetworkPolicy{}, &v1beta1.ExecutionSandbox{}} {
				if err := base.Get(context.Background(), key, object); err != nil {
					t.Fatalf("first revoke removed %T before Pod disappearance: %v", object, err)
				}
			}
			if creates != 0 {
				t.Fatalf("revoke path created %d replacement children", creates)
			}

			allowPodDisappearance = true
			deleteOrder = nil
			result, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			if err != nil || result != (reconcile.Result{}) {
				t.Fatalf("second revoke result=%#v err=%v, want completed deletion", result, err)
			}
			if got, want := strings.Join(deleteOrder, ","), "Service,Secret,Pod,NetworkPolicy,ExecutionSandbox"; got != want {
				t.Fatalf("second revoke delete order=%q, want %q", got, want)
			}
			for _, object := range []client.Object{&corev1.Pod{}, &networkingv1.NetworkPolicy{}, &v1beta1.ExecutionSandbox{}} {
				if err := base.Get(context.Background(), key, object); !apierrors.IsNotFound(err) {
					t.Fatalf("completed revoke retained %T: %v", object, err)
				}
			}

			result, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			if err != nil || result != (reconcile.Result{}) {
				t.Fatalf("idempotent revoke result=%#v err=%v", result, err)
			}
			if creates != 0 {
				t.Fatalf("idempotent revoke created %d replacement children", creates)
			}
		})
	}
}

func TestExecutionSandboxReconcilerRevokeContainsAPIErrors(t *testing.T) {
	tests := []struct {
		name         string
		failurePoint string
	}{
		{name: "Service delete", failurePoint: "Service"},
		{name: "Secret delete", failurePoint: "Secret"},
		{name: "Pod delete", failurePoint: "Pod"},
		{name: "Pod deletion observation", failurePoint: "PodGet"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme, worker, sandbox, children := newReadyExecutionSandboxRevokeFixture(t)
			worker.Labels[v1beta1.LabelController] = "ctl-b"
			objects := []client.Object{worker, sandbox}
			objects = append(objects, children...)
			base := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&v1beta1.ExecutionSandbox{}).
				WithObjects(objects...).
				Build()
			attempts := map[string]int{}
			creates := 0
			podDeleteAttempted := false
			cl := interceptor.NewClient(base, interceptor.Funcs{
				Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					creates++
					return underlying.Create(ctx, object, opts...)
				},
				Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					kind := ""
					switch object.(type) {
					case *corev1.Service:
						kind = "Service"
					case *corev1.Secret:
						kind = "Secret"
					case *corev1.Pod:
						kind = "Pod"
						podDeleteAttempted = true
					case *networkingv1.NetworkPolicy:
						kind = "NetworkPolicy"
					case *v1beta1.ExecutionSandbox:
						kind = "ExecutionSandbox"
					}
					attempts[kind]++
					if kind == tt.failurePoint {
						return fmt.Errorf("injected %s failure", kind)
					}
					if kind == "Pod" && tt.failurePoint == "PodGet" {
						return nil
					}
					return underlying.Delete(ctx, object, opts...)
				},
				Get: func(ctx context.Context, underlying client.WithWatch, objectKey client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*corev1.Pod); ok && podDeleteAttempted {
						attempts["PodGet"]++
						if tt.failurePoint == "PodGet" {
							return errors.New("injected PodGet failure")
						}
					}
					return underlying.Get(ctx, objectKey, object, opts...)
				},
			})
			r := &ExecutionSandboxReconciler{
				Client: cl, RunnerImage: "runner:v1", ControllerName: "ctl-a", DefaultRuntime: "deepagents",
			}
			key := client.ObjectKeyFromObject(sandbox)

			result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			if err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("revoke error=%v, want injected non-NotFound error", err)
			}
			if result != (reconcile.Result{}) {
				t.Fatalf("revoke API failure result=%#v, want error-driven retry", result)
			}
			for _, action := range []string{"Service", "Secret", "Pod", "PodGet"} {
				if attempts[action] == 0 {
					t.Fatalf("%s failure skipped independent revoke action %s: %#v", tt.name, action, attempts)
				}
			}
			if attempts["NetworkPolicy"] != 0 || attempts["ExecutionSandbox"] != 0 {
				t.Fatalf("%s failure crossed isolation deletion boundary: %#v", tt.name, attempts)
			}
			for _, object := range []client.Object{&networkingv1.NetworkPolicy{}, &v1beta1.ExecutionSandbox{}} {
				if getErr := base.Get(context.Background(), key, object); getErr != nil {
					t.Fatalf("%s failure removed %T: %v", tt.name, object, getErr)
				}
			}
			if creates != 0 {
				t.Fatalf("%s failure created %d replacement children", tt.name, creates)
			}
		})
	}
}

func TestExecutionSandboxWorkerWatchMapsOnlyOwnedRelatedSandboxes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{
		&v1beta1.ExecutionSandbox{ObjectMeta: metav1.ObjectMeta{
			Name: "matching", Namespace: "ns-a", Labels: map[string]string{
				v1beta1.LabelController: "ctl-a", v1beta1.LabelWorker: "researcher",
			},
		}},
		&v1beta1.ExecutionSandbox{ObjectMeta: metav1.ObjectMeta{
			Name: "other-namespace", Namespace: "ns-b", Labels: map[string]string{
				v1beta1.LabelController: "ctl-a", v1beta1.LabelWorker: "researcher",
			},
		}},
		&v1beta1.ExecutionSandbox{ObjectMeta: metav1.ObjectMeta{
			Name: "foreign-controller", Namespace: "ns-a", Labels: map[string]string{
				v1beta1.LabelController: "ctl-b", v1beta1.LabelWorker: "researcher",
			},
		}},
		&v1beta1.ExecutionSandbox{ObjectMeta: metav1.ObjectMeta{
			Name: "other-worker", Namespace: "ns-a", Labels: map[string]string{
				v1beta1.LabelController: "ctl-a", v1beta1.LabelWorker: "writer",
			},
		}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &ExecutionSandboxReconciler{Client: cl, ControllerName: "ctl-a"}
	worker := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{Name: "researcher", Namespace: "ns-a"}}

	requests := r.executionSandboxesForWorker(context.Background(), worker)
	if len(requests) != 1 || requests[0].NamespacedName != (types.NamespacedName{Name: "matching", Namespace: "ns-a"}) {
		t.Fatalf("worker map requests=%#v, want only ns-a/matching", requests)
	}
}

func TestExecutionSandboxWorkerWatchPredicatesPreserveRevocationEvents(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sandbox := &v1beta1.ExecutionSandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "exec", Namespace: "ns-a", Labels: map[string]string{
			v1beta1.LabelController: "ctl-a", v1beta1.LabelWorker: "researcher",
		},
	}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	r := &ExecutionSandboxReconciler{Client: cl, ControllerName: "ctl-a"}
	predicates := ExecutionSandboxWorkerPredicates("ctl-a")
	owned := &v1beta1.Worker{ObjectMeta: metav1.ObjectMeta{
		Name: "researcher", Namespace: "ns-a", Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
	}}
	foreign := owned.DeepCopy()
	foreign.Labels[v1beta1.LabelController] = "ctl-b"
	unrelated := owned.DeepCopy()
	unrelated.Name = "writer"
	unlabelled := owned.DeepCopy()
	unlabelled.Labels = nil

	assertOneRequest := func(name string, accepted bool, object client.Object) {
		t.Helper()
		if !accepted {
			t.Fatalf("%s event was rejected", name)
		}
		requests := r.executionSandboxesForWorker(context.Background(), object)
		if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(sandbox) {
			t.Fatalf("%s requests=%#v, want sandbox %s", name, requests, client.ObjectKeyFromObject(sandbox))
		}
	}
	assertOneRequest("create", predicates.Create(event.CreateEvent{Object: owned}), owned)
	assertOneRequest("delete", predicates.Delete(event.DeleteEvent{Object: owned}), owned)
	assertOneRequest(
		"controller label removed",
		predicates.Update(event.UpdateEvent{ObjectOld: owned, ObjectNew: foreign}),
		foreign,
	)
	assertOneRequest(
		"controller label added",
		predicates.Update(event.UpdateEvent{ObjectOld: foreign, ObjectNew: owned}),
		owned,
	)

	if !predicates.Create(event.CreateEvent{Object: unrelated}) {
		t.Fatal("owned unrelated Worker create should reach the scoped mapper")
	}
	if requests := r.executionSandboxesForWorker(context.Background(), unrelated); len(requests) != 0 {
		t.Fatalf("unrelated Worker enqueued sandboxes: %#v", requests)
	}
	if predicates.Create(event.CreateEvent{Object: foreign}) ||
		predicates.Delete(event.DeleteEvent{Object: foreign}) ||
		predicates.Create(event.CreateEvent{Object: unlabelled}) {
		t.Fatal("foreign or unlabelled Worker event passed current-controller predicate")
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
	if container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet == nil ||
		container.ReadinessProbe.HTTPGet.Path != "/healthz" ||
		container.ReadinessProbe.HTTPGet.Port.StrVal != "http" {
		t.Fatalf("runner readiness probe is not tied to its health endpoint: %#v", container.ReadinessProbe)
	}
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

func TestExecutionSandboxReconcilerConvergesInvalidComputeResources(t *testing.T) {
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
		Spec: v1beta1.WorkerSpec{
			Model: "qwen-max",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
			}},
		},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "exec-invalid-resources", Namespace: "agentteams-system", UID: "sandbox-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: "researcher", UID: "worker-uid"},
			SessionID: "thread-hash",
			Resources: &v1beta1.ExecutionSandboxResourceRequirements{
				Limits: v1beta1.ExecutionSandboxResourceValues{Memory: "-1Mi"},
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
	resourceVersion := updated.ResourceVersion
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("repeat Reconcile invalid resources: %v", err)
	}
	if err := cl.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get repeated invalid sandbox: %v", err)
	}
	if updated.ResourceVersion != resourceVersion {
		t.Fatalf("unchanged invalid status was rewritten: resourceVersion %q -> %q", resourceVersion, updated.ResourceVersion)
	}
	for _, object := range []client.Object{&corev1.Secret{}, &corev1.Pod{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}} {
		object.SetName(key.Name)
		object.SetNamespace(key.Namespace)
		if err := cl.Get(context.Background(), key, object); !apierrors.IsNotFound(err) {
			t.Fatalf("repeat invalid sandbox left %T behind: %v", object, err)
		}
	}
}

func TestExecutionSandboxReconcilerConvergesInvalidPolicies(t *testing.T) {
	tests := []struct {
		name       string
		mutateSpec func(*v1beta1.ExecutionSandboxSpec)
		ceilings   []v1beta1.DeepAgentsEgressRule
	}{
		{
			name: "idle timeout",
			mutateSpec: func(spec *v1beta1.ExecutionSandboxSpec) {
				spec.IdleTimeout = "not-a-duration"
			},
		},
		{
			name: "max lifetime",
			mutateSpec: func(spec *v1beta1.ExecutionSandboxSpec) {
				spec.MaxLifetime = "0s"
			},
		},
		{
			name: "subsecond idle timeout",
			mutateSpec: func(spec *v1beta1.ExecutionSandboxSpec) {
				spec.IdleTimeout = "500ms"
			},
		},
		{
			name: "non-runtime max lifetime syntax",
			mutateSpec: func(spec *v1beta1.ExecutionSandboxSpec) {
				spec.MaxLifetime = "1000ms"
			},
		},
		{
			name: "requested egress CIDR",
			mutateSpec: func(spec *v1beta1.ExecutionSandboxSpec) {
				spec.Egress[0].CIDR = "not-a-cidr"
			},
			ceilings: []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.0/12", Ports: []int32{443}}},
		},
		{
			name: "requested egress protocol",
			mutateSpec: func(spec *v1beta1.ExecutionSandboxSpec) {
				spec.Egress[0].Protocol = "ICMP"
			},
			ceilings: []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.0/12", Ports: []int32{443}}},
		},
		{
			name: "requested egress port",
			mutateSpec: func(spec *v1beta1.ExecutionSandboxSpec) {
				spec.Egress[0].Ports = []int32{0}
			},
			ceilings: []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.0/12", Ports: []int32{443}}},
		},
		{
			name:       "configured egress ceiling CIDR",
			mutateSpec: func(*v1beta1.ExecutionSandboxSpec) {},
			ceilings:   []v1beta1.DeepAgentsEgressRule{{CIDR: "invalid-ceiling", Ports: []int32{443}}},
		},
		{
			name:       "configured egress ceiling protocol",
			mutateSpec: func(*v1beta1.ExecutionSandboxSpec) {},
			ceilings:   []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.0/12", Protocol: "ICMP", Ports: []int32{443}}},
		},
		{
			name:       "configured egress ceiling port",
			mutateSpec: func(*v1beta1.ExecutionSandboxSpec) {},
			ceilings:   []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.0/12", Ports: []int32{65536}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
				ObjectMeta: metav1.ObjectMeta{
					Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid",
					Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
				},
				Spec: v1beta1.WorkerSpec{Runtime: "deepagents", RuntimeConfig: &v1beta1.WorkerRuntimeConfig{
					DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"}},
				}},
			}
			expiresAt := metav1.NewTime(time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC))
			sandbox := &v1beta1.ExecutionSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name: "exec-invalid-policy", Namespace: "agentteams-system", UID: "sandbox-uid", Generation: 2,
					Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
				},
				Spec: v1beta1.ExecutionSandboxSpec{
					WorkerRef:   v1beta1.ExecutionSandboxWorkerRef{Name: worker.Name, UID: string(worker.UID)},
					SessionID:   "thread-hash",
					IdleTimeout: "30m",
					MaxLifetime: "8h",
					Egress:      []v1beta1.DeepAgentsEgressRule{{CIDR: "10.96.0.10/32", Ports: []int32{443}}},
				},
				Status: v1beta1.ExecutionSandboxStatus{
					ObservedGeneration: 1,
					Phase:              "Ready",
					Endpoint:           "http://exec-invalid-policy.agentteams-system.svc:8080",
					PodName:            "exec-invalid-policy",
					ExpiresAt:          &expiresAt,
				},
			}
			tt.mutateSpec(&sandbox.Spec)
			key := types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}
			children := []client.Object{
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}},
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}},
				&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}},
				&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}},
			}
			objects := []client.Object{worker, sandbox}
			objects = append(objects, children...)
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&v1beta1.ExecutionSandbox{}).
				WithObjects(objects...).
				Build()
			r := &ExecutionSandboxReconciler{
				Client:         cl,
				RunnerImage:    "runner:v1",
				ControllerName: "ctl-a",
				DefaultRuntime: "deepagents",
				EgressCeilings: tt.ceilings,
			}

			if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile invalid policy: %v", err)
			}
			for _, object := range children {
				probe := object.DeepCopyObject().(client.Object)
				if err := cl.Get(context.Background(), key, probe); !apierrors.IsNotFound(err) {
					t.Fatalf("invalid policy left %T behind: %v", object, err)
				}
			}
			var updated v1beta1.ExecutionSandbox
			if err := cl.Get(context.Background(), key, &updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.ObservedGeneration != updated.Generation || updated.Status.Phase != "Failed" ||
				updated.Status.Endpoint != "" || updated.Status.PodName != "" || updated.Status.ExpiresAt != nil ||
				sandboxReadyReason(updated.Status.Conditions) != "InvalidPolicy" {
				t.Fatalf("invalid policy status=%#v", updated.Status)
			}
			ready := apiMeta.FindStatusCondition(updated.Status.Conditions, "Ready")
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.ObservedGeneration != updated.Generation || ready.Message == "" {
				t.Fatalf("invalid policy Ready condition=%#v", ready)
			}
			resourceVersion := updated.ResourceVersion
			if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
				t.Fatalf("repeat Reconcile invalid policy: %v", err)
			}
			if err := cl.Get(context.Background(), key, &updated); err != nil {
				t.Fatal(err)
			}
			if updated.ResourceVersion != resourceVersion {
				t.Fatalf("unchanged invalid policy status was rewritten: %q -> %q", resourceVersion, updated.ResourceVersion)
			}
			for _, object := range children {
				probe := object.DeepCopyObject().(client.Object)
				if err := cl.Get(context.Background(), key, probe); !apierrors.IsNotFound(err) {
					t.Fatalf("repeat invalid policy left %T behind: %v", object, err)
				}
			}
		})
	}
}

func TestExecutionSandboxReconcilerKeepsTerminatingRunnerIsolated(t *testing.T) {
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
		Spec: v1beta1.WorkerSpec{Runtime: "deepagents", RuntimeConfig: &v1beta1.WorkerRuntimeConfig{
			DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"}},
		}},
	}
	expiresAt := metav1.NewTime(time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC))
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "exec-terminating", Namespace: "agentteams-system", UID: "sandbox-uid", Generation: 2,
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef:   v1beta1.ExecutionSandboxWorkerRef{Name: worker.Name, UID: string(worker.UID)},
			SessionID:   "thread-hash",
			IdleTimeout: "invalid",
			MaxLifetime: "8h",
		},
		Status: v1beta1.ExecutionSandboxStatus{
			ObservedGeneration: 1,
			Phase:              "Ready",
			Endpoint:           "http://exec-terminating.agentteams-system.svc:8080",
			PodName:            "exec-terminating",
			ExpiresAt:          &expiresAt,
		},
	}
	key := types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Data: map[string][]byte{"token": []byte("capability")}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: key.Name, Namespace: key.Namespace,
		Labels: map[string]string{v1beta1.LabelExecutionSandbox: sandbox.Name},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{v1beta1.LabelExecutionSandbox: sandbox.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
		},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1beta1.ExecutionSandbox{}).
		WithObjects(worker, sandbox, secret, pod, service, policy).
		Build()
	podDeleteRequested := false
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
			if _, ok := object.(*corev1.Pod); ok {
				podDeleteRequested = true
				return nil
			}
			return underlying.Delete(ctx, object, opts...)
		},
		Get: func(ctx context.Context, underlying client.WithWatch, objectKey client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			err := underlying.Get(ctx, objectKey, object, opts...)
			if livePod, ok := object.(*corev1.Pod); ok && err == nil && podDeleteRequested {
				terminating := metav1.NewTime(time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC))
				livePod.DeletionTimestamp = &terminating
			}
			return err
		},
	})
	r := &ExecutionSandboxReconciler{
		Client: cl, RunnerImage: "runner:v1", ControllerName: "ctl-a", DefaultRuntime: "deepagents",
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile terminating Pod: %v", err)
	}
	if result.RequeueAfter != executionSandboxRequeue || !podDeleteRequested {
		t.Fatalf("result=%#v deleteRequested=%v, want terminating requeue", result, podDeleteRequested)
	}
	if err := base.Get(context.Background(), key, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Service still routes to invalid Runner: %v", err)
	}
	var isolated networkingv1.NetworkPolicy
	if err := base.Get(context.Background(), key, &isolated); err != nil {
		t.Fatalf("default-deny NetworkPolicy missing: %v", err)
	}
	if len(isolated.Spec.Ingress) != 0 || len(isolated.Spec.Egress) != 0 || len(isolated.Spec.PolicyTypes) != 2 ||
		isolated.Spec.PodSelector.MatchLabels[v1beta1.LabelExecutionSandbox] != sandbox.Name ||
		!metav1.IsControlledBy(&isolated, sandbox) {
		t.Fatalf("terminating Runner policy is not owned default-deny: %#v", isolated)
	}
	if err := base.Get(context.Background(), key, &corev1.Secret{}); err != nil {
		t.Fatalf("capability Secret removed before Pod absence: %v", err)
	}
	var failed v1beta1.ExecutionSandbox
	if err := base.Get(context.Background(), key, &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status.Phase != "Failed" || failed.Status.Endpoint != "" || failed.Status.PodName != "" || failed.Status.ExpiresAt != nil ||
		sandboxReadyReason(failed.Status.Conditions) != "InvalidPolicy" {
		t.Fatalf("terminating invalid sandbox status=%#v", failed.Status)
	}

	if err := base.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}); err != nil {
		t.Fatalf("simulate Pod disappearance: %v", err)
	}
	result, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("Reconcile absent Pod result=%#v err=%v", result, err)
	}
	for _, object := range []client.Object{&corev1.Secret{}, &networkingv1.NetworkPolicy{}} {
		if err := base.Get(context.Background(), key, object); !apierrors.IsNotFound(err) {
			t.Fatalf("absent Pod left %T behind: %v", object, err)
		}
	}
	if err := base.Get(context.Background(), key, &failed); err != nil {
		t.Fatal(err)
	}
	resourceVersion := failed.ResourceVersion
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("stable invalid reconcile: %v", err)
	}
	if err := base.Get(context.Background(), key, &failed); err != nil {
		t.Fatal(err)
	}
	if failed.ResourceVersion != resourceVersion {
		t.Fatalf("stable invalid status rewritten: %q -> %q", resourceVersion, failed.ResourceVersion)
	}
}

func TestExecutionSandboxReconcilerAttemptsAllContainmentAfterAPIFailures(t *testing.T) {
	tests := []struct {
		name              string
		failServiceDelete bool
		failPodGet        bool
		failPolicyUpdate  bool
		failStatusUpdate  bool
		failPodDelete     bool
	}{
		{name: "Service delete", failServiceDelete: true},
		{name: "Pod Get", failPodGet: true},
		{name: "NetworkPolicy ensure", failPolicyUpdate: true},
		{name: "status update", failStatusUpdate: true},
		{name: "Pod delete", failPodDelete: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
				ObjectMeta: metav1.ObjectMeta{
					Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid",
					Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
				},
				Spec: v1beta1.WorkerSpec{Runtime: "deepagents", RuntimeConfig: &v1beta1.WorkerRuntimeConfig{
					DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"}},
				}},
			}
			sandbox := &v1beta1.ExecutionSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name: "exec-failure", Namespace: "agentteams-system", UID: "sandbox-uid", Generation: 2,
					Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
				},
				Spec: v1beta1.ExecutionSandboxSpec{
					WorkerRef: v1beta1.ExecutionSandboxWorkerRef{Name: worker.Name, UID: string(worker.UID)},
					SessionID: "thread-hash", IdleTimeout: "invalid", MaxLifetime: "8h",
				},
				Status: v1beta1.ExecutionSandboxStatus{ObservedGeneration: 1, Phase: "Ready", Endpoint: "http://runner", PodName: "exec-failure"},
			}
			key := types.NamespacedName{Name: sandbox.Name, Namespace: sandbox.Namespace}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Data: map[string][]byte{"token": []byte("capability")}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{
				v1beta1.LabelExecutionSandbox: sandbox.Name,
			}}}
			service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
			policy := &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{v1beta1.LabelExecutionSandbox: sandbox.Name}},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
					Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
				},
			}
			base := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&v1beta1.ExecutionSandbox{}).
				WithObjects(worker, sandbox, secret, pod, service, policy).
				Build()
			attempts := map[string]int{}
			cl := interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, underlying client.WithWatch, objectKey client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*corev1.Pod); ok {
						attempts["pod-get"]++
						if tt.failPodGet {
							return errors.New("injected Pod Get failure")
						}
					}
					return underlying.Get(ctx, objectKey, object, opts...)
				},
				Update: func(ctx context.Context, underlying client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					if _, ok := object.(*networkingv1.NetworkPolicy); ok {
						attempts["policy-ensure"]++
						if tt.failPolicyUpdate {
							return errors.New("injected NetworkPolicy update failure")
						}
					}
					return underlying.Update(ctx, object, opts...)
				},
				Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					switch object.(type) {
					case *corev1.Service:
						attempts["service-delete"]++
						if tt.failServiceDelete {
							return errors.New("injected Service delete failure")
						}
						return underlying.Delete(ctx, object, opts...)
					case *corev1.Pod:
						attempts["pod-delete"]++
						if tt.failPodDelete {
							return errors.New("injected Pod delete failure")
						}
						// Hold the Pod so capability cleanup cannot be justified.
						return nil
					default:
						return underlying.Delete(ctx, object, opts...)
					}
				},
				SubResourceUpdate: func(ctx context.Context, underlying client.Client, subResourceName string, object client.Object, opts ...client.SubResourceUpdateOption) error {
					if subResourceName == "status" {
						attempts["status-update"]++
						if tt.failStatusUpdate {
							return errors.New("injected status update failure")
						}
					}
					return underlying.SubResource(subResourceName).Update(ctx, object, opts...)
				},
			})
			r := &ExecutionSandboxReconciler{Client: cl, RunnerImage: "runner:v1", ControllerName: "ctl-a", DefaultRuntime: "deepagents"}

			var logOutput bytes.Buffer
			ctx := log.IntoContext(context.Background(), captureLogger(&logOutput))
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			if err != nil || result.RequeueAfter != executionSandboxRequeue {
				t.Fatalf("result=%#v err=%v, want fixed containment requeue without error", result, err)
			}
			if !strings.Contains(logOutput.String(), "sandbox containment remains incomplete") ||
				!strings.Contains(logOutput.String(), sandbox.Namespace) || !strings.Contains(logOutput.String(), sandbox.Name) {
				t.Fatalf("containment error log lacks identity/context: %q", logOutput.String())
			}
			for _, action := range []string{"policy-ensure", "status-update", "service-delete", "pod-delete"} {
				if attempts[action] == 0 {
					t.Fatalf("%s failure skipped independent containment action %s: %#v", tt.name, action, attempts)
				}
			}
			if attempts["pod-get"] == 0 {
				t.Fatalf("%s failure skipped Pod absence observation", tt.name)
			}
			if !tt.failStatusUpdate {
				var failed v1beta1.ExecutionSandbox
				if getErr := base.Get(context.Background(), key, &failed); getErr != nil {
					t.Fatal(getErr)
				}
				if failed.Status.Phase != "Failed" || failed.Status.Endpoint != "" || sandboxReadyReason(failed.Status.Conditions) != "InvalidPolicy" {
					t.Fatalf("working status API did not fail closed: %#v", failed.Status)
				}
			}
			var containedPolicy networkingv1.NetworkPolicy
			if getErr := base.Get(context.Background(), key, &containedPolicy); getErr != nil {
				t.Fatalf("capability NetworkPolicy was not retained: %v", getErr)
			}
			if !tt.failPolicyUpdate && (len(containedPolicy.Spec.Ingress) != 0 || len(containedPolicy.Spec.Egress) != 0 ||
				len(containedPolicy.Spec.PolicyTypes) != 2) {
				t.Fatalf("working policy API did not establish default-deny: %#v", containedPolicy.Spec)
			}
			serviceErr := base.Get(context.Background(), key, &corev1.Service{})
			if tt.failServiceDelete {
				if serviceErr != nil {
					t.Fatalf("injected Service delete did not retain Service: %v", serviceErr)
				}
			} else if !apierrors.IsNotFound(serviceErr) {
				t.Fatalf("working Service delete did not cut routing: %v", serviceErr)
			}
			for _, object := range []client.Object{&corev1.Secret{}, &networkingv1.NetworkPolicy{}} {
				if getErr := base.Get(context.Background(), key, object); getErr != nil {
					t.Fatalf("unknown/present Pod lost capability material %T: %v", object, getErr)
				}
			}
		})
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
		Spec: v1beta1.WorkerSpec{
			Model: "qwen-max",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
			}},
		},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "exec-abc", Namespace: "agentteams-system", UID: "sandbox-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
		Spec: v1beta1.WorkerSpec{
			Model: "qwen-max",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
			}},
		},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "exec-policy-refresh", Namespace: "agentteams-system", UID: "sandbox-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
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
			Labels:            map[string]string{v1beta1.LabelController: "ctl-a"},
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

func newReadyExecutionSandboxRevokeFixture(
	t *testing.T,
) (*runtime.Scheme, *v1beta1.Worker, *v1beta1.ExecutionSandbox, []client.Object) {
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "researcher", Namespace: "agentteams-system", UID: "worker-uid",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
		Spec: v1beta1.WorkerSpec{
			Runtime: "deepagents",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox"},
			}},
		},
	}
	sandbox := &v1beta1.ExecutionSandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "exec-ready", Namespace: worker.Namespace, UID: "sandbox-uid",
			Labels: map[string]string{
				v1beta1.LabelController: "ctl-a",
				v1beta1.LabelWorker:     worker.Name,
			},
		},
		Spec: v1beta1.ExecutionSandboxSpec{
			WorkerRef:   v1beta1.ExecutionSandboxWorkerRef{Name: worker.Name, UID: string(worker.UID)},
			SessionID:   "thread-hash",
			Image:       "runner:v1",
			IdleTimeout: "30m",
			MaxLifetime: "8h",
		},
		Status: v1beta1.ExecutionSandboxStatus{
			ObservedGeneration: 1,
			Phase:              "Ready",
			PodName:            "exec-ready",
			Endpoint:           "http://exec-ready.agentteams-system.svc:8080",
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "PodReady",
			}},
		},
	}
	key := client.ObjectKeyFromObject(sandbox)
	immutable := true
	labels := map[string]string{
		v1beta1.LabelController:       "ctl-a",
		v1beta1.LabelWorker:           worker.Name,
		v1beta1.LabelExecutionSandbox: sandbox.Name,
	}
	children := []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
			Immutable:  &immutable,
			Data:       map[string][]byte{"token": []byte("01234567890123456789012345678901")},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionTrue,
				}},
			},
		},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels}},
	}
	return scheme, worker, sandbox, children
}

func TestExecutionSandboxReconcilerRevokesNonDeepAgentsWorker(t *testing.T) {
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

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{
		Name: "exec-invalid", Namespace: "default",
	}})
	if err != nil || result != (reconcile.Result{}) {
		t.Fatalf("Reconcile stale runtime=(%#v, %v), want completed revoke", result, err)
	}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(sandbox), &v1beta1.ExecutionSandbox{}); !apierrors.IsNotFound(err) {
		t.Fatalf("non-DeepAgents ExecutionSandbox was not revoked: %v", err)
	}
}
