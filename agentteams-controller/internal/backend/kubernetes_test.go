package backend

import (
	"context"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeK8sCoreClient struct {
	pods       map[string]map[string]*corev1.Pod
	configMaps map[string]map[string]*corev1.ConfigMap
	cmGetErr   error          // if non-nil, every ConfigMap Get returns this error
	getCalls   map[string]int // key: "namespace/name" -> count (for caching-behavior tests)
}

func newFakeK8sCoreClient(objects ...*corev1.Pod) *fakeK8sCoreClient {
	client := &fakeK8sCoreClient{
		pods:       map[string]map[string]*corev1.Pod{},
		configMaps: map[string]map[string]*corev1.ConfigMap{},
		getCalls:   map[string]int{},
	}
	for _, obj := range objects {
		client.injectPod(obj)
	}
	return client
}

func (f *fakeK8sCoreClient) injectPod(pod *corev1.Pod) {
	ns := pod.Namespace
	if ns == "" {
		ns = "default"
	}
	if f.pods[ns] == nil {
		f.pods[ns] = map[string]*corev1.Pod{}
	}
	f.pods[ns][pod.Name] = pod.DeepCopy()
}

// injectConfigMap stores a ConfigMap under its namespace/name so that fake
// ConfigMaps(ns).Get(name) returns it. Used by agent-pod-template tests.
func (f *fakeK8sCoreClient) injectConfigMap(cm *corev1.ConfigMap) {
	ns := cm.Namespace
	if ns == "" {
		ns = "default"
	}
	if f.configMaps[ns] == nil {
		f.configMaps[ns] = map[string]*corev1.ConfigMap{}
	}
	f.configMaps[ns][cm.Name] = cm.DeepCopy()
}

func (f *fakeK8sCoreClient) getCount(namespace, name string) int {
	return f.getCalls[namespace+"/"+name]
}

func (f *fakeK8sCoreClient) Pods(namespace string) K8sPodClient {
	if f.pods[namespace] == nil {
		f.pods[namespace] = map[string]*corev1.Pod{}
	}
	return &fakeK8sPodClient{
		namespace: namespace,
		store:     f.pods[namespace],
		getCalls:  f.getCalls,
	}
}

func (f *fakeK8sCoreClient) ConfigMaps(namespace string) K8sConfigMapClient {
	if f.configMaps[namespace] == nil {
		f.configMaps[namespace] = map[string]*corev1.ConfigMap{}
	}
	return &fakeK8sConfigMapClient{
		namespace: namespace,
		store:     f.configMaps[namespace],
		forcedErr: f.cmGetErr,
	}
}

func (f *fakeK8sCoreClient) Services(_ string) K8sServiceClient {
	return &fakeK8sServiceClient{}
}

func (f *fakeK8sCoreClient) Namespaces() K8sNamespaceClient {
	return &fakeK8sNamespaceClient{}
}

func (f *fakeK8sCoreClient) ServiceAccounts(_ string) K8sServiceAccountClient {
	panic("not implemented")
}

func (f *fakeK8sCoreClient) TokenReviews() K8sTokenReviewClient {
	panic("not implemented")
}

// fakeK8sServiceClient is a no-op stub for tests that don't exercise Services.
type fakeK8sServiceClient struct{}

func (f *fakeK8sServiceClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*corev1.Service, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, "")
}
func (f *fakeK8sServiceClient) Create(_ context.Context, svc *corev1.Service, _ metav1.CreateOptions) (*corev1.Service, error) {
	return svc, nil
}
func (f *fakeK8sServiceClient) Update(_ context.Context, svc *corev1.Service, _ metav1.UpdateOptions) (*corev1.Service, error) {
	return svc, nil
}
func (f *fakeK8sServiceClient) Delete(_ context.Context, _ string, _ metav1.DeleteOptions) error {
	return nil
}
func (f *fakeK8sServiceClient) List(_ context.Context, _ metav1.ListOptions) (*corev1.ServiceList, error) {
	return &corev1.ServiceList{}, nil
}

// fakeK8sNamespaceClient is a no-op stub for tests that don't exercise Namespaces.
type fakeK8sNamespaceClient struct{}

func (f *fakeK8sNamespaceClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*corev1.Namespace, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "")
}
func (f *fakeK8sNamespaceClient) Create(_ context.Context, ns *corev1.Namespace, _ metav1.CreateOptions) (*corev1.Namespace, error) {
	return ns, nil
}

type fakeK8sConfigMapClient struct {
	namespace string
	store     map[string]*corev1.ConfigMap
	forcedErr error
}

