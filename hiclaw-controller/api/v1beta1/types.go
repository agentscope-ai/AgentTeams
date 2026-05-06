// +k8s:deepcopy-gen=package

package v1beta1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	GroupName = "hiclaw.io"
	Version   = "v1beta1"
)

// LabelController marks the hiclaw-controller instance that owns a CR.
// The value must equal the owning controller's HICLAW_CONTROLLER_NAME
// environment variable. When set, the controller's informer cache
// filters CR events by this label so multiple controller instances in
// the same namespace do not reconcile each other's resources.
const LabelController = "hiclaw.io/controller"

// AccessEntry declares one cloud-permission grant under a logical
// service. v1 supported services: "object-storage", "ai-gateway".
//
// Scope is a schema-less JSON blob in the CR layer: it may reference
// logical names (bucketRef: workspace, gatewayRef: default) and
// template variables (${self.name}, ${self.kind}, ${self.namespace}).
// The hiclaw-controller resolves it to real resource values before
// calling hiclaw-credential-provider; the provider never sees the
// CR-layer form.
//
// AccessEntry is only honored when the controller runs with a
// credential-provider sidecar (gateway.provider=ai-gateway or
// storage.provider=oss). In local higress+minio deployments the
// field is accepted by the CRD but not read by the controller.
type AccessEntry struct {
	Service     string                `json:"service"`
	Permissions []string              `json:"permissions,omitempty"`
	Scope       *apiextensionsv1.JSON `json:"scope,omitempty"`
}

// MCPServer declares one MCP server the agent can call via mcporter.
// Name maps to the key in mcporter-servers.json (used by tool calls as <name>.<tool>).
// URL is the full endpoint (e.g. https://apig.example.com/mcp-servers/github/mcp).
// Transport: "http" (Streamable HTTP, default) | "sse".
//
// The controller translates this slice directly into mcporter-servers.json and
// injects an Authorization: Bearer <consumer-key> header using the same
// gateway consumer key the agent uses for LLM access. The controller does not
// perform any gateway-side authorization for MCP servers — upstream access
// control is the gateway operator's responsibility (or, for local Higress
// deployments, handled out-of-band by Manager skills).
type MCPServer struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Transport string `json:"transport,omitempty"`
}

// +genclient
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Worker represents an AI agent worker in HiClaw.
type Worker struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkerSpec   `json:"spec"`
	Status            WorkerStatus `json:"status,omitempty"`
}

type WorkerSpec struct {
	Model         string             `json:"model"`
	Runtime       string             `json:"runtime,omitempty"` // openclaw | copaw | hermes (default: openclaw)
	Image         string             `json:"image,omitempty"`   // custom Docker image
	Identity      string             `json:"identity,omitempty"`
	Soul          string             `json:"soul,omitempty"`
	Agents        string             `json:"agents,omitempty"`
	Skills        []string           `json:"skills,omitempty"`
	McpServers    []MCPServer        `json:"mcpServers,omitempty"`
	Package       string             `json:"package,omitempty"` // file://, http(s)://, or nacos:// URI
	Expose        []ExposePort       `json:"expose,omitempty"`  // ports to expose via Higress gateway
	ChannelPolicy *ChannelPolicySpec `json:"channelPolicy,omitempty"`

	// State is the desired lifecycle state of the worker.
	// Valid values: "Running" (default), "Sleeping", "Stopped".
	// The controller reconciles actual backend state toward this desired state.
	State *string `json:"state,omitempty"`

	// AccessEntries declares the cloud permissions this worker should be
	// granted via hiclaw-credential-provider. See AccessEntry for semantics.
	// When empty the controller applies a sensible default (object-storage
	// scoped to agents/<name>/* and shared/*).
	AccessEntries []AccessEntry `json:"accessEntries,omitempty"`

	// Env holds user-defined environment variables injected into the worker
	// container. Keys that collide with variables already set by the
	// controller or backend (HICLAW_*, OPENCLAW_*, HOME, and similar
	// internal keys) are silently ignored with a warning log — the system
	// value always wins.
	Env map[string]string `json:"env,omitempty"`

	// Labels are user-defined Pod labels stamped onto the worker Pod.
	// Merged under the four-layer priority order (see controller docs):
	// pod-template < CR metadata.labels < CR spec.labels < controller
	// system labels. Entries whose keys collide with controller-forced
	// system labels (hiclaw.io/controller, hiclaw.io/worker, etc.) are
	// silently overridden. Must carry the omitempty tag so Teams that
	// embed WorkerSpec-shaped hashes keep a stable spec hash when the
	// field is absent.
	Labels map[string]string `json:"labels,omitempty"`
}

