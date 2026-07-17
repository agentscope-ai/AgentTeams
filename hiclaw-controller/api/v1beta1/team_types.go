package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TeamSpec   `json:"spec"`
	Status            TeamStatus `json:"status,omitempty"`
}
type TeamSpec struct {
	Description  string           `json:"description,omitempty"`
	TeamName     string           `json:"teamName,omitempty"`
	Admin        *TeamAdminSpec   `json:"admin,omitempty"`
	HumanMembers []TeamMemberSpec `json:"humanMembers,omitempty"`

	// WorkerMembers references existing Worker CRs as team members.
	// The TeamReconciler validates membership, provisions rooms, injects
	// runtime context, and aggregates member status from these references.
	// +kubebuilder:validation:MaxItems=128
	WorkerMembers []TeamWorkerRef `json:"workerMembers,omitempty"`

	PeerMentions  *bool              `json:"peerMentions,omitempty"`  // default true
	ChannelPolicy *ChannelPolicySpec `json:"channelPolicy,omitempty"` // team-wide overrides

	// ModelProvider is the APIG Model API name for the team-level LLM provider
	// override. When set, the TeamReconciler forwards it to the leader and worker
	// member contexts so all members of this team route through the named model
	// API instead of the cluster default. Empty means "use the default provider".
	ModelProvider string `json:"modelProvider,omitempty"`

	// HeartbeatEvery configures the Team Leader agent's periodic heartbeat
	// check interval. The TeamReconciler writes this value into the leader
	// Worker's openclaw.json and coordination context AGENTS.md.
	// Example: "30m". Empty means leader heartbeat is disabled.
	HeartbeatEvery string `json:"heartbeatEvery,omitempty"`

	// Deprecated: Leader defines the team leader's runtime configuration.
	// Retained for backward compatibility during migration. Ignored when
	// WorkerMembers is non-empty.
	Leader LeaderSpec `json:"leader,omitempty"`
	// Deprecated: Workers defines team worker runtime configurations.
	// Retained for backward compatibility during migration. Ignored when
	// WorkerMembers is non-empty.
	Workers []TeamWorkerSpec `json:"workers,omitempty"`
}

// TeamWorkerRef references an existing Worker CR as a team member.
type TeamWorkerRef struct {
	// Name is the metadata.name of the referenced Worker CR.
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Role is this member's role within the team: "team_leader" or "worker".
	// Empty defaults to "worker".
	Role string `json:"role,omitempty"`
}

func (s TeamSpec) EffectiveTeamName(metadataName string) string {
	if s.TeamName != "" {
		return s.TeamName
	}
	return metadataName
}

