package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type Manager struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ManagerSpec   `json:"spec"`
	Status            ManagerStatus `json:"status,omitempty"`
}
type ManagerSpec struct {
	Model         string                     `json:"model"`
	ModelProvider string                     `json:"modelProvider,omitempty"` // APIG Model API name for per-manager LLM provider
	Runtime       string                     `json:"runtime,omitempty"`       // openclaw | copaw (default: openclaw)
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

// +genclient
// +kubebuilder:subresource:status
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Project binds a Team to a working repo (RW) plus optional source repos (RO).
// Git access is via the existing gitea-mcp with a per-worker Gitea identity (no
// controller Gitea client): the controller only projects a flat manifest to MinIO
// at shared/projects/<id>/ and records the assigned workers + per-repo access.
// RW/RO is enforced via the assigned worker-user's Gitea repo-collaborator role
// (#13), applied by the provisioning helper (scripts/provision-worker-gitea.sh);
// the manifest carries the access as the source of that mapping. The CR is
// source of truth, MinIO is cache (decision #6).
