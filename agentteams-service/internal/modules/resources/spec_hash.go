package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// SpecHash returns the canonical spec digest: sha256 over
// CanonicalSpecJSON, hex encoded.
//
// The digest tracks the canonical spec only: observed_*, generation,
// version, spec_hash and the timestamps never participate, so status
// writes cannot move it and UpdateSpec can use it as the generation
// signal.
func SpecHash(s Spec) (string, error) {
	b, err := CanonicalSpecJSON(s)
	if err != nil {
		return "", err
	}
	return hashCanonicalSpec(b), nil
}

// CanonicalSpecJSON returns the normalised JSON that SpecHash hashes
// (exposed for tests and debugging).
//
// Key names are the res.workers column names (snake_case).
// generation / spec_hash / version / created_at / updated_at / deleted_at
// and every observed_* column are excluded: the Spec type does not carry
// them, so status writes can never move the digest.
//
// Empty strings, nil/empty slices, nil/empty maps and nil json.RawMessage
// values are omitted. container_managed is always emitted because false is
// a meaningful value (externally managed container), not "unset".
//
// JSON columns are unmarshalled and re-marshalled so whitespace and object
// key order in the caller's RawMessage cannot change the digest; invalid
// JSON is an error. encoding/json sorts map keys, which makes the output
// byte-stable.
func CanonicalSpecJSON(s Spec) ([]byte, error) {
	m := make(map[string]any, 32)

	str := func(key, value string) {
		if value != "" {
			m[key] = value
		}
	}
	raw := func(key string, value json.RawMessage) error {
		if len(value) == 0 {
			return nil
		}
		var v any
		if err := json.Unmarshal(value, &v); err != nil {
			return fmt.Errorf("resources: spec field %s is not valid JSON: %w", key, err)
		}
		m[key] = v
		return nil
	}

	str("worker_name", s.WorkerName)
	str("team_slug", s.TeamSlug)
	str("model", s.Model)
	str("model_provider", s.ModelProvider)
	str("subagent_model", s.SubagentModel)

	str("llm_stream_first_content_timeout", s.LLMStreamFirstContentTimeout)
	str("llm_stream_idle_timeout", s.LLMStreamIdleTimeout)
	str("llm_max_retries", s.LLMMaxRetries)
	str("llm_backoff_base", s.LLMBackoffBase)
	str("llm_backoff_cap", s.LLMBackoffCap)
	str("llm_max_concurrent", s.LLMMaxConcurrent)
	str("llm_max_qpm", s.LLMMaxQPM)
	str("llm_rate_limit_pause", s.LLMRateLimitPause)
	str("llm_rate_limit_jitter", s.LLMRateLimitJitter)
	str("llm_acquire_timeout", s.LLMAcquireTimeout)

	str("runtime", s.Runtime)
	str("image", s.Image)
	str("identity", s.Identity)
	str("soul", s.Soul)
	str("agents", s.Agents)

	if len(s.Skills) > 0 {
		m["skills_json"] = s.Skills
	}
	if err := raw("remote_skills_json", s.RemoteSkills); err != nil {
		return nil, err
	}
	if err := raw("mcp_servers_json", s.MCPServers); err != nil {
		return nil, err
	}
	str("package_ref", s.PackageRef)
	if err := raw("expose_json", s.Expose); err != nil {
		return nil, err
	}
	if err := raw("channel_policy_json", s.ChannelPolicy); err != nil {
		return nil, err
	}
	if err := raw("resources_json", s.Resources); err != nil {
		return nil, err
	}
	if len(s.Env) > 0 {
		m["env_json"] = s.Env
	}

	// Always present: false is a real value, not an absent one.
	m["container_managed"] = s.ContainerManaged
	str("desired_state", s.DesiredState)

	return json.Marshal(m)
}

// hashCanonicalSpec hashes an already canonicalised spec JSON document.
func hashCanonicalSpec(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
