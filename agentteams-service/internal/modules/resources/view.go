package resources

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-service/internal/kernel/identity"
)

// WorkerResponse is the frozen public representation of one worker.
//
// Field names, JSON tags and DECLARATION ORDER are copied verbatim from
// agentteams-controller internal/server/types.go:92-146 (71dc233a2): the
// JSON encoding order is the struct declaration order, so any reordering
// here is a byte-level G1 regression. It has 43 top-level fields — the
// documented count of "45" also counted port/domain, which are in
// fact the nested fields of ExposedPortInfo, not top-level fields.
//
// Controller-typed fields (mcpServers / channelPolicy) are carried as
// json.RawMessage in this module: the service does not import the
// controller API package (separate Go modules), and raw passthrough keeps
// the controller's bytes intact.
type WorkerResponse struct {
	EnvEditable      bool              `json:"envEditable"`
	Env              map[string]string `json:"env,omitempty"`
	Name             string            `json:"name"`
	WorkerName       string            `json:"workerName,omitempty"`
	Phase            string            `json:"phase"`
	ContainerManaged bool              `json:"containerManaged"`
	State            string            `json:"state,omitempty"` // desired lifecycle state
	Model            string            `json:"model,omitempty"`
	// ModelProvider is the worker's LLM provider (an APIG Model API name).
	ModelProvider string `json:"modelProvider,omitempty"`
	// SubagentModel is the model used by spawned subagents ("" = inherit
	// the worker's primary model).
	SubagentModel                string `json:"subagentModel,omitempty"`
	LlmStreamFirstContentTimeout string `json:"llmStreamFirstContentTimeout,omitempty"`
	LlmStreamIdleTimeout         string `json:"llmStreamIdleTimeout,omitempty"`
	LlmMaxRetries                string `json:"llmMaxRetries,omitempty"`
	LlmBackoffBase               string `json:"llmBackoffBase,omitempty"`
	LlmBackoffCap                string `json:"llmBackoffCap,omitempty"`
	LlmMaxConcurrent             string `json:"llmMaxConcurrent,omitempty"`
	LlmMaxQpm                    string `json:"llmMaxQpm,omitempty"`
	LlmRateLimitPause            string `json:"llmRateLimitPause,omitempty"`
	LlmRateLimitJitter           string `json:"llmRateLimitJitter,omitempty"`
	LlmAcquireTimeout            string `json:"llmAcquireTimeout,omitempty"`
	Runtime                      string `json:"runtime,omitempty"`
	// RuntimeDeprecated marks legacy runtimes that are upgrade-compat only
	// (CoPaw): new creation is rejected; migrate to QwenPaw.
	RuntimeDeprecated bool     `json:"runtimeDeprecated,omitempty"`
	Image             string   `json:"image,omitempty"`
	Identity          string   `json:"identity,omitempty"`
	Soul              string   `json:"soul,omitempty"`
	Agents            string   `json:"agents,omitempty"`
	Skills            []string `json:"skills,omitempty"`
	// McpServers is []v1beta1.MCPServer in the controller; raw JSON here
	// (see the type comment above).
	McpServers     json.RawMessage `json:"mcpServers,omitempty"`
	Package        string          `json:"package,omitempty"`
	BackendRuntime string          `json:"backendRuntime,omitempty"`
	// ChannelPolicy is *v1beta1.ChannelPolicySpec in the controller.
	ChannelPolicy  json.RawMessage `json:"channelPolicy,omitempty"`
	ContainerState string          `json:"containerState,omitempty"`
	MatrixUserID   string          `json:"matrixUserID,omitempty"`
	RoomID         string          `json:"roomID,omitempty"`
	Message        string          `json:"message,omitempty"`
	LastActiveAt   string          `json:"lastActiveAt,omitempty"`
	// AgentStatus is the runtime task-level state reported by the worker
	// heartbeat: "idle" / "running" / "disabled"; empty = not reported.
	AgentStatus      string            `json:"agentStatus,omitempty"`
	RunningTaskCount *int              `json:"runningTaskCount,omitempty"`
	LastRunAt        string            `json:"lastRunAt,omitempty"`
	LastFinishAt     string            `json:"lastFinishAt,omitempty"`
	ExposedPorts     []ExposedPortInfo `json:"exposedPorts,omitempty"`
	Team             string            `json:"team,omitempty"`
	Role             string            `json:"role,omitempty"`
}

