package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/executor"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HumanReconciler reconciles Human resources.
type HumanReconciler struct {
	client.Client
	Executor *executor.Shell
}

func (r *HumanReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	var human v1beta1.Human
	if err := r.Get(ctx, req.NamespacedName, &human); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !human.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&human, finalizerName) {
			if err := r.handleDelete(ctx, &human); err != nil {
				logger.Error(err, "failed to delete human", "name", human.Name)
				return reconcile.Result{RequeueAfter: 30 * time.Second}, err
			}
			controllerutil.RemoveFinalizer(&human, finalizerName)
			if err := r.Update(ctx, &human); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(&human, finalizerName) {
		controllerutil.AddFinalizer(&human, finalizerName)
		if err := r.Update(ctx, &human); err != nil {
			return reconcile.Result{}, err
		}
	}

	switch human.Status.Phase {
	case "":
		return r.handleCreate(ctx, &human)
	case "Failed":
		return r.handleCreate(ctx, &human)
	default:
		return r.handleUpdate(ctx, &human)
	}
}

func (r *HumanReconciler) handleCreate(ctx context.Context, h *v1beta1.Human) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("creating human", "name", h.Name)

	h.Status.Phase = "Pending"
	if err := r.Status().Update(ctx, h); err != nil {
		return reconcile.Result{}, err
	}

	// Build matrix ID from name and domain
	matrixID := h.Name
	if h.Status.MatrixUserID != "" {
		matrixID = h.Status.MatrixUserID
	}

	args := []string{
		"--matrix-id", matrixID,
		"--name", h.Spec.DisplayName,
		"--level", fmt.Sprintf("%d", h.Spec.PermissionLevel),
	}
	if len(h.Spec.AccessibleTeams) > 0 {
		args = append(args, "--teams", strings.Join(h.Spec.AccessibleTeams, ","))
	}
	if len(h.Spec.AccessibleWorkers) > 0 {
		args = append(args, "--workers", strings.Join(h.Spec.AccessibleWorkers, ","))
	}
	if h.Spec.Email != "" {
		args = append(args, "--email", h.Spec.Email)
	}
	if h.Spec.Note != "" {
		args = append(args, "--note", h.Spec.Note)
	}

	result, err := r.Executor.Run(ctx,
		"/opt/hiclaw/agent/skills/human-management/scripts/create-human.sh",
		args...,
	)
	if err != nil {
		h.Status.Phase = "Failed"
		h.Status.Message = fmt.Sprintf("create-human.sh failed: %v", err)
		r.Status().Update(ctx, h)
		return reconcile.Result{RequeueAfter: time.Minute}, err
	}

	h.Status.Phase = "Active"
	h.Status.Message = ""
	if result.JSON != nil {
		if mid, ok := result.JSON["matrix_user_id"].(string); ok {
			h.Status.MatrixUserID = mid
		}
		if pw, ok := result.JSON["password"].(string); ok {
			h.Status.InitialPassword = pw
		}
		if sent, ok := result.JSON["email_sent"].(bool); ok {
			h.Status.EmailSent = sent
		}
		if rooms, ok := result.JSON["rooms_invited"].([]interface{}); ok {
			for _, r := range rooms {
				if s, ok := r.(string); ok {
					h.Status.Rooms = append(h.Status.Rooms, s)
				}
			}
		}
	}
	if err := r.Status().Update(ctx, h); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("human created", "name", h.Name, "matrixUserID", h.Status.MatrixUserID)
	return reconcile.Result{}, nil
}

func (r *HumanReconciler) handleUpdate(ctx context.Context, h *v1beta1.Human) (reconcile.Result, error) {
	// TODO: detect permission level / accessible teams changes and reconfigure
	return reconcile.Result{}, nil
}

func (r *HumanReconciler) handleDelete(ctx context.Context, h *v1beta1.Human) error {
	logger := log.FromContext(ctx)
	logger.Info("deleting human", "name", h.Name)

	// Remove from humans-registry
	_, err := r.Executor.RunSimple(ctx,
		"/opt/hiclaw/agent/skills/human-management/scripts/manage-humans-registry.sh",
		"--action", "remove", "--name", h.Name,
	)
	if err != nil {
		logger.Error(err, "failed to remove human from registry", "name", h.Name)
	}

	// TODO: remove from all groupAllowFrom and kick from rooms

	return nil
}

// SetupWithManager registers the HumanReconciler with the controller manager.
func (r *HumanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Human{}).
		Complete(r)
}