func (f *fakeK8sConfigMapClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	if f.forcedErr != nil {
		return nil, f.forcedErr
	}
	if cm, ok := f.store[name]; ok {
		return cm.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
}

type fakeK8sPodClient struct {
	namespace string
	store     map[string]*corev1.Pod
	getCalls  map[string]int
}

func (f *fakeK8sPodClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.Pod, error) {
	f.getCalls[f.namespace+"/"+name]++
	if pod, ok := f.store[name]; ok {
		return pod.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
}

func (f *fakeK8sPodClient) Create(_ context.Context, pod *corev1.Pod, _ metav1.CreateOptions) (*corev1.Pod, error) {
	if _, exists := f.store[pod.Name]; exists {
		return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, pod.Name)
	}
	created := pod.DeepCopy()
	if created.Namespace == "" {
		created.Namespace = f.namespace
	}
	f.store[created.Name] = created
	return created.DeepCopy(), nil
}

func (f *fakeK8sPodClient) Delete(_ context.Context, name string, _ metav1.DeleteOptions) error {
	if _, exists := f.store[name]; !exists {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
	}
	delete(f.store, name)
	return nil
}

func newTestK8sBackend(objects ...*corev1.Pod) *K8sBackend {
	b, _ := newTestK8sBackendWithFake(K8sConfig{}, objects...)
	return b
}

// newTestK8sBackendWithFake returns both the backend and the underlying fake
// client so tests can inspect Get call counts and inject the controller Pod.
func newTestK8sBackendWithFake(extra K8sConfig, objects ...*corev1.Pod) (*K8sBackend, *fakeK8sCoreClient) {
	client := newFakeK8sCoreClient(objects...)
	cfg := K8sConfig{
		Namespace:        "hiclaw",
		WorkerImage:      "agentteams/worker-agent:latest",
		CopawWorkerImage: "agentteams/copaw-worker:latest",
		WorkerCPU:        "1000m",
		WorkerMemory:     "2Gi",
		ControllerName:   extra.ControllerName,
	}
	return NewK8sBackendWithClient(client, cfg, "agentteams-worker-", nil), client
}

func TestK8sCreate(t *testing.T) {
	t.Setenv("AGENTTEAMS_FS_BUCKET", "agentteams-fs")
	t.Setenv("AGENTTEAMS_REGION", "cn-hangzhou")

	b := newTestK8sBackend()

	result, err := b.Create(context.Background(), CreateRequest{
		Name: "alice",
		Env: map[string]string{
			"AGENTTEAMS_MATRIX_URL": "http://matrix:6167",
		},
		ControllerURL:      "http://controller:8090",
		ServiceAccountName: "agentteams-worker-test1",
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if result.Backend != "k8s" {
		t.Fatalf("expected k8s backend, got %s", result.Backend)
	}
	if result.Status != StatusStarting {
		t.Fatalf("expected starting status, got %s", result.Status)
	}

	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected worker pod to exist: %v", err)
	}
	if pod.Spec.ServiceAccountName != "agentteams-worker-test1" {
		t.Fatalf("expected SA agentteams-worker-test1, got %q", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("expected default automount disabled")
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].Name != "agentteams-token" {
		t.Fatalf("expected projected volume agentteams-token, got %+v", pod.Spec.Volumes)
	}
	projSrc := pod.Spec.Volumes[0].Projected.Sources[0].ServiceAccountToken
	if projSrc.Audience != "agentteams-controller" {
		t.Fatalf("expected default audience agentteams-controller, got %q", projSrc.Audience)
	}
	if projSrc.ExpirationSeconds == nil || *projSrc.ExpirationSeconds != DefaultAuthTokenExpirationSeconds {
		t.Fatalf("expected default token expiration %d, got %v", DefaultAuthTokenExpirationSeconds, projSrc.ExpirationSeconds)
	}

	envs := map[string]string{}
	for _, env := range pod.Spec.Containers[0].Env {
		envs[env.Name] = env.Value
	}
	if envs["AGENTTEAMS_RUNTIME"] != "k8s" {
		t.Fatalf("expected AGENTTEAMS_RUNTIME=k8s, got %q", envs["AGENTTEAMS_RUNTIME"])
	}
	if envs["AGENTTEAMS_AUTH_TOKEN_FILE"] != "/var/run/secrets/agentteams/token" {
		t.Fatalf("expected AGENTTEAMS_AUTH_TOKEN_FILE, got %q", envs["AGENTTEAMS_AUTH_TOKEN_FILE"])
	}
	if envs["AGENTTEAMS_CONTROLLER_URL"] != "http://controller:8090" {
		t.Fatalf("expected injected controller URL, got %q", envs["AGENTTEAMS_CONTROLLER_URL"])
	}
	if envs["AGENTTEAMS_FS_BUCKET"] != "agentteams-fs" {
		t.Fatalf("expected AGENTTEAMS_FS_BUCKET from process env, got %q", envs["AGENTTEAMS_FS_BUCKET"])
	}
	if _, ok := envs["AGENTTEAMS_OSS_BUCKET"]; ok {
		t.Fatalf("unexpected legacy AGENTTEAMS_OSS_BUCKET in worker pod env")
	}
	if envs["AGENTTEAMS_REGION"] != "cn-hangzhou" {
		t.Fatalf("expected AGENTTEAMS_REGION from process env, got %q", envs["AGENTTEAMS_REGION"])
	}
}

func TestK8sCreate_WorkerDepsDataMount(t *testing.T) {
	b := newTestK8sBackend()

	_, err := b.Create(context.Background(), CreateRequest{
		Name: "alice",
		WorkersDeps: &WorkerDepsSpec{PodVolume: &WorkerDepsPodVolume{
			Name:      "hiclaw-worker-deps",
			ClaimName: "agentteams",
			Mounts: []WorkerDepsPodVolumeMount{
				{MountPath: "/mnt/agentteams/data", SubPath: "workers-deps/alice/data"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if len(pod.Spec.Volumes) != 2 {
		t.Fatalf("volumes=%+v, want token + worker deps", pod.Spec.Volumes)
	}
	if pod.Spec.Volumes[1].PersistentVolumeClaim == nil || pod.Spec.Volumes[1].PersistentVolumeClaim.ClaimName != "agentteams" {
		t.Fatalf("worker deps volume=%+v", pod.Spec.Volumes[1])
	}
	mounts := pod.Spec.Containers[0].VolumeMounts
	if len(mounts) != 2 {
		t.Fatalf("volumeMounts=%+v, want token + data", mounts)
	}
	if mounts[1].MountPath != "/mnt/agentteams/data" || mounts[1].SubPath != "workers-deps/alice/data" || mounts[1].ReadOnly {
		t.Fatalf("data mount=%+v", mounts[1])
	}
}

func TestK8sCreateCustomAudience(t *testing.T) {
	b := newTestK8sBackend()

	_, err := b.Create(context.Background(), CreateRequest{
		Name:                  "bob",
		AuthAudience:          "custom-audience",
		AuthExpirationSeconds: 7200,
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-bob", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected worker pod to exist: %v", err)
	}
	projSrc := pod.Spec.Volumes[0].Projected.Sources[0].ServiceAccountToken
	if projSrc.Audience != "custom-audience" {
		t.Fatalf("expected custom-audience, got %q", projSrc.Audience)
	}
	if projSrc.ExpirationSeconds == nil || *projSrc.ExpirationSeconds != 7200 {
		t.Fatalf("expected custom expiration 7200, got %v", projSrc.ExpirationSeconds)
	}
}

func TestK8sCreateTokenExpirationClampsMinimum(t *testing.T) {
	b := newTestK8sBackend()

	_, err := b.Create(context.Background(), CreateRequest{
		Name:                  "min-exp",
		AuthExpirationSeconds: 300,
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-min-exp", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected worker pod to exist: %v", err)
	}
	projSrc := pod.Spec.Volumes[0].Projected.Sources[0].ServiceAccountToken
	if projSrc.ExpirationSeconds == nil || *projSrc.ExpirationSeconds != MinAuthTokenExpirationSeconds {
		t.Fatalf("expected min expiration %d, got %v", MinAuthTokenExpirationSeconds, projSrc.ExpirationSeconds)
	}
}

func TestK8sCreateConflict(t *testing.T) {
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentteams-worker-alice",
			Namespace: "hiclaw",
		},
	}
	b := newTestK8sBackend(existingPod)

	_, err := b.Create(context.Background(), CreateRequest{Name: "alice"})
	if err == nil {
		t.Fatal("expected conflict error")
	}
}

func TestK8sStatus(t *testing.T) {
	b := newTestK8sBackend(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentteams-worker-bob",
			Namespace: "hiclaw",
			Labels: map[string]string{
				"app":                  "agentteams-worker",
				"agentteams.io/worker": "bob",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})

	result, err := b.Status(context.Background(), "bob")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if result.Status != StatusRunning {
		t.Fatalf("expected running, got %s", result.Status)
	}
}

func TestK8sStatus_ContainerFailureReasons(t *testing.T) {
	cases := []struct {
		name        string
		podStatus   corev1.PodStatus
		wantStatus  WorkerStatus
		wantRaw     string
		wantMessage string
	}{
		{
			name: "pending image pull backoff fails worker",
			podStatus: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "worker",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "ImagePullBackOff",
							Message: `failed to pull image "registry.example.com/worker:missing": not found`,
						},
					},
				}},
			},
			wantStatus:  StatusFailed,
			wantRaw:     "ImagePullBackOff",
			wantMessage: `container worker: ImagePullBackOff: failed to pull image "registry.example.com/worker:missing": not found`,
		},
		{
			name: "init container config error fails worker",
			podStatus: corev1.PodStatus{
				Phase: corev1.PodPending,
				InitContainerStatuses: []corev1.ContainerStatus{{
					Name: "init",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "CreateContainerConfigError",
							Message: "secret missing",
						},
					},
				}},
			},
			wantStatus:  StatusFailed,
			wantRaw:     "CreateContainerConfigError",
			wantMessage: "container init: CreateContainerConfigError: secret missing",
		},
		{
			name: "ordinary pending container creation still starts",
			podStatus: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "worker",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ContainerCreating",
						},
					},
				}},
			},
			wantStatus:  StatusStarting,
			wantRaw:     "Pending",
			wantMessage: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeK8sCoreClient(&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "agentteams-worker-test",
					Namespace: "agentteams",
				},
				Status: tc.podStatus,
			})
			b := NewK8sBackendWithClient(client, K8sConfig{Namespace: "agentteams"}, "agentteams-worker-", nil)

			result, err := b.Status(context.Background(), "test")
			if err != nil {
				t.Fatalf("Status failed: %v", err)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("Status = %s, want %s", result.Status, tc.wantStatus)
			}
			if result.RawStatus != tc.wantRaw {
				t.Fatalf("RawStatus = %q, want %q", result.RawStatus, tc.wantRaw)
			}
			if result.Message != tc.wantMessage {
				t.Fatalf("Message = %q, want %q", result.Message, tc.wantMessage)
			}
		})
	}
}

