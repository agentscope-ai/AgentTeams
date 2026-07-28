package controller

import (
	"context"
	"encoding/json"
	"fmt"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *HumanReconciler) reconcileHumanWorkerAllowlists(ctx context.Context, s *humanScope) error {
	desired := make(map[string]struct{}, len(s.human.Spec.AccessibleWorkers))
	for _, workerName := range s.human.Spec.AccessibleWorkers {
		desired[workerName] = struct{}{}
	}
	return r.syncHumanWorkerAllowlists(ctx, s.human, s.identity.MatrixUserID, desired)
}

func (r *HumanReconciler) cleanupHumanWorkerAllowlists(ctx context.Context, s *humanScope) error {
	userID := s.human.Status.MatrixUserID
	if userID == "" {
		userID = s.identity.MatrixUserID
	}
	return r.syncHumanWorkerAllowlists(ctx, s.human, userID, nil)
}

func (r *HumanReconciler) syncHumanWorkerAllowlists(
	ctx context.Context,
	human *v1beta1.Human,
	userID string,
	desired map[string]struct{},
) error {
	var workers v1beta1.WorkerList
	if err := r.List(ctx, &workers, client.InNamespace(human.Namespace)); err != nil {
		return fmt.Errorf("list workers for human allowlist: %w", err)
	}

	var errs []error
	for i := range workers.Items {
		worker := &workers.Items[i]
		_, shouldAllow := desired[worker.Name]
		if err := r.syncHumanWorkerAllowlist(ctx, worker, human.Name, userID, shouldAllow); err != nil {
			errs = append(errs, err)
		}
	}
	return kerrors.NewAggregate(errs)
}

func (r *HumanReconciler) syncHumanWorkerAllowlist(
	ctx context.Context,
	worker *v1beta1.Worker,
	humanName string,
	userID string,
	shouldAllow bool,
) error {
	managed, err := managedHumanAllowlist(worker.Annotations)
	if err != nil {
		return fmt.Errorf("parse managed Human allowlist for Worker %s: %w", worker.Name, err)
	}

	managedID, isManaged := managed[humanName]
	if !shouldAllow && !isManaged {
		return nil
	}

	base := worker.DeepCopy()
	changed := false
	if shouldAllow {
		if worker.Spec.ChannelPolicy == nil {
			worker.Spec.ChannelPolicy = &v1beta1.ChannelPolicySpec{}
		}
		allowlist := worker.Spec.ChannelPolicy.GroupAllowExtra
		if !containsString(allowlist, userID) {
			if isManaged && managedID != userID {
				worker.Spec.ChannelPolicy.GroupAllowExtra = removeString(allowlist, managedID)
			}
			worker.Spec.ChannelPolicy.GroupAllowExtra = append(worker.Spec.ChannelPolicy.GroupAllowExtra, userID)
			managed[humanName] = userID
			changed = true
		} else if isManaged && managedID != userID {
			worker.Spec.ChannelPolicy.GroupAllowExtra = removeString(allowlist, managedID)
			delete(managed, humanName)
			changed = true
		}
	} else {
		if worker.Spec.ChannelPolicy != nil {
			worker.Spec.ChannelPolicy.GroupAllowExtra = removeString(
				worker.Spec.ChannelPolicy.GroupAllowExtra,
				managedID,
			)
		}
		delete(managed, humanName)
		changed = true
	}

	if !changed {
		return nil
	}
	if err := setManagedHumanAllowlist(worker, managed); err != nil {
		return err
	}
	if err := r.Patch(ctx, worker, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch Worker %s Human allowlist: %w", worker.Name, err)
	}
	return nil
}

func managedHumanAllowlist(annotations map[string]string) (map[string]string, error) {
	managed := map[string]string{}
	if annotations == nil || annotations[v1beta1.AnnotationHumanManagedGroupAllowExtra] == "" {
		return managed, nil
	}
	if err := json.Unmarshal(
		[]byte(annotations[v1beta1.AnnotationHumanManagedGroupAllowExtra]),
		&managed,
	); err != nil {
		return nil, err
	}
	return managed, nil
}

func setManagedHumanAllowlist(worker *v1beta1.Worker, managed map[string]string) error {
	if worker.Annotations == nil {
		worker.Annotations = map[string]string{}
	}
	if len(managed) == 0 {
		delete(worker.Annotations, v1beta1.AnnotationHumanManagedGroupAllowExtra)
		return nil
	}
	value, err := json.Marshal(managed)
	if err != nil {
		return fmt.Errorf("marshal managed Human allowlist: %w", err)
	}
	worker.Annotations[v1beta1.AnnotationHumanManagedGroupAllowExtra] = string(value)
	return nil
}
