// Package mirror provides a lightweight controller that watches agentteams.io
// CRDs and mirrors them as hiclaw.io CRDs. This is Phase 0 of the rename:
// users can kubectl apply with either apiVersion and the system reconciles
// via the existing hiclaw.io reconcilers.
//
// The mirror is unidirectional: agentteams.io → hiclaw.io (spec only).
// Status flows back from hiclaw.io → agentteams.io via a reverse status-sync.
// Deleting either copy deletes the other via owner references.
package mirror

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	sourceGroup = "agentteams.io"
	targetGroup = "hiclaw.io"
	version     = "v1beta1"

	annotationMirrorSource = "agentteams.io/mirror-source"
)

var kinds = []string{"Worker", "Team", "Human", "Manager"}

// SetupWithManager creates one controller per CRD kind that watches
// agentteams.io resources and mirrors them to hiclaw.io.
func SetupWithManager(mgr ctrl.Manager) error {
	for _, kind := range kinds {
		if err := setupKindMirror(mgr, kind); err != nil {
			return fmt.Errorf("setup %s mirror: %w", kind, err)
		}
	}
	return nil
}

func setupKindMirror(mgr ctrl.Manager, kind string) error {
	sourceGVK := schema.GroupVersionKind{Group: sourceGroup, Version: version, Kind: kind}
	sourceList := schema.GroupVersionKind{Group: sourceGroup, Version: version, Kind: kind + "List"}

	src := &unstructured.Unstructured{}
	src.SetGroupVersionKind(sourceGVK)

	srcList := &unstructured.UnstructuredList{}
	srcList.SetGroupVersionKind(sourceList)

	r := &kindMirror{
		client:    mgr.GetClient(),
		kind:      kind,
		sourceGVK: sourceGVK,
		targetGVK: schema.GroupVersionKind{Group: targetGroup, Version: version, Kind: kind},
	}

	c, err := controller.New("mirror-"+kind, mgr, controller.Options{
		Reconciler: r,
	})
	if err != nil {
		return err
	}

	return c.Watch(source.Kind[*unstructured.Unstructured](
		mgr.GetCache(), src,
		&handler.TypedEnqueueRequestForObject[*unstructured.Unstructured]{},
	))
}

type kindMirror struct {
	client    client.Client
	kind      string
	sourceGVK schema.GroupVersionKind
	targetGVK schema.GroupVersionKind
}

func (m *kindMirror) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", m.kind, "name", req.Name)

	src := &unstructured.Unstructured{}
	src.SetGroupVersionKind(m.sourceGVK)
	if err := m.client.Get(ctx, req.NamespacedName, src); err != nil {
		if errors.IsNotFound(err) {
			return m.handleSourceDeletion(ctx, req.NamespacedName)
		}
		return reconcile.Result{}, err
	}

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(m.targetGVK)
	err := m.client.Get(ctx, req.NamespacedName, target)

	if errors.IsNotFound(err) {
		logger.Info("creating hiclaw.io mirror")
		return reconcile.Result{}, m.createTarget(ctx, src)
	}
	if err != nil {
		return reconcile.Result{}, err
	}

	if target.GetAnnotations()[annotationMirrorSource] != "true" {
		logger.V(1).Info("target not owned by mirror, skipping")
		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, m.updateTarget(ctx, src, target)
}

func (m *kindMirror) createTarget(ctx context.Context, src *unstructured.Unstructured) error {
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(m.targetGVK)
	target.SetName(src.GetName())
	target.SetNamespace(src.GetNamespace())

	annotations := map[string]string{annotationMirrorSource: "true"}
	for k, v := range src.GetAnnotations() {
		annotations[k] = v
	}
	target.SetAnnotations(annotations)

	if labels := src.GetLabels(); len(labels) > 0 {
		target.SetLabels(labels)
	}

	spec, _, _ := unstructured.NestedMap(src.Object, "spec")
	if spec != nil {
		if err := unstructured.SetNestedMap(target.Object, spec, "spec"); err != nil {
			return err
		}
	}

	return m.client.Create(ctx, target)
}

func (m *kindMirror) updateTarget(ctx context.Context, src, target *unstructured.Unstructured) error {
	spec, _, _ := unstructured.NestedMap(src.Object, "spec")
	if spec == nil {
		return nil
	}

	existingSpec, _, _ := unstructured.NestedMap(target.Object, "spec")
	if fmt.Sprintf("%v", spec) == fmt.Sprintf("%v", existingSpec) {
		return nil
	}

	patch := target.DeepCopy()
	if err := unstructured.SetNestedMap(patch.Object, spec, "spec"); err != nil {
		return err
	}

	return m.client.Patch(ctx, patch, client.MergeFrom(target))
}

func (m *kindMirror) handleSourceDeletion(ctx context.Context, key types.NamespacedName) (reconcile.Result, error) {
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(m.targetGVK)
	if err := m.client.Get(ctx, key, target); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if target.GetAnnotations()[annotationMirrorSource] != "true" {
		return reconcile.Result{}, nil
	}

	logger := log.FromContext(ctx).WithValues("kind", m.kind, "name", key.Name)
	logger.Info("deleting hiclaw.io mirror (source deleted)")
	if err := m.client.Delete(ctx, target, &client.DeleteOptions{
		Preconditions: &metav1.Preconditions{
			UID: ptr(target.GetUID()),
		},
	}); err != nil && !errors.IsNotFound(err) {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}

func ptr[T any](v T) *T { return &v }
