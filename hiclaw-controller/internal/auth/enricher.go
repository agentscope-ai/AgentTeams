package auth

import (
	"context"
	"fmt"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IdentityEnricher resolves additional identity fields (role, team) from
// the backing store. Called after authentication to fill the full CallerIdentity.
type IdentityEnricher interface {
	EnrichIdentity(ctx context.Context, identity *CallerIdentity) error
}

// CREnricher enriches CallerIdentity for worker callers. Team membership is
// resolved by reverse-querying Team CR via informer cache field indexers
// (Team CR.spec.{leader.name,workerMembers,workers} as single source of
// truth) — Worker CR annotations are NOT consulted, to avoid drift between
// Worker and Team reconcile cycles.
//
// In the decoupled model, members are standalone Worker CRs referenced from
// Team.spec.workerMembers; in the legacy model, members live inline in
// Team.spec.workers without their own Worker CRs. Both shapes are covered
// transparently by LookupWorkerTeamRole.
type CREnricher struct {
	client    client.Client
	namespace string
}

func NewCREnricher(c client.Client, namespace string) *CREnricher {
	return &CREnricher{client: c, namespace: namespace}
}

func (e *CREnricher) EnrichIdentity(ctx context.Context, identity *CallerIdentity) error {
	if identity == nil {
		return nil
	}

	// Admin and Manager identities are fully resolved from SA name alone.
	if identity.Role == RoleAdmin || identity.Role == RoleManager {
		return nil
	}

	// Reverse-lookup team membership via Team CR cache. This works for
	// both decoupled (spec.workerMembers) and legacy (spec.workers)
	// shapes, and covers leaders via spec.leader.name.
	teamName, isLeader := LookupWorkerTeamRole(ctx, e.client, e.namespace, identity.Username)
	if teamName != "" {
		identity.Team = teamName
		if isLeader {
			identity.Role = RoleTeamLeader
		}
	}

	// Try Worker CR for runtime name (decoupled members and standalone
	// workers always have a Worker CR; legacy team members do not).
	var worker v1beta1.Worker
	key := client.ObjectKey{Name: identity.Username, Namespace: e.namespace}
	switch err := e.client.Get(ctx, key, &worker); {
	case err == nil:
		identity.WorkerName = worker.Spec.EffectiveWorkerName(worker.Name)
		return nil
	case apierrors.IsNotFound(err):
		// Worker CR missing — legacy team member path. Resolve runtime
		// name from the Team CR (already located above).
	default:
		return fmt.Errorf("enrich identity: get worker %q: %w", identity.Username, err)
	}

	if teamName != "" {
		identity.WorkerName = legacyTeamMemberRuntimeName(ctx, e.client, e.namespace, teamName, identity.Username, isLeader)
	}
	return nil
}

// legacyTeamMemberRuntimeName resolves the runtime name for a legacy team
// member (no Worker CR). Decoupled members never reach this path because
// their Worker CR Get succeeds in the caller above.
func legacyTeamMemberRuntimeName(ctx context.Context, c client.Reader, namespace, teamName, username string, isLeader bool) string {
	var team v1beta1.Team
	if err := c.Get(ctx, client.ObjectKey{Name: teamName, Namespace: namespace}, &team); err != nil {
		return username
	}
	if isLeader {
		return team.Spec.Leader.EffectiveWorkerName()
	}
	for _, w := range team.Spec.Workers {
		if w.Name == username {
			return w.EffectiveWorkerName()
		}
	}
	return username
}
