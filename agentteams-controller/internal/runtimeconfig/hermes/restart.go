package hermes

import (
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/runtimeconfig"
)

// RestartKeys returns, in input order, the keys of keys whose declared
// field takes effect only after a process restart (applies == restart),
// resolved with the surface's exact-then-wildcard convention (FieldFor /
// MatchKey). Keys the surface does not declare are skipped: the write
// path is expected to have validated them already, and this helper only
// classifies.
//
// A non-empty result means the caller must report a pending restart to
// the user and restart only after the user confirms and the session is
// drained - never silently mid-conversation. The drain itself belongs
// to the orchestrator, not to this helper.
func RestartKeys(s runtimeconfig.Surface, keys []string) []string {
	var out []string
	for _, key := range keys {
		if fld, ok := FieldFor(s, key); ok && fld.Applies == runtimeconfig.AppliesRestart {
			out = append(out, key)
		}
	}
	return out
}
