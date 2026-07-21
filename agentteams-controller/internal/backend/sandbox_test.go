package backend

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// ── Mock SandboxPlugin ──────────────────────────────────────────────────

type fakeSandboxPlugin struct {
	createClaimSpec       sandbox.SandboxClaimSpec
	createClaimErr        error
	deleteClaimErr        error
	deleteSandboxErr      error
	hibernateErr          error
	resumeErr             error
	hibernateSandboxID    string
	resumeSandboxID       string
	hibernateSandboxIDs   []string
	resumeSandboxIDs      []string
	deleteClaimID         string
	deleteSandboxIDs      []string
	statusPhase           string
	claimStatusPhase      string
	claimStatusErr        error
	claimStatusID         string
	claimMessage          string
	claimDesiredReplicas  *int64
	claimClaimedReplicas  *int64
	sandboxStatusErr      error
	listSandboxesErr      error
	sandboxes             []sandbox.SandboxStatus
	listLabelSelectors    []map[string]string
	sandboxClaimID        string
	readyConditionStatus  bool
	readyConditionMessage string
}

func (f *fakeSandboxPlugin) Type() string { return "fake" }
func (f *fakeSandboxPlugin) Capabilities(_ sandbox.ProviderConfig) sandbox.ProviderCapabilities {
	return sandbox.ProviderCapabilities{Hibernate: true}
}
func (f *fakeSandboxPlugin) Validate(_ sandbox.ProviderConfig) error { return nil }
func (f *fakeSandboxPlugin) HealthCheck(_ context.Context, _ sandbox.ProviderConfig) error {
	return nil
}

func (f *fakeSandboxPlugin) CreateSandboxClaim(_ context.Context, spec sandbox.SandboxClaimSpec, _ sandbox.ProviderConfig) (sandbox.SandboxHandle, error) {
	f.createClaimSpec = spec
	if f.createClaimErr != nil {
		return sandbox.SandboxHandle{}, f.createClaimErr
	}
	return sandbox.SandboxHandle{SandboxID: spec.Name}, nil
}

func (f *fakeSandboxPlugin) DeleteSandboxClaim(_ context.Context, id string, _ sandbox.ProviderConfig) error {
	f.deleteClaimID = id
	return f.deleteClaimErr
}

func (f *fakeSandboxPlugin) DeleteSandbox(_ context.Context, id string, _ sandbox.ProviderConfig) error {
	f.deleteSandboxIDs = append(f.deleteSandboxIDs, id)
	return f.deleteSandboxErr
}

func (f *fakeSandboxPlugin) HibernateSandbox(_ context.Context, id string, _ sandbox.ProviderConfig) error {
	f.hibernateSandboxID = id
	f.hibernateSandboxIDs = append(f.hibernateSandboxIDs, id)
	return f.hibernateErr
}

func (f *fakeSandboxPlugin) ResumeSandbox(_ context.Context, id string, _ sandbox.ProviderConfig) error {
	f.resumeSandboxID = id
	f.resumeSandboxIDs = append(f.resumeSandboxIDs, id)
	return f.resumeErr
}

func (f *fakeSandboxPlugin) GetSandboxClaimStatus(_ context.Context, id string, _ sandbox.ProviderConfig) (sandbox.SandboxStatus, error) {
	f.claimStatusID = id
	if f.claimStatusErr != nil {
		return sandbox.SandboxStatus{}, f.claimStatusErr
	}
	phase := f.claimStatusPhase
	if phase == "" {
		phase = f.statusPhase
	}
	sandboxID := f.sandboxClaimID
	if sandboxID == "" {
		sandboxID = "bound-" + f.createClaimSpec.Name
	}
	return sandbox.SandboxStatus{
		SandboxID:             sandboxID,
		Phase:                 phase,
		Message:               f.claimMessage,
		ReadyConditionStatus:  f.readyConditionStatus,
		ReadyConditionMessage: f.readyConditionMessage,
		DesiredReplicas:       f.claimDesiredReplicas,
		ClaimedReplicas:       f.claimClaimedReplicas,
	}, nil
}

func (f *fakeSandboxPlugin) GetSandboxStatus(_ context.Context, id string, _ sandbox.ProviderConfig) (sandbox.SandboxStatus, error) {
	if f.sandboxStatusErr != nil {
		return sandbox.SandboxStatus{}, f.sandboxStatusErr
	}
	for _, status := range f.sandboxes {
		if status.SandboxID == id {
			return status, nil
		}
	}
	return sandbox.SandboxStatus{}, sandbox.ErrNotFound
}

func (f *fakeSandboxPlugin) ListSandboxes(_ context.Context, labels map[string]string, _ sandbox.ProviderConfig) ([]sandbox.SandboxStatus, error) {
	copied := map[string]string{}
	for k, v := range labels {
		copied[k] = v
	}
	f.listLabelSelectors = append(f.listLabelSelectors, copied)
	if f.listSandboxesErr != nil {
		return nil, f.listSandboxesErr
	}
	if len(f.sandboxes) > 0 {
		return append([]sandbox.SandboxStatus(nil), f.sandboxes...), nil
	}
	return nil, nil
}

// ── Helper ──────────────────────────────────────────────────────────────

func newTestSandboxBackend(plugin *fakeSandboxPlugin) *SandboxBackend {
	return NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{
			Namespace:     "test-ns",
			DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		},
		SandboxConfig{
			Namespace:      "test-ns",
			WorkerImage:    "test/worker:latest",
			WorkerCPU:      "500m",
			WorkerMemory:   "1Gi",
			ControllerName: "ctl-x",
		},
		"agentteams-worker-",
		nil, // scheme not needed for these tests
		newFakeK8sCoreClient(),
		nil, // remoteCache
	)
}

func int64Ptr(v int64) *int64 {
	return &v
}

func getTestSandboxSet(t *testing.T, backend *SandboxBackend) *unstructured.Unstructured {
	t.Helper()
	obj, err := backend.providerConfig.DynamicClient.Resource(sandboxSetGVR).Namespace("test-ns").Get(context.Background(), BuiltinSandboxInstanceName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get SandboxSet: %v", err)
	}
	return obj
}

