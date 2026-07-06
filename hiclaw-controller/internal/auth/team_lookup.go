package auth

import (
	"context"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Team field indexer names registered by app.initFieldIndexers. Duplicated
// as string constants here (instead of importing the controller package) to
// avoid a circular dependency between auth and controller.
const (
	teamWorkerMembersField = "spec.workerMembers.name"
)

// LookupWorkerTeam returns the team name a given worker (by CR name) belongs
// to, by reverse-querying the Team CR cache via informer field indexers.
//
// Used by enricher / middleware / API handlers as the single source of truth
// for team membership: never read team annotations on Worker CRs,
// because Team CR.spec.workerMembers is authoritative.
//
// Returns "" when the worker is not a member of any team, including when the
// client lookup fails — callers treat the worker as standalone in that case.
func LookupWorkerTeam(ctx context.Context, c client.Reader, namespace, workerName string) string {
	if workerName == "" || c == nil {
		return ""
	}
	if name, _, ok := lookupDecoupledTeamRole(ctx, c, namespace, workerName); ok {
		return name
	}
	return ""
}

// LookupWorkerTeamRole returns (teamName, isLeader) for a given worker. When
// the worker is a Team Leader, isLeader=true; when a regular member,
// isLeader=false. When the worker is not a team member, returns ("", false).
func LookupWorkerTeamRole(ctx context.Context, c client.Reader, namespace, workerName string) (string, bool) {
	if workerName == "" || c == nil {
		return "", false
	}
	if name, isLeader, ok := lookupDecoupledTeamRole(ctx, c, namespace, workerName); ok {
		return name, isLeader
	}
	return "", false
}

// lookupDecoupledTeamRole resolves membership through spec.workerMembers.
func lookupDecoupledTeamRole(ctx context.Context, c client.Reader, namespace, workerName string) (teamName string, isLeader bool, ok bool) {
	var list v1beta1.TeamList
	if err := c.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(teamWorkerMembersField, workerName)},
	); err != nil {
		return "", false, false
	}
	for i := range list.Items {
		team := &list.Items[i]
		for _, ref := range team.Spec.WorkerMembers {
			if ref.Name != workerName {
				continue
			}
			return team.Name, ref.Role == "team_leader", true
		}
	}
	return "", false, false
}
