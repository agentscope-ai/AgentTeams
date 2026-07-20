// +k8s:deepcopy-gen=package

package v1beta1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	GroupName = "agentteams.io"
	Version   = "v1beta1"
)

// LabelController marks the agentteams-controller instance that owns a CR.
// The value must equal the owning controller's AGENTTEAMS_CONTROLLER_NAME
// environment variable. When set, the controller's informer cache
// filters CR events by this label so multiple controller instances in
// the same namespace do not reconcile each other's resources.
const LabelController = "agentteams.io/controller"

const (
	LabelWorker  = "agentteams.io/worker"
	LabelManager = "agentteams.io/manager"
	LabelRole    = "agentteams.io/role"
	LabelRuntime = "agentteams.io/runtime"
	LabelTeam    = "agentteams.io/team"
)

// LabelWorkerSvcName records the ClusterIP Service name created for a
// Worker when spec.serviceEnabled is true. Removed when the service is
// disabled or deleted.
const LabelWorkerSvcName = "agentteams.io/worker-svc-name"

// LabelWorkerEdgeUUID records the per-Worker UUID used to identify this
// worker on an external Edge host (Edge DeployMode). The value is stable
// across reconciles so credential issuance and rotation can target the
// same identity on the remote side.
const LabelWorkerEdgeUUID = "agentteams.io/worker-edge-uuid"

// AnnotationEdgeAppliedUUID tracks the UUID that was last used to issue
// an SA token, used for rotation detection. When the current
// LabelWorkerEdgeUUID differs from this annotation, the controller
// re-issues credentials and updates the annotation to match.
const AnnotationEdgeAppliedUUID = "agentteams.io/edge-applied-uuid"

// AccessEntry declares one cloud-permission grant under a logical
// service. v1 supported services: "object-storage", "ai-gateway", "ai-registry", "schedulerx3".
//
// Scope is a schema-less JSON blob in the CR layer: it may reference
// logical names (bucketRef: workspace, gatewayRef: default) and
// template variables (${self.name}, ${self.kind}, ${self.namespace}).
// The agentteams-controller resolves it to real resource values before
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

// AgentIdentitySpec carries the non-secret workload identity facts a runtime
// needs to exchange for scoped data-plane credentials.
type AgentIdentitySpec struct {
	WorkloadIdentityName string `json:"workloadIdentityName,omitempty"`
}

// CredentialRef identifies one runtime credential provider without carrying
// the real credential value.
type CredentialRef struct {
	TokenVaultName               string `json:"tokenVaultName,omitempty"`
	APIKeyCredentialProviderName string `json:"apiKeyCredentialProviderName,omitempty"`
}

// CredentialBinding grants a worker-like member access to one referenced
// runtime credential. The value is resolved by the runtime, not by controller.
type CredentialBinding struct {
	CredentialRef CredentialRef `json:"credentialRef"`
	ToolWhitelist []string      `json:"toolWhitelist,omitempty"`
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

// RemoteSkill identifies one skill from a remote source.
// version and label are mutually exclusive; set at most one.
type RemoteSkill struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Label   string `json:"label,omitempty"`
}

// RemoteSkillSource groups remote skills by source and auth mode.
// Source format: nacos://host:port/{namespace-id}
// AuthType values: "nacos" (username:password embedded in source URL as nacos://user:pass@host:port/namespace),
// "sts-hiclaw" (STS credential provider), "none" (unauthenticated). Empty auto-detects:
// embedded username/password selects "nacos"; otherwise "none".
type RemoteSkillSource struct {
	Source   string        `json:"source"`
	AuthType string        `json:"authType,omitempty"`
	Skills   []RemoteSkill `json:"skills"`
}

// AgentResourceRequirements declares optional CPU/memory requests and limits
// for one managed agent Pod. Empty fields fall back to controller/backend
// defaults field-by-field.
type AgentResourceRequirements struct {
	Requests AgentResourceValues `json:"requests,omitempty"`
	Limits   AgentResourceValues `json:"limits,omitempty"`
}

// AgentResourceValues holds Kubernetes quantity strings for CPU and memory.
type AgentResourceValues struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// BackendRuntime constants define backend runtime identifiers used by Worker
// specs.
const (
	BackendRuntimePod     = "pod"
	BackendRuntimeSandbox = "sandbox"
)