// ExposedPortInfo mirrors agentteams-controller types.go:148-151.
type ExposedPortInfo struct {
	Port   int    `json:"port"`
	Domain string `json:"domain"`
}

// WorkerListResponse mirrors the controller's worker list envelope
// (types.go:153-156). Workers is never nil so an empty list encodes as
// "workers": [] (not null).
type WorkerListResponse struct {
	Workers []WorkerResponse `json:"workers"`
	Total   int              `json:"total"`
}

// WorkerSpecView mirrors the controller's fields=spec projection
// (types.go:194-222): the stable configuration subset, in WorkerResponse
// field order, excluding soul/agents/env/skills/mcpServers/channelPolicy
// and every status field. It deliberately does NOT carry modelProvider
// (the controller source does not).
type WorkerSpecView struct {
	Name                         string            `json:"name"`
	WorkerName                   string            `json:"workerName,omitempty"`
	ContainerManaged             bool              `json:"containerManaged"`
	State                        string            `json:"state,omitempty"`
	Model                        string            `json:"model,omitempty"`
	SubagentModel                string            `json:"subagentModel,omitempty"`
	LlmStreamFirstContentTimeout string            `json:"llmStreamFirstContentTimeout,omitempty"`
	LlmStreamIdleTimeout         string            `json:"llmStreamIdleTimeout,omitempty"`
	LlmMaxRetries                string            `json:"llmMaxRetries,omitempty"`
	LlmBackoffBase               string            `json:"llmBackoffBase,omitempty"`
	LlmBackoffCap                string            `json:"llmBackoffCap,omitempty"`
	LlmMaxConcurrent             string            `json:"llmMaxConcurrent,omitempty"`
	LlmMaxQpm                    string            `json:"llmMaxQpm,omitempty"`
	LlmRateLimitPause            string            `json:"llmRateLimitPause,omitempty"`
	LlmRateLimitJitter           string            `json:"llmRateLimitJitter,omitempty"`
	LlmAcquireTimeout            string            `json:"llmAcquireTimeout,omitempty"`
	Runtime                      string            `json:"runtime,omitempty"`
	RuntimeDeprecated            bool              `json:"runtimeDeprecated,omitempty"`
	Image                        string            `json:"image,omitempty"`
	Identity                     string            `json:"identity,omitempty"`
	Package                      string            `json:"package,omitempty"`
	BackendRuntime               string            `json:"backendRuntime,omitempty"`
	MatrixUserID                 string            `json:"matrixUserID,omitempty"`
	RoomID                       string            `json:"roomID,omitempty"`
	ExposedPorts                 []ExposedPortInfo `json:"exposedPorts,omitempty"`
	Team                         string            `json:"team,omitempty"`
	Role                         string            `json:"role,omitempty"`
}

// WorkerStatusView mirrors the controller's fields=status projection
// (types.go:228-241).
type WorkerStatusView struct {
	Name             string `json:"name"`
	WorkerName       string `json:"workerName,omitempty"`
	Phase            string `json:"phase"`
	ContainerState   string `json:"containerState,omitempty"`
	Message          string `json:"message,omitempty"`
	LastActiveAt     string `json:"lastActiveAt,omitempty"`
	AgentStatus      string `json:"agentStatus,omitempty"`
	RunningTaskCount *int   `json:"runningTaskCount,omitempty"`
	LastRunAt        string `json:"lastRunAt,omitempty"`
	LastFinishAt     string `json:"lastFinishAt,omitempty"`
	Team             string `json:"team,omitempty"`
	Role             string `json:"role,omitempty"`
}

