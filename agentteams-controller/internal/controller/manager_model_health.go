package controller

import (
	"context"
	"os"
	"time"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/modelhealth"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Model failure is diagnostic only and never triggers container recreation.
func (r *ManagerReconciler) reconcileManagerModelHealth(ctx context.Context, s *managerScope) {
	if os.Getenv("AGENTTEAMS_MODEL_PREFLIGHT_ENABLED") == "0" || s.provResult == nil || s.manager.Spec.DesiredState() != "Running" {
		return
	}
	checker, ok := r.Deployer.(interface {
		CheckManagerModel(context.Context, string, string, string, string, modelhealth.Notify) error
	})
	if !ok {
		return
	}
	notifier, ok := r.Provisioner.(interface {
		ModelHealthAdmin() string
		SendModelHealthNotice(context.Context, string, string, string, string) error
	})
	if !ok {
		return
	}
	if !r.modelHealthRunning.CompareAndSwap(false, true) {
		return
	}
	runtime := resolveManagerRuntime(s.manager.Spec.Runtime, r.DefaultRuntime)
	key, token, room := s.provResult.GatewayKey, s.provResult.MatrixToken, s.manager.Status.RoomID
	go func() {
		defer r.modelHealthRunning.Store(false)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		send := func(ctx context.Context, room, txn, body string) error {
			return notifier.SendModelHealthNotice(ctx, room, token, txn, body)
		}
		if err := checker.CheckManagerModel(ctx, runtime, key, notifier.ModelHealthAdmin(), room, send); err != nil {
			log.FromContext(ctx).Info("Manager model health diagnostics unavailable", "reason", err.Error())
		}
	}()
}
