// Package v1beta1 registers the agentteams.io/v1beta1 API group, reusing the
// same Go types as hiclaw.io/v1beta1 via type aliases. This enables the
// controller to watch both groups with the same reconcile logic during the
// HiClaw → AgentTeams rename transition (Phase 0, see #861).
//
// +k8s:deepcopy-gen=package
package v1beta1

import (
	hiclaw "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	GroupName = "agentteams.io"
	Version   = "v1beta1"
)

var (
	SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}
	SchemeBuilder      = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme        = SchemeBuilder.AddToScheme
)

// Type aliases — the agentteams.io group uses the exact same struct
// definitions as hiclaw.io, just registered under a different group.
type (
	Worker      = hiclaw.Worker
	WorkerList  = hiclaw.WorkerList
	Team        = hiclaw.Team
	TeamList    = hiclaw.TeamList
	Human       = hiclaw.Human
	HumanList   = hiclaw.HumanList
	Manager     = hiclaw.Manager
	ManagerList = hiclaw.ManagerList
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&Worker{},
		&WorkerList{},
		&Team{},
		&TeamList{},
		&Human{},
		&HumanList{},
		&Manager{},
		&ManagerList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
