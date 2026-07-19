package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Team",type=string,JSONPath=`.spec.team`
// +kubebuilder:printcolumn:name="Repos",type=integer,JSONPath=`.status.repoCount`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
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
	Runtime       string                     `json:"runtime,omitempty"`       // openclaw | copaw | hermes | openhuman | qwenpaw (default: openclaw)
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

// WorkerStatus holds the observed runtime state of a Worker.
type WorkerStatus struct {
	ObservedGeneration int64               `json:"observedGeneration,omitempty"`
	SpecHash           string              `json:"specHash,omitempty"`
	Phase              string              `json:"phase,omitempty"` // Pending/Running/Sleeping/Failed
	MatrixUserID       string              `json:"matrixUserID,omitempty"`
	RoomID             string              `json:"roomID,omitempty"`
	ContainerState     string              `json:"containerState,omitempty"`
	LastHeartbeat      string              `json:"lastHeartbeat,omitempty"`
	LastActiveAt       string              `json:"lastActiveAt,omitempty"`
	LLMCallsLastHeartbeat int              `json:"llmCallsLastHeartbeat,omitempty"`
	LLMCallsTotal         int              `json:"llmCallsTotal,omitempty"`
	Message            string              `json:"message,omitempty"`
	ExposedPorts       []ExposedPortStatus `json:"exposedPorts,omitempty"`

	// HealthState is the controller-derived health classification for this worker.
	// Values: "" (unknown), "healthy", "stalled", "zombie", "idle".
	// Set by the HealthMonitorController based on heartbeat/activity staleness.
	HealthState string `json:"healthState,omitempty"`

	// LastHealthTransition records when HealthState last changed (RFC3339).
	LastHealthTransition string `json:"lastHealthTransition,omitempty"`

	// BackendRuntime records the backend type currently used for this worker's container.
	// Set after successful creation or backend switch.
	// Values: "pod" (default), or "" (unset = migration, treated as spec default).
	// Only meaningful in incluster mode; Docker mode leaves this empty.
	BackendRuntime string `json:"backendRuntime,omitempty"`

	// DeployMode records where the current backend resource was actually
	// provisioned.
	DeployMode string `json:"deployMode,omitempty"`
}

// +kubebuilder:object:root=true
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// WorkerList is a list of Worker resources.
type WorkerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Worker `json:"items"`
}
