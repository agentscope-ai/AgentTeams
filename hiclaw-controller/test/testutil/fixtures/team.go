package fixtures

import (
	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewTestTeam builds a Team CR with the given leader and worker member refs.
// Member runtime config belongs to the referenced Worker CRs.
func NewTestTeam(name, leaderName string, workerNames ...string) *v1beta1.Team {
	team := &v1beta1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: DefaultNamespace,
		},
		Spec: v1beta1.TeamSpec{
			WorkerMembers: []v1beta1.TeamWorkerRef{
				{Name: leaderName, Role: "team_leader"},
			},
		},
	}
	for _, wn := range workerNames {
		team.Spec.WorkerMembers = append(team.Spec.WorkerMembers, v1beta1.TeamWorkerRef{
			Name: wn,
			Role: "worker",
		})
	}
	return team
}

// NewTestTeamWorkers builds the Worker CRs referenced by a test Team's
// spec.workerMembers.
func NewTestTeamWorkers(team *v1beta1.Team) []*v1beta1.Worker {
	workers := make([]*v1beta1.Worker, 0, len(team.Spec.WorkerMembers))
	for _, member := range team.Spec.WorkerMembers {
		worker := NewTestWorker(member.Name)
		worker.Namespace = team.Namespace
		worker.Spec.Runtime = "copaw"
		workers = append(workers, worker)
	}
	return workers
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
