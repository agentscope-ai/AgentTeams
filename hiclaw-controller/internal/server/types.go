package server

import v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"

// --- Worker API types ---

type CreateWorkerRequest struct {
	Name          string                     `json:"name"`
	Model         string                     `json:"model,omitempty"`
	Runtime       string                     `json:"runtime,omitempty"`
	Image         string                     `json:"image,omitempty"`
	Identity      string                     `json:"identity,omitempty"`
	Soul          string                     `json:"soul,omitempty"`
	Agents        string                     `json:"agents,omitempty"`
	Skills        []string                   `json:"skills,omitempty"`
	McpServers    []v1beta1.MCPServer        `json:"mcpServers,omitempty"`
	Package       string                     `json:"package,omitempty"`
	Expose        []v1beta1.ExposePort       `json:"expose,omitempty"`
	ChannelPolicy *v1beta1.ChannelPolicySpec `json:"channelPolicy,omitempty"`

	// ContainerManaged indicates whether the controller should manage
	// container lifecycle for this worker. When false, container
	// reconciliation is skipped entirely (for remote/pip workers).
	// Default is true (controller manages container).
	ContainerManaged *bool   `json:"containerManaged,omitempty"`
	State            *string `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped

	// Team context (set by Team handler or CLI)
	Team       string `json:"team,omitempty"`
	TeamLeader string `json:"teamLeader,omitempty"`
	Role       string `json:"role,omitempty"` // team_leader | worker
}

type UpdateWorkerRequest struct {
	Model         string                     `json:"model,omitempty"`
	Runtime       string                     `json:"runtime,omitempty"`
	Image         string                     `json:"image,omitempty"`
	Identity      string                     `json:"identity,omitempty"`
	Soul          string                     `json:"soul,omitempty"`
	Agents        string                     `json:"agents,omitempty"`
	Skills        []string                   `json:"skills,omitempty"`
	McpServers    []v1beta1.MCPServer        `json:"mcpServers,omitempty"`
	Package       string                     `json:"package,omitempty"`
	Expose        []v1beta1.ExposePort       `json:"expose,omitempty"`
	ChannelPolicy *v1beta1.ChannelPolicySpec `json:"channelPolicy,omitempty"`

	// ContainerManaged indicates whether the controller should manage
	// container lifecycle for this worker. When false, container
	// reconciliation is skipped entirely (for remote/pip workers).
	// Default is true (controller manages container).
	ContainerManaged *bool   `json:"containerManaged,omitempty"`
	State            *string `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped
}

type WorkerResponse struct {
	Name             string            `json:"name"`
	Phase            string            `json:"phase"`
	ContainerManaged bool              `json:"containerManaged,omitempty"`
	State            string            `json:"state,omitempty"` // desired lifecycle state
	Model            string            `json:"model,omitempty"`
	Runtime          string            `json:"runtime,omitempty"`
	Image            string            `json:"image,omitempty"`
	ContainerState   string            `json:"containerState,omitempty"`
	MatrixUserID     string            `json:"matrixUserID,omitempty"`
	RoomID           string            `json:"roomID,omitempty"`
	Message          string            `json:"message,omitempty"`
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

// --- Team API types ---

type CreateTeamRequest struct {
	Name          string                     `json:"name"`
	Description   string                     `json:"description,omitempty"`
	Admin         *v1beta1.TeamAdminSpec     `json:"admin,omitempty"`
	Leader        TeamLeaderRequest          `json:"leader"`
	Workers       []TeamWorkerRequest        `json:"workers,omitempty"`
	PeerMentions  *bool                      `json:"peerMentions,omitempty"`
	ChannelPolicy *v1beta1.ChannelPolicySpec `json:"channelPolicy,omitempty"`
}

type TeamLeaderRequest struct {
	Name              string                      `json:"name"`
	Model             string                      `json:"model,omitempty"`
	Identity          string                      `json:"identity,omitempty"`
	Soul              string                      `json:"soul,omitempty"`
	Agents            string                      `json:"agents,omitempty"`
	Package           string                      `json:"package,omitempty"`
	McpServers        []v1beta1.MCPServer         `json:"mcpServers,omitempty"`
	Heartbeat         *TeamLeaderHeartbeatRequest `json:"heartbeat,omitempty"`
	WorkerIdleTimeout string                      `json:"workerIdleTimeout,omitempty"`
	ChannelPolicy     *v1beta1.ChannelPolicySpec  `json:"channelPolicy,omitempty"`
	State             *string                     `json:"state,omitempty"` // desired lifecycle state for leader
}

type TeamLeaderHeartbeatRequest struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Every   string `json:"every,omitempty"`
}

type TeamWorkerRequest struct {
	Name          string                     `json:"name"`
	Model         string                     `json:"model,omitempty"`
	Runtime       string                     `json:"runtime,omitempty"`
	Image         string                     `json:"image,omitempty"`
	Identity      string                     `json:"identity,omitempty"`
	Soul          string                     `json:"soul,omitempty"`
	Agents        string                     `json:"agents,omitempty"`
	Skills        []string                   `json:"skills,omitempty"`
	McpServers    []v1beta1.MCPServer        `json:"mcpServers,omitempty"`
	Package       string                     `json:"package,omitempty"`
	Expose        []v1beta1.ExposePort       `json:"expose,omitempty"`
	ChannelPolicy *v1beta1.ChannelPolicySpec `json:"channelPolicy,omitempty"`
	State         *string                    `json:"state,omitempty"` // desired lifecycle state for worker
}