func getSandboxSetWorkerContainer(t *testing.T, obj *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	containers, ok, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil || !ok || len(containers) != 1 {
		t.Fatalf("SandboxSet containers=%v ok=%v err=%v", containers, ok, err)
	}
	container, ok := containers[0].(map[string]interface{})
	if !ok {
		t.Fatalf("SandboxSet container=%T", containers[0])
	}
	return container
}

func assertSandboxSetResources(t *testing.T, container map[string]interface{}, cpu, memory string) {
	t.Helper()
	resources, ok := container["resources"].(map[string]interface{})
	if !ok {
		t.Fatalf("SandboxSet resources=%T", container["resources"])
	}
	requests, ok := resources["requests"].(map[string]interface{})
	if !ok {
		t.Fatalf("SandboxSet resource requests=%T", resources["requests"])
	}
	limits, ok := resources["limits"].(map[string]interface{})
	if !ok {
		t.Fatalf("SandboxSet resource limits=%T", resources["limits"])
	}
	if requests["cpu"] != cpu || limits["cpu"] != cpu {
		t.Fatalf("SandboxSet cpu requests/limits=%v/%v, want %s/%s", requests["cpu"], limits["cpu"], cpu, cpu)
	}
	if requests["memory"] != memory || limits["memory"] != memory {
		t.Fatalf("SandboxSet memory requests/limits=%v/%v, want %s/%s", requests["memory"], limits["memory"], memory, memory)
	}
}

// ── Tests: Create ───────────────────────────────────────────────────────

