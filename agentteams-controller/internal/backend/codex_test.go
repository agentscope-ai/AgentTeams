package backend

import (
	"context"
	"strings"
	"testing"
)

func TestCodexRequiresExplicitImageInsteadOfOpenClawFallback(t *testing.T) {
	backend := NewDockerBackend(DockerConfig{WorkerImage: "openclaw:latest"}, "test-")
	_, err := backend.Create(context.Background(), CreateRequest{Name: "codex", Runtime: RuntimeCodex})
	if err == nil || !strings.Contains(err.Error(), "codex runtime requires") {
		t.Fatalf("missing Codex image must fail before Docker I/O, got %v", err)
	}
}
