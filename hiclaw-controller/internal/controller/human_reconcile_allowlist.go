package controller

import (
	"context"
	"sort"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const humanManagedAllowlistAnnotation = "hiclaw.io/human-group-allow-extra"

// reconcileHumanWorkerAllowlist keeps standalone Worker group allowlists aligned
// with Human.spec.accessibleWorkers. Room membership lets a Human enter the
// room; groupAllowExtra lets runtimes such as CoPaw accept that Human's
// messages once they are there.
func (r *HumanReconciler) reconcileHumanWorkerAllowlist(ctx context.Context, s *humanScope) {
	logger := log.FromContext(ctx)
	h := s.human

	matrixUserID := h.Status.MatrixUserID
	if matrixUserID == "" {
		matrixUserID = r.Provisioner.MatrixUserID(s.username)
	}

	desired := make(map[string]struct{}, len(h.Spec.AccessibleWorkers))
	for _, name := range h.Spec.AccessibleWorkers {
		if strings.TrimSpace(name) != "" {
			desired[name] = struct{}{}
		}
	}

	var workers v1beta1.WorkerList
	if err := r.List(ctx, &workers, client.InNamespace(h.Namespace)); err != nil {
		logger.Error(err, "failed to list workers for human allowlist sync")
		return
	}

	for i := range workers.Items {
		w := &workers.Items[i]
		_, shouldAllow := desired[w.Name]
		managed := annotationSet(w.Annotations[humanManagedAllowlistAnnotation])
		_, isManaged := managed[h.Name]

		if shouldAllow {
			if err := r.addHumanToWorkerAllowlist(ctx, w, h.Name, matrixUserID, isManaged); err != nil {
				logger.Error(err, "failed to add human to worker allowlist", "worker", w.Name, "human", h.Name)
			}
			continue
		}

		if isManaged {
			if err := r.removeHumanFromWorkerAllowlist(ctx, w, h.Name, matrixUserID); err != nil {
				logger.Error(err, "failed to remove human from worker allowlist", "worker", w.Name, "human", h.Name)
			}
		}
	}
}

func (r *HumanReconciler) cleanupHumanWorkerAllowlist(ctx context.Context, s *humanScope) {
	logger := log.FromContext(ctx)
	matrixUserID := s.human.Status.MatrixUserID
	if matrixUserID == "" {
		matrixUserID = r.Provisioner.MatrixUserID(s.username)
	}

	var workers v1beta1.WorkerList
	if err := r.List(ctx, &workers, client.InNamespace(s.human.Namespace)); err != nil {
		logger.Error(err, "failed to list workers for human allowlist cleanup")
		return
	}
	for i := range workers.Items {
		w := &workers.Items[i]
		if _, ok := annotationSet(w.Annotations[humanManagedAllowlistAnnotation])[s.human.Name]; ok {
			if err := r.removeHumanFromWorkerAllowlist(ctx, w, s.human.Name, matrixUserID); err != nil {
				logger.Error(err, "failed to clean up human worker allowlist", "worker", w.Name, "human", s.human.Name)
			}
		}
	}
}

func (r *HumanReconciler) addHumanToWorkerAllowlist(ctx context.Context, w *v1beta1.Worker, humanName, matrixUserID string, alreadyManaged bool) error {
	base := w.DeepCopy()
	if w.Annotations == nil {
		w.Annotations = map[string]string{}
	}
	managed := annotationSet(w.Annotations[humanManagedAllowlistAnnotation])
	changed := false

	if w.Spec.ChannelPolicy == nil {
		w.Spec.ChannelPolicy = &v1beta1.ChannelPolicySpec{}
		changed = true
	}
	if !containsStringValue(w.Spec.ChannelPolicy.GroupAllowExtra, matrixUserID) {
		w.Spec.ChannelPolicy.GroupAllowExtra = append(w.Spec.ChannelPolicy.GroupAllowExtra, matrixUserID)
		managed[humanName] = struct{}{}
		changed = true
	} else if alreadyManaged {
		managed[humanName] = struct{}{}
	}

	w.Annotations[humanManagedAllowlistAnnotation] = encodeAnnotationSet(managed)
	if len(managed) == 0 {
		delete(w.Annotations, humanManagedAllowlistAnnotation)
	}
	if !changed {
		return nil
	}
	return r.Patch(ctx, w, client.MergeFrom(base))
}

func (r *HumanReconciler) removeHumanFromWorkerAllowlist(ctx context.Context, w *v1beta1.Worker, humanName, matrixUserID string) error {
	base := w.DeepCopy()
	if w.Spec.ChannelPolicy != nil {
		w.Spec.ChannelPolicy.GroupAllowExtra = removeString(w.Spec.ChannelPolicy.GroupAllowExtra, matrixUserID)
	}
	managed := annotationSet(w.Annotations[humanManagedAllowlistAnnotation])
	delete(managed, humanName)
	if w.Annotations != nil {
		if len(managed) == 0 {
			delete(w.Annotations, humanManagedAllowlistAnnotation)
		} else {
			w.Annotations[humanManagedAllowlistAnnotation] = encodeAnnotationSet(managed)
		}
	}
	return r.Patch(ctx, w, client.MergeFrom(base))
}

func annotationSet(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out[part] = struct{}{}
		}
	}
	return out
}

func encodeAnnotationSet(values map[string]struct{}) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func removeString(values []string, target string) []string {
	out := values[:0]
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

func containsStringValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