func TestSandboxBackend_Create_Basic(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Create(context.Background(), CreateRequest{
		Name:   "alice",
		Env:    map[string]string{"CUSTOM_VAR": "hello"},
		Labels: map[string]string{v1beta1.LabelWorker: "alice"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if result.Name != "alice" {
		t.Errorf("result.Name = %q, want %q", result.Name, "alice")
	}
	if result.Backend != "sandbox" {
		t.Errorf("result.Backend = %q, want %q", result.Backend, "sandbox")
	}
	if result.Status != StatusStarting {
		t.Errorf("result.Status = %v, want %v", result.Status, StatusStarting)
	}
	if result.AppID != "agentteams-worker-alice" {
		t.Errorf("result.AppID = %q, want claim name", result.AppID)
	}

	// Verify SandboxClaim spec passed to plugin uses provider-supported fields.
	spec := plugin.createClaimSpec
	if spec.Name != "agentteams-worker-alice" {
		t.Errorf("spec.Name = %q, want pod-like claim name", spec.Name)
	}
	if spec.Namespace != "test-ns" {
		t.Errorf("spec.Namespace = %q, want %q", spec.Namespace, "test-ns")
	}
	if spec.SandboxSetName != BuiltinSandboxInstanceName {
		t.Errorf("spec.SandboxSetName = %q, want sandbox set name", spec.SandboxSetName)
	}
	if spec.Labels[sandboxAgentNameLabel] != BuiltinSandboxInstanceName {
		t.Fatalf("SandboxClaim labels=%v, want agent identity label", spec.Labels)
	}
	if spec.Labels[v1beta1.LabelWorker] != "alice" {
		t.Fatalf("SandboxClaim labels=%v, want worker identity label", spec.Labels)
	}
	if spec.InplaceUpdate == nil || spec.InplaceUpdate.Image != "test/worker:latest" {
		t.Fatalf("inplaceUpdate=%+v, want image test/worker:latest", spec.InplaceUpdate)
	}
}

func TestSandboxBackend_Create_SandboxSetUsesEnvMountContract(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	if _, err := backend.Create(context.Background(), CreateRequest{Name: "alice"}); err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	obj := getTestSandboxSet(t, backend)
	replicas, ok, err := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	if err != nil || !ok {
		t.Fatalf("SandboxSet replicas ok=%v err=%v", ok, err)
	}
	if replicas != 1 {
		t.Fatalf("SandboxSet replicas=%d, want 1", replicas)
	}
	grace, ok, err := unstructured.NestedInt64(obj.Object, "spec", "template", "spec", "terminationGracePeriodSeconds")
	if err != nil || !ok {
		t.Fatalf("SandboxSet terminationGracePeriodSeconds ok=%v err=%v", ok, err)
	}
	if grace != 0 {
		t.Fatalf("SandboxSet terminationGracePeriodSeconds=%d, want 0", grace)
	}
	container := getSandboxSetWorkerContainer(t, obj)
	if got := container["name"]; got != "worker" {
		t.Fatalf("SandboxSet container name=%v, want worker", got)
	}
	if got := container["image"]; got != defaultSandboxAgentRuntimeImage {
		t.Fatalf("SandboxSet container image=%v, want %s", got, defaultSandboxAgentRuntimeImage)
	}
	assertSandboxSetResources(t, container, "500m", "1Gi")
	envItems, ok := container["env"].([]interface{})
	if !ok {
		t.Fatalf("SandboxSet env=%T", container["env"])
	}
	env := map[string]string{}
	for _, item := range envItems {
		entry := item.(map[string]interface{})
		env[entry["name"].(string)] = entry["value"].(string)
	}
	if len(env) != 2 {
		t.Fatalf("SandboxSet env=%v, want only env mount contract", env)
	}
	if env["AGENTTEAMS_WORKER_ENV_MOUNT_DIR"] != "/mnt/agentteams/env" {
		t.Fatalf("AGENTTEAMS_WORKER_ENV_MOUNT_DIR=%q", env["AGENTTEAMS_WORKER_ENV_MOUNT_DIR"])
	}
	if env["AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED"] != "1" {
		t.Fatalf("AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED=%q", env["AGENTTEAMS_WORKER_ENV_MOUNT_REQUIRED"])
	}
	if _, ok := env["AGENTTEAMS_AUTH_TOKEN_FILE"]; ok {
		t.Fatalf("SandboxSet must not set AGENTTEAMS_AUTH_TOKEN_FILE: %v", env)
	}
}

func TestSandboxBackend_Create_SandboxSetUsesConfiguredPrewarmSize(t *testing.T) {
	tests := []struct {
		name        string
		prewarmSize int
		want        int64
	}{
		{name: "positive", prewarmSize: 3, want: 3},
		{name: "zero", prewarmSize: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &fakeSandboxPlugin{}
			backend := NewSandboxBackend(
				plugin,
				sandbox.ProviderConfig{
					Namespace:     "test-ns",
					DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
				},
				SandboxConfig{
					Namespace:                    "test-ns",
					WorkerImage:                  "test/worker:latest",
					WorkerCPU:                    "500m",
					WorkerMemory:                 "1Gi",
					SandboxPrewarmSize:           tt.prewarmSize,
					SandboxPrewarmSizeConfigured: true,
					ControllerName:               "ctl-x",
				},
				"agentteams-worker-",
				nil,
				newFakeK8sCoreClient(),
				nil,
			)

			if _, err := backend.Create(context.Background(), CreateRequest{Name: "alice"}); err != nil {
				t.Fatalf("Create() error: %v", err)
			}
			obj := getTestSandboxSet(t, backend)
			replicas, ok, err := unstructured.NestedInt64(obj.Object, "spec", "replicas")
			if err != nil || !ok {
				t.Fatalf("SandboxSet replicas ok=%v err=%v", ok, err)
			}
			if replicas != tt.want {
				t.Fatalf("SandboxSet replicas=%d, want %d", replicas, tt.want)
			}
		})
	}
}

func TestSandboxBackend_Create_WorkerResourcesDoNotRewriteSandboxSetResources(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	if _, err := backend.Create(context.Background(), CreateRequest{
		Name: "alice",
		Resources: &ResourceRequirements{
			CPURequest:    "1500m",
			CPULimit:      "2",
			MemoryRequest: "3Gi",
			MemoryLimit:   "4Gi",
		},
	}); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	obj := getTestSandboxSet(t, backend)
	container := getSandboxSetWorkerContainer(t, obj)
	assertSandboxSetResources(t, container, "500m", "1Gi")
}

func TestSandboxBackend_Create_PreservesManagerIdentityLabel(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin).WithPrefix("")

	result, err := backend.Create(context.Background(), CreateRequest{
		Name:   "agentteams-manager",
		Labels: map[string]string{v1beta1.LabelManager: "default"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if result.AppID != "agentteams-manager" {
		t.Fatalf("AppID=%q, want manager claim name", result.AppID)
	}
	if plugin.createClaimSpec.Name != "agentteams-manager" {
		t.Fatalf("claim name=%q, want manager pod-like name", plugin.createClaimSpec.Name)
	}
	if plugin.createClaimSpec.Labels[v1beta1.LabelManager] != "default" {
		t.Fatalf("claim labels=%v, want manager identity label", plugin.createClaimSpec.Labels)
	}
	if plugin.createClaimSpec.Labels[sandboxAgentNameLabel] != BuiltinSandboxInstanceName {
		t.Fatalf("claim labels=%v, want sandbox agent label", plugin.createClaimSpec.Labels)
	}
}

func TestSandboxBackend_Create_AlwaysUsesSandboxClaim(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Create(context.Background(), CreateRequest{
		Name:           "alice",
		BackendRuntime: v1beta1.BackendRuntimeSandbox,
		Labels:         map[string]string{v1beta1.LabelWorker: "alice"},
		WorkersDeps: &WorkerDepsSpec{
			InplaceUpdateImage: "agentteams/worker:v2",
			DynamicVolumeMounts: []WorkerDepsDynamicVolumeMount{
				{PVName: "agentteams", MountPath: "/var/run/secrets/agentteams", SubPath: "workers-deps/alice/token", ReadOnly: true, Attributes: map[string]string{"credentialProviderName": "agentteams-token"}},
				{PVName: "agentteams", MountPath: "/mnt/agentteams/env", SubPath: "workers-deps/alice/env", ReadOnly: true, Attributes: map[string]string{"credentialProviderName": "agentteams-env"}},
				{PVName: "agentteams", MountPath: "/mnt/agentteams/data", SubPath: "workers-deps/alice/data", ReadOnly: false, Attributes: map[string]string{"credentialProviderName": "agentteams-data"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if result.AppID != "agentteams-worker-alice" {
		t.Fatalf("AppID=%q, want claim name", result.AppID)
	}
	if plugin.createClaimSpec.Name != "agentteams-worker-alice" {
		t.Fatalf("claim name=%q", plugin.createClaimSpec.Name)
	}
	if plugin.createClaimSpec.Labels[v1beta1.LabelWorker] != "alice" {
		t.Fatalf("claim labels=%v, want worker identity label", plugin.createClaimSpec.Labels)
	}
	if plugin.createClaimSpec.SandboxSetName != BuiltinSandboxInstanceName {
		t.Fatalf("SandboxSetName=%q, want %s", plugin.createClaimSpec.SandboxSetName, BuiltinSandboxInstanceName)
	}
	if plugin.createClaimSpec.InplaceUpdate == nil || plugin.createClaimSpec.InplaceUpdate.Image != "agentteams/worker:v2" {
		t.Fatalf("claim inplaceUpdate=%+v", plugin.createClaimSpec.InplaceUpdate)
	}
	if len(plugin.createClaimSpec.DynamicVolumesMount) != 3 {
		t.Fatalf("claim dynamic volumes=%+v", plugin.createClaimSpec.DynamicVolumesMount)
	}
	for _, mount := range plugin.createClaimSpec.DynamicVolumesMount {
		switch mount.MountPath {
		case "/var/run/secrets/agentteams", "/mnt/agentteams/env":
			if !mount.ReadOnly {
				t.Fatalf("%s mount should be read-only: %+v", mount.MountPath, mount)
			}
			if mount.MountPath == "/var/run/secrets/agentteams" && mount.Attributes["credentialProviderName"] != "agentteams-token" {
				t.Fatalf("token mount attributes=%v", mount.Attributes)
			}
			if _, ok := mount.Attributes["credProviderName"]; ok {
				t.Fatalf("legacy credProviderName should be omitted: %v", mount.Attributes)
			}
		case "/mnt/agentteams/data":
			if mount.ReadOnly {
				t.Fatalf("data mount should be read-write: %+v", mount)
			}
		default:
			t.Fatalf("unexpected dynamic mount: %+v", mount)
		}
	}
	if got := plugin.createClaimSpec.Annotations[sandbox.AnnotationLastAppliedSpecHash]; got != "" {
		t.Fatalf("legacy last-applied hash annotation on SandboxClaim=%q, want empty", got)
	}
}

func TestSandboxBackend_Create_ClaimNameFollowsPodNamingOverrides(t *testing.T) {
	tests := []struct {
		name          string
		req           CreateRequest
		containerPref string
		wantClaimName string
	}{
		{
			name:          "default container prefix",
			req:           CreateRequest{Name: "alice"},
			containerPref: "agentteams-worker-",
			wantClaimName: "agentteams-worker-alice",
		},
		{
			name:          "name prefix override",
			req:           CreateRequest{Name: "alice", NamePrefix: "custom-worker-"},
			containerPref: "agentteams-worker-",
			wantClaimName: "custom-worker-alice",
		},
		{
			name:          "container name override",
			req:           CreateRequest{Name: "alice", NamePrefix: "custom-worker-", ContainerName: "explicit-claim"},
			containerPref: "agentteams-worker-",
			wantClaimName: "explicit-claim",
		},
		{
			name:          "empty backend prefix uses input name",
			req:           CreateRequest{Name: "agentteams-manager"},
			containerPref: "",
			wantClaimName: "agentteams-manager",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &fakeSandboxPlugin{}
			backend := newTestSandboxBackend(plugin).WithPrefix(tt.containerPref)

			result, err := backend.Create(context.Background(), tt.req)
			if err != nil {
				t.Fatalf("Create() error: %v", err)
			}
			if plugin.createClaimSpec.Name != tt.wantClaimName {
				t.Fatalf("claim name=%q, want %q", plugin.createClaimSpec.Name, tt.wantClaimName)
			}
			if result.AppID != tt.wantClaimName {
				t.Fatalf("AppID=%q, want %q", result.AppID, tt.wantClaimName)
			}
		})
	}
}

func TestSandboxBackend_Create_ImageResolution(t *testing.T) {
	tests := []struct {
		name      string
		runtime   string
		reqImage  string
		config    SandboxConfig
		wantImage string
	}{
		{
			name:      "explicit image wins",
			reqImage:  "custom/image:v1",
			config:    SandboxConfig{WorkerImage: "default/image:latest"},
			wantImage: "custom/image:v1",
		},
		{
			name:      "copaw runtime",
			runtime:   RuntimeCopaw,
			config:    SandboxConfig{WorkerImage: "default:latest", CopawWorkerImage: "copaw:v2"},
			wantImage: "copaw:v2",
		},
		{
			name:      "hermes runtime",
			runtime:   RuntimeHermes,
			config:    SandboxConfig{WorkerImage: "default:latest", HermesWorkerImage: "hermes:v3"},
			wantImage: "hermes:v3",
		},
		{
			name:      "qwenpaw runtime",
			runtime:   RuntimeQwenPaw,
			config:    SandboxConfig{WorkerImage: "default:latest", QwenPawWorkerImage: "qwenpaw:v4"},
			wantImage: "qwenpaw:v4",
		},
		{
			name:      "default worker image",
			config:    SandboxConfig{WorkerImage: "default/worker:latest"},
			wantImage: "default/worker:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &fakeSandboxPlugin{}
			tt.config.Namespace = "test-ns"
			tt.config.WorkerCPU = "500m"
			tt.config.WorkerMemory = "1Gi"
			tt.config.ControllerName = "ctl-x"
			backend := NewSandboxBackend(plugin, sandbox.ProviderConfig{Namespace: "test-ns", DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}, tt.config, "prefix-", nil, newFakeK8sCoreClient(), nil)

			_, err := backend.Create(context.Background(), CreateRequest{
				Name:    "worker1",
				Runtime: tt.runtime,
				Image:   tt.reqImage,
			})
			if err != nil {
				t.Fatalf("Create() error: %v", err)
			}
			if plugin.createClaimSpec.InplaceUpdate == nil || plugin.createClaimSpec.InplaceUpdate.Image != tt.wantImage {
				t.Errorf("inplaceUpdate=%+v, want image %q", plugin.createClaimSpec.InplaceUpdate, tt.wantImage)
			}
		})
	}
}

func TestParseCapabilitiesPool(t *testing.T) {
	caps := ParseCapabilities("hibernate,pool,unknown")
	if !caps.Hibernate || !caps.Pool {
		t.Fatalf("caps=%+v, want hibernate+pool", caps)
	}

	caps = ParseCapabilities("unknown")
	if caps.Hibernate || caps.Pool {
		t.Fatalf("caps=%+v, want unknown token ignored", caps)
	}
}

func TestSandboxBackend_Create_NoImageError(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{Namespace: "test-ns", DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())},
		SandboxConfig{Namespace: "test-ns"}, // no images configured
		"prefix-",
		nil,
		newFakeK8sCoreClient(),
		nil,
	)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "x"})
	if err == nil {
		t.Fatal("expected error when no image configured")
	}
}

func TestSandboxBackend_Create_UsesFixedSandboxSetName(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{Namespace: "test-ns", DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())},
		SandboxConfig{
			Namespace:      "test-ns",
			WorkerImage:    "img:latest",
			WorkerCPU:      "500m",
			WorkerMemory:   "1Gi",
			ControllerName: "ctl-x",
		},
		"prefix-",
		nil,
		newFakeK8sCoreClient(),
		nil,
	)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "x", SandboxSetName: "ignored"})
	if err != nil {
		t.Fatalf("Create() error=%v", err)
	}
	if plugin.createClaimSpec.SandboxSetName != BuiltinSandboxInstanceName {
		t.Fatalf("SandboxSetName=%q, want %s", plugin.createClaimSpec.SandboxSetName, BuiltinSandboxInstanceName)
	}
}

