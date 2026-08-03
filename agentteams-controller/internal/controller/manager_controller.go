package controller

import (
	"context"
	"fmt"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ManagerEmbeddedConfig holds embedded-mode settings for the Manager Agent
// container (workspace mount, host share, extra env from the controller's env).
type ManagerEmbeddedConfig struct {
	WorkspaceDir       string            // host path for /root/manager-workspace
	HostShareDir       string            // host path for /host-share
	ExtraEnv           map[string]string // infrastructure env vars forwarded to agent
	ManagerConsolePort string            // host port for manager console (default: 18888)
}

// ManagerReconciler reconciles Manager resources.
type ManagerReconciler struct {
	client.Client

	Provisioner      service.ManagerProvisioner
	Deployer         service.ManagerDeployer
	Backend          *backend.Registry
	EnvBuilder       service.ManagerEnvBuilderI
	ManagerResources *backend.ResourceRequirements
	ResourcePrefix   auth.ResourcePrefix    // tenant prefix used to derive Pod names and labels
	EmbeddedConfig   *ManagerEmbeddedConfig // non-nil in embedded mode only
	GatewayClient    gateway.Client         // gateway client for modelProvider resolution

	// DefaultRuntime is the value passed to backend.CreateRequest.RuntimeFallback
	// when a Manager CR omits spec.runtime. Sourced from AGENTTEAMS_MANAGER_RUNTIME
	// (Config.ManagerRuntime). Distinct from WorkerReconciler.DefaultRuntime
	// because Backend.Create is shared and cannot tell which env var applies.
	DefaultRuntime string

	// ControllerName identifies this controller instance. Stamped on every
	// Manager Pod via agentteams.io/controller so multi-instance deployments
	// sharing a namespace do not cross-watch each other's resources.
	ControllerName             string
	AuthTokenExpirationSeconds int64

	// UserLanguage / UserTimezone are install-time hints (AGENTTEAMS_LANGUAGE /
	// TZ) used only to render the first-boot Manager onboarding prompt
	// in reconcileManagerWelcome. Empty strings fall back to the same
	// defaults the legacy `start-manager-agent.sh` welcome heredoc used
	// (zh / Asia/Shanghai), which keeps observable behavior identical
	// when an admin upgrades from the legacy single-container manager.
	UserLanguage string
	UserTimezone string
}

// managerContainerName returns the container/pod name for a Manager CR.
// Default Manager ("default") uses ManagerDefaultName (e.g. "agentteams-manager")
// for install-script / CMS service-name compatibility; other Managers use
// "${prefix}manager-{name}".
func (r *ManagerReconciler) managerContainerName(name string) string {
	return r.ResourcePrefix.ManagerPodName(name)
}

func (r *ManagerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (retres reconcile.Result, reterr error) {
	start := time.Now()
	defer func() { metrics.Observe("manager", start, reterr) }()

	logger := log.FromContext(ctx)

	var mgr v1beta1.Manager
	if err := r.Get(ctx, req.NamespacedName, &mgr); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	patchBase := client.MergeFrom(mgr.DeepCopy())

	s := &managerScope{
		manager:   &mgr,
		patchBase: patchBase,
	}

	defer func() {
		if !mgr.DeletionTimestamp.IsZero() {
			return
		}

		mgr.Status.Phase = computeManagerPhase(&mgr, reterr)
		if reterr == nil {
			mgr.Status.ObservedGeneration = mgr.Generation
			mgr.Status.Message = ""
		} else {
			mgr.Status.Message = reterr.Error()
		}
		if mgr.Spec.Image != "" {
			mgr.Status.Version = mgr.Spec.Image
		}

		if err := r.Status().Patch(ctx, &mgr, patchBase); err != nil {
			logger.Error(err, "failed to patch manager status")
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	if !mgr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&mgr, finalizerName) {
			return r.reconcileManagerDelete(ctx, s)
		}
		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&mgr, finalizerName) {
		controllerutil.AddFinalizer(&mgr, finalizerName)
		if err := r.Update(ctx, &mgr); err != nil {
			return reconcile.Result{}, err
		}
	}

	return r.reconcileManagerNormal(ctx, s)
}

// reconcileManagerNormal runs the declarative convergence loop: infrastructure,
// config, container. Critical-path phases are serial with early return on error.
func (r *ManagerReconciler) reconcileManagerNormal(ctx context.Context, s *managerScope) (reconcile.Result, error) {
	if s.manager.Spec.ModelProvider != "" && r.GatewayClient != nil {
		info, err := r.GatewayClient.ResolveModelProvider(ctx, s.manager.Spec.ModelProvider)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("resolve model provider %q: %w", s.manager.Spec.ModelProvider, err)
		}
		s.modelProviderInfo = info
	}

	if res, err := r.reconcileManagerInfrastructure(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	if s.modelProviderInfo != nil && r.GatewayClient != nil && s.provResult != nil {
		consumerName := "manager"
		if err := r.GatewayClient.AuthorizeAIRoutes(ctx, consumerName, s.modelProviderInfo.HttpApiID); err != nil {
			return reconcile.Result{}, fmt.Errorf("authorize model provider %s for manager: %w", s.modelProviderInfo.HttpApiID, err)
		}
	}
	if err := r.Provisioner.EnsureManagerServiceAccount(ctx, s.manager.Name); err != nil {
		return reconcile.Result{}, fmt.Errorf("ServiceAccount: %w", err)
	}
	if res, err := r.reconcileManagerConfig(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	if res, err := r.reconcileManagerContainer(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}
	// Welcome message must run AFTER the container is up: the Manager's
	// matrix user only joins the Admin DM room once OpenClaw inside the
	// container has performed its first /sync. Sending earlier means the
	// message lands as historical timeline that the agent may skip on
	// startup. reconcileManagerWelcome itself short-circuits when the
	// container isn't Running yet and requeues until membership lands.
	if res, err := r.reconcileManagerWelcome(ctx, s); err != nil || res.RequeueAfter > 0 {
		return res, err
	}

	m := s.manager
	logger := log.FromContext(ctx)
	if m.Status.ObservedGeneration == 0 {
		logger.Info("manager created", "name", m.Name, "roomID", m.Status.RoomID)
	} else if m.Generation != m.Status.ObservedGeneration {
		logger.Info("manager updated", "name", m.Name)
	}

	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

func (r *ManagerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Manager{})

	if r.Backend != nil {
		// Watch Pods (for pod backend)
		if wb, _ := r.Backend.GetBackendForType(context.Background(), "pod"); wb != nil {
			bldr = bldr.Watches(
				&corev1.Pod{},
				handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
					managerName := obj.GetLabels()[v1beta1.LabelManager]
					if managerName == "" {
						return nil
					}
					return []reconcile.Request{
						{NamespacedName: client.ObjectKey{
							Name:      managerName,
							Namespace: obj.GetNamespace(),
						}},
					}
				}),
				builder.WithPredicates(PodLifecyclePredicates(v1beta1.LabelManager, r.ControllerName)),
			)
		}
		// Watch Sandbox CRs and transient SandboxClaim CRs (for sandbox backend)
		if wb, _ := r.Backend.GetBackendForType(context.Background(), "sandbox"); wb != nil {
			if sb, ok := wb.(*backend.SandboxBackend); ok {
				bldr = bldr.Watches(
					sb.WatchObject(),
					handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
						managerName := obj.GetLabels()[v1beta1.LabelManager]
						if managerName == "" {
							return nil
						}
						return []reconcile.Request{
							{NamespacedName: client.ObjectKey{
								Name:      managerName,
								Namespace: obj.GetNamespace(),
							}},
						}
					}),
					builder.WithPredicates(SandboxLifecyclePredicates(v1beta1.LabelManager, r.ControllerName)),
				)
				bldr = bldr.Watches(
					sb.ClaimWatchObject(),
					handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
						managerName := obj.GetLabels()[v1beta1.LabelManager]
						if managerName == "" {
							return nil
						}
						return []reconcile.Request{
							{NamespacedName: client.ObjectKey{
								Name:      managerName,
								Namespace: obj.GetNamespace(),
							}},
						}
					}),
					builder.WithPredicates(SandboxLifecyclePredicates(v1beta1.LabelManager, r.ControllerName)),
				)
			}
		}
	}

	return bldr.Complete(r)
}