// WorkerSpecStatusView mirrors the controller's fields=spec,status
// projection (types.go:245-281): the union of the two views in
// WorkerResponse field order.
type WorkerSpecStatusView struct {
	Name                         string            `json:"name"`
	WorkerName                   string            `json:"workerName,omitempty"`
	Phase                        string            `json:"phase"`
	ContainerManaged             bool              `json:"containerManaged"`
	State                        string            `json:"state,omitempty"`
	Model                        string            `json:"model,omitempty"`
	SubagentModel                string            `json:"subagentModel,omitempty"`
	LlmStreamFirstContentTimeout string            `json:"llmStreamFirstContentTimeout,omitempty"`
	LlmStreamIdleTimeout         string            `json:"llmStreamIdleTimeout,omitempty"`
	LlmMaxRetries                string            `json:"llmMaxRetries,omitempty"`
	LlmBackoffBase               string            `json:"llmBackoffBase,omitempty"`
	LlmBackoffCap                string            `json:"llmBackoffCap,omitempty"`
	LlmMaxConcurrent             string            `json:"llmMaxConcurrent,omitempty"`
	LlmMaxQpm                    string            `json:"llmMaxQpm,omitempty"`
	LlmRateLimitPause            string            `json:"llmRateLimitPause,omitempty"`
	LlmRateLimitJitter           string            `json:"llmRateLimitJitter,omitempty"`
	LlmAcquireTimeout            string            `json:"llmAcquireTimeout,omitempty"`
	Runtime                      string            `json:"runtime,omitempty"`
	RuntimeDeprecated            bool              `json:"runtimeDeprecated,omitempty"`
	Image                        string            `json:"image,omitempty"`
	Identity                     string            `json:"identity,omitempty"`
	Package                      string            `json:"package,omitempty"`
	BackendRuntime               string            `json:"backendRuntime,omitempty"`
	ContainerState               string            `json:"containerState,omitempty"`
	MatrixUserID                 string            `json:"matrixUserID,omitempty"`
	RoomID                       string            `json:"roomID,omitempty"`
	Message                      string            `json:"message,omitempty"`
	LastActiveAt                 string            `json:"lastActiveAt,omitempty"`
	AgentStatus                  string            `json:"agentStatus,omitempty"`
	RunningTaskCount             *int              `json:"runningTaskCount,omitempty"`
	LastRunAt                    string            `json:"lastRunAt,omitempty"`
	LastFinishAt                 string            `json:"lastFinishAt,omitempty"`
	ExposedPorts                 []ExposedPortInfo `json:"exposedPorts,omitempty"`
	Team                         string            `json:"team,omitempty"`
	Role                         string            `json:"role,omitempty"`
}

