package controller

import (
	"context"
	"fmt"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/metrics"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	finalizerName       = "hiclaw.io/cleanup"
	reconcileInterval   = 5 * time.Minute
	reconcileRetryDelay = 30 * time.Second
)

// WorkerReconciler reconciles standalone Worker resources. Team members are
// owned by Team CRs and are reconciled by TeamReconciler through the shared
// member_reconcile helpers, not by WorkerReconciler.
type WorkerReconciler struct {
	client.Client

	Provisioner    service.WorkerProvisioner
	Deployer       service.WorkerDeployer
	Backend        *backend.Registry
	EnvBuilder     service.WorkerEnvBuilderI
	ResourcePrefix auth.ResourcePrefix   // tenant prefix used to derive SA names
	Legacy         *service.LegacyCompat // nil in incluster mode
	GatewayClient  gateway.Client        // gateway client for modelProvider resolution

	// DefaultRuntime is the value passed to backend.CreateRequest.RuntimeFallback
	// when a Worker CR omits spec.runtime. Sourced from
	// HICLAW_DEFAULT_WORKER_RUNTIME (Config.DefaultWorkerRuntime). Empty means
	// "no operator preference" — backend.ResolveRuntime will fall back to
	// "openclaw".
	DefaultRuntime string

	// ControllerName identifies this controller instance. Stamped on every
	// Pod/SA/Secret created under this reconciler via the
	// hiclaw.io/controller label so multiple controller instances sharing a
	// namespace do not cross-watch each other's resources.
	ControllerName string
}

func (r *WorkerReconciler) Reconcile(ctx context.Context, req reconcile.Request) (retres reconcile.Result, reterr error) {
	start := time.Now()
	defer func() { metrics.Observe("worker", start, reterr) }()

	logger := log.FromContext(ctx)

	var worker v1beta1.Worker
	if err := r.Get(ctx, req.NamespacedName, &worker); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	patchBase := client.MergeFrom(worker.DeepCopy())

	// Shared MemberState captured by the defer so phase computation can
	// observe the actual container state recorded during reconcile.
	state := &MemberState{}

	// Unified status patch at the end of every reconcile. ObservedGeneration
	// is only written when reconcile succeeds, preventing the infinite-loop
	// bug where a failed status write triggered re-reconcile with
	// Generation != ObservedGeneration.
	defer func() {
		if !worker.DeletionTimestamp.IsZero() {
			return
		}
		worker.Status.Phase = computeWorkerPhase(&worker, state.ContainerState, reterr)
		if reterr == nil {
			worker.Status.ObservedGeneration = worker.Generation
			worker.Status.Message = ""
		} else {
			worker.Status.Message = reterr.Error()
		}
		if err := r.Status().Patch(ctx, &worker, patchBase); err != nil {
			logger.Error(err, "failed to patch worker status")
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	if !worker.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&worker, finalizerName) {
			return r.reconcileDelete(ctx, &worker)
		}
		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&worker, finalizerName) {
		base := worker.DeepCopy()
		controllerutil.AddFinalizer(&worker, finalizerName)
		if err := r.Patch(ctx, &worker, client.MergeFrom(base)); err != nil {
			return reconcile.Result{}, err
		}
	}

	return r.reconcileNormal(ctx, &worker, state)
}

