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
// resolved by reverse-querying Team.spec.workerMembers via informer cache field
// indexers. Worker CR annotations are NOT consulted, to avoid drift between
// Worker and Team reconcile cycles.
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

	teamName, isLeader := LookupWorkerTeamRole(ctx, e.client, e.namespace, identity.Username)
	if teamName != "" {
		identity.Team = teamName
		if isLeader {
			identity.Role = RoleTeamLeader
		}
	}

	var worker v1beta1.Worker
	key := client.ObjectKey{Name: identity.Username, Namespace: e.namespace}
	switch err := e.client.Get(ctx, key, &worker); {
	case err == nil:
		if err := e.validateRemoteWorkerIdentity(identity, &worker); err != nil {
			return err
		}
		identity.WorkerName = worker.Spec.EffectiveWorkerName(worker.Name)
		return nil
	case apierrors.IsNotFound(err):
		if identity.ClusterID != "" {
			return fmt.Errorf("remote identity %q is not backed by a Worker CR", identity.Username)
		}
		return nil
	default:
		return fmt.Errorf("enrich identity: get worker %q: %w", identity.Username, err)
	}
}

func (e *CREnricher) validateRemoteWorkerIdentity(identity *CallerIdentity, worker *v1beta1.Worker) error {
	if identity.ClusterID == "" {
		return nil
	}
	deployMode, targetCluster := remoteWorkerAppliedTarget(worker)
	if deployMode != v1beta1.DeployModeRemote {
		return fmt.Errorf("remote identity %q is not backed by a remote Worker", identity.Username)
	}
	if targetCluster == nil {
		return fmt.Errorf("remote Worker %q has no targetCluster", worker.Name)
	}
	if targetCluster.ID != identity.ClusterID {
		return fmt.Errorf("remote identity %q cluster %q does not match Worker target cluster %q",
			identity.Username, identity.ClusterID, targetCluster.ID)
	}
	if targetCluster.Namespace != identity.ServiceAccountNamespace {
		return fmt.Errorf("remote identity %q namespace %q does not match Worker target namespace %q",
			identity.Username, identity.ServiceAccountNamespace, targetCluster.Namespace)
	}
	expectedSA := e.prefix.SAName(RoleWorker, worker.Name)
	if identity.ServiceAccountName != expectedSA {
		return fmt.Errorf("remote identity %q serviceAccount %q does not match Worker serviceAccount %q",
			identity.Username, identity.ServiceAccountName, expectedSA)
	}
	return nil
}

func remoteWorkerAppliedTarget(worker *v1beta1.Worker) (string, *v1beta1.TargetClusterSpec) {
	if worker.Status.DeployMode != "" || worker.Status.TargetCluster != nil {
		deployMode := worker.Status.DeployMode
		if deployMode == "" {
			deployMode = v1beta1.DeployModeLocal
		}
		return deployMode, worker.Status.TargetCluster
	}
	if worker.Spec.DeployMode == nil || *worker.Spec.DeployMode == "" {
		return v1beta1.DeployModeLocal, nil
	}
	return *worker.Spec.DeployMode, worker.Spec.TargetCluster
}