func TestSandboxBackend_Create_AnnotationsFromTemplate(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	fakeClient := newFakeK8sCoreClient()
	fakeClient.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-controller", Namespace: "test-ns"},
		Data: map[string]string{
			"pod-template.yaml": `
metadata:
  annotations:
    network.alibabacloud.com/security-group-ids: "sg-bp1xxx"
    kubeone.ali/appinstance-name: "magic-ctl"
spec:
  containers:
  - name: worker
    image: placeholder
`,
		},
	})

	backend := NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{Namespace: "test-ns", DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())},
		SandboxConfig{
			Namespace:      "test-ns",
			WorkerImage:    "worker:latest",
			WorkerCPU:      "500m",
			WorkerMemory:   "1Gi",
			ControllerName: "my-controller",
		},
		"prefix-",
		nil,
		fakeClient,
		nil,
	)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	spec := plugin.createClaimSpec
	// Annotations should flow through to SandboxClaim metadata and spec.annotations.
	if spec.Annotations["network.alibabacloud.com/security-group-ids"] != "sg-bp1xxx" {
		t.Errorf("claim annotations missing security-group-ids, got %v", spec.Annotations)
	}
	if spec.Annotations["kubeone.ali/appinstance-name"] != "magic-ctl" {
		t.Errorf("claim annotations missing appinstance-name, got %v", spec.Annotations)
	}
	if got := spec.Annotations[sandbox.AnnotationLastAppliedSpecHash]; got != "" {
		t.Errorf("legacy last-applied hash annotation = %q, want empty", got)
	}
}