func TestK8sStopAndDelete(t *testing.T) {
	b := newTestK8sBackend(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentteams-worker-carol",
			Namespace: "hiclaw",
		},
	})

	if err := b.Stop(context.Background(), "carol"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	result, err := b.Status(context.Background(), "carol")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if result.Status != StatusNotFound {
		t.Fatalf("expected not_found after stop, got %s", result.Status)
	}
}

func TestNormalizeK8sPodPhase(t *testing.T) {
	cases := []struct {
		phase    corev1.PodPhase
		expected WorkerStatus
	}{
		{corev1.PodRunning, StatusRunning},
		{corev1.PodPending, StatusStarting},
		{corev1.PodSucceeded, StatusStopped},
		{corev1.PodFailed, StatusStopped},
		{corev1.PodUnknown, StatusUnknown},
	}
	for _, tc := range cases {
		if got := normalizeK8sPodPhase(tc.phase); got != tc.expected {
			t.Fatalf("normalizeK8sPodPhase(%q)=%s, want %s", tc.phase, got, tc.expected)
		}
	}
}

func TestBuildHostAliases(t *testing.T) {
	aliases := buildHostAliases([]string{
		"matrix-local.agentteams.io:10.0.0.1",
		"aigw-local.agentteams.io:10.0.0.1",
		"bad-entry",
	})
	if len(aliases) != 1 {
		t.Fatalf("expected 1 host alias, got %d", len(aliases))
	}
	if len(aliases[0].Hostnames) != 2 {
		t.Fatalf("expected 2 hostnames, got %d", len(aliases[0].Hostnames))
	}
}

