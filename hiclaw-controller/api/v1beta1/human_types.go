package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
