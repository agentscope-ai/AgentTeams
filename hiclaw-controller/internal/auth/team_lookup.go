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
	teamLeaderNameField    = "spec.leader.name"
	teamWorkerNameField    = "spec.workerNames"        // legacy spec.workers
	teamWorkerMembersField = "spec.workerMembers.name" // decoupled spec.workerMembers
)

// LookupWorkerTeam returns the team name a given worker (by CR name) belongs
// to, by reverse-querying the Team CR cache via informer field indexers.
//
// Used by enricher / middleware / API handlers as the single source of truth
// for team membership: never read hiclaw.io/team annotations on Worker CRs,
// because Team CR.spec.workerMembers/workers/leader.name is authoritative.
//
// Returns "" when the worker is not a member of any team, including when the
// client lookup fails — callers treat the worker as standalone in that case.
func LookupWorkerTeam(ctx context.Context, c client.Reader, namespace, workerName string) string {
	if workerName == "" || c == nil {
		return ""
	}
	if name := lookupTeamByField(ctx, c, namespace, teamLeaderNameField, workerName); name != "" {
		return name
	}
	if name := lookupTeamByField(ctx, c, namespace, teamWorkerMembersField, workerName); name != "" {
		return name
	}
	if name := lookupTeamByField(ctx, c, namespace, teamWorkerNameField, workerName); name != "" {
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
	if name := lookupTeamByField(ctx, c, namespace, teamLeaderNameField, workerName); name != "" {
		return name, true
	}
	if name := lookupTeamByField(ctx, c, namespace, teamWorkerMembersField, workerName); name != "" {
		return name, false
	}
	if name := lookupTeamByField(ctx, c, namespace, teamWorkerNameField, workerName); name != "" {
		return name, false
	}
	return "", false
}

// lookupTeamByField runs a cache-backed List with an exact-match field
// selector. Returns the first matching Team's metadata.name, or "" on miss
// or transient error. Errors are intentionally swallowed: identity/auth
// callers degrade to "no team" rather than fail the request.
func lookupTeamByField(ctx context.Context, c client.Reader, namespace, field, value string) string {
	var list v1beta1.TeamList
	if err := c.List(ctx, &list,
		client.InNamespace(namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(field, value)},
	); err != nil {
		return ""
	}
	if len(list.Items) == 0 {
		return ""
	}
	return list.Items[0].Name
}
