//go:build integration

package controller_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	controllerpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/sandboxpolicy"
	serverpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/server"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/test/testutil/fixtures"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestExecutionSandboxAPIServerFinalizerRetainsIsolationUntilTerminatingPodIsAbsent(t *testing.T) {
	worker, sandbox := createExecutionSandboxThroughHTTP(t, "lifecycle")
	t.Cleanup(func() {
		cleanupExecutionSandboxAPIObjects(t, sandbox)
		cleanupWorkerAPIObject(t, worker)
	})
	createExecutionSandboxAPIChildren(t, sandbox, true)

	staleUID := sandbox.UID
	staleResourceVersion := sandbox.ResourceVersion
	if err := k8sClient.Delete(ctx, sandbox, client.Preconditions{
		UID: &staleUID, ResourceVersion: &staleResourceVersion,
	}); err != nil {
		t.Fatalf("request ExecutionSandbox deletion: %v", err)
	}
	assertEventually(t, func() error {
		var deleting v1beta1.ExecutionSandbox
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sandbox), &deleting); err != nil {
			return err
		}
		if deleting.DeletionTimestamp.IsZero() || !hasString(deleting.Finalizers, v1beta1.ExecutionSandboxCleanupFinalizer) {
			return &lifecycleAssertionError{message: "sandbox is not retained by cleanup finalizer"}
		}
		return nil
	})

	reconciler := &controllerpkg.ExecutionSandboxReconciler{
		Client: k8sClient, APIReader: k8sClient, ControllerName: "ctl-a",
	}
	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)})
	if err != nil || result.RequeueAfter != 5*time.Second {
		t.Fatalf("first cleanup result=%#v err=%v, want terminating-Pod requeue", result, err)
	}
	for _, object := range []client.Object{&corev1.Service{}, &corev1.Secret{}} {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sandbox), object); !apierrors.IsNotFound(err) {
			t.Fatalf("first cleanup retained %T: %v", object, err)
		}
	}
	var terminatingPod corev1.Pod
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sandbox), &terminatingPod); err != nil {
		t.Fatalf("get terminating Pod: %v", err)
	}
	if terminatingPod.DeletionTimestamp.IsZero() {
		t.Fatal("Runner Pod was not placed into termination")
	}
	for _, object := range []client.Object{&networkingv1.NetworkPolicy{}, &v1beta1.ExecutionSandbox{}} {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sandbox), object); err != nil {
			t.Fatalf("terminating Pod lost %T: %v", object, err)
		}
	}
	var retainedSandbox v1beta1.ExecutionSandbox
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sandbox), &retainedSandbox); err != nil {
		t.Fatal(err)
	}
	if !hasString(retainedSandbox.Finalizers, v1beta1.ExecutionSandboxCleanupFinalizer) {
		t.Fatalf("terminating Pod removed cleanup finalizer: %v", retainedSandbox.Finalizers)
	}

	terminatingPod.Finalizers = nil
	if err := k8sClient.Update(ctx, &terminatingPod); err != nil {
		t.Fatalf("release terminating Pod: %v", err)
	}
	assertEventually(t, func() error {
		var pod corev1.Pod
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sandbox), &pod)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return &lifecycleAssertionError{message: "Runner Pod still exists"}
	})

	result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)})
	if err != nil || result != (reconcile.Result{}) {
		t.Fatalf("final cleanup result=%#v err=%v", result, err)
	}
	for _, object := range []client.Object{&networkingv1.NetworkPolicy{}, &v1beta1.ExecutionSandbox{}} {
		assertEventually(t, func() error {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sandbox), object)
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			return &lifecycleAssertionError{message: "cleanup target still exists"}
		})
	}
}