// reconcileNormal builds a MemberContext from the Worker CR, runs the shared
// member reconcile phases, and writes runtime state back to Worker.Status.
// Legacy Manager groupAllowFrom is updated here only for standalone workers;
// team leaders are handled by TeamReconciler.
func (r *WorkerReconciler) reconcileNormal(ctx context.Context, w *v1beta1.Worker, state *MemberState) (reconcile.Result, error) {
	deps := MemberDeps{
		Provisioner:    r.Provisioner,
		Deployer:       r.Deployer,
		Backend:        r.Backend,
		EnvBuilder:     r.EnvBuilder,
		ResourcePrefix: r.ResourcePrefix,
		DefaultRuntime: r.DefaultRuntime,
		GatewayClient:  r.GatewayClient,
	}
	mctx := r.workerMemberContext(w)

	if w.Spec.ModelProvider != "" && r.GatewayClient != nil {
		info, err := r.GatewayClient.ResolveModelProvider(ctx, w.Spec.ModelProvider)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("resolve model provider %q: %w", w.Spec.ModelProvider, err)
		}
		mctx.ModelProviderInfo = info
	}

	// Validate cross-cluster deployment fields before entering phases.
	if err := ValidateMemberDeployment(mctx); err != nil {
		return reconcile.Result{}, err
	}

	if res, err := ReconcileMemberInfra(ctx, deps, mctx, state); err != nil || res.RequeueAfter > 0 {
		applyMemberStateToWorker(w, state)
		return res, err
	}
	if err := EnsureModelProviderAuth(ctx, deps, mctx, state); err != nil {
		applyMemberStateToWorker(w, state)
		return reconcile.Result{}, err
	}
	if err := EnsureMemberServiceAccount(ctx, deps, mctx); err != nil {
		applyMemberStateToWorker(w, state)
		return reconcile.Result{}, err
	}
	if err := ReconcileMemberConfig(ctx, deps, mctx, state); err != nil {
		applyMemberStateToWorker(w, state)
		return reconcile.Result{}, err
	}
	if res, err := ReconcileMemberContainer(ctx, deps, mctx, state); err != nil || res.RequeueAfter > 0 {
		applyMemberStateToWorker(w, state)
		return res, err
	}
	if err := ReconcileMemberService(ctx, &mctx, &deps); err != nil {
		applyMemberStateToWorker(w, state)
		return reconcile.Result{}, err
	}
	_ = ReconcileMemberExpose(ctx, deps, mctx, state)
	applyMemberStateToWorker(w, state)

	r.reconcileLegacy(ctx, w, state)

	logger := log.FromContext(ctx)
	if w.Status.ObservedGeneration == 0 {
		logger.Info("worker created", "name", w.Name, "roomID", w.Status.RoomID)
	} else if w.Generation != w.Status.ObservedGeneration {
		logger.Info("worker updated", "name", w.Name)
	}

	return reconcile.Result{RequeueAfter: reconcileInterval}, nil
}

// reconcileDelete cleans up all infrastructure for the Worker and then removes
// the finalizer. Legacy Manager groupAllowFrom is rolled back here only for
// standalone workers.
func (r *WorkerReconciler) reconcileDelete(ctx context.Context, w *v1beta1.Worker) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("deleting worker", "name", w.Name)

	deps := MemberDeps{
		Provisioner:    r.Provisioner,
		Deployer:       r.Deployer,
		Backend:        r.Backend,
		EnvBuilder:     r.EnvBuilder,
		ResourcePrefix: r.ResourcePrefix,
		DefaultRuntime: r.DefaultRuntime,
		GatewayClient:  r.GatewayClient,
	}
	mctx := r.workerMemberContext(w)

	_ = ReconcileMemberDelete(ctx, deps, mctx)

	if r.Legacy != nil && r.Legacy.Enabled() {
		workerMatrixID := r.Provisioner.MatrixUserID(w.Name)
		if mctx.Role == RoleStandalone {
			if err := r.Legacy.UpdateManagerGroupAllowFrom(workerMatrixID, false); err != nil {
				logger.Error(err, "failed to update Manager groupAllowFrom (non-fatal)")
			}
		}
		if err := r.Legacy.RemoveFromWorkersRegistry(mctx.RuntimeName); err != nil {
			logger.Error(err, "failed to remove from workers registry (non-fatal)")
		}
	}

	base := w.DeepCopy()
	controllerutil.RemoveFinalizer(w, finalizerName)
	if err := r.Patch(ctx, w, client.MergeFrom(base)); err != nil {
		return reconcile.Result{}, err
	}

	logger.Info("worker deleted", "name", w.Name)
	return reconcile.Result{}, nil
}