type TeamAdminSpec struct {
	Name         string `json:"name"`
	MatrixUserID string `json:"matrixUserId,omitempty"`
}
type TeamMemberSpec struct {
	Name         string `json:"name"`
	MatrixUserID string `json:"matrixUserId,omitempty"`
	Role         string `json:"role,omitempty"` // coordinator (default)
}
type LeaderSpec struct {
	Name              string                     `json:"name"`
	WorkerName        string                     `json:"workerName,omitempty"`
	Model             string                     `json:"model,omitempty"`
	ModelProvider     string                     `json:"modelProvider,omitempty"` // APIG Model API name for per-leader LLM provider
	Runtime           string                     `json:"runtime,omitempty"`
	Image             string                     `json:"image,omitempty"`
	Identity          string                     `json:"identity,omitempty"`
	Soul              string                     `json:"soul,omitempty"`
	Agents            string                     `json:"agents,omitempty"`
	Package           string                     `json:"package,omitempty"`
	RemoteSkills      []RemoteSkillSource        `json:"remoteSkills,omitempty"` // remote skills from source registries
	McpServers        []MCPServer                `json:"mcpServers,omitempty"`
	Heartbeat         *TeamLeaderHeartbeatSpec   `json:"heartbeat,omitempty"`
	WorkerIdleTimeout string                     `json:"workerIdleTimeout,omitempty"`
	ChannelPolicy     *ChannelPolicySpec         `json:"channelPolicy,omitempty"`
	State             *string                    `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped
	Resources         *AgentResourceRequirements `json:"resources,omitempty"`

	// AccessEntries declares the cloud permissions this leader should be
	// granted via hiclaw-credential-provider. See AccessEntry for semantics.
	// When empty the controller applies team-member defaults (agents/<name>/*
	// + shared/* + teams/<team>/* on the configured bucket).
	AccessEntries []AccessEntry `json:"accessEntries,omitempty"`

	// DeployMode specifies where the leader pod runs.
	// "Local" (default): created in the controller's own cluster.
	// "Edge": externally hosted outside the controller's managed pod path.
	DeployMode *string `json:"deployMode,omitempty"`

	// ServiceEnabled controls whether a ClusterIP Service is created
	// alongside the leader pod (same cluster, namespace, name).
	ServiceEnabled *bool `json:"serviceEnabled,omitempty"`

	// Env holds user-defined environment variables injected into the
	// leader container. See WorkerSpec.Env for the collision policy.
	Env map[string]string `json:"env,omitempty"`

	// Labels are user-defined Pod labels stamped onto the leader Pod.
	// Merged on top of Team.metadata.labels and below controller system
	// labels (see WorkerSpec.Labels godoc). omitempty preserves zero-value
	// wire compatibility for callers that never set this field.
	Labels map[string]string `json:"labels,omitempty"`
}
type TeamLeaderHeartbeatSpec struct {
	Enabled bool   `json:"enabled,omitempty"`
	Every   string `json:"every,omitempty"`
}
type TeamWorkerSpec struct {
	Name          string                     `json:"name"`
	WorkerName    string                     `json:"workerName,omitempty"`
	Model         string                     `json:"model,omitempty"`
	ModelProvider string                     `json:"modelProvider,omitempty"` // APIG Model API name for per-worker LLM provider
	Runtime       string                     `json:"runtime,omitempty"`
	Image         string                     `json:"image,omitempty"`
	Identity      string                     `json:"identity,omitempty"`
	Soul          string                     `json:"soul,omitempty"`
	Agents        string                     `json:"agents,omitempty"`
	Skills        []string                   `json:"skills,omitempty"`
	RemoteSkills  []RemoteSkillSource        `json:"remoteSkills,omitempty"` // remote skills from source registries
	McpServers    []MCPServer                `json:"mcpServers,omitempty"`
	Package       string                     `json:"package,omitempty"`
	Expose        []ExposePort               `json:"expose,omitempty"`
	ChannelPolicy *ChannelPolicySpec         `json:"channelPolicy,omitempty"`
	IdleTimeout   string                     `json:"idleTimeout,omitempty"`
	State         *string                    `json:"state,omitempty"` // desired lifecycle state: Running, Sleeping, Stopped
	Resources     *AgentResourceRequirements `json:"resources,omitempty"`

	// AccessEntries declares the cloud permissions this team worker should be
	// granted via hiclaw-credential-provider. See AccessEntry for semantics.
	// When empty the controller applies team-member defaults (agents/<name>/*
	// + shared/* + teams/<team>/* on the configured bucket).
	AccessEntries []AccessEntry `json:"accessEntries,omitempty"`

	// DeployMode specifies where the team worker pod runs.
	// "Local" (default): created in the controller's own cluster.
	// "Edge": externally hosted outside the controller's managed pod path.
	DeployMode *string `json:"deployMode,omitempty"`

	// ServiceEnabled controls whether a ClusterIP Service is created
	// alongside the team worker pod (same cluster, namespace, name).
	ServiceEnabled *bool `json:"serviceEnabled,omitempty"`

	// Env holds user-defined environment variables injected into this
	// team worker's container. See WorkerSpec.Env for the collision policy.
	Env map[string]string `json:"env,omitempty"`

	// Labels are user-defined Pod labels stamped onto this team worker's
	// Pod. Merged on top of Team.metadata.labels and below controller
	// system labels (see WorkerSpec.Labels godoc). omitempty preserves
	// zero-value wire compatibility for callers that never set this field.
	Labels map[string]string `json:"labels,omitempty"`
}

// EffectiveWorkerName returns the runtime identity key for a team leader.
// Empty workerName falls back to spec.name supplied by caller.
func (s LeaderSpec) EffectiveWorkerName() string {
	if s.WorkerName != "" {
		return s.WorkerName
	}
	return s.Name

}

// EffectiveWorkerName returns the runtime identity key for a team worker.
// Empty workerName falls back to spec.name supplied by caller.
func (s TeamWorkerSpec) EffectiveWorkerName() string {
	if s.WorkerName != "" {
		return s.WorkerName
	}
	return s.Name
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
	// Name is the member's canonical Worker CR name from
	// Team.Spec.WorkerMembers. Uniquely identifies the entry within
	// Team.Status.Members.
	Name string `json:"name"`
	// RuntimeName is the member's runtime identity key (Matrix localpart,
	// OSS path key, room alias key). Empty falls back to Name.
	RuntimeName string `json:"runtimeName,omitempty"`
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
	// SpecHash mirrors the referenced Worker.Status.SpecHash after status
	// aggregation so Team consumers can inspect the member runtime revision.
	SpecHash string `json:"specHash,omitempty"`
	// Observed flips to true the instant ReconcileMemberInfra succeeds and
	// stays true even if later phases fail.
	Observed bool `json:"observed,omitempty"`
	// Ready mirrors backend.Status ∈ {Running, Ready}, re-evaluated by
	// summarizeBackendReadiness on each reconcile pass. Aggregates into
	// Team.Status.LeaderReady and Team.Status.ReadyWorkers.
	Ready bool `json:"ready,omitempty"`
	// Phase is the member lifecycle phase: Pending, Starting, Running,
	// Updating, Stopping, Sleeping, Stopped, Failed.
	Phase string `json:"phase,omitempty"`
	// ContainerState is the raw backend container status.
	ContainerState string `json:"containerState,omitempty"`
	// Message holds per-member error detail from reconcile. Cleared on success.
	Message string `json:"message,omitempty"`
	// LastActiveAt is the latest runtime-reported business activity time.
	LastActiveAt string `json:"lastActiveAt,omitempty"`
	// LastHeartbeat is the latest heartbeat timestamp for this member.
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
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
