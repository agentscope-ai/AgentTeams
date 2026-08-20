package controller

import (
	"sort"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"github.com/go-logr/logr"
)

// mergeUserEnv injects user-declared env vars into sysEnv with
// system-wins precedence: any key already present in sysEnv is kept,
// and the user's value is discarded with a single INFO-level warning
// per ignored key. Collisions are sorted before logging so identical
// inputs produce identical log output (makes tests deterministic).
//
// The merge semantics live in service.MergeUserEnv so that server-side
// address resolution (EffectiveWorkerConsolePort) shares the exact same
// rule as container creation.
//
// Both maps may be nil. sysEnv is mutated in place and must be non-nil
// when userEnv is non-empty; callers always pass the builder's output,
// which is guaranteed non-nil.
func mergeUserEnv(sysEnv, userEnv map[string]string, logger logr.Logger, subject string) {
	ignored := service.MergeUserEnv(sysEnv, userEnv)
	if len(ignored) == 0 {
		return
	}
	sort.Strings(ignored)
	logger.Info("user-defined env keys ignored (reserved by system)",
		"subject", subject,
		"keys", ignored)
}