func TestSandboxBackend_Create_LabelsFromTemplate(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	fakeClient := newFakeK8sCoreClient()
	fakeClient.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-controller", Namespace: "test-ns"},
		Data: map[string]string{
			"pod-template.yaml": `
metadata:
  labels:
    custom-label: "from-template"
    app: "template-app"
    security.agents.kruise.io/agent-name: "from-template"
spec:
  containers:
  - name: worker
    image: placeholder
`,
		},
	})

	backend := NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{Namespace: "test-ns", DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())},
		SandboxConfig{
			Namespace:      "test-ns",
			WorkerImage:    "worker:latest",
			WorkerCPU:      "500m",
			WorkerMemory:   "1Gi",
			ControllerName: "my-controller",
		},
		"prefix-",
		nil,
		fakeClient,
		nil,
	)

	_, err := backend.Create(context.Background(), CreateRequest{
		Name:   "alice",
		Labels: map[string]string{"app": "agentteams-worker"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	spec := plugin.createClaimSpec
	// Template labels should be merged.
	if spec.Labels["custom-label"] != "from-template" {
		t.Errorf("template label 'custom-label' not merged, got labels: %v", spec.Labels)
	}
	// Request labels should override.
	if spec.Labels["app"] != "agentteams-worker" {
		t.Errorf("request label 'app' not present, got labels: %v", spec.Labels)
	}
	if spec.Labels[sandboxAgentNameLabel] != BuiltinSandboxInstanceName {
		t.Errorf("agent-name label not forced, got labels: %v", spec.Labels)
	}
	// Runtime label always set.
	if spec.Labels["agentteams.io/runtime"] != "openclaw" {
		t.Errorf("runtime label missing, got labels: %v", spec.Labels)
	}
}

func TestSandboxBackend_Create_PluginError(t *testing.T) {
	plugin := &fakeSandboxPlugin{createClaimErr: errors.New("boom")}
	backend := newTestSandboxBackend(plugin)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "x"})
	if err == nil {
		t.Fatal("expected error from plugin")
	}
}

// ── Tests: Delete ───────────────────────────────────────────────────────

