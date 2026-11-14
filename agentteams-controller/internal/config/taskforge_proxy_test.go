package config

import "testing"

func TestLoadConfigTaskForgeProxy(t *testing.T) {
	t.Setenv("AGENTTEAMS_TASKFORGE_URL", "http://taskforge:8099/")
	t.Setenv("AGENTTEAMS_TASKFORGE_INTERNAL_TOKEN", "tf-token")
	cfg := LoadConfig()
	if cfg.TaskForgeURL != "http://taskforge:8099" {
		t.Fatalf("TaskForgeURL = %q", cfg.TaskForgeURL)
	}
	if cfg.TaskForgeToken != "tf-token" || !cfg.TaskForgeEnabled {
		t.Fatalf("token/enabled = %q/%v", cfg.TaskForgeToken, cfg.TaskForgeEnabled)
	}
}