// DesiredState returns the effective desired state, defaulting to "Running".
func (s WorkerSpec) DesiredState() string {
	if s.State != nil && *s.State != "" {
		return *s.State
	}
	return "Running"
}

// ExposePort defines a container port to expose via the Higress gateway.
type ExposePort struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol,omitempty"` // http (default) | grpc
}

// ChannelPolicySpec defines additive/subtractive overrides on top of default
// communication policies. Values are Matrix user IDs (@user:domain) or
// short usernames (auto-resolved to full IDs by config generation scripts).
type ChannelPolicySpec struct {
	GroupAllowExtra []string `json:"groupAllowExtra,omitempty"`
	GroupDenyExtra  []string `json:"groupDenyExtra,omitempty"`
	DmAllowExtra    []string `json:"dmAllowExtra,omitempty"`
	DmDenyExtra     []string `json:"dmDenyExtra,omitempty"`
}

type WorkerStatus struct {
	ObservedGeneration int64               `json:"observedGeneration,omitempty"`
	Phase              string              `json:"phase,omitempty"` // Pending/Running/Sleeping/Failed
	MatrixUserID       string              `json:"matrixUserID,omitempty"`
	RoomID             string              `json:"roomID,omitempty"`
	ContainerState     string              `json:"containerState,omitempty"`
	LastHeartbeat      string              `json:"lastHeartbeat,omitempty"`
	Message            string              `json:"message,omitempty"`
	ExposedPorts       []ExposedPortStatus `json:"exposedPorts,omitempty"`
}

// ExposedPortStatus records a port that has been exposed via Higress.
type ExposedPortStatus struct {
	Port   int    `json:"port"`
	Domain string `json:"domain"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type WorkerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Worker `json:"items"`
}

// +genclient
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Team represents a group of workers led by a Team Leader.
type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TeamSpec   `json:"spec"`
	Status            TeamStatus `json:"status,omitempty"`
}

type TeamSpec struct {
	Description   string             `json:"description,omitempty"`
	Admin         *TeamAdminSpec     `json:"admin,omitempty"`
	Leader        LeaderSpec         `json:"leader"`
	Workers       []TeamWorkerSpec   `json:"workers,omitempty"`
	PeerMentions  *bool              `json:"peerMentions,omitempty"`  // default true
	ChannelPolicy *ChannelPolicySpec `json:"channelPolicy,omitempty"` // team-wide overrides
}

type TeamAdminSpec struct {
	Name         string `json:"name"`
	MatrixUserID string `json:"matrixUserId,omitempty"`
}

type LeaderSpec struct {
	Name              string                   `json:"name"`
	Model             string                   `json:"model,omitempty"`
	Identity          string                   `json:"identity,omitempty"`
	Soul              string                   `json:"soul,omitempty"`
	Agents            string                   `json:"agents,omitempty"`
	Package           string                   `json:"package,omitempty"`
	Heartbeat         *TeamLeaderHeartbeatSpec `json:"heartbeat,omitempty"`
	WorkerIdleTimeout string                   `json:"workerIdleTimeout,omitempty"`
	ChannelPolicy     *ChannelPolicySpec       `json:"channelPolicy,omitempty"`
	State             *string                  `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped

	// AccessEntries declares the cloud permissions this leader should be
	// granted via hiclaw-credential-provider. See AccessEntry for semantics.
	// When empty the controller applies team-member defaults (agents/<name>/*
	// + shared/* + teams/<team>/* on the configured bucket).
	AccessEntries []AccessEntry `json:"accessEntries,omitempty"`

	// Env holds user-defined environment variables injected into the
	// leader container. See WorkerSpec.Env for the collision policy.
	Env map[string]string `json:"env,omitempty"`

	// Labels are user-defined Pod labels stamped onto the leader Pod.
	// Merged on top of Team.metadata.labels and below controller system
	// labels (see WorkerSpec.Labels godoc). omitempty preserves
	// hashMemberSourceSpec stability for Teams that never set this
	// field.
	Labels map[string]string `json:"labels,omitempty"`
}