func TestExecutionSandboxHTTPDeleteAPIServerRejectsStaleGeneration(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, *v1beta1.Worker, *v1beta1.ExecutionSandbox) *v1beta1.ExecutionSandbox
	}{
		{
			name: "concurrent heartbeat resourceVersion",
			prepare: func(t *testing.T, _ *v1beta1.Worker, stale *v1beta1.ExecutionSandbox) *v1beta1.ExecutionSandbox {
				t.Helper()
				var current v1beta1.ExecutionSandbox
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stale), &current); err != nil {
					t.Fatal(err)
				}
				heartbeat := metav1.NewTime(time.Now().UTC())
				current.Status.LastHeartbeat = &heartbeat
				if err := k8sClient.Status().Update(ctx, &current); err != nil {
					t.Fatalf("concurrent heartbeat: %v", err)
				}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stale), &current); err != nil {
					t.Fatal(err)
				}
				return &current
			},
		},
		{
			name: "replacement sandbox UID",
			prepare: func(t *testing.T, _ *v1beta1.Worker, stale *v1beta1.ExecutionSandbox) *v1beta1.ExecutionSandbox {
				t.Helper()
				var current v1beta1.ExecutionSandbox
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stale), &current); err != nil {
					t.Fatal(err)
				}
				current.Finalizers = nil
				if err := k8sClient.Update(ctx, &current); err != nil {
					t.Fatalf("remove old sandbox finalizer: %v", err)
				}
				uid := current.UID
				resourceVersion := current.ResourceVersion
				if err := k8sClient.Delete(ctx, &current, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}); err != nil {
					t.Fatalf("delete old sandbox: %v", err)
				}
				assertEventually(t, func() error {
					var old v1beta1.ExecutionSandbox
					err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stale), &old)
					if apierrors.IsNotFound(err) {
						return nil
					}
					if err != nil {
						return err
					}
					return &lifecycleAssertionError{message: "old sandbox still exists"}
				})
				replacement := &v1beta1.ExecutionSandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: stale.Name, Namespace: stale.Namespace,
						Labels: copyStringMap(stale.Labels), Finalizers: []string{v1beta1.ExecutionSandboxCleanupFinalizer},
						OwnerReferences: append([]metav1.OwnerReference(nil), stale.OwnerReferences...),
					},
					Spec: *stale.Spec.DeepCopy(),
				}
				if err := k8sClient.Create(ctx, replacement); err != nil {
					t.Fatalf("create replacement sandbox: %v", err)
				}
				if replacement.UID == stale.UID {
					t.Fatalf("replacement UID=%q equals stale UID", replacement.UID)
				}
				return replacement
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker, stale := createExecutionSandboxThroughHTTP(t, "stale-delete")
			t.Cleanup(func() {
				cleanupExecutionSandboxAPIObjects(t, stale)
				cleanupWorkerAPIObject(t, worker)
			})
			current := tt.prepare(t, worker, stale)
			staleClient := &staleExecutionSandboxReadClient{
				Client: k8sClient, key: client.ObjectKeyFromObject(stale), stale: stale.DeepCopy(),
			}
			handler := serverpkg.NewExecutionSandboxHandler(
				staleClient, worker.Namespace, "ctl-a", "deepagents", sandboxpolicy.Default(), nil,
			)
			req := httptest.NewRequest(http.MethodDelete, "/sandbox", nil)
			req.SetPathValue("name", worker.Name)
			req.SetPathValue("sessionId", stale.Spec.SessionID)
			rec := httptest.NewRecorder()

			handler.Delete(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want API-server precondition conflict", rec.Code, rec.Body.String())
			}
			var preserved v1beta1.ExecutionSandbox
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(current), &preserved); err != nil {
				t.Fatalf("current sandbox was deleted: %v", err)
			}
			if preserved.UID != current.UID || !preserved.DeletionTimestamp.IsZero() {
				t.Fatalf("current sandbox mutated: UID=%q deletionTimestamp=%v", preserved.UID, preserved.DeletionTimestamp)
			}
		})
	}
}