// reconcileLegacy writes the worker to the legacy workers-registry and grants
// the standalone worker publish rights into the Manager's group DM room.
func (r *WorkerReconciler) reconcileLegacy(ctx context.Context, w *v1beta1.Worker, state *MemberState) {
	if r.Legacy == nil || !r.Legacy.Enabled() {
		return
	}
	logger := log.FromContext(ctx)

	role := w.Annotations["hiclaw.io/role"]
	teamName := w.Annotations["hiclaw.io/team"]
	teamLeaderName := w.Annotations["hiclaw.io/team-leader"]
	memberRole := roleForAnnotations(role, teamLeaderName)

	// Only standalone workers grant themselves group-DM publish rights. Team
	// leaders are handled by TeamReconciler; team workers never go through
	// WorkerReconciler post-refactor.
	if memberRole == RoleStandalone && state.ProvResult != nil {
		if err := r.Legacy.UpdateManagerGroupAllowFrom(state.ProvResult.MatrixUserID, true); err != nil {
			logger.Error(err, "failed to update Manager groupAllowFrom (non-fatal)")
		}
	}

	runtimeName := w.Spec.EffectiveWorkerName(w.Name)
	if err := r.Legacy.UpdateWorkersRegistry(service.WorkerRegistryEntry{
		Name:         runtimeName,
		MatrixUserID: r.Provisioner.MatrixUserID(runtimeName),
		RoomID:       w.Status.RoomID,
		Runtime:      w.Spec.Runtime,
		Deployment:   "local",
		Skills:       w.Spec.Skills,
		Role:         role,
		TeamID:       nilIfEmpty(teamName),
		Image:        nilIfEmpty(w.Spec.Image),
	}); err != nil {
		logger.Error(err, "registry update failed (non-fatal)")
	}
}

// workerMemberContext translates a Worker CR into a MemberContext for the
// shared member reconcile helpers. The returned PodLabels are built by
// layering four sources low-to-high: ConfigMap-based pod template (added
// downstream by ApplyPodTemplate), the CR's metadata.labels, the CR's
// spec.labels, and the controller-forced system labels (controller name
// and member role). Controller-forced keys deliberately come last so
// anything the user writes that collides (e.g. `hiclaw.io/controller`)
// is silently overridden rather than rejected.
func (r *WorkerReconciler) workerMemberContext(w *v1beta1.Worker) MemberContext {
	role := roleForAnnotations(w.Annotations["hiclaw.io/role"], w.Annotations["hiclaw.io/team-leader"])
	runtimeName := w.Spec.EffectiveWorkerName(w.Name)

	// Cross-cluster deployment fields.
	deployMode := v1beta1.DeployModeLocal
	if w.Spec.DeployMode != nil {
		deployMode = *w.Spec.DeployMode
	}
	var targetClusterID, targetNamespace string
	if w.Spec.TargetCluster != nil {
		targetClusterID = w.Spec.TargetCluster.ID
		targetNamespace = w.Spec.TargetCluster.Namespace
	}
	var serviceEnabled bool
	if w.Spec.ServiceEnabled != nil {
		serviceEnabled = *w.Spec.ServiceEnabled
	}

	return MemberContext{
		Name:               w.Name,
		RuntimeName:        runtimeName,
		Namespace:          w.Namespace,
		Role:               role,
		Spec:               w.Spec,
		Generation:         w.Generation,
		ObservedGeneration: w.Status.ObservedGeneration,
		PodLabels: mergeLabels(
			w.ObjectMeta.Labels,
			w.Spec.Labels,
			map[string]string{
				v1beta1.LabelController: r.ControllerName,
				"hiclaw.io/role":        role.String(),
			},
		),
		// SpecChanged is gated on ObservedGeneration > 0 so a brand-new
		// Worker (Generation=1, ObservedGeneration=0) reports
		// SpecChanged=false. Initial creation then goes through the
		// StatusNotFound branch in ensureMemberContainerPresent
		// unambiguously. Without the gate, a second reconcile queued by
		// the finalizer write can read a stale informer cache
		// (ObservedGeneration still 0) after the just-created container
		// is already Running, fall into the spec-change branch, and Delete
		// the container via force=true (SIGKILL, exit 137).
		SpecChanged:          w.Status.ObservedGeneration > 0 && w.Generation != w.Status.ObservedGeneration,
		IsUpdate:             w.Status.Phase != "" && w.Status.Phase != "Pending" && w.Status.Phase != "Failed",
		TeamName:             w.Annotations["hiclaw.io/team"],
		TeamLeaderName:       w.Annotations["hiclaw.io/team-leader"],
		TeamAdminMatrixID:    w.Annotations["hiclaw.io/team-admin-id"],
		ExistingMatrixUserID: w.Status.MatrixUserID,
		ExistingRoomID:       w.Status.RoomID,
		CurrentExposedPorts:  w.Status.ExposedPorts,
		Owner:                w,
		DeployMode:           deployMode,
		TargetClusterID:      targetClusterID,
		TargetNamespace:      targetNamespace,
		ServiceEnabled:       serviceEnabled,
	}
}

