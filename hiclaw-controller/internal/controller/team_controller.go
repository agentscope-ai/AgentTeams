package controller

import (
	"context"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/metrics"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Team cache field indexer keys. Registered in app.initFieldIndexers and
// consumed by the auth enricher to resolve team membership by worker name
// without enumerating every Team.
const (
	TeamLeaderNameField    = "spec.leader.name"
	TeamWorkerNameField    = "spec.workerNames"
	TeamWorkerMembersField = "spec.workerMembers.name"
	migrationFinalizerName = "agentteams.io/migration-in-flight"
)

// TeamReconciler reconciles Team resources that reference existing Worker CRs
// through spec.workerMembers.
type TeamReconciler struct {
	client.Client

	Provisioner service.WorkerProvisioner
	Deployer    service.WorkerDeployer
	Backend     *backend.Registry
	EnvBuilder  service.WorkerEnvBuilderI
	Legacy      *service.LegacyCompat // nil in incluster mode

	// DefaultRuntime is forwarded into MemberDeps.DefaultRuntime for every
	// team member this reconciler converges. Sourced from
	// AGENTTEAMS_DEFAULT_WORKER_RUNTIME (Config.DefaultWorkerRuntime) — NOT from
	// AGENTTEAMS_MANAGER_RUNTIME — because team leader and worker containers are
	// both created through backend.WorkerBackend.Create as worker-type pods.
	// Empty means "no operator preference"; backend.ResolveRuntime then falls
	// back to RuntimeOpenClaw.
	DefaultRuntime string

	// DefaultBackendRuntime is the cluster-level default backendRuntime ("pod" or "sandbox").
	// Used for inline team members and as fallback for decoupled members without spec.backendRuntime.
	// Sourced from AGENTTEAMS_WORKER_BACKEND_RUNTIME env var.
	DefaultBackendRuntime string

	AgentFSDir string // for writing inline configs to the local agent FS

	// ControllerName, when non-empty, is merged as agentteams.io/controller
	// into the PodLabels of every team member MemberContext this reconciler
	// builds, so the resulting Pods match the owning controller instance's
	// label-scoped cache. Post-refactor (PR #666) Teams no longer create
	// child Worker CRs, so the label is applied directly to Pods via
	// MemberContext.PodLabels → backend.CreateRequest.Labels. Empty in
	// embedded mode.
	ControllerName string

	// WorkerDepsStorageBucket/Endpoint identify the main workspace OSS bucket
	// used for the built-in sandbox token/env/data mounts.
	WorkerDepsStorageBucket   string
	WorkerDepsStorageEndpoint string
	MountAuthType             string
	MountRoleName             string

	// ResourcePrefix scopes team-member ServiceAccount and Pod names per
	// AgentTeams tenant instance. Forwarded into MemberDeps.ResourcePrefix so
	// createMemberContainer uses it when computing saName. Empty collapses
	// to DefaultResourcePrefix ("hiclaw-").
	ResourcePrefix auth.ResourcePrefix

	GatewayClient               gateway.Client // gateway client for modelProvider resolution
	DynamicClient               dynamic.Interface
	RemoteDynamicClientProvider backend.RemoteDynamicClientProvider
	AuthTokenExpirationSeconds  int64

	// SystemAdminUser is the global system admin username (from
	// AGENTTEAMS_ADMIN_USER). Resolved to a full Matrix user ID and always
	// included in every worker's allowlist so the operator admin retains
	// visibility regardless of team membership.
	SystemAdminUser string

	// SoloOperator (AGENTTEAMS_SOLO_OPERATOR), when true, forces every Team's
	// PeerMentions to true regardless of Spec.PeerMentions — with a single
	// human operator, cross-mentions can't leak to strangers, so always
	// looping them in is the correct default.
	SoloOperator bool
}

func (r *TeamReconciler) Reconcile(ctx context.Context, req reconcile.Request) (retres reconcile.Result, reterr error) {
	start := time.Now()
	defer func() { metrics.Observe("team", start, reterr) }()

	logger := log.FromContext(ctx)

	var team v1beta1.Team
	if err := r.Get(ctx, req.NamespacedName, &team); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !team.DeletionTimestamp.IsZero() {
		changed := false
		if controllerutil.ContainsFinalizer(&team, finalizerName) {
			if err := r.handleDelete(ctx, &team); err != nil {
				logger.Error(err, "failed to delete team", "name", team.Name)
				return reconcile.Result{RequeueAfter: 30 * time.Second}, err
			}
			controllerutil.RemoveFinalizer(&team, finalizerName)
			changed = true
		}
		if controllerutil.ContainsFinalizer(&team, migrationFinalizerName) {
			controllerutil.RemoveFinalizer(&team, migrationFinalizerName)
			changed = true
		}
		if changed {
			if err := r.Update(ctx, &team); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&team, finalizerName) {
		controllerutil.AddFinalizer(&team, finalizerName)
		if err := r.Update(ctx, &team); err != nil {
			return reconcile.Result{}, err
		}
	}

	return r.reconcileTeamNormal(ctx, &team)
}

func (r *TeamReconciler) reconcileTeamNormal(ctx context.Context, t *v1beta1.Team) (reconcile.Result, error) {
	patchBase := client.MergeFrom(t.DeepCopy())
	if t.Status.Phase == "" {
		t.Status.Phase = "Pending"
		if err := r.Status().Patch(ctx, t, patchBase); err != nil {
			return reconcile.Result{}, err
		}
		patchBase = client.MergeFrom(t.DeepCopy())
	}

	if len(t.Spec.WorkerMembers) == 0 && (t.Spec.Leader.Name != "" || len(t.Spec.Workers) > 0) {
		return r.reconcileTeamLegacy(ctx, t, patchBase)
	}
	return r.reconcileTeamDecoupled(ctx, t, patchBase)
}

func (r *TeamReconciler) SetupWithManager(mgr ctrl.Manager) (controller.Controller, error) {
	bldr := ctrl.NewControllerManagedBy(mgr).For(&v1beta1.Team{})

	if r.Backend != nil {
		// Watch Pods (for pod backend workers)
		if wb, _ := r.Backend.GetBackendForType(context.Background(), "pod"); wb != nil {
			bldr = bldr.Watches(
				&corev1.Pod{},
				handler.EnqueueRequestsFromMapFunc(TeamPodMapFunc("")),
				builder.WithPredicates(PodLifecyclePredicates(v1beta1.LabelTeam, r.ControllerName)),
			)
		}
		// Watch Sandbox CRs and transient SandboxClaim CRs (for sandbox backend workers)
		if wb, _ := r.Backend.GetBackendForType(context.Background(), "sandbox"); wb != nil {
			if sb, ok := wb.(*backend.SandboxBackend); ok {
				bldr = bldr.Watches(
					sb.WatchObject(),
					handler.EnqueueRequestsFromMapFunc(TeamPodMapFunc("")),
					builder.WithPredicates(SandboxLifecyclePredicates(v1beta1.LabelTeam, r.ControllerName)),
				)
				bldr = bldr.Watches(
					sb.ClaimWatchObject(),
					handler.EnqueueRequestsFromMapFunc(TeamPodMapFunc("")),
					builder.WithPredicates(SandboxLifecyclePredicates(v1beta1.LabelTeam, r.ControllerName)),
				)
			}
		}
	}

	// Watch Worker CRs whose status changes for the decoupled path. When a
	// referenced Worker's status changes, the owning Team is enqueued via the
	// spec.workerMembers.name field indexer.
	bldr = bldr.Watches(
		&v1beta1.Worker{},
		handler.EnqueueRequestsFromMapFunc(r.workerToTeamRequests),
		builder.WithPredicates(workerStatusChangePredicate()),
	)

	return bldr.Build(r)
}

// TeamPodMapFunc returns a MapFunc for routing Pod events to Team reconcile
// requests. If namespace is non-empty, it overrides obj.GetNamespace() — used
// for remote clusters where Pod namespace != CR namespace.
func TeamPodMapFunc(namespace string) handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		teamName := obj.GetLabels()[v1beta1.LabelTeam]
		if teamName == "" {
			return nil
		}
		ns := namespace
		if ns == "" {
			ns = obj.GetNamespace()
		}
		return []reconcile.Request{
			{NamespacedName: client.ObjectKey{
				Name:      teamName,
				Namespace: ns,
			}},
		}
	}
}
