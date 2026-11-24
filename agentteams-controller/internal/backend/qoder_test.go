package backend

import (
	"context"
	"strings"
	"testing"
)

func TestQoderRequiresExplicitImageBeforeBackendIO(t *testing.T) {
	docker := NewDockerBackend(DockerConfig{WorkerImage: "openclaw:latest"}, "test-")
	_, err := docker.Create(context.Background(), CreateRequest{Name: "qoder", Runtime: RuntimeQoder})
	if err == nil || !strings.Contains(err.Error(), "qoder runtime requires") {
		t.Fatalf("missing Qoder image must fail before Docker I/O, got %v", err)
	}
	if got := defaultRuntime(RuntimeQoder); got != RuntimeQoder {
		t.Fatalf("Qoder runtime was replaced by %q", got)
	}
}