func TestK8sWithPrefix(t *testing.T) {
	managerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentteams-manager",
			Namespace: "hiclaw",
			Labels: map[string]string{
				"app":                   "agentteams-manager",
				"agentteams.io/manager": "default",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	b := newTestK8sBackend(managerPod)

	// Original backend (prefix "agentteams-worker-") should NOT find the manager pod
	result, err := b.Status(context.Background(), "agentteams-manager")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if result.Status != StatusNotFound {
		t.Fatalf("expected not_found with worker prefix, got %s", result.Status)
	}

	// WithPrefix("") should find it by exact name
	mb := b.WithPrefix("")
	result, err = mb.Status(context.Background(), "agentteams-manager")
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if result.Status != StatusRunning {
		t.Fatalf("expected running with empty prefix, got %s", result.Status)
	}

	// WithPrefix does not mutate the original backend
	if b.containerPrefix != "agentteams-worker-" {
		t.Fatalf("original prefix mutated: %q", b.containerPrefix)
	}
	if mb.containerPrefix != "" {
		t.Fatalf("new prefix not empty: %q", mb.containerPrefix)
	}
}

func TestK8sWithPrefixDelete(t *testing.T) {
	managerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentteams-manager",
			Namespace: "hiclaw",
		},
	}
	b := newTestK8sBackend(managerPod)
	mb := b.WithPrefix("")

	if err := mb.Delete(context.Background(), "agentteams-manager"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	result, err := mb.Status(context.Background(), "agentteams-manager")
	if err != nil {
		t.Fatalf("Status after delete failed: %v", err)
	}
	if result.Status != StatusNotFound {
		t.Fatalf("expected not_found after delete, got %s", result.Status)
	}
}

func TestK8sWithPrefixStop(t *testing.T) {
	managerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agentteams-manager",
			Namespace: "hiclaw",
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	b := newTestK8sBackend(managerPod)
	mb := b.WithPrefix("")

	// Stop on K8s backend is equivalent to Delete
	if err := mb.Stop(context.Background(), "agentteams-manager"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	result, err := mb.Status(context.Background(), "agentteams-manager")
	if err != nil {
		t.Fatalf("Status after stop failed: %v", err)
	}
	if result.Status != StatusNotFound {
		t.Fatalf("expected not_found after stop, got %s", result.Status)
	}
}

// TestK8sCreateRuntimeWorkingDir verifies WorkingDir / HOME defaulting per
// runtime. The hermes runtime now shares the openclaw layout: WorkingDir ==
// HOME == /root/agentteams-fs/agents/<name> (== MinIO mirror root). Only copaw
// now shares the same layout.
func TestK8sCreateRuntimeWorkingDir(t *testing.T) {
	cases := []struct {
		name           string
		runtime        string
		wantWorkingDir string
		wantHome       string
	}{
		{"openclaw", RuntimeOpenClaw, "/root/agentteams-fs/agents/x", "/root/agentteams-fs/agents/x"},
		{"hermes", RuntimeHermes, "/root/agentteams-fs/agents/x", "/root/agentteams-fs/agents/x"},
		{"copaw", RuntimeCopaw, "/root/agentteams-fs/agents/x", "/root/agentteams-fs/agents/x"},
		{"empty_default", "", "/root/agentteams-fs/agents/x", "/root/agentteams-fs/agents/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeK8sCoreClient()
			b := NewK8sBackendWithClient(client, K8sConfig{
				Namespace:         "hiclaw",
				WorkerImage:       "agentteams/worker-agent:latest",
				CopawWorkerImage:  "agentteams/copaw-worker:latest",
				HermesWorkerImage: "agentteams/hermes-worker:latest",
				WorkerCPU:         "1000m",
				WorkerMemory:      "2Gi",
			}, "agentteams-worker-", nil)

			if _, err := b.Create(context.Background(), CreateRequest{
				Name:    "x",
				Runtime: tc.runtime,
			}); err != nil {
				t.Fatalf("Create failed: %v", err)
			}
			pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-x", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Get pod failed: %v", err)
			}
			if got := pod.Spec.Containers[0].WorkingDir; got != tc.wantWorkingDir {
				t.Fatalf("WorkingDir = %q, want %q", got, tc.wantWorkingDir)
			}
			var gotHome string
			for _, ev := range pod.Spec.Containers[0].Env {
				if ev.Name == "HOME" {
					gotHome = ev.Value
					break
				}
			}
			if gotHome != tc.wantHome {
				t.Fatalf("HOME = %q, want %q", gotHome, tc.wantHome)
			}
		})
	}
}