// applyMemberStateToWorker copies runtime state into Worker.Status fields.
// Phase, ObservedGeneration, Message are owned by the deferred patch in
// Reconcile; this helper only touches infra/runtime fields.
func applyMemberStateToWorker(w *v1beta1.Worker, state *MemberState) {
	if state == nil {
		return
	}
	if state.MatrixUserID != "" {
		w.Status.MatrixUserID = state.MatrixUserID
	}
	if state.RoomID != "" {
		w.Status.RoomID = state.RoomID
	}
	if state.ContainerState != "" {
		w.Status.ContainerState = state.ContainerState
	}
	if state.ExposedPorts != nil || len(w.Spec.Expose) == 0 {
		w.Status.ExposedPorts = state.ExposedPorts
	}
}

// computeWorkerPhase determines the Worker status phase from the reconcile
// outcome. Delegates to the shared computeMemberPhase function.
func computeWorkerPhase(w *v1beta1.Worker, containerState string, reconcileErr error) string {
	return computeMemberPhase(w.Status.Phase, w.Status.MatrixUserID, w.Spec.DesiredState(), containerState, reconcileErr)
}

func (r *WorkerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Worker{})

	if r.Backend != nil {
		if wb := r.Backend.DetectWorkerBackend(context.Background()); wb != nil && wb.Name() == "k8s" {
			bldr = bldr.Watches(
				&corev1.Pod{},
				handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
					workerName := obj.GetLabels()["hiclaw.io/worker"]
					if workerName == "" {
						return nil
					}
					// Skip pods owned by a Team (those are reconciled via
					// the Team controller's own pod watch).
					if obj.GetLabels()["hiclaw.io/team"] != "" {
						return nil
					}
					return []reconcile.Request{
						{NamespacedName: client.ObjectKey{
							Name:      workerName,
							Namespace: obj.GetNamespace(),
						}},
					}
				}),
				builder.WithPredicates(podLifecyclePredicates("hiclaw.io/worker", r.ControllerName)),
			)
		}
	}

	return bldr.Complete(r)
}

// podLifecyclePredicates filters Pod events to only trigger reconciliation on
// create, delete, or phase transitions. A pod is considered "ours" only when
// it carries both:
//
//   - labelKey (one of "hiclaw.io/worker" / "hiclaw.io/team" /
//     "hiclaw.io/manager") with a non-empty value — identifying which CR
//     kind owns the pod.
//   - hiclaw.io/controller == controllerName — identifying which controller
//     instance owns the pod.
//
// The controller filter is defense-in-depth against the informer cache label
// selector configured in app.startInCluster (opts.Cache.ByObject for Pods).
// If a future watch source is wired without that cache filter, this predicate
// still prevents cross-instance reconcile when two hiclaw-controller
// releases share a namespace.
func podLifecyclePredicates(labelKey, controllerName string) predicate.Predicate {
	matches := func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[labelKey] != "" && l[v1beta1.LabelController] == controllerName
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return matches(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return matches(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !matches(e.ObjectNew) {
				return false
			}
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 {
				return true
			}
			return oldPod.Status.Phase != newPod.Status.Phase
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// --- Package-level helpers ---

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// roleForAnnotations maps Worker CR annotations to a MemberRole.
func roleForAnnotations(role, teamLeaderName string) MemberRole {
	if role == "team_leader" {
		return RoleTeamLeader
	}
	if teamLeaderName != "" {
		return RoleTeamWorker
	}
	return RoleStandalone
}