// DeployMode constants define where the worker pod runs.
const (
	DeployModeLocal  = "Local"
	DeployModeRemote = "Remote"
	DeployModeEdge   = "Edge"
)

// WorkerResourceSpec defines a compact resource shape. CPU and memory are
// applied as both requests and limits where this helper is used.
type WorkerResourceSpec struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// Worker volume provider constants.
const (
	WorkerVolumeTypeOSS = "OSS"
)

// +genclient
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Worker represents an AI agent worker in AgentTeams.
type Worker struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkerSpec   `json:"spec"`
	Status            WorkerStatus `json:"status,omitempty"`
}

type WorkerSpec struct {
	Model         string                     `json:"model"`
	ModelProvider string                     `json:"modelProvider,omitempty"` // APIG Model API name for per-worker LLM provider
	Runtime       string                     `json:"runtime,omitempty"`       // openclaw | copaw | hermes | qwenpaw (default: openclaw)
	Image         string                     `json:"image,omitempty"`         // custom Docker image
	WorkerName    string                     `json:"workerName,omitempty"`    // business/runtime identity (Matrix localpart, OSS path key)
	Identity      string                     `json:"identity,omitempty"`
	Soul          string                     `json:"soul,omitempty"`
	Agents        string                     `json:"agents,omitempty"`
	Skills        []string                   `json:"skills,omitempty"`       // built-in skills only
	RemoteSkills  []RemoteSkillSource        `json:"remoteSkills,omitempty"` // remote skills from source registries
	McpServers    []MCPServer                `json:"mcpServers,omitempty"`
	Package       string                     `json:"package,omitempty"` // file://, http(s)://, or nacos://[user:pass@]host:port/...; optional ?authType=nacos|sts-hiclaw|none
	Expose        []ExposePort               `json:"expose,omitempty"`  // ports to expose via Higress gateway
	ChannelPolicy *ChannelPolicySpec         `json:"channelPolicy,omitempty"`
	Channels      *ChannelsSpec              `json:"channels,omitempty"`
	Resources     *AgentResourceRequirements `json:"resources,omitempty"`
	IdleTimeout   string                     `json:"idleTimeout,omitempty"`

	// ContainerManaged indicates whether the controller should manage
	// container lifecycle for this worker. When false, container
	// reconciliation is skipped entirely (for remote/pip workers).
	// Default is true (controller manages container).
	ContainerManaged *bool `json:"containerManaged,omitempty"`

	// State is the desired lifecycle state of the worker.
	// Valid values: "Running" (default), "Sleeping", "Stopped".
	// The controller reconciles actual backend state toward this desired state.
	State *string `json:"state,omitempty"`

	// AccessEntries declares the cloud permissions this worker should be
	// granted via hiclaw-credential-provider. See AccessEntry for semantics.
	// When empty the controller applies a sensible default (object-storage
	// scoped to agents/<name>/* and shared/*).
	AccessEntries []AccessEntry `json:"accessEntries,omitempty"`

	// AgentIdentity carries non-secret workload identity metadata used by
	// managed runtimes when resolving runtime credential bindings.
	AgentIdentity *AgentIdentitySpec `json:"agentIdentity,omitempty"`

	// CredentialBindings declares credential references available to the
	// worker runtime. Bindings never contain real credential values and are
	// intentionally separate from Env, which is container-global.
	CredentialBindings []CredentialBinding `json:"credentialBindings,omitempty"`

	// DeployMode specifies where the worker pod runs.
	// "Local" (default): created in the controller's own cluster.
	// "Edge": externally hosted outside the controller's managed pod path.
	DeployMode *string `json:"deployMode,omitempty"`

	// ServiceEnabled controls whether a ClusterIP Service is created
	// alongside the worker pod (same cluster, namespace, name).
	ServiceEnabled *bool `json:"serviceEnabled,omitempty"`

	// Env holds user-defined environment variables injected into the worker
	// container. Keys that collide with variables already set by the
	// controller or backend (AGENTTEAMS_*, OPENCLAW_*, HOME, and similar
	// internal keys) are silently ignored with a warning log — the system
	// value always wins.
	Env map[string]string `json:"env,omitempty"`

	// BackendRuntime specifies the container runtime backend for this worker.
	// "pod" (default): creates a standard Kubernetes Pod.
	// Only effective in incluster mode; ignored in embedded (Docker) mode.
	BackendRuntime *string `json:"backendRuntime,omitempty"`

	// Labels are user-defined Pod labels stamped onto the worker Pod.
	// Merged under the four-layer priority order (see controller docs):
	// pod-template < CR metadata.labels < CR spec.labels < controller
	// system labels. Entries whose keys collide with controller-forced
	// system labels (agentteams.io/controller, agentteams.io/worker, etc.) are
	// silently overridden. Must carry the omitempty tag so Teams that
	// embed WorkerSpec-shaped hashes keep a stable spec hash when the
	// field is absent.
	Labels map[string]string `json:"labels,omitempty"`

	// Volumes is reserved for runtimes that provide custom external storage
	// mounts. It is not supported by the open-source pod backend.
	Volumes []WorkerVolumeSpec `json:"volumes,omitempty"`

	// Mounts is reserved for runtimes that provide custom dynamic mounts. It is
	// not supported by the open-source pod backend.
	Mounts []WorkerMountSpec `json:"mounts,omitempty"`
}

