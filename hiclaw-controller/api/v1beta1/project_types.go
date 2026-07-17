package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ProjectSpec   `json:"spec"`
	Status            ProjectStatus `json:"status,omitempty"`
}
type ProjectSpec struct {
	Team        string `json:"team"` // required — team-scoped (decision #2)
	Description string `json:"description,omitempty"`
	// ProjectName is an immutable DNS-safe storage identity. Empty defaults to
	// metadata.name; Status.StorageKey captures the resolved value permanently.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="projectName is immutable"
	ProjectName string        `json:"projectName,omitempty"` // runtime/storage identity; defaults to metadata.name
	Repos       []ProjectRepo `json:"repos"`                 // >=1; exactly one SHOULD be access=rw
	Workers     []string      `json:"workers,omitempty"`     // runtime-names; empty = all members of spec.team
}

// ProjectRepo binds one repo at a given access level.
// access=rw|ro is enforced via the assigned worker-user's Gitea repo-collaborator
// role (#13: ro→read, rw→write), applied by the provisioning helper; carried in
// the manifest as the source of that mapping. No credential material lives here.
type ProjectRepo struct {
	URL    string `json:"url"`
	Access string `json:"access"`         // rw | ro — enforced via the worker-user's Gitea collaborator role (#13)
	Name   string `json:"name,omitempty"` // friendly label; defaults to owner/repo slug
}

// EffectiveProjectName mirrors WorkerSpec.EffectiveWorkerName / TeamSpec.EffectiveTeamName.
// Empty ProjectName falls back to metadata.name supplied by caller.
func (s ProjectSpec) EffectiveProjectName(metadataName string) string {
	if s.ProjectName != "" {
		return s.ProjectName
	}
	return metadataName
}

type ProjectStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// StorageKey is the immutable resolved object-storage path component.
	StorageKey string `json:"storageKey,omitempty"`
	// Phase: Pending/Provisioning/Ready/Degraded/Failed are reconciler-computed;
	// Completed/Archived are operator-set only (decision #18).
	Phase           string             `json:"phase,omitempty"`
	Message         string             `json:"message,omitempty"`
	RepoCount       int                `json:"repoCount,omitempty"`       // backs the Repos printer column
	RecordedWorkers []string           `json:"recordedWorkers,omitempty"` // workers recorded in the manifest; operator helper (#12) provisions Gitea user/mcp-gitea-<worker>/collaborator role from it
	Conditions      []ProjectCondition `json:"conditions,omitempty"`
}

// ProjectCondition mirrors the standard condition idiom.
// Type is one of StorageIdentityReady|ReposResolved|WorkersRecorded|
// MinIOProjected|ArchiveProjected|LeaderNotified|DeprovisionPending.
type ProjectCondition struct {
	Type               string      `json:"type"`
	Status             string      `json:"status"` // True|False|Unknown
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// ConditionByType returns a pointer to the ProjectCondition entry of the given
// type, or nil when absent.
func (s *ProjectStatus) ConditionByType(condType string) *ProjectCondition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == condType {
			return &s.Conditions[i]
		}
	}
	return nil
}

// SetCondition upserts a condition by type. LastTransitionTime is only bumped
// when the status actually changes, matching the standard k8s condition
// idiom (avoids status-patch churn on every reconcile pass).
func (s *ProjectStatus) SetCondition(condType, status, reason, message string) {
	now := metav1.Now()
	if existing := s.ConditionByType(condType); existing != nil {
		if existing.Status != status {
			existing.LastTransitionTime = now
		}
		existing.Status = status
		existing.Reason = reason
		existing.Message = message
		return
	}
	s.Conditions = append(s.Conditions, ProjectCondition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}