// TestK8sCreateResolvesImageFromRuntime verifies that the K8s backend selects
// the correct image and runtime label based on req.Runtime, with empty values
// falling back to the caller-provided RuntimeFallback (worker reconciler →
// AGENTTEAMS_DEFAULT_WORKER_RUNTIME, manager reconciler → AGENTTEAMS_MANAGER_RUNTIME).
func TestK8sCreateResolvesImageFromRuntime(t *testing.T) {
	cases := []struct {
		name      string
		runtime   string
		fallback  string
		wantImage string
		wantLabel string
	}{
		{"explicit_copaw", RuntimeCopaw, "", "agentteams/copaw-worker:latest", RuntimeCopaw},
		{"explicit_hermes", RuntimeHermes, "", "agentteams/hermes-worker:latest", RuntimeHermes},
		{"explicit_qwenpaw", RuntimeQwenPaw, "", "agentteams/qwenpaw-worker:latest", RuntimeQwenPaw},
		{"explicit_openclaw", RuntimeOpenClaw, "", "agentteams/worker-agent:latest", RuntimeOpenClaw},
		{"empty_no_fallback", "", "", "agentteams/worker-agent:latest", RuntimeOpenClaw},
		{"empty_with_copaw_fallback", "", RuntimeCopaw, "agentteams/copaw-worker:latest", RuntimeCopaw},
		{"empty_with_hermes_fallback", "", RuntimeHermes, "agentteams/hermes-worker:latest", RuntimeHermes},
		{"empty_with_qwenpaw_fallback", "", RuntimeQwenPaw, "agentteams/qwenpaw-worker:latest", RuntimeQwenPaw},
		{"explicit_overrides_fallback", RuntimeOpenClaw, RuntimeHermes, "agentteams/worker-agent:latest", RuntimeOpenClaw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeK8sCoreClient()
			b := NewK8sBackendWithClient(client, K8sConfig{
				Namespace:          "hiclaw",
				WorkerImage:        "agentteams/worker-agent:latest",
				CopawWorkerImage:   "agentteams/copaw-worker:latest",
				HermesWorkerImage:  "agentteams/hermes-worker:latest",
				QwenPawWorkerImage: "agentteams/qwenpaw-worker:latest",
				WorkerCPU:          "1000m",
				WorkerMemory:       "2Gi",
			}, "agentteams-worker-", nil)

			if _, err := b.Create(context.Background(), CreateRequest{
				Name:            "x",
				Runtime:         tc.runtime,
				RuntimeFallback: tc.fallback,
			}); err != nil {
				t.Fatalf("Create failed: %v", err)
			}

			pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-x", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Get pod failed: %v", err)
			}
			if got := pod.Spec.Containers[0].Image; got != tc.wantImage {
				t.Fatalf("image = %q, want %q", got, tc.wantImage)
			}
			if got := pod.Labels["agentteams.io/runtime"]; got != tc.wantLabel {
				t.Fatalf("runtime label = %q, want %q", got, tc.wantLabel)
			}
		})
	}
}

// ── Integration tests: K8sBackend.Create + PodTemplate + ownerRefs ───────

// testControllerName is the canonical ControllerName used across integration
// tests that exercise the agent PodTemplate ConfigMap lookup path.
const testControllerName = "hiclaw-ctl"

// injectTemplateConfigMap installs a ConfigMap named testControllerName in
// the "hiclaw" namespace with the PodTemplateSpec YAML under the canonical
// data key, mirroring what a real user's `kubectl apply -f cm.yaml` does.
func injectTemplateConfigMap(t *testing.T, fake *fakeK8sCoreClient, content string) {
	t.Helper()
	fake.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testControllerName,
			Namespace: "hiclaw",
		},
		Data: map[string]string{AgentPodTemplateConfigMapKey: content},
	})
}

