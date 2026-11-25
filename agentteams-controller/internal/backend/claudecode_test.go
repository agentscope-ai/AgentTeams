package backend

import (
	"context"
	"strings"
	"testing"
)

func TestClaudeCodeRequiresExplicitImageBeforeBackendIO(t *testing.T) {
	for name, create := range map[string]func(context.Context, CreateRequest) (*WorkerResult, error){
		"docker":  NewDockerBackend(DockerConfig{WorkerImage: "openclaw:latest"}, "test-").Create,
		"k8s":     NewK8sBackendWithClient(newFakeK8sCoreClient(), K8sConfig{Namespace: "test", WorkerImage: "openclaw:latest"}, "test-", nil).Create,
		"sandbox": (&SandboxBackend{config: SandboxConfig{WorkerImage: "openclaw:latest"}}).Create,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := create(context.Background(), CreateRequest{Name: "claudecode", Runtime: RuntimeClaudeCode})
			if err == nil || !strings.Contains(err.Error(), "CLAUDECODE_WORKER_IMAGE") {
				t.Fatalf("missing ClaudeCode image must fail before backend I/O, got %v", err)
			}
		})
	}
	if got := defaultRuntime(RuntimeClaudeCode); got != RuntimeClaudeCode {
		t.Fatalf("ClaudeCode runtime was replaced by %q", got)
	}
}

func TestClaudeCodeHandoffUsesConfiguredTargetImage(t *testing.T) {
	for name, target := range map[string]func(string) string{
		"docker": NewDockerBackend(DockerConfig{WorkerImage: "openclaw:latest", ClaudeCodeWorkerImage: "claudecode:v1"}, "test-").HandoffTargetImage,
		"k8s":    NewK8sBackendWithClient(newFakeK8sCoreClient(), K8sConfig{ClaudeCodeWorkerImage: "claudecode:v1"}, "test-", nil).HandoffTargetImage,
	} {
		t.Run(name, func(t *testing.T) {
			if got := target(RuntimeClaudeCode); got != "claudecode:v1" {
				t.Fatalf("wrong ClaudeCode handoff image: %q", got)
			}
		})
	}
}
