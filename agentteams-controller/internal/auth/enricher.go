package auth

import (
	"context"
	"fmt"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Team field indexers registered by app.initFieldIndexers. Duplicated as
// string constants here (instead of importing the controller package) to
// avoid a circular dependency between auth and controller.
const (
	teamLeaderNameField    = "spec.leader.name"
	teamWorkerNameField    = "spec.workerNames"
	teamWorkerMembersField = "spec.workerMembers.name"
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
	prefix    ResourcePrefix
}

func NewCREnricher(c client.Client, namespace string, prefix ...ResourcePrefix) *CREnricher {
	p := DefaultResourcePrefix
	if len(prefix) > 0 {
		p = prefix[0].Or(DefaultResourcePrefix)
	}
	return &CREnricher{client: c, namespace: namespace, prefix: p}
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
		identity.WorkerName = runtimeName
		if role := worker.Annotations["agentteams.io/role"]; role == "team_leader" {
			identity.Role = RoleTeamLeader
		}
		if team := worker.Annotations["agentteams.io/team"]; team != "" {
			identity.Team = team
		}
		if identity.Team == "" {
			team, role, ok, lerr := e.lookupTeamWorkerMember(ctx, identity.Username)
			if lerr != nil {
				return fmt.Errorf("enrich identity: lookup worker member %q: %w", identity.Username, lerr)
			}
			if ok {
				identity.Team = team.Name
				if role == "team_leader" {
					identity.Role = RoleTeamLeader
				}
			}
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
				identity.WorkerName = runtimeName
				break
			}
		}
		return nil
	}
	if team, role, ok, werr := e.lookupTeamWorkerMember(ctx, identity.Username); werr != nil {
		return fmt.Errorf("enrich identity: lookup worker member %q: %w", identity.Username, werr)
	} else if ok {
		identity.Team = team.Name
		if role == "team_leader" {
			identity.Role = RoleTeamLeader
		}
		return nil
	}

	// No Worker CR and no Team membership: leave as a vanilla Worker caller.
	// The authorizer will apply the worker-scope permission check against the
	// username itself.
	return nil
}

func (e *CREnricher) lookupTeamWorkerMember(ctx context.Context, name string) (*v1beta1.Team, string, bool, error) {
	var list v1beta1.TeamList
	if err := e.client.List(ctx, &list,
		client.InNamespace(e.namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(teamWorkerMembersField, name)},
	); err != nil {
		if err := e.client.List(ctx, &list, client.InNamespace(e.namespace)); err != nil {
			return nil, "", false, err
		}
	}
	for i := range list.Items {
		team := &list.Items[i]
		for _, ref := range team.Spec.WorkerMembers {
			if ref.Name != name {
				continue
			}
			role := ref.Role
			if role == "" {
				role = "worker"
			}
			return team, role, true, nil
		}
	}
	return nil, "", false, nil
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