// K1: End-to-end Aliyun-shaped template — SG annotation, ANSM label,
// imagePullSecrets, nodeSelector, tolerations, sysctls, kubeone annotation
// all flow through unchanged while overlay.labels/annotations still merge.
func TestK8sCreate_TemplateEndToEndAliyunShape(t *testing.T) {
	b, fake := newTestK8sBackendWithFake(K8sConfig{ControllerName: testControllerName})
	injectTemplateConfigMap(t, fake, `metadata:
  annotations:
    network.alibabacloud.com/security-group-ids: sg-bp1xxx
    kubeone.ali/appinstance-name: magic-ctl
  labels:
    nsm.alibabacloud.com/inject-sidecar: ansm-magic-xxx
spec:
  securityContext:
    sysctls:
      - name: net.ipv4.fib_multipath_hash_policy
        value: "1"
  imagePullSecrets:
    - name: regsecret
  nodeSelector:
    type: virtual-kubelet
  tolerations:
    - key: virtual-kubelet.io/provider
      operator: Exists
      effect: NoSchedule
    - key: virtual-kubelet.io/compute-type
      value: acs
      effect: NoSchedule
`)

	if _, err := b.Create(context.Background(), CreateRequest{
		Name: "alice",
		Labels: map[string]string{
			"app":                  "agentteams-worker",
			"agentteams.io/worker": "alice",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if pod.Annotations["network.alibabacloud.com/security-group-ids"] != "sg-bp1xxx" {
		t.Fatalf("SG annotation: %+v", pod.Annotations)
	}
	if pod.Annotations["kubeone.ali/appinstance-name"] != "magic-ctl" {
		t.Fatalf("appinstance annotation: %+v", pod.Annotations)
	}
	if pod.Labels["nsm.alibabacloud.com/inject-sidecar"] != "ansm-magic-xxx" {
		t.Fatalf("ANSM label: %+v", pod.Labels)
	}
	if pod.Labels["agentteams.io/worker"] != "alice" || pod.Labels["app"] != "agentteams-worker" {
		t.Fatalf("overlay labels: %+v", pod.Labels)
	}
	if pod.Spec.SecurityContext == nil || len(pod.Spec.SecurityContext.Sysctls) != 1 {
		t.Fatalf("sysctls: %+v", pod.Spec.SecurityContext)
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "regsecret" {
		t.Fatalf("imagePullSecrets: %+v", pod.Spec.ImagePullSecrets)
	}
	if pod.Spec.NodeSelector["type"] != "virtual-kubelet" {
		t.Fatalf("nodeSelector: %+v", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 2 {
		t.Fatalf("tolerations: %+v", pod.Spec.Tolerations)
	}
}

// K2: No ControllerName (nothing to look up) → backend produces the same Pod
// shape it always did (agentteams-token projected volume, SA override,
// automount=false, default resources).
func TestK8sCreate_NoTemplateBackwardCompat(t *testing.T) {
	b, _ := newTestK8sBackendWithFake(K8sConfig{})
	if _, err := b.Create(context.Background(), CreateRequest{Name: "bob"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-bob", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if pod.Spec.ServiceAccountName != "agentteams-worker-bob" {
		t.Fatalf("SA: %q", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("automount must be false")
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].Name != "agentteams-token" {
		t.Fatalf("volumes: %+v", pod.Spec.Volumes)
	}
	if pod.Spec.Containers[0].Resources.Limits.Cpu().String() != "1" {
		t.Fatalf("cpu: %+v", pod.Spec.Containers[0].Resources)
	}
}

// K3: ControllerName is set but the ConfigMap does not exist → degrades
// gracefully to empty-template behavior, equivalent to K2.
func TestK8sCreate_TemplateConfigMapMissing(t *testing.T) {
	b, _ := newTestK8sBackendWithFake(K8sConfig{ControllerName: testControllerName})
	if _, err := b.Create(context.Background(), CreateRequest{Name: "carol"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

// K4: Template YAML malformed → logs but does NOT fail Create.
func TestK8sCreate_TemplateMalformed(t *testing.T) {
	b, fake := newTestK8sBackendWithFake(K8sConfig{ControllerName: testControllerName})
	injectTemplateConfigMap(t, fake, "this: is: not: valid: yaml: : :")
	if _, err := b.Create(context.Background(), CreateRequest{Name: "dave"}); err != nil {
		t.Fatalf("Create should tolerate malformed template: %v", err)
	}
}

// K5: CreateRequest.Owner — the backend stamps a single controller
// OwnerReference on the created Pod, pointing at the CR supplied by the
// reconciler. This is the Kubernetes-native GC path that replaces the old
// "inherit from controller Pod" logic.
func TestK8sCreate_SetsControllerReferenceFromOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	client := newFakeK8sCoreClient()
	b := NewK8sBackendWithClient(client, K8sConfig{
		Namespace:   "hiclaw",
		WorkerImage: "agentteams/worker-agent:latest",
	}, "agentteams-worker-", scheme)

	owner := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eve",
			Namespace: "hiclaw",
			UID:       "worker-uid-123",
		},
	}

	if _, err := b.Create(context.Background(), CreateRequest{
		Name:  "eve",
		Owner: owner,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-eve", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("expected exactly one ownerRef, got %+v", pod.OwnerReferences)
	}
	ref := pod.OwnerReferences[0]
	if ref.UID != owner.UID {
		t.Fatalf("ownerRef UID = %q, want %q", ref.UID, owner.UID)
	}
	if ref.Kind != "Worker" {
		t.Fatalf("ownerRef Kind = %q, want Worker", ref.Kind)
	}
	if ref.Name != owner.Name {
		t.Fatalf("ownerRef Name = %q, want %q", ref.Name, owner.Name)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Fatalf("ownerRef Controller must be true, got %+v", ref.Controller)
	}
	if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Fatalf("ownerRef BlockOwnerDeletion must be true, got %+v", ref.BlockOwnerDeletion)
	}
}

// TestK8sCreate_OwnerRequiresScheme asserts Create fails fast when a reconciler
// provides Owner but the backend was built without a scheme — a programmer
// error we want to catch loudly rather than silently drop the ownerRef.
func TestK8sCreate_OwnerRequiresScheme(t *testing.T) {
	client := newFakeK8sCoreClient()
	b := NewK8sBackendWithClient(client, K8sConfig{Namespace: "hiclaw", WorkerImage: "img"}, "agentteams-worker-", nil)

	owner := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "hiclaw", UID: "u"},
	}
	if _, err := b.Create(context.Background(), CreateRequest{Name: "a", Owner: owner}); err == nil {
		t.Fatal("expected Create to fail when Owner is set but scheme is nil")
	}
}

// K8: CreateRequest.Resources overrides the K8sConfig worker CPU/Memory
// defaults on the final Pod. Exercises the full overlay.ResourcesOverride
// path through ApplyPodTemplate.
func TestK8sCreate_ResourcesOverrideFromCreateRequest(t *testing.T) {
	b, _ := newTestK8sBackendWithFake(K8sConfig{})

	if _, err := b.Create(context.Background(), CreateRequest{
		Name: "frank",
		Resources: &ResourceRequirements{
			CPULimit:      "4",
			MemoryLimit:   "8Gi",
			CPURequest:    "500m",
			MemoryRequest: "1Gi",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-frank", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	res := pod.Spec.Containers[0].Resources
	if got := res.Limits.Cpu().String(); got != "4" {
		t.Fatalf("cpu limit: got %q, want 4", got)
	}
	if got := res.Limits.Memory().String(); got != "8Gi" {
		t.Fatalf("mem limit: got %q, want 8Gi", got)
	}
	if got := res.Requests.Cpu().String(); got != "500m" {
		t.Fatalf("cpu request: got %q, want 500m", got)
	}
	if got := res.Requests.Memory().String(); got != "1Gi" {
		t.Fatalf("mem request: got %q, want 1Gi", got)
	}
}

// K9: Partial CreateRequest.Resources (only CPU limit set) merges onto
// defaults: overridden field wins, unmentioned fields fall back to defaults.
func TestK8sCreate_ResourcesOverridePartial(t *testing.T) {
	b, _ := newTestK8sBackendWithFake(K8sConfig{})

	if _, err := b.Create(context.Background(), CreateRequest{
		Name:      "grace",
		Resources: &ResourceRequirements{CPULimit: "3"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, _ := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-grace", metav1.GetOptions{})
	res := pod.Spec.Containers[0].Resources
	if got := res.Limits.Cpu().String(); got != "3" {
		t.Fatalf("cpu limit (override): got %q, want 3", got)
	}
	if got := res.Limits.Memory().String(); got != "2Gi" {
		t.Fatalf("mem limit (default): got %q, want 2Gi", got)
	}
	if got := res.Requests.Cpu().String(); got != "100m" {
		t.Fatalf("cpu request (default): got %q, want 100m", got)
	}
}

// K10: Resources override wins over a template that also specifies resources
// (overlay.ResourcesOverride takes precedence over template container.Resources).
func TestK8sCreate_ResourcesOverrideBeatsTemplate(t *testing.T) {
	b, fake := newTestK8sBackendWithFake(K8sConfig{ControllerName: testControllerName})
	injectTemplateConfigMap(t, fake, `spec:
  containers:
    - name: worker
      resources:
        limits:
          cpu: "4"
          memory: 8Gi
`)

	if _, err := b.Create(context.Background(), CreateRequest{
		Name:      "henry",
		Resources: &ResourceRequirements{CPULimit: "8"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, _ := b.client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-henry", metav1.GetOptions{})
	got := pod.Spec.Containers[0].Resources.Limits.Cpu().String()
	if got != "8" {
		t.Fatalf("expected override=8 to win over template=4, got %q", got)
	}
}

// TestBuildDefaultResources_EmptyFallback covers the "K8sConfig fields empty"
// branch in buildDefaultResources.
func TestBuildDefaultResources_EmptyFallback(t *testing.T) {
	r := buildDefaultResources("", "")
	if got := r.Limits.Cpu().String(); got != "1" {
		t.Fatalf("default cpu: %q", got)
	}
	if got := r.Limits.Memory().String(); got != "2Gi" {
		t.Fatalf("default mem: %q", got)
	}
}

// TestK8sCreate_CustomResourcePrefix verifies that the worker pod's "app"
// label and the default SA-name fallback derive from K8sConfig.ResourcePrefix
// — critical for multi-tenant deployments sharing a namespace where the
// hard-coded "agentteams-worker" value would cause collisions across tenants.
func TestK8sCreate_CustomResourcePrefix(t *testing.T) {
	client := newFakeK8sCoreClient()
	cfg := K8sConfig{
		Namespace:      "hiclaw",
		WorkerImage:    "agentteams/worker-agent:latest",
		WorkerCPU:      "1000m",
		WorkerMemory:   "2Gi",
		ResourcePrefix: "teamB-",
	}
	b := NewK8sBackendWithClient(client, cfg, "teamB-worker-", nil)

	if _, err := b.Create(context.Background(), CreateRequest{
		Name:               "alice",
		ServiceAccountName: "teamB-worker-alice",
		Labels: map[string]string{
			"app":                  "teamB-worker",
			"agentteams.io/worker": "alice",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "teamB-worker-alice", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod lookup: %v", err)
	}
	if pod.Labels["app"] != "teamB-worker" {
		t.Fatalf("app label = %q, want teamB-worker", pod.Labels["app"])
	}
}

// TestK8sCreate_DefaultSAFallback verifies that when ServiceAccountName is
// omitted from a CreateRequest, the backend falls back to "${prefix}worker-<name>".
func TestK8sCreate_DefaultSAFallback(t *testing.T) {
	client := newFakeK8sCoreClient()
	cfg := K8sConfig{
		Namespace:      "hiclaw",
		WorkerImage:    "agentteams/worker-agent:latest",
		ResourcePrefix: "acme-",
	}
	b := NewK8sBackendWithClient(client, cfg, "acme-worker-", nil)

	if _, err := b.Create(context.Background(), CreateRequest{Name: "bob"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, err := b.client.Pods("hiclaw").Get(context.Background(), "acme-worker-bob", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pod lookup: %v", err)
	}
	if pod.Spec.ServiceAccountName != "acme-worker-bob" {
		t.Fatalf("SA = %q, want acme-worker-bob", pod.Spec.ServiceAccountName)
	}
}

func TestK8sResolveClient_Local(t *testing.T) {
	b := newTestK8sBackend()

	cli, ns, err := b.resolveClient(context.Background())
	if err != nil {
		t.Fatalf("resolveClient: %v", err)
	}
	if cli != b.client {
		t.Error("expected local client to be returned")
	}
	if ns != b.namespace {
		t.Errorf("namespace = %q, want %q", ns, b.namespace)
	}
}

// TestK8sCreate_LocalKeepsOwnerReference is the complementary assertion:
// when DeployMode is empty (Local), Create stamps the controller OwnerReference.
func TestK8sCreate_LocalKeepsOwnerReference(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	client := newFakeK8sCoreClient()
	b := NewK8sBackendWithClient(client, K8sConfig{
		Namespace:   "hiclaw",
		WorkerImage: "agentteams/worker-agent:latest",
	}, "agentteams-worker-", scheme)

	owner := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "hiclaw", UID: "u-2"},
	}
	if _, err := b.Create(context.Background(), CreateRequest{
		Name:  "bob",
		Owner: owner,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod, err := client.Pods("hiclaw").Get(context.Background(), "agentteams-worker-bob", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(pod.OwnerReferences) != 1 {
		t.Fatalf("expected 1 ownerReference for Local pod, got %+v", pod.OwnerReferences)
	}
}

// ── Tests: podReadyCondition ─────────────────────────────────────────────

func TestPodReadyCondition(t *testing.T) {
	cases := []struct {
		name        string
		conditions  []corev1.PodCondition
		wantMessage string
		wantReady   bool
	}{
		{
			name:        "nil conditions",
			conditions:  nil,
			wantMessage: "",
			wantReady:   true,
		},
		{
			name: "Ready=True",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			wantMessage: "",
			wantReady:   true,
		},
		{
			name: "Ready=False with message",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Message: "back-off 5m0s restarting..."},
			},
			wantMessage: "back-off 5m0s restarting...",
			wantReady:   false,
		},
		{
			name: "Ready=False without message",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Message: ""},
			},
			wantMessage: "",
			wantReady:   false,
		},
		{
			name: "Other conditions but no Ready",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
			wantMessage: "",
			wantReady:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ready := podReadyCondition(tc.conditions)
			if msg != tc.wantMessage {
				t.Errorf("message = %q, want %q", msg, tc.wantMessage)
			}
			if ready != tc.wantReady {
				t.Errorf("ready = %v, want %v", ready, tc.wantReady)
			}
		})
	}
}

// ── Tests: K8sBackend.Status() Ready condition scenarios ─────────────────

func TestK8sStatus_ReadyCondition(t *testing.T) {
	cases := []struct {
		name        string
		podPhase    corev1.PodPhase
		conditions  []corev1.PodCondition
		wantStatus  WorkerStatus
		wantMessage string
	}{
		{
			name:     "Running + Ready=True",
			podPhase: corev1.PodRunning,
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			wantStatus:  StatusRunning,
			wantMessage: "",
		},
		{
			name:     "Running + Ready=False with message",
			podPhase: corev1.PodRunning,
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Message: "crash info"},
			},
			wantStatus:  StatusFailed,
			wantMessage: "crash info",
		},
		{
			name:     "Running + Ready=False without message",
			podPhase: corev1.PodRunning,
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Message: ""},
			},
			wantStatus:  StatusStarting,
			wantMessage: "",
		},
		{
			name:        "Pending does not check conditions",
			podPhase:    corev1.PodPending,
			conditions:  nil,
			wantStatus:  StatusStarting,
			wantMessage: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestK8sBackend(&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "agentteams-worker-test",
					Namespace: "hiclaw",
				},
				Status: corev1.PodStatus{
					Phase:      tc.podPhase,
					Conditions: tc.conditions,
				},
			})

			result, err := b.Status(context.Background(), "test")
			if err != nil {
				t.Fatalf("Status() error: %v", err)
			}
			if result.Status != tc.wantStatus {
				t.Errorf("Status = %v, want %v", result.Status, tc.wantStatus)
			}
			if result.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", result.Message, tc.wantMessage)
			}
		})
	}
}
