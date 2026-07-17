package config

import "testing"

// TestConfigBuildersPopulateQwenPawWorkerImage asserts each backend-config
// builder wires the QwenPaw worker image (Issue 1/7). Without the builder line
// the field stays empty and the backend switch arm falls back to the generic
// worker image, so a QwenPaw runtime silently starts the wrong container.
func TestConfigBuildersPopulateQwenPawWorkerImage(t *testing.T) {
	const defaultImage = "agentteams/agentteams-qwenpaw-worker:latest"

	c := &Config{}

	if got := c.DockerConfig().QwenPawWorkerImage; got != defaultImage {
		t.Errorf("DockerConfig().QwenPawWorkerImage = %q, want %q", got, defaultImage)
	}
	if got := c.K8sConfig().QwenPawWorkerImage; got != defaultImage {
		t.Errorf("K8sConfig().QwenPawWorkerImage = %q, want %q", got, defaultImage)
	}
	if got := c.SandboxConfig().QwenPawWorkerImage; got != defaultImage {
		t.Errorf("SandboxConfig().QwenPawWorkerImage = %q, want %q", got, defaultImage)
	}
}

// TestConfigBuildersQwenPawWorkerImageOverride asserts the env override flows
// through every builder.
func TestConfigBuildersQwenPawWorkerImageOverride(t *testing.T) {
	const override = "registry.example.com/custom-qwenpaw:v9"
	t.Setenv("AGENTTEAMS_QWENPAW_WORKER_IMAGE", override)

	c := &Config{}

	if got := c.DockerConfig().QwenPawWorkerImage; got != override {
		t.Errorf("DockerConfig().QwenPawWorkerImage = %q, want %q", got, override)
	}
	if got := c.K8sConfig().QwenPawWorkerImage; got != override {
		t.Errorf("K8sConfig().QwenPawWorkerImage = %q, want %q", got, override)
	}
	if got := c.SandboxConfig().QwenPawWorkerImage; got != override {
		t.Errorf("SandboxConfig().QwenPawWorkerImage = %q, want %q", got, override)
	}
}