type WorkerVolumeSpec struct {
	Name string               `json:"name"`
	Type string               `json:"type"` // OSS
	OSS  *WorkerOSSVolumeSpec `json:"oss,omitempty"`
}

type WorkerMountSpec struct {
	Name      string `json:"name"`
	VolumeRef string `json:"volumeRef"`
	SubPath   string `json:"subPath"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly"`
}

type WorkerOSSVolumeSpec struct {
	Bucket   string            `json:"bucket,omitempty"`
	Endpoint string            `json:"endpoint,omitempty"`
	Auth     WorkerOSSAuthSpec `json:"auth,omitempty"`
}

type WorkerOSSAuthSpec struct {
	Type      string                   `json:"type,omitempty"`
	RRSA      *WorkerOSSRRSASpec       `json:"rrsa,omitempty"`
	AccessKey *WorkerAccessKeyAuthSpec `json:"accessKey,omitempty"`
}

type WorkerOSSRRSASpec struct {
	RoleName string `json:"roleName,omitempty"`
	RoleARN  string `json:"roleArn,omitempty"`
}

type WorkerAccessKeyAuthSpec struct {
	// SecretRef names the target-cluster Secret used by the CSI driver for
	// mounting this OSS volume. The controller does not read this Secret when
	// writing worker-deps objects; those are written through the main AgentTeams
	// workspace OSS client.
	SecretRef NamespacedSecretRef `json:"secretRef,omitempty"`
}

type NamespacedSecretRef struct {
	Name string `json:"name,omitempty"`
	// Namespace must be omitted for worker OSS AccessKey auth. The mount
	// Secret is resolved in the worker targetNamespace.
	Namespace string `json:"namespace,omitempty"`
}

// GetBackendRuntime returns the explicitly set backendRuntime from spec, or empty string
// if not set. Empty means "use cluster-level default from AGENTTEAMS_WORKER_BACKEND_RUNTIME".
func (s WorkerSpec) GetBackendRuntime() string {
	if s.BackendRuntime != nil && *s.BackendRuntime != "" {
		return *s.BackendRuntime
	}
	return ""
}

// DesiredContainerMan returns the effective desired containerManaged, defaulting to true.
func (s WorkerSpec) DesiredContainerMan() bool {
	if s.ContainerManaged != nil {
		return *s.ContainerManaged
	}
	return true
}

// DesiredState returns the effective desired state, defaulting to "Running".
func (s WorkerSpec) DesiredState() string {
	if s.State != nil && *s.State != "" {
		return *s.State
	}
	return "Running"
}