func TestSandboxBackend_Delete(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	err := backend.Delete(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
}

func TestSandboxBackend_DeleteDeletesClaimAndActualSandboxes(t *testing.T) {
	plugin := &fakeSandboxPlugin{sandboxes: []sandbox.SandboxStatus{
		{SandboxID: "sandbox-a", Phase: sandbox.PhaseRunning},
		{SandboxID: "sandbox-b", Phase: sandbox.PhaseHibernated},
	}}
	backend := newTestSandboxBackend(plugin)

	err := backend.Delete(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if plugin.deleteClaimID != "agentteams-worker-alice" {
		t.Fatalf("delete claim ID=%q, want agentteams-worker-alice", plugin.deleteClaimID)
	}
	if len(plugin.deleteSandboxIDs) != 2 || plugin.deleteSandboxIDs[0] != "sandbox-a" || plugin.deleteSandboxIDs[1] != "sandbox-b" {
		t.Fatalf("delete sandbox IDs=%v, want sandbox-a/sandbox-b", plugin.deleteSandboxIDs)
	}
	wantSelector := map[string]string{
		v1beta1.LabelWorker:     "alice",
		v1beta1.LabelController: "ctl-x",
	}
	if !hasListSelector(plugin.listLabelSelectors, wantSelector) {
		t.Fatalf("missing worker sandbox selector %v, got %v", wantSelector, plugin.listLabelSelectors)
	}
}

func TestSandboxBackend_Delete_Error(t *testing.T) {
	plugin := &fakeSandboxPlugin{deleteClaimErr: errors.New("not found")}
	backend := newTestSandboxBackend(plugin)

	err := backend.Delete(context.Background(), "alice")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ── Tests: Start/Stop ───────────────────────────────────────────────────

func TestSandboxBackend_Start(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusPhase:     sandbox.PhaseCompleted,
		claimDesiredReplicas: int64Ptr(1),
		claimClaimedReplicas: int64Ptr(1),
		sandboxes:            []sandbox.SandboxStatus{{SandboxID: "sandbox-from-pool", Phase: sandbox.PhaseHibernated}},
	}
	backend := newTestSandboxBackend(plugin)

	err := backend.Start(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if plugin.resumeSandboxID != "sandbox-from-pool" {
		t.Fatalf("resume sandbox ID=%q, want actual sandbox", plugin.resumeSandboxID)
	}
}

func TestSandboxBackend_Stop_Hibernate(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusPhase:     sandbox.PhaseCompleted,
		claimDesiredReplicas: int64Ptr(1),
		claimClaimedReplicas: int64Ptr(1),
		sandboxes:            []sandbox.SandboxStatus{{SandboxID: "sandbox-from-pool", Phase: sandbox.PhaseRunning}},
	}
	backend := newTestSandboxBackend(plugin)

	err := backend.Stop(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if plugin.hibernateSandboxID != "sandbox-from-pool" {
		t.Fatalf("hibernate sandbox ID=%q, want actual sandbox", plugin.hibernateSandboxID)
	}
}

func TestSandboxBackend_Stop_FallbackToDelete(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		hibernateErr:   sandbox.ErrCapabilityNotSupported,
		statusPhase:    sandbox.PhaseRunning,
		sandboxClaimID: "sandbox-from-pool",
		sandboxes:      []sandbox.SandboxStatus{{SandboxID: "sandbox-from-pool", Phase: sandbox.PhaseRunning}},
	}
	backend := newTestSandboxBackend(plugin)

	// Stop should fallback to Delete when hibernate not supported.
	err := backend.Stop(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Stop() should fallback to Delete, got error: %v", err)
	}
	if len(plugin.deleteSandboxIDs) != 1 || plugin.deleteSandboxIDs[0] != "sandbox-from-pool" {
		t.Fatalf("delete sandbox IDs=%v, want sandbox-from-pool", plugin.deleteSandboxIDs)
	}
}

func TestSandboxBackend_StopHibernatesActualSandboxAfterClaimDeleted(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusErr: sandbox.ErrNotFound,
		sandboxes:      []sandbox.SandboxStatus{{SandboxID: "sandbox-from-pool", Phase: sandbox.PhaseRunning}},
	}
	backend := newTestSandboxBackend(plugin)

	err := backend.Stop(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if plugin.hibernateSandboxID != "sandbox-from-pool" {
		t.Fatalf("hibernate sandbox ID=%q, want actual sandbox", plugin.hibernateSandboxID)
	}
	if plugin.deleteClaimID != "" {
		t.Fatalf("stop should not delete claim, deleted %q", plugin.deleteClaimID)
	}
}

func TestSandboxBackend_StartResumesActualSandboxAfterClaimDeleted(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusErr: sandbox.ErrNotFound,
		sandboxes:      []sandbox.SandboxStatus{{SandboxID: "sandbox-from-pool", Phase: sandbox.PhaseHibernated}},
	}
	backend := newTestSandboxBackend(plugin)

	err := backend.Start(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if plugin.resumeSandboxID != "sandbox-from-pool" {
		t.Fatalf("resume sandbox ID=%q, want actual sandbox", plugin.resumeSandboxID)
	}
}

func TestSandboxBackend_StartStopConflictWhenActualLabelsMatchMultiple(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusPhase:     sandbox.PhaseCompleted,
		claimDesiredReplicas: int64Ptr(1),
		claimClaimedReplicas: int64Ptr(1),
		sandboxes: []sandbox.SandboxStatus{
			{SandboxID: "sandbox-a", Phase: sandbox.PhaseRunning},
			{SandboxID: "sandbox-b", Phase: sandbox.PhaseRunning},
		},
	}
	backend := newTestSandboxBackend(plugin)

	if err := backend.Start(context.Background(), "alice"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Start() error=%v, want ErrConflict", err)
	}
	if err := backend.Stop(context.Background(), "alice"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Stop() error=%v, want ErrConflict", err)
	}
	if len(plugin.resumeSandboxIDs) != 0 {
		t.Fatalf("must not resume an arbitrary sandbox, resumed=%v", plugin.resumeSandboxIDs)
	}
	if len(plugin.hibernateSandboxIDs) != 0 {
		t.Fatalf("must not hibernate arbitrary sandboxes, hibernated=%v", plugin.hibernateSandboxIDs)
	}
}

func TestSandboxBackend_StopConflictsWhenClaimDeletedAndMultipleActualSandboxesMatch(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusErr: sandbox.ErrNotFound,
		sandboxes: []sandbox.SandboxStatus{
			{SandboxID: "sandbox-a", Phase: sandbox.PhaseRunning},
			{SandboxID: "sandbox-b", Phase: sandbox.PhaseRunning},
		},
	}
	backend := newTestSandboxBackend(plugin)

	if err := backend.Stop(context.Background(), "alice"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Stop() error=%v, want ErrConflict", err)
	}
	if len(plugin.hibernateSandboxIDs) != 0 {
		t.Fatalf("must not hibernate arbitrary sandboxes, hibernated=%v", plugin.hibernateSandboxIDs)
	}
	if len(plugin.deleteSandboxIDs) != 0 {
		t.Fatalf("must not delete arbitrary sandboxes, deleted=%v", plugin.deleteSandboxIDs)
	}
}

func TestSandboxBackend_StopDeletesActualSandboxWhenClaimDeletedAndHibernateUnsupported(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusErr: sandbox.ErrNotFound,
		hibernateErr:   sandbox.ErrCapabilityNotSupported,
		sandboxes:      []sandbox.SandboxStatus{{SandboxID: "sandbox-a", Phase: sandbox.PhaseRunning}},
	}
	backend := newTestSandboxBackend(plugin)

	if err := backend.Stop(context.Background(), "alice"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if plugin.deleteClaimID != "agentteams-worker-alice" {
		t.Fatalf("delete claim ID=%q, want agentteams-worker-alice", plugin.deleteClaimID)
	}
	if len(plugin.deleteSandboxIDs) != 1 || plugin.deleteSandboxIDs[0] != "sandbox-a" {
		t.Fatalf("delete sandbox IDs=%v, want [sandbox-a]", plugin.deleteSandboxIDs)
	}
}

func TestSandboxBackend_StartReturnsConflictWhenClaimDeletedAndMultipleActualSandboxesMatch(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusErr: sandbox.ErrNotFound,
		sandboxes: []sandbox.SandboxStatus{
			{SandboxID: "sandbox-a", Phase: sandbox.PhaseHibernated},
			{SandboxID: "sandbox-b", Phase: sandbox.PhaseHibernated},
		},
	}
	backend := newTestSandboxBackend(plugin)

	err := backend.Start(context.Background(), "alice")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Start() error=%v, want ErrConflict", err)
	}
	if len(plugin.resumeSandboxIDs) != 0 {
		t.Fatalf("start must not resume an arbitrary sandbox, resumed=%v", plugin.resumeSandboxIDs)
	}
}

// ── Tests: Status ───────────────────────────────────────────────────────

func TestSandboxBackend_Status_Running(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusPhase:     sandbox.PhaseCompleted,
		claimDesiredReplicas: int64Ptr(1),
		claimClaimedReplicas: int64Ptr(1),
		sandboxes: []sandbox.SandboxStatus{{
			SandboxID:            "sandbox-from-pool",
			Phase:                sandbox.PhaseRunning,
			ReadyConditionStatus: true,
		}},
	}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusRunning {
		t.Errorf("status = %v, want %v", result.Status, StatusRunning)
	}
	if plugin.claimStatusID != "agentteams-worker-alice" {
		t.Fatalf("status claim ID=%q, want agentteams-worker-alice", plugin.claimStatusID)
	}
}