type UpdateTeamRequest struct {
	Description   string                     `json:"description,omitempty"`
	Admin         *v1beta1.TeamAdminSpec     `json:"admin,omitempty"`
	Leader        *TeamLeaderRequest         `json:"leader,omitempty"`
	Workers       []TeamWorkerRequest        `json:"workers,omitempty"`
	PeerMentions  *bool                      `json:"peerMentions,omitempty"`
	ChannelPolicy *v1beta1.ChannelPolicySpec `json:"channelPolicy,omitempty"`
}

type TeamResponse struct {
	Name               string                           `json:"name"`
	Phase              string                           `json:"phase"`
	Description        string                           `json:"description,omitempty"`
	LeaderName         string                           `json:"leaderName"`
	LeaderHeartbeat    *v1beta1.TeamLeaderHeartbeatSpec `json:"leaderHeartbeat,omitempty"`
	WorkerIdleTimeout  string                           `json:"workerIdleTimeout,omitempty"`
	TeamRoomID         string                           `json:"teamRoomID,omitempty"`
	LeaderDMRoomID     string                           `json:"leaderDMRoomID,omitempty"`
	LeaderReady        bool                             `json:"leaderReady"`
	ReadyWorkers       int                              `json:"readyWorkers"`
	TotalWorkers       int                              `json:"totalWorkers"`
	Message            string                           `json:"message,omitempty"`
	WorkerNames        []string                         `json:"workerNames,omitempty"`
	WorkerExposedPorts map[string][]ExposedPortInfo     `json:"workerExposedPorts,omitempty"`
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

// UpdateHumanRequest is the body for PUT /api/v1/humans/{name}.
//
// Partial-update semantics mirror UpdateWorkerRequest / UpdateTeamRequest:
//
//   - Plain string fields (DisplayName / Email / Note): empty string =
//     "field absent in JSON", preserves existing value. To clear a string
//     to empty, callers send DELETE+CREATE instead.
//   - PermissionLevel uses *int so the request can disambiguate "field
//     absent" from "set to 0" — the comment on v1beta1.HumanSpec
//     PermissionLevel reads "1=Admin, 2=Team, 3=Worker" and 0 is outside
//     the documented enum, but we still allow the round-trip rather than
//     re-encoding type semantics in the REST surface.
//   - AccessibleTeams / AccessibleWorkers use the nil-vs-[]-distinct
//     pattern: nil (field absent) preserves existing; []string{}
//     (explicit empty slice) clears the list. JSON's standard
//     unmarshaling honors this — `"accessibleTeams":[]` decodes to a
//     non-nil empty slice.
type UpdateHumanRequest struct {
	DisplayName       string   `json:"displayName,omitempty"`
	Email             string   `json:"email,omitempty"`
	PermissionLevel   *int     `json:"permissionLevel,omitempty"`
	AccessibleTeams   []string `json:"accessibleTeams,omitempty"`
	AccessibleWorkers []string `json:"accessibleWorkers,omitempty"`
	Note              string   `json:"note,omitempty"`
}

type HumanResponse struct {
	Name            string   `json:"name"`
	Phase           string   `json:"phase"`
	DisplayName     string   `json:"displayName"`
	MatrixUserID    string   `json:"matrixUserID,omitempty"`
	InitialPassword string   `json:"initialPassword,omitempty"`
	Rooms           []string `json:"rooms,omitempty"`
	Message         string   `json:"message,omitempty"`

	// Spec echo (added 2026-05 with PUT /humans/{name} route — fixes #729).
	// Surfacing these fields lets callers do read-modify-write without a
	// separate kubectl/raw-CR fetch. Useful for the upcoming UpdateHuman
	// flow where caller adds/removes one entry from accessibleTeams
	// without overwriting other spec state.
	Email             string   `json:"email,omitempty"`
	PermissionLevel   int      `json:"permissionLevel,omitempty"`
	AccessibleTeams   []string `json:"accessibleTeams,omitempty"`
	AccessibleWorkers []string `json:"accessibleWorkers,omitempty"`
	Note              string   `json:"note,omitempty"`
}

type HumanListResponse struct {
	Humans []HumanResponse `json:"humans"`
	Total  int             `json:"total"`
}

// --- Manager API types ---

type CreateManagerRequest struct {
	Name       string                 `json:"name"`
	Model      string                 `json:"model"`
	Runtime    string                 `json:"runtime,omitempty"`
	Image      string                 `json:"image,omitempty"`
	Soul       string                 `json:"soul,omitempty"`
	Agents     string                 `json:"agents,omitempty"`
	Skills     []string               `json:"skills,omitempty"`
	McpServers []v1beta1.MCPServer    `json:"mcpServers,omitempty"`
	Package    string                 `json:"package,omitempty"`
	Config     *v1beta1.ManagerConfig `json:"config,omitempty"`
	State      *string                `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped
}

type UpdateManagerRequest struct {
	Model      string                 `json:"model,omitempty"`
	Runtime    string                 `json:"runtime,omitempty"`
	Image      string                 `json:"image,omitempty"`
	Soul       string                 `json:"soul,omitempty"`
	Agents     string                 `json:"agents,omitempty"`
	Skills     []string               `json:"skills,omitempty"`
	McpServers []v1beta1.MCPServer    `json:"mcpServers,omitempty"`
	Package    string                 `json:"package,omitempty"`
	Config     *v1beta1.ManagerConfig `json:"config,omitempty"`
	State      *string                `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped
}

type ManagerResponse struct {
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	State        string `json:"state,omitempty"` // desired lifecycle state
	Model        string `json:"model,omitempty"`
	Runtime      string `json:"runtime,omitempty"`
	Image        string `json:"image,omitempty"`
	MatrixUserID string `json:"matrixUserID,omitempty"`
	RoomID       string `json:"roomID,omitempty"`
	Version      string `json:"version,omitempty"`
	Message      string `json:"message,omitempty"`
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

// --- Lifecycle API types ---

type WorkerLifecycleResponse struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}