type TeamLeaderHeartbeatSpec struct {
	Enabled bool   `json:"enabled,omitempty"`
	Every   string `json:"every,omitempty"`
}

type TeamWorkerSpec struct {
	Name          string             `json:"name"`
	Model         string             `json:"model,omitempty"`
	Runtime       string             `json:"runtime,omitempty"`
	Image         string             `json:"image,omitempty"`
	Identity      string             `json:"identity,omitempty"`
	Soul          string             `json:"soul,omitempty"`
	Agents        string             `json:"agents,omitempty"`
	Skills        []string           `json:"skills,omitempty"`
	McpServers    []MCPServer        `json:"mcpServers,omitempty"`
	Package       string             `json:"package,omitempty"`
	Expose        []ExposePort       `json:"expose,omitempty"`
	ChannelPolicy *ChannelPolicySpec `json:"channelPolicy,omitempty"`
	State         *string            `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped

	// AccessEntries declares the cloud permissions this team worker should be
	// granted via hiclaw-credential-provider. See AccessEntry for semantics.
	// When empty the controller applies team-member defaults (agents/<name>/*
	// + shared/* + teams/<team>/* on the configured bucket).
	AccessEntries []AccessEntry `json:"accessEntries,omitempty"`

	// Env holds user-defined environment variables injected into this
	// team worker's container. See WorkerSpec.Env for the collision policy.
	Env map[string]string `json:"env,omitempty"`

	// Labels are user-defined Pod labels stamped onto this team worker's
	// Pod. Merged on top of Team.metadata.labels and below controller
	// system labels (see WorkerSpec.Labels godoc). omitempty preserves
	// hashMemberSourceSpec stability for existing Teams.
	Labels map[string]string `json:"labels,omitempty"`
}

type TeamStatus struct {
	Phase          string `json:"phase,omitempty"` // Pending/Active/Degraded/Failed
	TeamRoomID     string `json:"teamRoomID,omitempty"`
	LeaderDMRoomID string `json:"leaderDMRoomID,omitempty"`
	LeaderReady    bool   `json:"leaderReady,omitempty"`
	ReadyWorkers   int    `json:"readyWorkers,omitempty"`
	TotalWorkers   int    `json:"totalWorkers,omitempty"`
	Message        string `json:"message,omitempty"`
	// Members carries per-member state (one entry per leader + worker).
	// TeamReconciler sorts the slice by Name for stable status patches and
	// deterministic test assertions.
	//
	// This slice replaces the previous ObservedMembers / MemberSpecHashes /
	// WorkerExposedPorts trio — each of which maintained its own stale-
	// cleanup loop and contributed independent patch churn. Consolidating
	// them here means adding a new per-member field costs one struct field
	// (vs one status field + one map + one cleanup loop + one consumer).
	Members []TeamMemberStatus `json:"members,omitempty"`
}

// MemberByName returns a pointer to the TeamMemberStatus entry for name,
// or nil when no such member has been recorded. Callers that need to
// create-on-absent must use the controller-package memberStatus helper
// instead — we keep creation out of the API types to avoid accidental
// mutation from API response codepaths.
func (s *TeamStatus) MemberByName(name string) *TeamMemberStatus {
	for i := range s.Members {
		if s.Members[i].Name == name {
			return &s.Members[i]
		}
	}
	return nil
}

// TeamMemberStatus captures all per-member state for one team member
// (leader or worker). Collects the fields that previously lived in the
// scattered ObservedMembers / MemberSpecHashes / WorkerExposedPorts maps.
type TeamMemberStatus struct {
	// Name is the member's canonical name (matches Team.Spec.Leader.Name or
	// Team.Spec.Workers[i].Name). Uniquely identifies the entry within
	// Team.Status.Members.
	Name string `json:"name"`
	// Role is "team_leader" or "worker". Mirrors MemberContext.Role and the
	// synthesized WorkerResponse.Role exposed via /api/v1/workers/<name>.
	Role string `json:"role,omitempty"`
	// RoomID is the member's personal communication room with the Manager —
	// same semantic as Worker.Status.RoomID for standalone workers. Distinct
	// from Team.Status.TeamRoomID (shared team room) and
	// Team.Status.LeaderDMRoomID (Leader↔Admin DM). Consumers reading this
	// include hiclaw CLI (`hiclaw get workers <name> -o json | jq .roomID`)
	// and the Manager Agent when it needs to target a specific member.
	RoomID string `json:"roomID,omitempty"`
	// MatrixUserID is the member's Matrix MXID. Populated by
	// ReconcileMemberInfra alongside RoomID.
	MatrixUserID string `json:"matrixUserID,omitempty"`
	// SpecHash is the fnv64a hash of hashMemberSourceSpec output at the last
	// successful full-phase reconcile. Empty means "never fully reconciled";
	// memberSpecChanged treats that as "not changed" so initial create is
	// not preempted by a transient Delete (see memberSpecChanged doc in
	// team_controller.go).
	SpecHash string `json:"specHash,omitempty"`
	// Observed flips to true the instant ReconcileMemberInfra succeeds and
	// stays true even if later phases fail. It drives:
	//   - IsUpdate selection in buildDesiredMembers (Refresh vs Provision)
	//   - stale detection (members in Status.Members but no longer in Spec)
	//
	// Dropping back to false on post-infra failure would force a Provision
	// retry that rotates the Matrix access token — triggering an openclaw
	// gateway restart on every partial failure (see commit 7babeb8).
	Observed bool `json:"observed,omitempty"`
	// Ready mirrors backend.Status ∈ {Running, Ready}, re-evaluated by
	// summarizeBackendReadiness on each reconcile pass. Aggregates into
	// Team.Status.LeaderReady and Team.Status.ReadyWorkers.
	Ready bool `json:"ready,omitempty"`
	// ExposedPorts records the ports currently exposed via Higress for this
	// member. Leader members never expose ports (this field stays nil).
	ExposedPorts []ExposedPortStatus `json:"exposedPorts,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type TeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Team `json:"items"`
}