// Response projects a res.workers row to the frozen public representation.
//
// Mirrors controller workerToResponse (resource_handler.go:1813-1863) plus
// the read-path reduction of ListWorkers/GetWorker:
//
//   - env is present only for admin/manager; everybody else gets nil and
//     envEditable=false (worker_env.go:16-20);
//   - an empty observed phase reads as "Pending";
//   - containerManaged comes from the row (the NOT NULL DEFAULT 1 column);
//   - exposedPorts is parsed from the stored expose JSON (see
//     parseExposedPorts for the R1 shape assumption);
//   - team is the cached first-hit team slug; role is always "",
//     because R1 has no membership-role column — the controller derives it
//     from teams.spec.workerMembers (resource_handler.go:2277-2288). R2
//     restores it with team_uid.
//
// R1 timestamp assumption (G1 open point): the controller responds with
// lastActiveAt/lastRunAt/lastFinishAt as strings copied from the CR status
// (already RFC3339). The service stores DATETIME(6) and renders RFC3339Nano
// in UTC. If the G1 golden shows a different controller rendering, change
// formatTime only.
func (w *Worker) Response(caller *identity.Identity) WorkerResponse {
	canManageEnv := caller != nil && caller.CanManageEnv()

	phase := w.ObservedPhase
	if phase == "" {
		phase = "Pending"
	}
	// The controller's WorkerSpec.DesiredState() defaults to "Running"
	// when spec.state is unset (api/v1beta1/types.go:374-379).
	state := w.DesiredState
	if state == "" {
		state = "Running"
	}

	resp := WorkerResponse{
		EnvEditable:                  canManageEnv,
		Name:                         w.Name,
		WorkerName:                   w.WorkerName,
		Phase:                        phase,
		ContainerManaged:             w.ContainerManaged,
		State:                        state,
		Model:                        w.Model,
		ModelProvider:                w.ModelProvider,
		SubagentModel:                w.SubagentModel,
		LlmStreamFirstContentTimeout: w.LLMStreamFirstContentTimeout,
		LlmStreamIdleTimeout:         w.LLMStreamIdleTimeout,
		LlmMaxRetries:                w.LLMMaxRetries,
		LlmBackoffBase:               w.LLMBackoffBase,
		LlmBackoffCap:                w.LLMBackoffCap,
		LlmMaxConcurrent:             w.LLMMaxConcurrent,
		LlmMaxQpm:                    w.LLMMaxQPM,
		LlmRateLimitPause:            w.LLMRateLimitPause,
		LlmRateLimitJitter:           w.LLMRateLimitJitter,
		LlmAcquireTimeout:            w.LLMAcquireTimeout,
		Runtime:                      w.Runtime,
		RuntimeDeprecated:            strings.EqualFold(w.Runtime, runtimeCopaw),
		Image:                        w.Image,
		Identity:                     w.Identity,
		Soul:                         w.Soul,
		Agents:                       w.Agents,
		Skills:                       w.Skills,
		McpServers:                   w.MCPServers,
		Package:                      w.PackageRef,
		// BackendRuntime has no res.workers column in R1; the controller
		// only echoes an explicitly-set spec.backendRuntime (normally ""),
		// so the empty/omitted value matches the common case.
		ChannelPolicy:    w.ChannelPolicy,
		ContainerState:   w.ObservedContainerState,
		MatrixUserID:     w.MatrixUserID,
		RoomID:           w.RoomID,
		Message:          w.Message,
		LastActiveAt:     formatTime(w.LastActiveAt),
		AgentStatus:      w.AgentStatus,
		RunningTaskCount: w.RunningTaskCount,
		LastRunAt:        formatTime(w.LastRunAt),
		LastFinishAt:     formatTime(w.LastFinishAt),
		// ExposedPorts has no R1 column: the controller reports it only in
		// status (populated by reconcile), never from the request. Deriving it
		// from spec.expose made create responses diverge from the controller
		// and poisoned the spec ETag. R2 adds the observed column.
		ExposedPorts: nil,
		Team:         w.TeamSlug,
		// Role: see the doc comment — no R1 column.
		Role: "",
	}
	if canManageEnv {
		resp.Env = w.Env
	}
	return resp
}

