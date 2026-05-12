package backend

import (
	"context"
	"errors"
	"testing"

	"github.com/hiclaw/hiclaw-controller/internal/backend/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ── Mock SandboxPlugin ──────────────────────────────────────────────────

type fakeSandboxPlugin struct {
	createSpec   sandbox.SandboxSpec // captured spec from last CreateSandbox call
	createErr    error
	deleteErr    error
	hibernateErr error
	resumeErr    error
	statusPhase  string
	statusErr    error
}

func (f *fakeSandboxPlugin) Type() string { return "fake" }
func (f *fakeSandboxPlugin) Capabilities(_ sandbox.ProviderConfig) sandbox.ProviderCapabilities {
	return sandbox.ProviderCapabilities{Hibernate: true}
}
func (f *fakeSandboxPlugin) Validate(_ sandbox.ProviderConfig) error { return nil }
func (f *fakeSandboxPlugin) HealthCheck(_ context.Context, _ sandbox.ProviderConfig) error {
	return nil
}

func (f *fakeSandboxPlugin) CreateSandbox(_ context.Context, spec sandbox.SandboxSpec, _ sandbox.ProviderConfig) (sandbox.SandboxHandle, error) {
	f.createSpec = spec
	if f.createErr != nil {
		return sandbox.SandboxHandle{}, f.createErr
	}
	return sandbox.SandboxHandle{SandboxID: spec.Name}, nil
}

func (f *fakeSandboxPlugin) DeleteSandbox(_ context.Context, _ string, _ sandbox.ProviderConfig) error {
	return f.deleteErr
}

func (f *fakeSandboxPlugin) HibernateSandbox(_ context.Context, _ string, _ sandbox.ProviderConfig) error {
	return f.hibernateErr
}

func (f *fakeSandboxPlugin) ResumeSandbox(_ context.Context, _ string, _ sandbox.ProviderConfig) error {
	return f.resumeErr
}

func (f *fakeSandboxPlugin) GetStatus(_ context.Context, _ string, _ sandbox.ProviderConfig) (sandbox.SandboxStatus, error) {
	if f.statusErr != nil {
		return sandbox.SandboxStatus{}, f.statusErr
	}
	return sandbox.SandboxStatus{Phase: f.statusPhase}, nil
}

// ── Helper ──────────────────────────────────────────────────────────────

func newTestSandboxBackend(plugin *fakeSandboxPlugin) *SandboxBackend {
	return NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{Namespace: "test-ns"},
		SandboxConfig{
			Namespace:    "test-ns",
			WorkerImage:  "test/worker:latest",
			WorkerCPU:    "500m",
			WorkerMemory: "1Gi",
		},
		"hiclaw-worker-",
		nil, // scheme not needed for these tests
		newFakeK8sCoreClient(),
	)
}

// ── Tests: Create ───────────────────────────────────────────────────────

func TestSandboxBackend_Create_Basic(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Create(context.Background(), CreateRequest{
		Name: "alice",
		Env:  map[string]string{"CUSTOM_VAR": "hello"},
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

	// Verify spec passed to plugin uses Template.
	spec := plugin.createSpec
	if len(spec.Template.Spec.Containers) == 0 {
		t.Fatal("spec.Template.Spec.Containers must not be empty")
	}
	workerContainer := spec.Template.Spec.Containers[0]
	if workerContainer.Image != "test/worker:latest" {
		t.Errorf("container.Image = %q, want %q", workerContainer.Image, "test/worker:latest")
	}
	if spec.Namespace != "test-ns" {
		t.Errorf("spec.Namespace = %q, want %q", spec.Namespace, "test-ns")
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
			backend := NewSandboxBackend(plugin, sandbox.ProviderConfig{Namespace: "test-ns"}, tt.config, "prefix-", nil, newFakeK8sCoreClient())

			_, err := backend.Create(context.Background(), CreateRequest{
				Name:    "worker1",
				Runtime: tt.runtime,
				Image:   tt.reqImage,
			})
			if err != nil {
				t.Fatalf("Create() error: %v", err)
			}
			workerContainer := plugin.createSpec.Template.Spec.Containers[0]
			if workerContainer.Image != tt.wantImage {
				t.Errorf("container.Image = %q, want %q", workerContainer.Image, tt.wantImage)
			}
		})
	}
}

func TestSandboxBackend_Create_NoImageError(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{Namespace: "test-ns"},
		SandboxConfig{Namespace: "test-ns"}, // no images configured
		"prefix-",
		nil,
		newFakeK8sCoreClient(),
	)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "x"})
	if err == nil {
		t.Fatal("expected error when no image configured")
	}
}

