package service

import (
	"path/filepath"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
)

// AllWorkerRuntimes is the full set of supported worker runtimes (mirrors
// the backend.Runtime* constants). The skill catalog uses it to derive the
// template→runtime mapping from BuiltinAgentDir.
var AllWorkerRuntimes = []string{
	backend.RuntimeOpenClaw,
	backend.RuntimeCopaw,
	backend.RuntimeHermes,
	backend.RuntimeOpenHuman,
	backend.RuntimeQwenPaw,
	backend.RuntimeDeepSeekHarness,
}

// BuiltinAgentDir returns the agent template directory that seeds the given
// (role, runtime) pair. It is the single source of truth for template
// selection: the Deployer seeds SOUL.md / AGENTS.md / builtin skills from
// this directory, and the read-only skill catalog derives its
// template→runtime availability mapping from the same function, so the two
// can never drift apart.
func BuiltinAgentDir(workerAgentDir, role, runtime string) string {
	baseDir := filepath.Dir(workerAgentDir)
	switch role {
	case "team_leader":
		return filepath.Join(baseDir, "team-leader-agent")
	default:
		switch runtime {
		case backend.RuntimeCopaw:
			return filepath.Join(baseDir, "copaw-worker-agent")
		case backend.RuntimeHermes:
			return filepath.Join(baseDir, "hermes-worker-agent")
		}
		return workerAgentDir
	}
}