func TestSandboxBackend_Status_UsesActualSandboxWhenClaimDeleted(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusErr: sandbox.ErrNotFound,
		sandboxes: []sandbox.SandboxStatus{{
			SandboxID:            "sandbox-from-pool",
			Phase:                sandbox.PhaseRunning,
			ReadyConditionStatus: true,
		}},
	}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusRunning {
		t.Errorf("status = %v, want %v", result.Status, StatusRunning)
	}
}

func TestSandboxBackend_Status_ClaimPendingIsStarting(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusPhase: sandbox.PhasePending,
		claimMessage:     "waiting for sandbox stock",
	}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusStarting {
		t.Fatalf("status = %v, want %v", result.Status, StatusStarting)
	}
	if result.Message != "waiting for sandbox stock" {
		t.Fatalf("message = %q, want claim message", result.Message)
	}
}

func TestSandboxBackend_Status_CompletedClaimReplicaMismatchFails(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusPhase:     sandbox.PhaseCompleted,
		claimMessage:         "Successfully claimed 0/1 sandboxes",
		claimDesiredReplicas: int64Ptr(1),
		claimClaimedReplicas: int64Ptr(0),
	}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %v, want %v", result.Status, StatusFailed)
	}
	if !strings.Contains(result.Message, "Successfully claimed 0/1 sandboxes") ||
		!strings.Contains(result.Message, "claimedReplicas=0") ||
		!strings.Contains(result.Message, "spec.replicas=1") {
		t.Fatalf("message %q should include claim details", result.Message)
	}
}

func TestSandboxBackend_Status_CompletedClaimMissingReplicasFails(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusPhase: sandbox.PhaseCompleted,
		claimMessage:     "Successfully claimed 1/1 sandboxes",
	}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %v, want %v", result.Status, StatusFailed)
	}
	if !strings.Contains(result.Message, "claimedReplicas=missing") ||
		!strings.Contains(result.Message, "spec.replicas=missing") {
		t.Fatalf("message %q should include missing replica details", result.Message)
	}
}

func TestSandboxBackend_Status_CompletedClaimNoActualSandboxIsStarting(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusPhase:     sandbox.PhaseCompleted,
		claimMessage:         "Successfully claimed 1/1 sandboxes",
		claimDesiredReplicas: int64Ptr(1),
		claimClaimedReplicas: int64Ptr(1),
	}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusStarting {
		t.Fatalf("status = %v, want %v", result.Status, StatusStarting)
	}
	if !strings.Contains(result.Message, "Successfully claimed 1/1 sandboxes") ||
		!strings.Contains(result.Message, "no matching sandbox found") {
		t.Fatalf("message %q should describe completed claim without actual sandbox", result.Message)
	}
}

func TestSandboxBackend_StatusUnknownWhenCompletedClaimAndMultipleActualSandboxesMatch(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusPhase:     sandbox.PhaseCompleted,
		claimDesiredReplicas: int64Ptr(1),
		claimClaimedReplicas: int64Ptr(1),
		sandboxes: []sandbox.SandboxStatus{
			{SandboxID: "sandbox-a", Phase: sandbox.PhaseRunning},
			{SandboxID: "sandbox-b", Phase: sandbox.PhaseRunning},
		},
	}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusUnknown {
		t.Fatalf("status = %v, want %v", result.Status, StatusUnknown)
	}
	if !strings.Contains(result.Message, "multiple sandboxes match alice") ||
		!strings.Contains(result.Message, "after SandboxClaim agentteams-worker-alice completed") ||
		!strings.Contains(result.Message, "sandbox-a,sandbox-b") {
		t.Fatalf("message %q should describe matching sandboxes", result.Message)
	}
}

func TestSandboxBackend_StatusUnknownWhenClaimDeletedAndMultipleActualSandboxesMatch(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusErr: sandbox.ErrNotFound,
		sandboxes: []sandbox.SandboxStatus{
			{SandboxID: "sandbox-a", Phase: sandbox.PhaseRunning},
			{SandboxID: "sandbox-b", Phase: sandbox.PhaseHibernated},
		},
	}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusUnknown {
		t.Fatalf("status = %v, want %v", result.Status, StatusUnknown)
	}
	if !strings.Contains(result.Message, "multiple sandboxes match alice") ||
		!strings.Contains(result.Message, "sandbox-a,sandbox-b") {
		t.Fatalf("message %q should describe matching sandboxes", result.Message)
	}
}

func TestSandboxBackend_Status_UsesManagerLabelCandidateForManagerPodName(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusErr: sandbox.ErrNotFound,
		sandboxes: []sandbox.SandboxStatus{{
			SandboxID:            "manager-sandbox-from-pool",
			Phase:                sandbox.PhaseRunning,
			ReadyConditionStatus: true,
		}},
	}
	backend := newTestSandboxBackend(plugin)
	backend.config.ResourcePrefix = "hiclaw-"

	result, err := backend.Status(context.Background(), "hiclaw-manager")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusRunning {
		t.Errorf("status = %v, want %v", result.Status, StatusRunning)
	}
	wantSelector := map[string]string{
		v1beta1.LabelManager:    "default",
		v1beta1.LabelController: "ctl-x",
	}
	if !hasListSelector(plugin.listLabelSelectors, wantSelector) {
		t.Fatalf("missing manager CR label selector %v, got %v", wantSelector, plugin.listLabelSelectors)
	}
}

func TestSandboxBackend_Status_Hibernated(t *testing.T) {
	plugin := &fakeSandboxPlugin{
		claimStatusErr: sandbox.ErrNotFound,
		sandboxes:      []sandbox.SandboxStatus{{SandboxID: "sandbox-from-pool", Phase: sandbox.PhaseHibernated}},
	}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusSleeping {
		t.Errorf("status = %v, want %v (Hibernated -> Sleeping)", result.Status, StatusSleeping)
	}
}

