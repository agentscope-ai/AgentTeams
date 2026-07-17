// +k8s:deepcopy-gen=package

package v1beta1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
// +kubebuilder:printcolumn:name="Team",type=string,JSONPath=`.spec.team`
// +kubebuilder:printcolumn:name="Repos",type=integer,JSONPath=`.status.repoCount`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Worker represents an AI agent worker in AgentTeams.
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
type ExposedPortStatus struct {
	Port   int    `json:"port"`
	Domain string `json:"domain"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
