package fixtures

import (
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewTestTeam builds a Team CR with the given leader name and worker names.
// The team is placed in the default test namespace with a leader running the
// copaw runtime (matching TeamReconciler's fixed runtime for leaders).
func NewTestTeam(name, leaderName string, workerNames ...string) *v1beta1.Team {
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
		},
		Spec: v1beta1.TeamSpec{
			Leader: v1beta1.LeaderSpec{
				Name:  leaderName,
				Model: "gpt-4o",
			},
		},
	}
	for _, wn := range workerNames {
		team.Spec.Workers = append(team.Spec.Workers, v1beta1.TeamWorkerSpec{
			Name:  wn,
			Model: "gpt-4o",
		})
	}
	return team
}

// WithTeamHeartbeat configures the leader's heartbeat on a Team CR.
func WithTeamHeartbeat(team *v1beta1.Team, every string) *v1beta1.Team {
	team.Spec.Leader.Heartbeat = &v1beta1.TeamLeaderHeartbeatSpec{
		Enabled: true,
		Every:   every,
	}
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

// WithTeamWorkerExpose adds an expose port to a specific team worker (by name).
func WithTeamWorkerExpose(team *v1beta1.Team, workerName string, port int) *v1beta1.Team {
	for i := range team.Spec.Workers {
		if team.Spec.Workers[i].Name == workerName {
			team.Spec.Workers[i].Expose = append(team.Spec.Workers[i].Expose, v1beta1.ExposePort{
				Port:     port,
				Protocol: "http",
			})
			return team
		}
	}
	return team
}
