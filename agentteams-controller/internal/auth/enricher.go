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
const teamWorkerMembersField = "spec.workerMembers.name"

// IdentityEnricher resolves additional identity fields (role, team) from
// the backing store. Called after authentication to fill the full CallerIdentity.
type IdentityEnricher interface {
	EnrichIdentity(ctx context.Context, identity *CallerIdentity) error
}

// CREnricher enriches Worker callers from their Worker CR and Team membership.
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

	// Every Worker identity is backed by a Worker CR.
	var worker v1beta1.Worker
	key := client.ObjectKey{Name: identity.Username, Namespace: e.namespace}
	err := e.client.Get(ctx, key, &worker)
	switch {
	case err == nil:
		runtimeName := worker.Spec.EffectiveWorkerName(worker.Name)
		identity.WorkerName = runtimeName
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
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("enrich identity: get worker %q: %w", identity.Username, err)
	}

	// No Worker CR: leave the caller unresolved. The authorizer will restrict it
	// to its own username.
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