// ResponseBytes renders the response exactly like
// httputil.WriteJSON/json.Encoder: compact JSON plus one trailing newline.
func (w *Worker) ResponseBytes(caller *identity.Identity) ([]byte, error) {
	b, err := json.Marshal(w.Response(caller))
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// SpecView projects a full response down to the fields=spec view.
func (r WorkerResponse) SpecView() WorkerSpecView {
	return WorkerSpecView{
		Name:                         r.Name,
		WorkerName:                   r.WorkerName,
		ContainerManaged:             r.ContainerManaged,
		State:                        r.State,
		Model:                        r.Model,
		SubagentModel:                r.SubagentModel,
		LlmStreamFirstContentTimeout: r.LlmStreamFirstContentTimeout,
		LlmStreamIdleTimeout:         r.LlmStreamIdleTimeout,
		LlmMaxRetries:                r.LlmMaxRetries,
		LlmBackoffBase:               r.LlmBackoffBase,
		LlmBackoffCap:                r.LlmBackoffCap,
		LlmMaxConcurrent:             r.LlmMaxConcurrent,
		LlmMaxQpm:                    r.LlmMaxQpm,
		LlmRateLimitPause:            r.LlmRateLimitPause,
		LlmRateLimitJitter:           r.LlmRateLimitJitter,
		LlmAcquireTimeout:            r.LlmAcquireTimeout,
		Runtime:                      r.Runtime,
		RuntimeDeprecated:            r.RuntimeDeprecated,
		Image:                        r.Image,
		Identity:                     r.Identity,
		Package:                      r.Package,
		BackendRuntime:               r.BackendRuntime,
		MatrixUserID:                 r.MatrixUserID,
		RoomID:                       r.RoomID,
		ExposedPorts:                 r.ExposedPorts,
		Team:                         r.Team,
		Role:                         r.Role,
	}
}

// StatusView projects a full response down to the fields=status view.
func (r WorkerResponse) StatusView() WorkerStatusView {
	return WorkerStatusView{
		Name:             r.Name,
		WorkerName:       r.WorkerName,
		Phase:            r.Phase,
		ContainerState:   r.ContainerState,
		Message:          r.Message,
		LastActiveAt:     r.LastActiveAt,
		AgentStatus:      r.AgentStatus,
		RunningTaskCount: r.RunningTaskCount,
		LastRunAt:        r.LastRunAt,
		LastFinishAt:     r.LastFinishAt,
		Team:             r.Team,
		Role:             r.Role,
	}
}

// SpecStatusView projects a full response down to the fields=spec,status
// view.
func (r WorkerResponse) SpecStatusView() WorkerSpecStatusView {
	return WorkerSpecStatusView{
		Name:                         r.Name,
		WorkerName:                   r.WorkerName,
		Phase:                        r.Phase,
		ContainerManaged:             r.ContainerManaged,
		State:                        r.State,
		Model:                        r.Model,
		SubagentModel:                r.SubagentModel,
		LlmStreamFirstContentTimeout: r.LlmStreamFirstContentTimeout,
		LlmStreamIdleTimeout:         r.LlmStreamIdleTimeout,
		LlmMaxRetries:                r.LlmMaxRetries,
		LlmBackoffBase:               r.LlmBackoffBase,
		LlmBackoffCap:                r.LlmBackoffCap,
		LlmMaxConcurrent:             r.LlmMaxConcurrent,
		LlmMaxQpm:                    r.LlmMaxQpm,
		LlmRateLimitPause:            r.LlmRateLimitPause,
		LlmRateLimitJitter:           r.LlmRateLimitJitter,
		LlmAcquireTimeout:            r.LlmAcquireTimeout,
		Runtime:                      r.Runtime,
		RuntimeDeprecated:            r.RuntimeDeprecated,
		Image:                        r.Image,
		Identity:                     r.Identity,
		Package:                      r.Package,
		BackendRuntime:               r.BackendRuntime,
		ContainerState:               r.ContainerState,
		MatrixUserID:                 r.MatrixUserID,
		RoomID:                       r.RoomID,
		Message:                      r.Message,
		LastActiveAt:                 r.LastActiveAt,
		AgentStatus:                  r.AgentStatus,
		RunningTaskCount:             r.RunningTaskCount,
		LastRunAt:                    r.LastRunAt,
		LastFinishAt:                 r.LastFinishAt,
		ExposedPorts:                 r.ExposedPorts,
		Team:                         r.Team,
		Role:                         r.Role,
	}
}

// formatTime renders a nullable DB timestamp as RFC3339Nano in UTC.
// See the R1 timestamp assumption on Response.
func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// exposedPortWire is the tolerant union of the two port shapes that can
// reach expose_json: the request-side spec.expose entry (port, protocol;
// api/v1beta1/types.go:391-394) and the controller status.exposedPorts
