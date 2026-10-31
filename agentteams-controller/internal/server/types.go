package server

import (
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
)

// --- Worker API types ---

type CreateWorkerRequest struct {
	Env                          map[string]string                  `json:"env,omitempty"`
	Name                         string                             `json:"name"`
	WorkerName                   string                             `json:"workerName,omitempty"`
	Model                        string                             `json:"model,omitempty"`
	SubagentModel                string                             `json:"subagentModel,omitempty"`
	LlmStreamFirstContentTimeout string                             `json:"llmStreamFirstContentTimeout,omitempty"`
	LlmStreamIdleTimeout         string                             `json:"llmStreamIdleTimeout,omitempty"`
	ModelProvider                string                             `json:"modelProvider,omitempty"`
	Runtime                      string                             `json:"runtime,omitempty"`
	Image                        string                             `json:"image,omitempty"`
	Identity                     string                             `json:"identity,omitempty"`
	Soul                         string                             `json:"soul,omitempty"`
	Agents                       string                             `json:"agents,omitempty"`
	Skills                       []string                           `json:"skills,omitempty"`
	RemoteSkills                 []v1beta1.RemoteSkillSource        `json:"remoteSkills,omitempty"`
	McpServers                   []v1beta1.MCPServer                `json:"mcpServers,omitempty"`
	Package                      string                             `json:"package,omitempty"`
	Expose                       []v1beta1.ExposePort               `json:"expose,omitempty"`
	ChannelPolicy                *v1beta1.ChannelPolicySpec         `json:"channelPolicy,omitempty"`
	Resources                    *v1beta1.AgentResourceRequirements `json:"resources,omitempty"`
	LlmMaxRetries                string                             `json:"llmMaxRetries,omitempty"`
	LlmBackoffBase               string                             `json:"llmBackoffBase,omitempty"`
	LlmBackoffCap                string                             `json:"llmBackoffCap,omitempty"`
	LlmMaxConcurrent             string                             `json:"llmMaxConcurrent,omitempty"`
	LlmMaxQpm                    string                             `json:"llmMaxQpm,omitempty"`
	LlmRateLimitPause            string                             `json:"llmRateLimitPause,omitempty"`
	LlmRateLimitJitter           string                             `json:"llmRateLimitJitter,omitempty"`
	LlmAcquireTimeout            string                             `json:"llmAcquireTimeout,omitempty"`

	// ContainerManaged indicates whether the controller should manage
	// container lifecycle for this worker. When false, container
	// reconciliation is skipped entirely (for remote/pip workers).
	// Default is true (controller manages container).
	ContainerManaged *bool   `json:"containerManaged,omitempty"`
	State            *string `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped
}

type UpdateWorkerRequest struct {
	Env        map[string]string `json:"env,omitempty"`
	WorkerName string            `json:"workerName,omitempty"`
	Model      string            `json:"model,omitempty"`
	// SubagentModel is a pointer so callers can distinguish omission (nil:
	// keep the current value) from an explicit clear ("" — the worker
	// returns to inheriting the team default / the runtime default).
	SubagentModel *string `json:"subagentModel,omitempty"`
	// LlmStream* follow the same pointer semantics (nil = keep, "" = clear
	// back to the team default / registry default).
	LlmStreamFirstContentTimeout *string                            `json:"llmStreamFirstContentTimeout,omitempty"`
	LlmStreamIdleTimeout         *string                            `json:"llmStreamIdleTimeout,omitempty"`
	LlmMaxRetries                *string                            `json:"llmMaxRetries,omitempty"`
	LlmBackoffBase               *string                            `json:"llmBackoffBase,omitempty"`
	LlmBackoffCap                *string                            `json:"llmBackoffCap,omitempty"`
	LlmMaxConcurrent             *string                            `json:"llmMaxConcurrent,omitempty"`
	LlmMaxQpm                    *string                            `json:"llmMaxQpm,omitempty"`
	LlmRateLimitPause            *string                            `json:"llmRateLimitPause,omitempty"`
	LlmRateLimitJitter           *string                            `json:"llmRateLimitJitter,omitempty"`
	LlmAcquireTimeout            *string                            `json:"llmAcquireTimeout,omitempty"`
	ModelProvider                string                             `json:"modelProvider,omitempty"`
	Runtime                      string                             `json:"runtime,omitempty"`
	Image                        string                             `json:"image,omitempty"`
	Identity                     string                             `json:"identity,omitempty"`
	Soul                         string                             `json:"soul,omitempty"`
	Agents                       string                             `json:"agents,omitempty"`
	Skills                       []string                           `json:"skills,omitempty"`
	RemoteSkills                 []v1beta1.RemoteSkillSource        `json:"remoteSkills,omitempty"`
	McpServers                   []v1beta1.MCPServer                `json:"mcpServers,omitempty"`
	Package                      string                             `json:"package,omitempty"`
	Expose                       []v1beta1.ExposePort               `json:"expose,omitempty"`
	ChannelPolicy                *v1beta1.ChannelPolicySpec         `json:"channelPolicy,omitempty"`
	Resources                    *v1beta1.AgentResourceRequirements `json:"resources,omitempty"`

	// ContainerManaged indicates whether the controller should manage
	// container lifecycle for this worker. When false, container
	// reconciliation is skipped entirely (for remote/pip workers).
	// Default is true (controller manages container).
	ContainerManaged *bool   `json:"containerManaged,omitempty"`
	State            *string `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped
}