func TestExecutionSandboxControllerExpiryAPIServerRejectsConcurrentHeartbeat(t *testing.T) {
	worker, stale := createExecutionSandboxThroughHTTP(t, "stale-expiry")
	t.Cleanup(func() {
		cleanupExecutionSandboxAPIObjects(t, stale)
		cleanupWorkerAPIObject(t, worker)
	})
	var current v1beta1.ExecutionSandbox
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stale), &current); err != nil {
		t.Fatal(err)
	}
	heartbeat := metav1.NewTime(time.Now().UTC())
	current.Status.LastHeartbeat = &heartbeat
	if err := k8sClient.Status().Update(ctx, &current); err != nil {
		t.Fatalf("concurrent heartbeat: %v", err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(stale), &current); err != nil {
		t.Fatal(err)
	}
	staleClient := &staleExecutionSandboxReadClient{
		Client: k8sClient, key: client.ObjectKeyFromObject(stale), stale: stale.DeepCopy(),
	}
	reconciler := &controllerpkg.ExecutionSandboxReconciler{
		Client: staleClient, APIReader: k8sClient, ControllerName: "ctl-a", DefaultRuntime: "deepagents",
		Now: func() time.Time {
			return stale.CreationTimestamp.Add(9 * time.Hour)
		},
	}

	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(stale)})
	if !apierrors.IsConflict(err) || result != (reconcile.Result{}) {
		t.Fatalf("stale expiry result=%#v err=%v, want API-server precondition conflict", result, err)
	}
	var preserved v1beta1.ExecutionSandbox
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&current), &preserved); err != nil {
		t.Fatalf("get heartbeat-updated sandbox: %v", err)
	}
	if preserved.UID != current.UID || preserved.ResourceVersion != current.ResourceVersion || !preserved.DeletionTimestamp.IsZero() {
		t.Fatalf(
			"heartbeat-updated sandbox mutated: UID=%q RV=%q deletionTimestamp=%v",
			preserved.UID,
			preserved.ResourceVersion,
			preserved.DeletionTimestamp,
		)
	}
}

type staleExecutionSandboxReadClient struct {
	client.Client
	key   types.NamespacedName
	stale *v1beta1.ExecutionSandbox
}

func (c *staleExecutionSandboxReadClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	opts ...client.GetOption,
) error {
	if sandbox, ok := object.(*v1beta1.ExecutionSandbox); ok && key == c.key {
		c.stale.DeepCopyInto(sandbox)
		return nil
	}
	return c.Client.Get(ctx, key, object, opts...)
}

func createExecutionSandboxThroughHTTP(t *testing.T, sessionSuffix string) (*v1beta1.Worker, *v1beta1.ExecutionSandbox) {
	t.Helper()
	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name: fixtures.UniqueName("sandbox-worker"), Namespace: "default",
			Labels: map[string]string{v1beta1.LabelController: "ctl-a"},
		},
		Spec: v1beta1.WorkerSpec{
			Model: "test-model", Runtime: "deepagents",
			RuntimeConfig: &v1beta1.WorkerRuntimeConfig{DeepAgents: &v1beta1.DeepAgentsRuntimeConfig{
				Execution: v1beta1.DeepAgentsExecutionConfig{Mode: "sandbox", IdleTimeout: "30m", MaxLifetime: "8h"},
			}},
		},
	}
	if err := k8sClient.Create(ctx, worker); err != nil {
		t.Fatalf("create sandbox Worker: %v", err)
	}
	sessionID := "thread-" + sessionSuffix
	handler := serverpkg.NewExecutionSandboxHandler(
		k8sClient, worker.Namespace, "ctl-a", "deepagents", sandboxpolicy.Default(), nil,
	)
	req := httptest.NewRequest(http.MethodPost, "/ensure", strings.NewReader(`{"sessionId":"`+sessionID+`"}`))
	req.SetPathValue("name", worker.Name)
	rec := httptest.NewRecorder()
	handler.Ensure(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ensure status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response serverpkg.ExecutionSandboxResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var sandbox v1beta1.ExecutionSandbox
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: response.Name, Namespace: worker.Namespace}, &sandbox); err != nil {
		t.Fatalf("get ensured sandbox: %v", err)
	}
	if sandbox.UID == "" || sandbox.ResourceVersion == "" || !hasString(sandbox.Finalizers, v1beta1.ExecutionSandboxCleanupFinalizer) {
		t.Fatalf("ensured sandbox lacks API identity/finalizer: UID=%q RV=%q finalizers=%v", sandbox.UID, sandbox.ResourceVersion, sandbox.Finalizers)
	}
	return worker, &sandbox
}