func hasListSelector(selectors []map[string]string, want map[string]string) bool {
	for _, selector := range selectors {
		if len(selector) != len(want) {
			continue
		}
		matched := true
		for k, v := range want {
			if selector[k] != v {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func TestSandboxBackend_Status_NotFound(t *testing.T) {
	plugin := &fakeSandboxPlugin{claimStatusErr: sandbox.ErrNotFound}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusNotFound {
		t.Errorf("status = %v, want %v", result.Status, StatusNotFound)
	}
}

func TestSandboxBackend_Status_ProviderUnavailable(t *testing.T) {
	plugin := &fakeSandboxPlugin{claimStatusErr: sandbox.ErrProviderUnavailable}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusUnknown {
		t.Errorf("status = %v, want %v (provider unavailable)", result.Status, StatusUnknown)
	}
}

func TestSandboxBackend_Status_TransientErrorIsNotCollapsedToNotFound(t *testing.T) {
	// Regression guard: a generic GetStatus error (RBAC / API timeout /
	// parse failure) must surface as an error, NOT as StatusNotFound.
	// Collapsing it to NotFound historically caused the reconciler to
	// recreate the sandbox on every transient API hiccup.
	plugin := &fakeSandboxPlugin{claimStatusErr: errors.New("transient api error")}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err == nil {
		t.Fatalf("Status() should propagate transient errors, got result=%+v", result)
	}
}

// ── Tests: ParseCapabilities ────────────────────────────────────────────

func TestParseCapabilities(t *testing.T) {
	tests := []struct {
		input string
		want  sandbox.ProviderCapabilities
	}{
		{"", sandbox.ProviderCapabilities{}},
		{"hibernate", sandbox.ProviderCapabilities{Hibernate: true}},
		{"hibernate,checkpoint", sandbox.ProviderCapabilities{Hibernate: true}},
		{"Hibernate", sandbox.ProviderCapabilities{Hibernate: true}},
		{"unknown,hibernate", sandbox.ProviderCapabilities{Hibernate: true}},
		{"none", sandbox.ProviderCapabilities{}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParseCapabilities(tt.input)
			if got != tt.want {
				t.Errorf("ParseCapabilities(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

// ── Tests: mapSandboxPhaseToWorkerStatus ────────────────────────────────

func TestMapSandboxPhaseToWorkerStatus(t *testing.T) {
	tests := []struct {
		phase  string
		expect WorkerStatus
	}{
		{"Running", StatusRunning},
		{"Starting", StatusStarting},
		{"Resuming", StatusStarting},
		{"Pending", StatusStarting},
		{"Hibernating", StatusStarting},
		{"Terminating", StatusStarting},
		{"Hibernated", StatusSleeping},
		{"Failed", StatusUnknown},
		{"Terminated", StatusNotFound},
		{"Unknown", StatusUnknown},
		{"", StatusStarting},
	}

	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			got := mapSandboxPhaseToWorkerStatus(tt.phase)
			if got != tt.expect {
				t.Errorf("mapSandboxPhaseToWorkerStatus(%q) = %v, want %v", tt.phase, got, tt.expect)
			}
		})
	}
}

// ── Tests: Misc ─────────────────────────────────────────────────────────

func TestSandboxBackend_Name(t *testing.T) {
	backend := newTestSandboxBackend(&fakeSandboxPlugin{})
	if got := backend.Name(); got != "sandbox" {
		t.Errorf("Name() = %q, want %q", got, "sandbox")
	}
}

func TestSandboxBackend_DefaultResources(t *testing.T) {
	backend := NewSandboxBackend(
		&fakeSandboxPlugin{},
		sandbox.ProviderConfig{Namespace: "ns", DynamicClient: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())},
		SandboxConfig{Namespace: "ns", WorkerImage: "img:v1"}, // no CPU/Memory set
		"p-",
		nil,
		newFakeK8sCoreClient(),
		nil,
	)
	if backend.config.WorkerCPU != "1000m" {
		t.Errorf("default WorkerCPU = %q, want '1000m'", backend.config.WorkerCPU)
	}
	if backend.config.WorkerMemory != "2Gi" {
		t.Errorf("default WorkerMemory = %q, want '2Gi'", backend.config.WorkerMemory)
	}
	if backend.config.SandboxPrewarmSize != 1 {
		t.Errorf("default SandboxPrewarmSize = %d, want 1", backend.config.SandboxPrewarmSize)
	}
}

func TestSandboxBackend_WithPrefix(t *testing.T) {
	backend := newTestSandboxBackend(&fakeSandboxPlugin{})
	cp := backend.WithPrefix("new-prefix-")
	if cp.containerPrefix != "new-prefix-" {
		t.Errorf("WithPrefix prefix = %q, want 'new-prefix-'", cp.containerPrefix)
	}
	// Original not affected.
	if backend.containerPrefix != "agentteams-worker-" {
		t.Errorf("original prefix changed: %q", backend.containerPrefix)
	}
}

// ── Tests: Status + Ready condition ───────────────────────────────────

func TestSandboxBackend_Status_ReadyCondition(t *testing.T) {
	cases := []struct {
		name                  string
		phase                 string
		readyConditionStatus  bool
		readyConditionMessage string
		wantStatus            WorkerStatus
		wantMessage           string
	}{
		{
			name:                 "Running + ReadyConditionStatus=true",
			phase:                "Running",
			readyConditionStatus: true,
			wantStatus:           StatusRunning,
			wantMessage:          "",
		},
		{
			name:                  "Running + ReadyConditionStatus=false + message",
			phase:                 "Running",
			readyConditionStatus:  false,
			readyConditionMessage: "back-off 5m0s restarting...",
			wantStatus:            StatusFailed,
			wantMessage:           "back-off 5m0s restarting...",
		},
		{
			name:                  "Running + ReadyConditionStatus=false + no message",
			phase:                 "Running",
			readyConditionStatus:  false,
			readyConditionMessage: "",
			wantStatus:            StatusStarting,
			wantMessage:           "",
		},
		{
			name:                 "Hibernated + ReadyConditionStatus=false does not downgrade",
			phase:                "Hibernated",
			readyConditionStatus: false,
			wantStatus:           StatusSleeping,
			wantMessage:          "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plugin := &fakeSandboxPlugin{
				claimStatusErr: sandbox.ErrNotFound,
				sandboxes: []sandbox.SandboxStatus{{
					SandboxID:             "sandbox-from-pool",
					Phase:                 tc.phase,
					ReadyConditionStatus:  tc.readyConditionStatus,
					ReadyConditionMessage: tc.readyConditionMessage,
				}},
			}
			backend := newTestSandboxBackend(plugin)

			result, err := backend.Status(context.Background(), "alice")
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
