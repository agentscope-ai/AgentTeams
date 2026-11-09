package controller

import (
	"context"
	"fmt"
	"sort"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// humanWorkerGrants derives direct Worker access from the current Human CRs.
// Use provisioned Matrix IDs, not resource names: external identities and
// spec.username need not match metadata.name. Nothing is persisted in Worker
// specs, so deleting a grant or Human removes it on the next projection.
func humanWorkerGrants(ctx context.Context, reader client.Reader, namespace string) (map[string][]string, error) {
	var humans v1beta1.HumanList
	if err := reader.List(ctx, &humans, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Human Worker grants: %w", err)
	}
	grants := make(map[string][]string)
	for _, human := range humans.Items {
		if !human.DeletionTimestamp.IsZero() || human.Status.MatrixUserID == "" {
			continue
		}
		for _, worker := range human.Spec.AccessibleWorkers {
			grants[worker] = append(grants[worker], human.Status.MatrixUserID)
		}
	}
	for worker, ids := range grants {
		sort.Strings(ids)
		grants[worker] = uniqueTeamStrings(ids)
	}
	return grants, nil
}

func withHumanWorkerGrants(spec v1beta1.WorkerSpec, matrixIDs []string) v1beta1.WorkerSpec {
	if len(matrixIDs) == 0 {
		return spec
	}
	// Clone before adding derived permissions. Explicit deny entries remain
	// untouched and are applied by the existing runtime policy consumers.
	copy := spec.DeepCopy()
	if copy.ChannelPolicy == nil {
		copy.ChannelPolicy = &v1beta1.ChannelPolicySpec{}
	}
	copy.ChannelPolicy.GroupAllowExtra = uniqueTeamStrings(append(copy.ChannelPolicy.GroupAllowExtra, matrixIDs...))
	copy.ChannelPolicy.DmAllowExtra = uniqueTeamStrings(append(copy.ChannelPolicy.DmAllowExtra, matrixIDs...))
	return *copy
}

func (r *WorkerReconciler) humanToWorkerRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	if _, ok := obj.(*v1beta1.Human); !ok {
		return nil
	}
	var workers v1beta1.WorkerList
	if err := r.List(ctx, &workers, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(workers.Items))
	for i := range workers.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&workers.Items[i])})
	}
	return requests
}