// +genclient
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Human represents a real human user with configurable access permissions.
type Human struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              HumanSpec   `json:"spec"`
	Status            HumanStatus `json:"status,omitempty"`
}

type HumanSpec struct {
	DisplayName       string   `json:"displayName"`
	Email             string   `json:"email,omitempty"`
	PermissionLevel   int      `json:"permissionLevel"` // 1=Admin, 2=Team, 3=Worker
	AccessibleTeams   []string `json:"accessibleTeams,omitempty"`
	AccessibleWorkers []string `json:"accessibleWorkers,omitempty"`
	Note              string   `json:"note,omitempty"`
}

type HumanStatus struct {
	Phase           string   `json:"phase,omitempty"` // Pending/Active/Failed
	MatrixUserID    string   `json:"matrixUserID,omitempty"`
	InitialPassword string   `json:"initialPassword,omitempty"` // Set on creation, shown once
	Rooms           []string `json:"rooms,omitempty"`
	EmailSent       bool     `json:"emailSent,omitempty"`
	Message         string   `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type HumanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Human `json:"items"`
}

// +genclient
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Manager represents the HiClaw Manager Agent — the coordinator that receives
// natural-language instructions from Admin and orchestrates Workers/Teams via
// the hiclaw CLI / Controller REST API.
type Manager struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ManagerSpec   `json:"spec"`
	Status            ManagerStatus `json:"status,omitempty"`
}

type ManagerSpec struct {
	Model      string        `json:"model"`
	Runtime    string        `json:"runtime,omitempty"`    // openclaw | copaw | hermes (default: openclaw)
	Image      string        `json:"image,omitempty"`      // custom Docker image
	Soul       string        `json:"soul,omitempty"`       // custom SOUL.md content
	Agents     string        `json:"agents,omitempty"`     // custom AGENTS.md content
	Skills     []string      `json:"skills,omitempty"`     // on-demand skills to enable
	McpServers []MCPServer   `json:"mcpServers,omitempty"` // MCP servers callable by the Manager via mcporter
	Package    string        `json:"package,omitempty"`    // file://, http(s)://, or nacos:// URI
	Config     ManagerConfig `json:"config,omitempty"`

	// State is the desired lifecycle state of the manager.
	// Valid values: "Running" (default), "Sleeping", "Stopped".
	// The controller reconciles actual backend state toward this desired state.
	State *string `json:"state,omitempty"`

	// AccessEntries declares the cloud permissions this manager should be
	// granted via hiclaw-credential-provider. See AccessEntry for semantics.
	// When empty the controller applies a sensible default (object-storage
	// scoped to agents/<name>/*, shared/*, and manager/*).
	AccessEntries []AccessEntry `json:"accessEntries,omitempty"`

	// Env holds user-defined environment variables injected into the
	// manager container. See WorkerSpec.Env for the collision policy.
	Env map[string]string `json:"env,omitempty"`

	// Labels are user-defined Pod labels stamped onto the manager Pod.
	// Merged under the four-layer priority order (see WorkerSpec.Labels
	// godoc): pod-template < CR metadata.labels < CR spec.labels <
	// controller system labels.
	Labels map[string]string `json:"labels,omitempty"`
}

// DesiredState returns the effective desired state, defaulting to "Running".
func (s ManagerSpec) DesiredState() string {
	if s.State != nil && *s.State != "" {
		return *s.State
	}
	return "Running"
}

type ManagerConfig struct {
	HeartbeatInterval string `json:"heartbeatInterval,omitempty"` // default: 15m
	WorkerIdleTimeout string `json:"workerIdleTimeout,omitempty"` // default: 720m
	NotifyChannel     string `json:"notifyChannel,omitempty"`     // default: admin-dm
}

type ManagerStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Phase              string `json:"phase,omitempty"` // Pending/Running/Updating/Failed
	MatrixUserID       string `json:"matrixUserID,omitempty"`
	RoomID             string `json:"roomID,omitempty"` // Admin DM room
	ContainerState     string `json:"containerState,omitempty"`
	Version            string `json:"version,omitempty"`
	Message            string `json:"message,omitempty"`

	// WelcomeSent records whether the controller has already delivered the
	// first-boot onboarding prompt to the Admin DM room. Used as the
	// idempotency guard for reconcileManagerWelcome — once true the
	// controller will not re-send even if the manager container is later
	// recreated. The Manager Agent's own `~/soul-configured` file remains
	// the orthogonal marker that the agent has finished the resulting
	// onboarding Q&A.
	WelcomeSent bool `json:"welcomeSent,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type ManagerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Manager `json:"items"`
}
