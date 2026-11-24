package backend

import (
	"context"
	"strings"
	"testing"
)

func TestOpenCodeRuntimeContract(t *testing.T) {
	if !ValidRuntime("opencode") || !UsesMemberRuntimeConfig("opencode") {
		t.Fatal("OpenCode must consume the common member runtime record")
	}
	if got := defaultRuntime("opencode"); got != "opencode" {
		t.Fatalf("OpenCode was silently resolved to %s", got)
	}
}

func TestOpenCodeRequiresItsOwnImage(t *testing.T) {
	for name, create := range map[string]func(context.Context, CreateRequest) (*WorkerResult, error){
		"docker":  NewDockerBackend(DockerConfig{WorkerImage: "openclaw:latest"}, "agentteams-worker-").Create,
		"k8s":     NewK8sBackendWithClient(newFakeK8sCoreClient(), K8sConfig{Namespace: "agentteams", WorkerImage: "openclaw:latest"}, "agentteams-worker-", nil).Create,
		"sandbox": (&SandboxBackend{config: SandboxConfig{WorkerImage: "openclaw:latest"}}).Create,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := create(context.Background(), CreateRequest{Name: "test", Runtime: "opencode"})
			if err == nil || !strings.Contains(err.Error(), "OPENCODE_WORKER_IMAGE") {
				t.Fatalf("must require an OpenCode image rather than launch another Harness: %v", err)
			}
		})
	}
}