type WorkerResponse struct {
	EnvEditable      bool              `json:"envEditable"`
	Env              map[string]string `json:"env,omitempty"`
	Name             string            `json:"name"`
	WorkerName       string            `json:"workerName,omitempty"`
	Phase            string            `json:"phase"`
	ContainerManaged bool              `json:"containerManaged"`
	State            string            `json:"state,omitempty"` // desired lifecycle state
	Model            string            `json:"model,omitempty"`
	// SubagentModel is the model used by spawned subagents ("" = inherit
	// the worker's primary model). See WorkerSpec.SubagentModel.
	SubagentModel string `json:"subagentModel,omitempty"`
	// LlmStream* mirror WorkerSpec ("" = unset). See WorkerSpec.LlmStream*.
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
	// RuntimeDeprecated marks legacy runtimes that are upgrade-compat
	// only (CoPaw): new creation is rejected; migrate to QwenPaw.
	RuntimeDeprecated bool                       `json:"runtimeDeprecated,omitempty"`
	Image             string                     `json:"image,omitempty"`
	Identity          string                     `json:"identity,omitempty"`
	Soul              string                     `json:"soul,omitempty"`
	Agents            string                     `json:"agents,omitempty"`
	Skills            []string                   `json:"skills,omitempty"`
	McpServers        []v1beta1.MCPServer        `json:"mcpServers,omitempty"`
	Package           string                     `json:"package,omitempty"`
	BackendRuntime    string                     `json:"backendRuntime,omitempty"`
	ChannelPolicy     *v1beta1.ChannelPolicySpec `json:"channelPolicy,omitempty"`
	ContainerState    string                     `json:"containerState,omitempty"`
	MatrixUserID      string                     `json:"matrixUserID,omitempty"`
	RoomID            string                     `json:"roomID,omitempty"`
	Message           string                     `json:"message,omitempty"`
	LastActiveAt      string                     `json:"lastActiveAt,omitempty"`
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

type ExposedPortInfo struct {
	Port   int    `json:"port"`
	Domain string `json:"domain"`
}

type WorkerListResponse struct {
	Workers []WorkerResponse `json:"workers"`
	Total   int              `json:"total"`
}

// workerListProjection is the GET /api/v1/workers envelope for a ?fields=
// projection: the same workers/total wrapping as WorkerListResponse, with
// each row already reduced to the requested view (WorkerSpecView,
// WorkerStatusView, or WorkerSpecStatusView).
type workerListProjection struct {
	Workers []any `json:"workers"`
	Total   int   `json:"total"`
}

// --- Worker list projection views (GET /api/v1/workers?fields=...) ---
//
// The views project a WorkerResponse down to a stable subset of its fields
// for list consumers that do not need the full payload. Each view is a
// struct (never a map) whose fields follow WorkerResponse's order, so the
// JSON encoding is byte-stable — issue #1475's ETag work hashes these
// projections.
//
// Split (issue #1475):
//   - spec:   stable configuration — identity, model and LLM tuning,
//     runtime, package, exposed ports, team membership. Excludes prompt
//     bodies and full configuration (soul/agents/env/skills/mcpServers/
//     channelPolicy) and every status field.
//   - status: volatile runtime state — phase, container state, heartbeat
//     and task fields.
//   - all:    the full WorkerResponse (the default; not a projection).
//
// team and role belong to both views: a membership change is a spec change
// as well.

// WorkerSpecView is the fields=spec projection of a worker.
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

// WorkerStatusView is the fields=status projection of a worker.
type WorkerStatusView struct {
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

// WorkerSpecStatusView is the fields=spec,status projection: the union of
// WorkerSpecView and WorkerStatusView in WorkerResponse field order.
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

// SpecView projects a full worker response down to its stable
// configuration fields (GET /api/v1/workers?fields=spec).
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

// StatusView projects a full worker response down to its volatile runtime
// fields (GET /api/v1/workers?fields=status).
func (r WorkerResponse) StatusView() WorkerStatusView {
	return WorkerStatusView{
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

// SpecStatusView projects a full worker response down to the spec+status
// union (GET /api/v1/workers?fields=spec,status).
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

// --- Team API types ---

type CreateTeamRequest struct {
	Name                         string                     `json:"name"`
	TeamName                     string                     `json:"teamName,omitempty"`
	Description                  string                     `json:"description,omitempty"`
	Admin                        *v1beta1.TeamAdminSpec     `json:"admin,omitempty"`
	HumanMembers                 []v1beta1.TeamMemberSpec   `json:"humanMembers,omitempty"`
	WorkerMembers                []v1beta1.TeamWorkerRef    `json:"workerMembers"`
	HeartbeatEvery               string                     `json:"heartbeatEvery,omitempty"`
	LlmStreamFirstContentTimeout string                     `json:"llmStreamFirstContentTimeout,omitempty"`
	LlmStreamIdleTimeout         string                     `json:"llmStreamIdleTimeout,omitempty"`
	LlmMaxRetries                string                     `json:"llmMaxRetries,omitempty"`
	LlmBackoffBase               string                     `json:"llmBackoffBase,omitempty"`
	LlmBackoffCap                string                     `json:"llmBackoffCap,omitempty"`
	LlmMaxConcurrent             string                     `json:"llmMaxConcurrent,omitempty"`
	LlmMaxQpm                    string                     `json:"llmMaxQpm,omitempty"`
	LlmRateLimitPause            string                     `json:"llmRateLimitPause,omitempty"`
	LlmRateLimitJitter           string                     `json:"llmRateLimitJitter,omitempty"`
	LlmAcquireTimeout            string                     `json:"llmAcquireTimeout,omitempty"`
	PeerMentions                 *bool                      `json:"peerMentions,omitempty"`
	ChannelPolicy                *v1beta1.ChannelPolicySpec `json:"channelPolicy,omitempty"`
}

type UpdateTeamRequest struct {
	TeamName                     string                     `json:"teamName,omitempty"`
	Description                  string                     `json:"description,omitempty"`
	Admin                        *v1beta1.TeamAdminSpec     `json:"admin,omitempty"`
	HumanMembers                 []v1beta1.TeamMemberSpec   `json:"humanMembers,omitempty"`
	WorkerMembers                []v1beta1.TeamWorkerRef    `json:"workerMembers,omitempty"`
	HeartbeatEvery               *string                    `json:"heartbeatEvery,omitempty"`
	LlmStreamFirstContentTimeout *string                    `json:"llmStreamFirstContentTimeout,omitempty"`
	LlmStreamIdleTimeout         *string                    `json:"llmStreamIdleTimeout,omitempty"`
	LlmMaxRetries                *string                    `json:"llmMaxRetries,omitempty"`
	LlmBackoffBase               *string                    `json:"llmBackoffBase,omitempty"`
	LlmBackoffCap                *string                    `json:"llmBackoffCap,omitempty"`
	LlmMaxConcurrent             *string                    `json:"llmMaxConcurrent,omitempty"`
	LlmMaxQpm                    *string                    `json:"llmMaxQpm,omitempty"`
	LlmRateLimitPause            *string                    `json:"llmRateLimitPause,omitempty"`
	LlmRateLimitJitter           *string                    `json:"llmRateLimitJitter,omitempty"`
	LlmAcquireTimeout            *string                    `json:"llmAcquireTimeout,omitempty"`
	PeerMentions                 *bool                      `json:"peerMentions,omitempty"`
	ChannelPolicy                *v1beta1.ChannelPolicySpec `json:"channelPolicy,omitempty"`
}

type TeamResponse struct {
	Name               string                       `json:"name"`
	TeamName           string                       `json:"teamName,omitempty"`
	Phase              string                       `json:"phase"`
	Description        string                       `json:"description,omitempty"`
	Admin              *v1beta1.TeamAdminSpec       `json:"admin,omitempty"`
	HumanMembers       []v1beta1.TeamMemberSpec     `json:"humanMembers,omitempty"`
	WorkerMembers      []v1beta1.TeamWorkerRef      `json:"workerMembers"`
	LeaderName         string                       `json:"leaderName"`
	HeartbeatEvery     string                       `json:"heartbeatEvery,omitempty"`
	TeamRoomID         string                       `json:"teamRoomID,omitempty"`
	LeaderDMRoomID     string                       `json:"leaderDMRoomID,omitempty"`
	LeaderReady        bool                         `json:"leaderReady"`
	ReadyWorkers       int                          `json:"readyWorkers"`
	TotalWorkers       int                          `json:"totalWorkers"`
	Message            string                       `json:"message,omitempty"`
	WorkerNames        []string                     `json:"workerNames,omitempty"`
	WorkerExposedPorts map[string][]ExposedPortInfo `json:"workerExposedPorts,omitempty"`
	// SubagentModel exposes the team-wide default (Team.spec.subagentModel) so
	// frontends can display the model members inherit; per-worker values stay
	// on WorkerResponse.
	SubagentModel string `json:"subagentModel,omitempty"`
	// LlmStream* expose the team-wide defaults (Team.spec.llmStream*) so
	// frontends can display the values members inherit.
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
}

type TeamListResponse struct {
	Teams []TeamResponse `json:"teams"`
	Total int            `json:"total"`
}

// --- Human API types ---

type CreateHumanRequest struct {
	Name              string   `json:"name"`
	DisplayName       string   `json:"displayName"`
	Email             string   `json:"email,omitempty"`
	PermissionLevel   int      `json:"permissionLevel"`
	AccessibleTeams   []string `json:"accessibleTeams,omitempty"`
	AccessibleWorkers []string `json:"accessibleWorkers,omitempty"`
	Note              string   `json:"note,omitempty"`
}

// UpdateHumanRequest is the merge-patch body of PUT /api/v1/humans/{name}.
// Pointer fields: absent = unchanged, present = replace (an empty non-nil
// slice clears the list). Matrix identity (name / username) is not
// updatable — changing it re-provisions the account.
type UpdateHumanRequest struct {
	DisplayName       *string   `json:"displayName,omitempty"`
	Email             *string   `json:"email,omitempty"`
	PermissionLevel   *int      `json:"permissionLevel,omitempty"`
	AccessibleTeams   *[]string `json:"accessibleTeams,omitempty"`
	AccessibleWorkers *[]string `json:"accessibleWorkers,omitempty"`
	Note              *string   `json:"note,omitempty"`
	// Capabilities follows the same merge-patch semantics as
	// AccessibleTeams: absent = unchanged, explicit list = replaces,
	// empty list = clears. Unknown values are rejected (400).
	Capabilities *[]string `json:"capabilities,omitempty"`
}

type HumanResponse struct {
	Name              string   `json:"name"`
	Phase             string   `json:"phase"`
	DisplayName       string   `json:"displayName"`
	Email             string   `json:"email,omitempty"`
	PermissionLevel   int      `json:"permissionLevel"`
	AccessibleTeams   []string `json:"accessibleTeams,omitempty"`
	AccessibleWorkers []string `json:"accessibleWorkers,omitempty"`
	Capabilities      []string `json:"capabilities,omitempty"`
	Note              string   `json:"note,omitempty"`
	MatrixUserID      string   `json:"matrixUserID,omitempty"`
	InitialPassword   string   `json:"initialPassword,omitempty"`
	Rooms             []string `json:"rooms,omitempty"`
	Message           string   `json:"message,omitempty"`
}

type HumanListResponse struct {
	Humans []HumanResponse `json:"humans"`
	Total  int             `json:"total"`
}

// --- Manager API types ---

type CreateManagerRequest struct {
	Name          string                             `json:"name"`
	Model         string                             `json:"model"`
	ModelProvider string                             `json:"modelProvider,omitempty"`
	Runtime       string                             `json:"runtime,omitempty"`
	Image         string                             `json:"image,omitempty"`
	Soul          string                             `json:"soul,omitempty"`
	Agents        string                             `json:"agents,omitempty"`
	Skills        []string                           `json:"skills,omitempty"`
	RemoteSkills  []v1beta1.RemoteSkillSource        `json:"remoteSkills,omitempty"`
	McpServers    []v1beta1.MCPServer                `json:"mcpServers,omitempty"`
	Package       string                             `json:"package,omitempty"`
	Config        *v1beta1.ManagerConfig             `json:"config,omitempty"`
	State         *string                            `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped
	Resources     *v1beta1.AgentResourceRequirements `json:"resources,omitempty"`
}

type UpdateManagerRequest struct {
	Model         string                             `json:"model,omitempty"`
	ModelProvider *string                            `json:"modelProvider,omitempty"`
	Runtime       string                             `json:"runtime,omitempty"`
	Image         string                             `json:"image,omitempty"`
	Soul          string                             `json:"soul,omitempty"`
	Agents        string                             `json:"agents,omitempty"`
	Skills        []string                           `json:"skills,omitempty"`
	RemoteSkills  []v1beta1.RemoteSkillSource        `json:"remoteSkills,omitempty"`
	McpServers    []v1beta1.MCPServer                `json:"mcpServers,omitempty"`
	Package       string                             `json:"package,omitempty"`
	Config        *v1beta1.ManagerConfig             `json:"config,omitempty"`
	State         *string                            `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped
	Resources     *v1beta1.AgentResourceRequirements `json:"resources,omitempty"`
}

type ManagerResponse struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	State   string `json:"state,omitempty"` // desired lifecycle state
	Model   string `json:"model,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	// RuntimeDeprecated marks legacy runtimes that are upgrade-compat
	// only (CoPaw): new creation is rejected; migrate to QwenPaw.
	RuntimeDeprecated bool   `json:"runtimeDeprecated,omitempty"`
	Image             string `json:"image,omitempty"`
	MatrixUserID      string `json:"matrixUserID,omitempty"`
	RoomID            string `json:"roomID,omitempty"`
	Version           string `json:"version,omitempty"`
	Message           string `json:"message,omitempty"`
	// WelcomeSent mirrors ManagerStatus.WelcomeSent so installers / CLI can
	// poll for first-boot onboarding completion (DM joined + LLM auth ready
	// + welcome prompt actually delivered). Always present (false until set)
	// so the install script can rely on a stable JSON shape.
	WelcomeSent bool `json:"welcomeSent"`
}

type ManagerListResponse struct {
	Managers []ManagerResponse `json:"managers"`
	Total    int               `json:"total"`
}

// --- Gateway API types ---

type CreateConsumerRequest struct {
	Name          string `json:"name"`
	CredentialKey string `json:"credential_key,omitempty"`
}

type ConsumerResponse struct {
	Name       string `json:"name"`
	ConsumerID string `json:"consumer_id"`
	APIKey     string `json:"api_key,omitempty"`
	Status     string `json:"status"`
}

// AIRouteListResponse is the read-only AI route catalog returned by
// GET /api/v1/gateway/ai-routes. Each entry is a gateway route: name is the
// route name (NOT a model ID — one route can serve several models),
// upstreams are the providers serving it, and allowedConsumers are the
// gateway consumers authorized on the route.
type AIRouteListResponse struct {
	Routes []gateway.AIRouteInfo `json:"routes"`
	Total  int                   `json:"total"`
}

// --- Lifecycle API types ---

type WorkerLifecycleResponse struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}
