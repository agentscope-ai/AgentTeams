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
	backend := NewDockerBackend(DockerConfig{WorkerImage: "openclaw:latest"}, "agentteams-worker-")
	_, err := backend.Create(context.Background(), CreateRequest{Name: "test", Runtime: "opencode"})
	if err == nil || !strings.Contains(err.Error(), "OPENCODE_WORKER_IMAGE") {
		t.Fatalf("must fail before Docker calls rather than launch another Harness: %v", err)
	}
}
