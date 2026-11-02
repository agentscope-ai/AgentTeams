package hermes

// SubagentModel is the AgentTeams-side subagent model selection, the
// input of the delegation projection. The zero value of a field means
// "not configured".
type SubagentModel struct {
	Model    string
	Provider string
	Fallback []string
}

// ProjectDelegation maps a SubagentModel to the delegation.* keys to
// write into the worker's config.yaml.
//
// An unset field omits its key entirely; the key is never written with
// an empty value. Omission is the only form that preserves Hermes'
// inheritance semantics: delegation.model and delegation.provider fall
// back to the parent's route when empty (plugins/delegation/default),
// and delegation.fallback_providers inherits the parent's chain when
// absent - while an explicit [] disables fallback, a different meaning
// (tools/delegate_tool_routes, _with_inherited_fallback).
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
	if len(m.Fallback) > 0 {
		out["delegation.fallback_providers"] = m.Fallback
	}
	return out
}
