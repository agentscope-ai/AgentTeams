package hermes

// SubagentModel is the AgentTeams-side subagent model selection, the
// input of the delegation projection. The zero value of a field means
// "not configured".
type SubagentModel struct {
	Model    string
	Provider string
	Fallback []map[string]any
}

// ProjectDelegation maps a SubagentModel to the delegation.* keys to
// write into the worker's config.yaml.
//
// Unset selections omit their keys; empty model/provider strings are
// not written. This preserves native routing: delegation.model and
// delegation.provider fall back to the parent's route when empty. An absent fallback leaves Hermes
// to inherit only for an unpinned route; an explicit [] disables fallback.
// Non-empty fallbacks use native provider/model route objects.
//
// The projection writes exactly these three keys: fallback entries
// never mix into delegation.model, and delegation.backend /
// delegation.backends are left alone - backend routing is the session's
// concern, not the model selection's.
func ProjectDelegation(m SubagentModel) map[string]any {
	out := map[string]any{}
	if m.Model != "" {
		out["delegation.model"] = m.Model
	}
	if m.Provider != "" {
		out["delegation.provider"] = m.Provider
	}
	if m.Fallback != nil {
		out["delegation.fallback_providers"] = m.Fallback
	}
	return out
}
