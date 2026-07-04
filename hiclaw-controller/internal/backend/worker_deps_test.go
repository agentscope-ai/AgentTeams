package backend

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"

	"github.com/hiclaw/hiclaw-controller/internal/backend/sandbox"
)

type captureSandboxPlugin struct {
	lastClaim sandbox.SandboxClaimSpec
}

func (c *captureSandboxPlugin) Type() string { return "capture" }
func (c *captureSandboxPlugin) Capabilities(config sandbox.ProviderConfig) sandbox.ProviderCapabilities {
	return config.Capabilities
}
func (c *captureSandboxPlugin) CreateSandboxClaim(_ context.Context, spec sandbox.SandboxClaimSpec, _ sandbox.ProviderConfig) (sandbox.SandboxHandle, error) {
	c.lastClaim = spec
	return sandbox.SandboxHandle{SandboxID: spec.Name}, nil
}
func (c *captureSandboxPlugin) DeleteSandboxClaim(_ context.Context, _ string, _ sandbox.ProviderConfig) error {
	return nil
}
func (c *captureSandboxPlugin) DeleteSandbox(_ context.Context, _ string, _ sandbox.ProviderConfig) error {
	return nil
}
func (c *captureSandboxPlugin) HibernateSandbox(_ context.Context, _ string, _ sandbox.ProviderConfig) error {
	return nil
}
func (c *captureSandboxPlugin) ResumeSandbox(_ context.Context, _ string, _ sandbox.ProviderConfig) error {
	return nil
}
func (c *captureSandboxPlugin) GetSandboxClaimStatus(_ context.Context, _ string, _ sandbox.ProviderConfig) (sandbox.SandboxStatus, error) {
	return sandbox.SandboxStatus{}, nil
}
func (c *captureSandboxPlugin) GetSandboxStatus(_ context.Context, _ string, _ sandbox.ProviderConfig) (sandbox.SandboxStatus, error) {
	return sandbox.SandboxStatus{}, nil
}
func (c *captureSandboxPlugin) ListSandboxes(_ context.Context, _ map[string]string, _ sandbox.ProviderConfig) ([]sandbox.SandboxStatus, error) {
	return nil, nil
}
func (c *captureSandboxPlugin) Validate(_ sandbox.ProviderConfig) error { return nil }
func (c *captureSandboxPlugin) HealthCheck(_ context.Context, _ sandbox.ProviderConfig) error {
	return nil
}

func TestSandboxBackendCreateBuildsWorkerDepsClaimContract(t *testing.T) {
	plugin := &captureSandboxPlugin{}
	scheme := runtime.NewScheme()
	backend := NewSandboxBackend(plugin, sandbox.ProviderConfig{
		Namespace:     "default",
		DynamicClient: fake.NewSimpleDynamicClient(scheme),
	}, SandboxConfig{
		Namespace:         "default",
		AgentRuntimeImage: "agent-runtime:latest",
		WorkerImage:       "worker:latest",
	}, "", scheme, nil, nil)

	if _, err := backend.Create(context.Background(), CreateRequest{
		Name:      "alice",
		Image:     "worker:custom",
		Env:       map[string]string{"AGENTTEAMS_TEST_ENV": "true"},
		AuthToken: "sa-token-alice",
	}); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if plugin.lastClaim.InplaceUpdate == nil || plugin.lastClaim.InplaceUpdate.Image != "worker:custom" {
		t.Fatalf("InplaceUpdate=%+v, want worker:custom", plugin.lastClaim.InplaceUpdate)
	}
	if !claimHasDynamicMount(plugin.lastClaim.DynamicVolumesMount, "/mnt/agentteams/env", "workers-deps/alice/env", "") {
		t.Fatalf("missing env dynamic mount: %+v", plugin.lastClaim.DynamicVolumesMount)
	}
	if !claimHasDynamicMount(plugin.lastClaim.DynamicVolumesMount, "/var/run/secrets/agentteams", "workers-deps/alice/token", "agentteams-token") {
		t.Fatalf("missing token dynamic mount: %+v", plugin.lastClaim.DynamicVolumesMount)
	}
}

func claimHasDynamicMount(mounts []sandbox.SandboxClaimDynamicVolumeMount, mountPath, subPath, credentialProvider string) bool {
	for _, mount := range mounts {
		if mount.PVName != "agentteams" || mount.MountPath != mountPath || mount.SubPath != subPath || !mount.ReadOnly {
			continue
		}
		if credentialProvider == "" {
			return true
		}
		return mount.Attributes["credentialProviderName"] == credentialProvider
	}
	return false
}