// EffectiveWorkerName returns the runtime identity key for a Worker.
// Empty WorkerName falls back to metadata.name supplied by caller.
func (s WorkerSpec) EffectiveWorkerName(metadataName string) string {
	if s.WorkerName != "" {
		return s.WorkerName
	}
	return metadataName
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

type ChannelsSpec struct {
	DingTalk *DingTalkChannelSpec `json:"dingtalk,omitempty"`
}

type DingTalkChannelSpec struct {
	Enabled          *bool  `json:"enabled"`
	ClientID         string `json:"clientId,omitempty"`
	ClientSecret     string `json:"clientSecret,omitempty"`
	RobotCode        string `json:"robotCode,omitempty"`
	ShowThinking     bool   `json:"showThinking,omitempty"`
	ShowToolCalls    bool   `json:"showToolCalls,omitempty"`
	StreamingEnabled bool   `json:"streamingEnabled,omitempty"`
	MessageType      string `json:"messageType,omitempty"`
	CardTemplateID   string `json:"cardTemplateId,omitempty"`
}

type WorkerStatus struct {
	ObservedGeneration int64               `json:"observedGeneration,omitempty"`
	SpecHash           string              `json:"specHash,omitempty"`
	Phase              string              `json:"phase,omitempty"` // Pending/Running/Sleeping/Failed
	MatrixUserID       string              `json:"matrixUserID,omitempty"`
	RoomID             string              `json:"roomID,omitempty"`
	ContainerState     string              `json:"containerState,omitempty"`
	LastHeartbeat      string              `json:"lastHeartbeat,omitempty"`
	LastActiveAt       string              `json:"lastActiveAt,omitempty"`
	Message            string              `json:"message,omitempty"`
	ExposedPorts       []ExposedPortStatus `json:"exposedPorts,omitempty"`

	// BackendRuntime records the backend type currently used for this worker's container.
	// Set after successful creation or backend switch.
	// Values: "pod" (default), or "" (unset = migration, treated as spec default).
	// Only meaningful in incluster mode; Docker mode leaves this empty.
	BackendRuntime string `json:"backendRuntime,omitempty"`

	// DeployMode records where the current backend resource was actually
	// provisioned.
	DeployMode string `json:"deployMode,omitempty"`
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
	// include the AgentTeams CLI (`agt get workers <name> -o json | jq .roomID`)
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
type Human struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              HumanSpec   `json:"spec"`
	Status            HumanStatus `json:"status,omitempty"`
}

type HumanSpec struct {
	DisplayName       string              `json:"displayName"`
	Username          string              `json:"username,omitempty"`
	Email             string              `json:"email,omitempty"`
	PermissionLevel   int                 `json:"permissionLevel"` // 1=Admin, 2=Team, 3=Worker
	AccessibleTeams   []string            `json:"accessibleTeams,omitempty"`
	AccessibleWorkers []string            `json:"accessibleWorkers,omitempty"`
	IdentitySource    *IdentitySourceSpec `json:"identitySource,omitempty"`
	Note              string              `json:"note,omitempty"`
}

type IdentitySourceSpec struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

type HumanStatus struct {
	Phase                       string   `json:"phase,omitempty"` // Pending/Active/Failed/Degraded
	MatrixUserID                string   `json:"matrixUserID,omitempty"`
	InitialPassword             string   `json:"initialPassword,omitempty"` // Set on creation, shown once
	DisplayNameSyncedGeneration int64    `json:"displayNameSyncedGeneration,omitempty"`
	Rooms                       []string `json:"rooms,omitempty"`
	EmailSent                   bool     `json:"emailSent,omitempty"`
	Message                     string   `json:"message,omitempty"`
}

// EffectiveUsername returns the Matrix localpart for a Human.
// Empty username falls back to metadata.name supplied by caller.
func (s HumanSpec) EffectiveUsername(metadataName string) string {
	if s.Username != "" {
		return s.Username
	}
	return metadataName
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

// Manager represents the AgentTeams Manager Agent — the coordinator that receives
// natural-language instructions from Admin and orchestrates Workers/Teams via
// the hiclaw CLI / Controller REST API.
type Manager struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ManagerSpec   `json:"spec"`
	Status            ManagerStatus `json:"status,omitempty"`
}

type ManagerSpec struct {
	Model         string                     `json:"model"`
	ModelProvider string                     `json:"modelProvider,omitempty"` // APIG Model API name for per-manager LLM provider
	Runtime       string                     `json:"runtime,omitempty"`       // openclaw | copaw | hermes (default: openclaw)
	Image         string                     `json:"image,omitempty"`         // custom Docker image
	Soul          string                     `json:"soul,omitempty"`          // custom SOUL.md content
	Agents        string                     `json:"agents,omitempty"`        // custom AGENTS.md content
	Skills        []string                   `json:"skills,omitempty"`        // on-demand skills to enable
	McpServers    []MCPServer                `json:"mcpServers,omitempty"`    // MCP servers callable by the Manager via mcporter
	Package       string                     `json:"package,omitempty"`       // file://, http(s)://, or nacos://; optional ?authType= for Nacos
	Config        ManagerConfig              `json:"config,omitempty"`
	Resources     *AgentResourceRequirements `json:"resources,omitempty"`

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
	SpecHash           string `json:"specHash,omitempty"`
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
