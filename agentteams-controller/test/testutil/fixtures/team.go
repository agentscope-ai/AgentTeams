package fixtures

import (
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewTestTeam builds a Team CR that references the given Worker CR names.
func NewTestTeam(name, leaderName string, workerNames ...string) *v1beta1.Team {
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
		},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{{Name: leaderName, Role: "team_leader"}},
		},
	}
	for _, wn := range workerNames {
		team.Spec.WorkerMembers = append(team.Spec.WorkerMembers, v1beta1.TeamWorkerRef{Name: wn, Role: "worker"})
	}
	return team
}

// WithTeamHeartbeat configures the Team Leader heartbeat interval.
func WithTeamHeartbeat(team *v1beta1.Team, every string) *v1beta1.Team {
	team.Spec.HeartbeatEvery = every
	return team
}

// WithTeamAdmin attaches a team admin to the Team CR. Used to verify admin
// gets added to both leader and worker channel policies.
func WithTeamAdmin(team *v1beta1.Team, name, matrixUserID string) *v1beta1.Team {
	team.Spec.Admin = &v1beta1.TeamAdminSpec{
		Name:         name,
		MatrixUserID: matrixUserID,
	}
	return team
}
