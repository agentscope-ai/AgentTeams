package backend

import (
	"context"
	"strings"
	"testing"
)

func TestClaudeCodeRequiresExplicitImageBeforeBackendIO(t *testing.T) {
	docker := NewDockerBackend(DockerConfig{WorkerImage: "openclaw:latest"}, "test-")
	_, err := docker.Create(context.Background(), CreateRequest{Name: "claudecode", Runtime: RuntimeClaudeCode})
	if err == nil || !strings.Contains(err.Error(), "claudecode runtime requires") {
		t.Fatalf("missing ClaudeCode image must fail before Docker I/O, got %v", err)
	}
	if got := defaultRuntime(RuntimeClaudeCode); got != RuntimeClaudeCode {
		t.Fatalf("ClaudeCode runtime was replaced by %q", got)
	}
}
