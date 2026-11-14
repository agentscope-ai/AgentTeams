package resources

import (
	"encoding/json"
	"fmt"
)

// wireSpec is the adapter-facing (controller API) shape of a worker spec.
// It uses the camelCase JSON tags of agentteams-controller
// api/v1beta1.WorkerSpec, so the controller can unmarshal it directly on
// POST /internal/v1/workers/{name}/provision.
//
// Why this is not CanonicalSpecJSON: the hash input uses the frozen DDL
// column names (snake_case) so the digest is stable across API
// renames. The wire payload is the opposite — it must speak the controller
// API, not the service's storage schema.
type wireSpec struct {
	WorkerName    string `json:"workerName,omitempty"`
	Model         string `json:"model,omitempty"`
	ModelProvider string `json:"modelProvider,omitempty"`
	SubagentModel string `json:"subagentModel,omitempty"`

	LLMStreamFirstContentTimeout string `json:"llmStreamFirstContentTimeout,omitempty"`
	LLMStreamIdleTimeout         string `json:"llmStreamIdleTimeout,omitempty"`
	LLMMaxRetries                string `json:"llmMaxRetries,omitempty"`
	LLMBackoffBase               string `json:"llmBackoffBase,omitempty"`
	LLMBackoffCap                string `json:"llmBackoffCap,omitempty"`
	LLMMaxConcurrent             string `json:"llmMaxConcurrent,omitempty"`
	LLMMaxQPM                    string `json:"llmMaxQpm,omitempty"`
	LLMRateLimitPause            string `json:"llmRateLimitPause,omitempty"`
	LLMRateLimitJitter           string `json:"llmRateLimitJitter,omitempty"`
	LLMAcquireTimeout            string `json:"llmAcquireTimeout,omitempty"`

	Runtime  string `json:"runtime,omitempty"`
	Image    string `json:"image,omitempty"`
	Identity string `json:"identity,omitempty"`
	Soul     string `json:"soul,omitempty"`
	Agents   string `json:"agents,omitempty"`

	Skills        []string          `json:"skills,omitempty"`
	RemoteSkills  json.RawMessage   `json:"remoteSkills,omitempty"`
	MCPServers    json.RawMessage   `json:"mcpServers,omitempty"`
	Package       string            `json:"package,omitempty"`
	Expose        json.RawMessage   `json:"expose,omitempty"`
	ChannelPolicy json.RawMessage   `json:"channelPolicy,omitempty"`
	Resources     json.RawMessage   `json:"resources,omitempty"`
	Env           map[string]string `json:"env,omitempty"`

	// ContainerManaged is always emitted: false is meaningful (external
	// container) and the controller's *bool must receive it explicitly.
	ContainerManaged bool   `json:"containerManaged"`
	State            string `json:"state,omitempty"`
}

// WireSpec renders the spec as the controller-API JSON carried in the
// outbox provision/update payload. It validates the raw JSON
// columns so a broken value fails at enqueue, not inside the controller.
func WireSpec(s Spec) (json.RawMessage, error) {
	ws := wireSpec{
		WorkerName:    s.WorkerName,
		Model:         s.Model,
		ModelProvider: s.ModelProvider,
		SubagentModel: s.SubagentModel,

		LLMStreamFirstContentTimeout: s.LLMStreamFirstContentTimeout,
		LLMStreamIdleTimeout:         s.LLMStreamIdleTimeout,
		LLMMaxRetries:                s.LLMMaxRetries,
		LLMBackoffBase:               s.LLMBackoffBase,
		LLMBackoffCap:                s.LLMBackoffCap,
		LLMMaxConcurrent:             s.LLMMaxConcurrent,
		LLMMaxQPM:                    s.LLMMaxQPM,
		LLMRateLimitPause:            s.LLMRateLimitPause,
		LLMRateLimitJitter:           s.LLMRateLimitJitter,
		LLMAcquireTimeout:            s.LLMAcquireTimeout,

		Runtime:  s.Runtime,
		Image:    s.Image,
		Identity: s.Identity,
		Soul:     s.Soul,
		Agents:   s.Agents,

		Skills:        s.Skills,
		RemoteSkills:  s.RemoteSkills,
		MCPServers:    s.MCPServers,
		Package:       s.PackageRef,
		Expose:        s.Expose,
		ChannelPolicy: s.ChannelPolicy,
		Resources:     s.Resources,
		Env:           s.Env,

		ContainerManaged: s.ContainerManaged,
		State:            s.DesiredState,
	}
	for name, raw := range map[string]json.RawMessage{
		"remoteSkills":  ws.RemoteSkills,
		"mcpServers":    ws.MCPServers,
		"expose":        ws.Expose,
		"channelPolicy": ws.ChannelPolicy,
		"resources":     ws.Resources,
	} {
		if len(raw) > 0 && !json.Valid(raw) {
			return nil, fmt.Errorf("resources: spec field %s is not valid JSON", name)
		}
	}
	b, err := json.Marshal(ws)
	if err != nil {
		return nil, fmt.Errorf("resources: encode wire spec: %w", err)
	}
	return b, nil
}
