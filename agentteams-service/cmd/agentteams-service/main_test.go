package main

import (
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/config"
)

// G1 disabled mode (default) boots nothing and exits 0 (58 §6, WIP config).
func TestRunDisabledExitsZero(t *testing.T) {
	t.Setenv(config.EnvEnabled, "false")
	t.Setenv(config.EnvResDSN, "")
	t.Setenv(config.EnvOpsDSN, "")
	if code := run(nil); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
}

// Enabled without DSNs: config error → exit 2 with the frozen message
// (任务书 §2.8 / config.Validate).
func TestRunEnabledMissingDSN(t *testing.T) {
	t.Setenv(config.EnvEnabled, "")
	t.Setenv(config.EnvResDSN, "")
	t.Setenv(config.EnvOpsDSN, "")
	if code := run([]string{"--enabled=true"}); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// A config flag parse error is also exit 2.
func TestRunBadFlag(t *testing.T) {
	if code := run([]string{"--no-such-flag"}); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