func createExecutionSandboxAPIChildren(t *testing.T, sandbox *v1beta1.ExecutionSandbox, holdPod bool) {
	t.Helper()
	controllerOwner := true
	blockOwnerDeletion := true
	owner := metav1.OwnerReference{
		APIVersion: v1beta1.SchemeGroupVersion.String(), Kind: "ExecutionSandbox", Name: sandbox.Name, UID: sandbox.UID,
		Controller: &controllerOwner, BlockOwnerDeletion: &blockOwnerDeletion,
	}
	metadata := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name: sandbox.Name, Namespace: sandbox.Namespace,
			Labels: map[string]string{
				v1beta1.LabelController:       "ctl-a",
				v1beta1.LabelWorker:           sandbox.Spec.WorkerRef.Name,
				v1beta1.LabelExecutionSandbox: sandbox.Name,
			},
			OwnerReferences: []metav1.OwnerReference{owner},
		}
	}
	immutable := true
	service := &corev1.Service{
		ObjectMeta: metadata(),
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080}}},
	}
	secret := &corev1.Secret{ObjectMeta: metadata(), Immutable: &immutable, Data: map[string][]byte{"token": []byte("test-runner-token-not-a-secret-0123456789")}}
	pod := &corev1.Pod{
		ObjectMeta: metadata(),
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "runner", Image: "runner:test"}},
		},
	}
	if holdPod {
		pod.Finalizers = []string{"tests.agentteams.io/hold-pod"}
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metadata(),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{v1beta1.LabelExecutionSandbox: sandbox.Name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	for _, object := range []client.Object{service, secret, pod, policy} {
		if err := k8sClient.Create(ctx, object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
	}
}

func cleanupExecutionSandboxAPIObjects(t *testing.T, sandbox *v1beta1.ExecutionSandbox) {
	t.Helper()
	key := client.ObjectKeyFromObject(sandbox)
	var pod corev1.Pod
	if err := k8sClient.Get(ctx, key, &pod); err == nil {
		pod.Finalizers = nil
		_ = k8sClient.Update(ctx, &pod)
		_ = k8sClient.Delete(ctx, &pod)
	}
	for _, object := range []client.Object{&corev1.Service{}, &corev1.Secret{}, &networkingv1.NetworkPolicy{}} {
		object.SetName(key.Name)
		object.SetNamespace(key.Namespace)
		_ = k8sClient.Delete(ctx, object)
	}
	var current v1beta1.ExecutionSandbox
	if err := k8sClient.Get(ctx, key, &current); err == nil {
		current.Finalizers = nil
		_ = k8sClient.Update(ctx, &current)
		_ = k8sClient.Delete(ctx, &current)
	}
}

func cleanupWorkerAPIObject(t *testing.T, worker *v1beta1.Worker) {
	t.Helper()
	var current v1beta1.Worker
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(worker), &current); err == nil {
		current.Finalizers = nil
		_ = k8sClient.Update(ctx, &current)
		_ = k8sClient.Delete(ctx, &current)
	}
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

type lifecycleAssertionError struct {
	message string
}

func (e *lifecycleAssertionError) Error() string { return e.message }
