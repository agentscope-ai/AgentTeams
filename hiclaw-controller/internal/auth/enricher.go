package auth

import (
	"context"
	"fmt"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Team field indexers registered by app.initFieldIndexers. Duplicated as
// string constants here (instead of importing the controller package) to
// avoid a circular dependency between auth and controller.
const (
	teamLeaderNameField = "spec.leader.name"
	teamWorkerNameField = "spec.workerNames"
)

// IdentityEnricher resolves additional identity fields (role, team) from
// the backing store. Called after authentication to fill the full CallerIdentity.
type IdentityEnricher interface {
	EnrichIdentity(ctx context.Context, identity *CallerIdentity) error
}

// CREnricher enriches CallerIdentity for worker callers. Standalone workers
// resolve from their Worker CR (annotations are authoritative). Team members
// no longer have Worker CRs post-refactor, so the enricher falls back to a
// reverse lookup against Team CRs via field indexers.
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

	// 1. Try Worker CR (standalone worker case).
	var worker v1beta1.Worker
	key := client.ObjectKey{Name: identity.Username, Namespace: e.namespace}
	err := e.client.Get(ctx, key, &worker)
	switch {
	case err == nil:
		runtimeName := worker.Spec.EffectiveWorkerName(worker.Name)
		identity.Username = runtimeName
		identity.WorkerName = runtimeName
		if role := worker.Annotations["hiclaw.io/role"]; role == "team_leader" {
			identity.Role = RoleTeamLeader
		}
		if team := worker.Annotations["hiclaw.io/team"]; team != "" {
			identity.Team = team
		}
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("enrich identity: get worker %q: %w", identity.Username, err)
	}

	// 2. Worker CR missing — fall back to Team CR reverse lookup. A worker
	//    name can only belong to one team at a time; the same is true for
	//    leaders (a leader is not referenced as a worker in its own Team).
	if team, ok, lerr := e.lookupTeamByField(ctx, teamLeaderNameField, identity.Username); lerr != nil {
		return fmt.Errorf("enrich identity: lookup team leader %q: %w", identity.Username, lerr)
	} else if ok {
		identity.Role = RoleTeamLeader
		identity.Team = team.Name
		runtimeName := team.Spec.Leader.EffectiveWorkerName()
		identity.Username = runtimeName
		identity.WorkerName = runtimeName
		return nil
	}

	if team, ok, werr := e.lookupTeamByField(ctx, teamWorkerNameField, identity.Username); werr != nil {
		return fmt.Errorf("enrich identity: lookup team worker %q: %w", identity.Username, werr)
	} else if ok {
		identity.Team = team.Name
		for _, w := range team.Spec.Workers {
			if w.Name == identity.Username {
				runtimeName := w.EffectiveWorkerName()
				identity.Username = runtimeName
				identity.WorkerName = runtimeName
				break
			}
		}
		return nil
	}

	// No Worker CR and no Team membership: leave as a vanilla Worker caller.
	// The authorizer will apply the worker-scope permission check against the
	// username itself.
	return nil
}

func (e *CREnricher) lookupTeamByField(ctx context.Context, field, value string) (*v1beta1.Team, bool, error) {
	var list v1beta1.TeamList
	if err := e.client.List(ctx, &list,
		client.InNamespace(e.namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(field, value)},
	); err != nil {
		return nil, false, err
	}
	if len(list.Items) == 0 {
		return nil, false, nil
	}
	return &list.Items[0], true, nil
}