func TestSandboxBackend_Create_EnvMergeWithTemplate(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	fakeClient := newFakeK8sCoreClient()
	// Inject a ConfigMap with pod template containing worker env.
	fakeClient.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-controller", Namespace: "test-ns"},
		Data: map[string]string{
			"pod-template.yaml": `
metadata: {}
spec:
  containers:
  - name: worker
    env:
    - name: TEMPLATE_VAR
      value: "from-template"
    - name: SHARED_VAR
      value: "template-value"
`,
		},
	})

	backend := NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{Namespace: "test-ns"},
		SandboxConfig{
			Namespace:      "test-ns",
			WorkerImage:    "img:latest",
			WorkerCPU:      "500m",
			WorkerMemory:   "1Gi",
			ControllerName: "my-controller",
		},
		"prefix-",
		nil,
		fakeClient,
	)

	_, err := backend.Create(context.Background(), CreateRequest{
		Name: "worker1",
		Env:  map[string]string{"SHARED_VAR": "req-value", "REQ_ONLY": "yes"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	// Verify env in the worker container.
	// ApplyPodTemplate replaces template env with overlay env entirely
	// (overlay.Container.Env is non-empty), so only req.Env values appear.
	workerContainer := plugin.createSpec.Template.Spec.Containers[0]
	envMap := map[string]string{}
	for _, e := range workerContainer.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["SHARED_VAR"] != "req-value" {
		t.Errorf("SHARED_VAR = %q, want 'req-value' (req.Env should override)", envMap["SHARED_VAR"])
	}
	if envMap["REQ_ONLY"] != "yes" {
		t.Errorf("REQ_ONLY = %q, want 'yes'", envMap["REQ_ONLY"])
	}
}

func TestSandboxBackend_Create_TokenVolumeInjected(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	// Verify hiclaw-token volume is present.
	spec := plugin.createSpec
	foundVolume := false
	for _, v := range spec.Template.Spec.Volumes {
		if v.Name == "hiclaw-token" {
			foundVolume = true
			if v.Projected == nil {
				t.Error("hiclaw-token volume must use projected source")
			}
			break
		}
	}
	if !foundVolume {
		t.Error("spec.Template.Spec.Volumes must contain 'hiclaw-token'")
	}

	// Verify hiclaw-token volume mount is on the worker container.
	workerContainer := spec.Template.Spec.Containers[0]
	foundMount := false
	for _, vm := range workerContainer.VolumeMounts {
		if vm.Name == "hiclaw-token" {
			foundMount = true
			if vm.MountPath != "/var/run/secrets/hiclaw" {
				t.Errorf("token mount path = %q, want '/var/run/secrets/hiclaw'", vm.MountPath)
			}
			if !vm.ReadOnly {
				t.Error("token mount must be readOnly")
			}
			break
		}
	}
	if !foundMount {
		t.Error("worker container must have 'hiclaw-token' volumeMount")
	}
}

func TestSandboxBackend_Create_AutomountFalse(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	spec := plugin.createSpec
	if spec.Template.Spec.AutomountServiceAccountToken == nil {
		t.Fatal("AutomountServiceAccountToken must be set")
	}
	if *spec.Template.Spec.AutomountServiceAccountToken != false {
		t.Errorf("AutomountServiceAccountToken = %v, want false",
			*spec.Template.Spec.AutomountServiceAccountToken)
	}
}

func TestSandboxBackend_Create_RestartPolicyDefault(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	spec := plugin.createSpec
	if spec.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("RestartPolicy = %q, want %q",
			spec.Template.Spec.RestartPolicy, corev1.RestartPolicyAlways)
	}
}

func TestSandboxBackend_Create_SidecarsPreserved(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	fakeClient := newFakeK8sCoreClient()
	// Template with a sidecar container.
	fakeClient.injectConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-controller", Namespace: "test-ns"},
		Data: map[string]string{
			"pod-template.yaml": `
metadata: {}
spec:
  containers:
  - name: worker
    image: placeholder
  - name: log-collector
    image: fluentd:latest
`,
		},
	})

	backend := NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{Namespace: "test-ns"},
		SandboxConfig{
			Namespace:      "test-ns",
			WorkerImage:    "real-worker:latest",
			WorkerCPU:      "500m",
			WorkerMemory:   "1Gi",
			ControllerName: "my-controller",
		},
		"prefix-",
		nil,
		fakeClient,
	)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "bob"})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	containers := plugin.createSpec.Template.Spec.Containers
	if len(containers) < 2 {
		t.Fatalf("expected at least 2 containers (worker + sidecar), got %d", len(containers))
	}
	// Worker container should have the resolved image, not placeholder.
	if containers[0].Name != "worker" || containers[0].Image != "real-worker:latest" {
		t.Errorf("containers[0] = {%s, %s}, want {worker, real-worker:latest}",
			containers[0].Name, containers[0].Image)
	}
	// Sidecar must be preserved.
	if containers[1].Name != "log-collector" || containers[1].Image != "fluentd:latest" {
		t.Errorf("containers[1] = {%s, %s}, want {log-collector, fluentd:latest}",
			containers[1].Name, containers[1].Image)
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
		sandbox.ProviderConfig{Namespace: "test-ns"},
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
	)

	_, err := backend.Create(context.Background(), CreateRequest{Name: "alice"})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	spec := plugin.createSpec
	// Annotations should flow through to both CR metadata and template metadata.
	if spec.Annotations["network.alibabacloud.com/security-group-ids"] != "sg-bp1xxx" {
		t.Errorf("CR annotations missing security-group-ids, got %v", spec.Annotations)
	}
	if spec.Template.Annotations["kubeone.ali/appinstance-name"] != "magic-ctl" {
		t.Errorf("template annotations missing appinstance-name, got %v", spec.Template.Annotations)
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
spec:
  containers:
  - name: worker
    image: placeholder
`,
		},
	})

	backend := NewSandboxBackend(
		plugin,
		sandbox.ProviderConfig{Namespace: "test-ns"},
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
	)

	_, err := backend.Create(context.Background(), CreateRequest{
		Name:   "alice",
		Labels: map[string]string{"app": "hiclaw-worker"},
	})
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	spec := plugin.createSpec
	// Template labels should be merged.
	if spec.Labels["custom-label"] != "from-template" {
		t.Errorf("template label 'custom-label' not merged, got labels: %v", spec.Labels)
	}
	// Request labels should override.
	if spec.Labels["app"] != "hiclaw-worker" {
		t.Errorf("request label 'app' not present, got labels: %v", spec.Labels)
	}
	// Runtime label always set.
	if spec.Labels["hiclaw.io/runtime"] != "openclaw" {
		t.Errorf("runtime label missing, got labels: %v", spec.Labels)
	}
}

func TestSandboxBackend_Create_PluginError(t *testing.T) {
	plugin := &fakeSandboxPlugin{createErr: errors.New("boom")}
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

func TestSandboxBackend_Delete_Error(t *testing.T) {
	plugin := &fakeSandboxPlugin{deleteErr: errors.New("not found")}
	backend := newTestSandboxBackend(plugin)

	err := backend.Delete(context.Background(), "alice")
	if err == nil {
		t.Fatal("expected error")
	}
}

// ── Tests: Start/Stop ───────────────────────────────────────────────────

func TestSandboxBackend_Start(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	err := backend.Start(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
}

func TestSandboxBackend_Stop_Hibernate(t *testing.T) {
	plugin := &fakeSandboxPlugin{}
	backend := newTestSandboxBackend(plugin)

	err := backend.Stop(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

func TestSandboxBackend_Stop_FallbackToDelete(t *testing.T) {
	plugin := &fakeSandboxPlugin{hibernateErr: sandbox.ErrCapabilityNotSupported}
	backend := newTestSandboxBackend(plugin)

	// Stop should fallback to Delete when hibernate not supported.
	err := backend.Stop(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Stop() should fallback to Delete, got error: %v", err)
	}
}

// ── Tests: Status ───────────────────────────────────────────────────────

func TestSandboxBackend_Status_Running(t *testing.T) {
	plugin := &fakeSandboxPlugin{statusPhase: "Running"}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusRunning {
		t.Errorf("status = %v, want %v", result.Status, StatusRunning)
	}
}

func TestSandboxBackend_Status_Hibernated(t *testing.T) {
	plugin := &fakeSandboxPlugin{statusPhase: "Hibernated"}
	backend := newTestSandboxBackend(plugin)

	result, err := backend.Status(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if result.Status != StatusStopped {
		t.Errorf("status = %v, want %v (Hibernated -> Stopped)", result.Status, StatusStopped)
	}
}

func TestSandboxBackend_Status_NotFound(t *testing.T) {
	plugin := &fakeSandboxPlugin{statusErr: sandbox.ErrNotFound}
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
	plugin := &fakeSandboxPlugin{statusErr: sandbox.ErrProviderUnavailable}
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
	plugin := &fakeSandboxPlugin{statusErr: errors.New("transient api error")}
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
		{"Hibernated", StatusStopped},
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
		sandbox.ProviderConfig{Namespace: "ns"},
		SandboxConfig{Namespace: "ns", WorkerImage: "img:v1"}, // no CPU/Memory set
		"p-",
		nil,
		newFakeK8sCoreClient(),
	)
	if backend.config.WorkerCPU != "1000m" {
		t.Errorf("default WorkerCPU = %q, want '1000m'", backend.config.WorkerCPU)
	}
	if backend.config.WorkerMemory != "2Gi" {
		t.Errorf("default WorkerMemory = %q, want '2Gi'", backend.config.WorkerMemory)
	}
}

func TestSandboxBackend_WithPrefix(t *testing.T) {
	backend := newTestSandboxBackend(&fakeSandboxPlugin{})
	cp := backend.WithPrefix("new-prefix-")
	if cp.containerPrefix != "new-prefix-" {
		t.Errorf("WithPrefix prefix = %q, want 'new-prefix-'", cp.containerPrefix)
	}
	// Original not affected.
	if backend.containerPrefix != "hiclaw-worker-" {
		t.Errorf("original prefix changed: %q", backend.containerPrefix)
	}
}
