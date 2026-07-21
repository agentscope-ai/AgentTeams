package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/metrics"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	finalizerName         = "agentteams.io/cleanup"
	reconcileInterval     = 5 * time.Minute
	edgeReconcileInterval = 1 * time.Minute
	edgeHeartbeatTimeout  = 2 * time.Minute
	reconcileRetryDelay   = 30 * time.Second
	// appServiceNotReadyRequeue is the short backoff used while the
	// controller's Matrix AppService token has not been registered/verified
	// with the homeserver yet (transient startup race, M_UNKNOWN_TOKEN).
	appServiceNotReadyRequeue = 5 * time.Second
)

// WorkerReconciler reconciles standalone Worker resources. Team members are
// owned by Team CRs and are reconciled by TeamReconciler through the shared
// member_reconcile helpers, not by WorkerReconciler.
type WorkerReconciler struct {
	client.Client

	Provisioner                 service.WorkerProvisioner
	Deployer                    service.WorkerDeployer
	Backend                     *backend.Registry
	EnvBuilder                  service.WorkerEnvBuilderI
	ResourcePrefix              auth.ResourcePrefix   // tenant prefix used to derive SA names
	Legacy                      *service.LegacyCompat // nil in incluster mode
	GatewayClient               gateway.Client        // gateway client for modelProvider resolution
	DynamicClient               dynamic.Interface
	RemoteDynamicClientProvider backend.RemoteDynamicClientProvider
	AuthTokenExpirationSeconds  int64

	// DefaultRuntime is the value passed to backend.CreateRequest.RuntimeFallback
	// when a Worker CR omits spec.runtime. Sourced from
	// AGENTTEAMS_DEFAULT_WORKER_RUNTIME (Config.DefaultWorkerRuntime). Empty means
	// "no operator preference" — backend.ResolveRuntime will fall back to
	// "openclaw".
	DefaultRuntime string

	// DefaultBackendRuntime is the cluster-level default backendRuntime ("pod" or "sandbox").
	// Used when Worker CR's spec.backendRuntime is not set.
	// Sourced from AGENTTEAMS_WORKER_BACKEND_RUNTIME env var.
	DefaultBackendRuntime string

	// ControllerName identifies this controller instance. Stamped on every
	// Pod/SA/Secret created under this reconciler via the
	// agentteams.io/controller label so multiple controller instances sharing a
	// namespace do not cross-watch each other's resources.
	ControllerName string

	// AuthCache is cleared after deleting a rotated Edge Worker's
	// ServiceAccount so old SA tokens cannot pass via cached TokenReview.
	AuthCache interface{ InvalidateCache() }

	// WorkerDepsStorageBucket/Endpoint identify the main workspace OSS bucket
	// used for the built-in sandbox token/env/data mounts.
	WorkerDepsStorageBucket   string
	WorkerDepsStorageEndpoint string
	MountAuthType             string
	MountRoleName             string
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
		if isEdgeWorker(&worker) && reterr == nil {
			if edgeHeartbeatStale(worker.Status.LastHeartbeat, edgeHeartbeatTimeout) {
				worker.Status.Phase = "Pending"
			} else if worker.Status.Phase == "" {
				worker.Status.Phase = "Pending"
			}
		} else {
			worker.Status.Phase = computeWorkerPhase(&worker, state.ContainerState, reterr)
		}
		if reterr == nil {
			worker.Status.ObservedGeneration = worker.Generation
			worker.Status.Message = state.Message
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

func isEdgeWorker(w *v1beta1.Worker) bool {
	return w != nil && w.Spec.DeployMode != nil && *w.Spec.DeployMode == v1beta1.DeployModeEdge
}

func edgeHeartbeatStale(lastHeartbeat string, timeout time.Duration) bool {
	if lastHeartbeat == "" {
		return true
	}
	ts, err := time.Parse(time.RFC3339, lastHeartbeat)
	if err != nil {
		return true
	}
	return time.Since(ts) > timeout
}

// reconcileNormal builds a MemberContext from the Worker CR, runs the shared
// member reconcile phases, and writes runtime state back to Worker.Status.
func (r *WorkerReconciler) reconcileNormal(ctx context.Context, w *v1beta1.Worker, state *MemberState) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	deps := MemberDeps{
		Provisioner:                 r.Provisioner,
		Deployer:                    r.Deployer,
		Backend:                     r.Backend,
		EnvBuilder:                  r.EnvBuilder,
		ResourcePrefix:              r.ResourcePrefix,
		DefaultRuntime:              r.DefaultRuntime,
		GatewayClient:               r.GatewayClient,
		DynamicClient:               r.DynamicClient,
		RemoteDynamicClientProvider: r.RemoteDynamicClientProvider,
		AuthTokenExpirationSeconds:  r.AuthTokenExpirationSeconds,
		ControllerName:              r.ControllerName,
		WorkerDepsStorageBucket:     r.WorkerDepsStorageBucket,
		WorkerDepsStorageEndpoint:   r.WorkerDepsStorageEndpoint,
		MountAuthType:               r.MountAuthType,
		MountRoleName:               r.MountRoleName,
	}
	effectiveSpec, resourceSpec, updateStrategy, err := r.effectiveWorkerSpec(ctx, w, false)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := validateWorkerDeploymentTargetImmutable(w, effectiveSpec); err != nil {
		return reconcile.Result{}, err
	}
	mctx := r.workerMemberContextWithSpec(w, effectiveSpec, resourceSpec, updateStrategy)

	if effectiveSpec.ModelProvider != "" && r.GatewayClient != nil {
		info, err := r.GatewayClient.ResolveModelProvider(ctx, effectiveSpec.ModelProvider)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("resolve model provider %q: %w", effectiveSpec.ModelProvider, err)
		}
		mctx.ModelProviderInfo = info
	}

	if mctx.DeployMode == v1beta1.DeployModeEdge {
		// Edge UUID rotation: when the UUID label changes, delete the SA so any
		// previously issued long-lived tokens are invalidated. The next call to
		// EdgeHandler.ExchangeToken will recreate the SA and mint a fresh token
		// bound to the new UUID. Skipped on first issuance (appliedUUID empty).
		currentUUID := w.Labels[v1beta1.LabelWorkerEdgeUUID]
		appliedUUID := w.Annotations[v1beta1.AnnotationEdgeAppliedUUID]
		if currentUUID != "" && appliedUUID != "" && currentUUID != appliedUUID {
			if err := r.Provisioner.DeleteServiceAccount(ctx, w.Name); err != nil {
				logger.Error(err, "failed to delete SA during edge UUID rotation")
				return reconcile.Result{}, err
			}
			if r.AuthCache != nil {
				r.AuthCache.InvalidateCache()
			}
			if w.Annotations == nil {
				w.Annotations = make(map[string]string)
			}
			w.Annotations[v1beta1.AnnotationEdgeAppliedUUID] = currentUUID
			if err := r.Update(ctx, w); err != nil {
				return reconcile.Result{}, err
			}
			logger.Info("edge UUID rotated, SA deleted", "oldUUID", appliedUUID, "newUUID", currentUUID)
		}
		// Edge workers run off-cluster: the controller does not manage Pods,
		// Services, or Expose for them. SA lifecycle is driven on demand by
		// EdgeHandler.ExchangeToken. The lightweight controller path still
		// provisions Matrix/gateway credentials and writes runtime.yaml for the
		// remote-managed local worker.
		if res, err := ReconcileMemberInfra(ctx, deps, mctx, state); err != nil || res.RequeueAfter > 0 {
			applyMemberStateToWorker(w, state)
			return res, err
		}
		if err := EnsureModelProviderAuth(ctx, deps, mctx, state); err != nil {
			applyMemberStateToWorker(w, state)
			return reconcile.Result{}, err
		}
		if err := ReconcileMemberConfig(ctx, deps, mctx, state); err != nil {
			applyMemberStateToWorker(w, state)
			return reconcile.Result{}, err
		}
		applyMemberStateToWorker(w, state)
		w.Status.SpecHash = mctx.AppliedSpecHash
		return reconcile.Result{RequeueAfter: edgeReconcileInterval}, nil
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
	applyDeploymentTargetStatus(w, mctx)
	svcName, err := ReconcileMemberService(ctx, &mctx, &deps)
	if err != nil {
		applyMemberStateToWorker(w, state)
		return reconcile.Result{}, err
	}
	// Stamp or remove the service-name label on the Worker CR.
	// IMPORTANT: snapshot base BEFORE mutating w so MergeFrom produces
	// a non-empty patch — capturing base after the mutation makes the
	// diff identical and the label change never lands.
	base := w.DeepCopy()
	if labelChanged := reconcileWorkerSvcLabel(w, svcName); labelChanged {
		if err := r.Patch(ctx, w, client.MergeFrom(base)); err != nil {
			return reconcile.Result{}, fmt.Errorf("patch worker svc label: %w", err)
		}
	}
	_ = ReconcileMemberExpose(ctx, deps, mctx, state)
	applyMemberStateToWorker(w, state)
	w.Status.SpecHash = mctx.AppliedSpecHash
	applyDeploymentTargetStatus(w, mctx)

	r.reconcileLegacyWithContext(ctx, w, mctx, state)

	if w.Status.ObservedGeneration == 0 {
		logger.Info("worker created", "name", w.Name, "roomID", w.Status.RoomID)
	} else if w.Generation != w.Status.ObservedGeneration {
		logger.Info("worker updated", "name", w.Name)
	}

	requeueAfter := minPositiveDuration(reconcileInterval, state.RequeueAfter)
	return reconcile.Result{RequeueAfter: requeueAfter}, nil
}

// reconcileDelete cleans up all infrastructure for the Worker and then removes
// the finalizer.
func (r *WorkerReconciler) reconcileDelete(ctx context.Context, w *v1beta1.Worker) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("deleting worker", "name", w.Name)

	deps := MemberDeps{
		Provisioner:                 r.Provisioner,
		Deployer:                    r.Deployer,
		Backend:                     r.Backend,
		EnvBuilder:                  r.EnvBuilder,
		ResourcePrefix:              r.ResourcePrefix,
		DefaultRuntime:              r.DefaultRuntime,
		GatewayClient:               r.GatewayClient,
		DynamicClient:               r.DynamicClient,
		RemoteDynamicClientProvider: r.RemoteDynamicClientProvider,
		AuthTokenExpirationSeconds:  r.AuthTokenExpirationSeconds,
		ControllerName:              r.ControllerName,
		WorkerDepsStorageBucket:     r.WorkerDepsStorageBucket,
		WorkerDepsStorageEndpoint:   r.WorkerDepsStorageEndpoint,
		MountAuthType:               r.MountAuthType,
		MountRoleName:               r.MountRoleName,
	}
	effectiveSpec, resourceSpec, updateStrategy, err := r.effectiveWorkerSpec(ctx, w, true)
	if err != nil {
		return reconcile.Result{}, err
	}
	effectiveSpec = workerSpecWithAppliedDeploymentTarget(effectiveSpec, w.Status)
	mctx := r.workerMemberContextWithSpec(w, effectiveSpec, resourceSpec, updateStrategy)

	_ = ReconcileMemberDelete(ctx, deps, mctx)

	if r.Legacy != nil && r.Legacy.Enabled() {
		workerMatrixID := r.Provisioner.MatrixUserID(w.Name)
		if err := r.Legacy.UpdateManagerGroupAllowFrom(workerMatrixID, false); err != nil {
			logger.Error(err, "failed to update Manager groupAllowFrom (non-fatal)")
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
	r.reconcileLegacyWithContext(ctx, w, r.workerMemberContext(w), state)
}

func (r *WorkerReconciler) reconcileLegacyWithContext(ctx context.Context, w *v1beta1.Worker, mctx MemberContext, state *MemberState) {
	if r.Legacy == nil || !r.Legacy.Enabled() {
		return
	}
	logger := log.FromContext(ctx)
	runtimeName := mctx.RuntimeName

	role, inTeam, err := r.decoupledTeamRoleForWorker(ctx, w.Namespace, w.Name)
	if err != nil {
		logger.Error(err, "failed to check decoupled Team membership before legacy worker update (non-fatal)", "worker", w.Name)
	}
	if inTeam {
		// Decoupled Team members are still reconciled by WorkerReconciler for
		// Worker CR lifecycle/config, but TeamReconciler owns legacy
		// team-scoped artifacts: workers-registry role/team_id rows,
		// Manager allow-list membership, and member channel policy overlays.
		// Writing standalone legacy state here races with TeamReconciler and
		// can make CI/user-visible config oscillate.
		if role != RoleTeamLeader {
			if err := r.Legacy.UpdateManagerGroupAllowFrom(r.Legacy.MatrixUserID(runtimeName), false); err != nil {
				logger.Error(err, "failed to revoke standalone Manager groupAllowFrom for Team worker (non-fatal)", "worker", w.Name, "runtimeName", runtimeName)
			}
		}
		return
	}

	// WorkerReconciler only handles standalone workers. Grant group-DM
	// publish rights for the standalone worker.
	if state.ProvResult != nil {
		if err := r.Legacy.UpdateManagerGroupAllowFrom(state.ProvResult.MatrixUserID, true); err != nil {
			logger.Error(err, "failed to update Manager groupAllowFrom (non-fatal)")
		}
	}

	if err := r.Legacy.UpdateWorkersRegistry(service.WorkerRegistryEntry{
		Name:         runtimeName,
		MatrixUserID: r.Provisioner.MatrixUserID(runtimeName),
		RoomID:       w.Status.RoomID,
		Runtime:      mctx.Spec.Runtime,
		Deployment:   "local",
		Skills:       mctx.Spec.Skills,
		Image:        nilIfEmpty(mctx.Spec.Image),
	}); err != nil {
		logger.Error(err, "registry update failed (non-fatal)")
	}
}

func (r *WorkerReconciler) decoupledTeamRoleForWorker(ctx context.Context, namespace, workerName string) (MemberRole, bool, error) {
	if r.Client == nil || workerName == "" {
		return "", false, nil
	}

	var teams v1beta1.TeamList
	if err := r.List(ctx, &teams,
		client.InNamespace(namespace),
		client.MatchingFields{TeamWorkerMembersField: workerName},
	); err != nil {
		// Unit-test fake clients and older controller setups may not have the
		// field index registered. Fall back to namespace enumeration so the
		// safety check still works.
		if listErr := r.List(ctx, &teams, client.InNamespace(namespace)); listErr != nil {
			return "", false, fmt.Errorf("list teams by workerMembers index: %w; fallback list: %v", err, listErr)
		}
	}

	for _, team := range teams.Items {
		if len(team.Spec.WorkerMembers) == 0 {
			continue
		}
		for _, ref := range team.Spec.WorkerMembers {
			if ref.Name != workerName {
				continue
			}
			if ref.Role == RoleTeamLeader.String() {
				return RoleTeamLeader, true, nil
			}
			return RoleTeamWorker, true, nil
		}
	}
	return "", false, nil
}

func (r *WorkerReconciler) effectiveWorkerSpec(_ context.Context, w *v1beta1.Worker, _ bool) (v1beta1.WorkerSpec, *v1beta1.AgentResourceRequirements, string, error) {
	spec := *w.Spec.DeepCopy()
	return spec, spec.Resources, "", nil
}

func validateWorkerDeploymentTargetImmutable(w *v1beta1.Worker, desired v1beta1.WorkerSpec) error {
	if w.Status.DeployMode == "" {
		return nil
	}
	currentMode := w.Status.DeployMode
	if currentMode == "" {
		currentMode = v1beta1.DeployModeLocal
	}
	desiredMode := workerSpecDeploymentMode(desired)
	if currentMode != desiredMode {
		return fmt.Errorf("spec.deployMode cannot be changed after the Worker runtime has been provisioned; delete and recreate the Worker to move it (current=%s, desired=%s)",
			currentMode,
			desiredMode)
	}
	return nil
}

func workerSpecWithAppliedDeploymentTarget(spec v1beta1.WorkerSpec, status v1beta1.WorkerStatus) v1beta1.WorkerSpec {
	if status.DeployMode == "" {
		return spec
	}
	mode := status.DeployMode
	if mode == "" {
		mode = v1beta1.DeployModeLocal
	}
	spec.DeployMode = &mode
	return spec
}

func applyDeploymentTargetStatus(w *v1beta1.Worker, m MemberContext) {
	w.Status.DeployMode = m.DeployMode
}

func workerSpecDeploymentMode(spec v1beta1.WorkerSpec) string {
	mode := v1beta1.DeployModeLocal
	if spec.DeployMode != nil && *spec.DeployMode != "" {
		mode = *spec.DeployMode
	}
	return mode
}

func agentResourcesToBackend(resources *v1beta1.AgentResourceRequirements) *backend.ResourceRequirements {
	if resources == nil ||
		(resources.Requests.CPU == "" &&
			resources.Requests.Memory == "" &&
			resources.Limits.CPU == "" &&
			resources.Limits.Memory == "") {
		return nil
	}
	return &backend.ResourceRequirements{
		CPURequest:    resources.Requests.CPU,
		CPULimit:      resources.Limits.CPU,
		MemoryRequest: resources.Requests.Memory,
		MemoryLimit:   resources.Limits.Memory,
	}
}

func mergeBackendResourceRequirements(defaults, override *backend.ResourceRequirements) *backend.ResourceRequirements {
	if override == nil {
		return defaults
	}
	if defaults == nil {
		return override
	}
	merged := *defaults
	if override.CPURequest != "" {
		merged.CPURequest = override.CPURequest
	}
	if override.CPULimit != "" {
		merged.CPULimit = override.CPULimit
	}
	if override.MemoryRequest != "" {
		merged.MemoryRequest = override.MemoryRequest
	}
	if override.MemoryLimit != "" {
		merged.MemoryLimit = override.MemoryLimit
	}
	return &merged
}

// workerMemberContext translates a Worker CR into a MemberContext for the
// shared member reconcile helpers. WorkerReconciler always produces a
// standalone context — team semantics are injected externally by
// TeamReconciler via Matrix Room invite and MinIO AGENTS.MD, never via
// Worker CR annotations.
//
// PodLabels are built by layering four sources low-to-high: ConfigMap-based
// pod template (added downstream by ApplyPodTemplate), the CR's
// metadata.labels, the CR's spec.labels, and the controller-forced system
// labels (controller name and member role). Controller-forced keys
// deliberately come last so anything the user writes that collides (e.g.
// `agentteams.io/controller`) is silently overridden rather than rejected.
func (r *WorkerReconciler) workerMemberContext(w *v1beta1.Worker) MemberContext {
	return r.workerMemberContextWithSpec(w, w.Spec, nil, "")
}

func (r *WorkerReconciler) workerMemberContextWithSpec(w *v1beta1.Worker, spec v1beta1.WorkerSpec, resourceSpec *v1beta1.AgentResourceRequirements, updateStrategy string) MemberContext {
	runtimeName := spec.EffectiveWorkerName(w.Name)
	effectiveRuntime := backend.ResolveRuntime(spec.Runtime, r.DefaultRuntime)
	backendRuntime := spec.GetBackendRuntime()
	if backendRuntime == "" {
		backendRuntime = r.DefaultBackendRuntime
	}
	hashSpec := workerSpecWithEffectiveBackendRuntimeForHash(spec, backendRuntime)
	appliedSpecHash := hashAppliedWorkerSpecForRuntimeAndResources(hashSpec, effectiveRuntime, resourceSpec)
	// Hash-based comparison is preferred: only pod-affecting fields trigger
	// recreation. Fall back to Generation comparison when SpecHash has not
	// been stored yet (first reconcile after upgrade).
	var specChanged bool
	if w.Status.SpecHash != "" {
		specChanged = w.Status.SpecHash != appliedSpecHash
	} else {
		specChanged = w.Status.ObservedGeneration > 0 && w.Generation != w.Status.ObservedGeneration
	}

	// Cross-cluster deployment fields.
	deployMode := v1beta1.DeployModeLocal
	if spec.DeployMode != nil {
		deployMode = *spec.DeployMode
	}
	var serviceEnabled bool
	if spec.ServiceEnabled != nil {
		serviceEnabled = *spec.ServiceEnabled
	}

	systemLabels := map[string]string{
		v1beta1.LabelController: r.ControllerName,
		v1beta1.LabelRole:       RoleStandalone.String(),
	}
	return MemberContext{
		Name:               w.Name,
		RuntimeName:        runtimeName,
		Namespace:          w.Namespace,
		Role:               RoleStandalone,
		Spec:               spec,
		Generation:         w.Generation,
		ObservedGeneration: w.Status.ObservedGeneration,
		PodLabels: mergeLabels(
			w.ObjectMeta.Labels,
			spec.Labels,
			systemLabels,
		),
		// SpecChanged is gated on ObservedGeneration > 0 so brand-new
		// Workers go through StatusNotFound create instead of a transient
		// spec-change delete. CurrentSpecHash lets sandbox read legacy live
		// annotations only when Worker.status.specHash is empty.
		SpecChanged:          specChanged,
		AppliedSpecHash:      appliedSpecHash,
		CurrentSpecHash:      w.Status.SpecHash,
		IsUpdate:             w.Status.Phase != "" && w.Status.Phase != "Pending" && w.Status.Phase != "Failed",
		TeamName:             w.Annotations["agentteams.io/team"],
		TeamLeaderName:       w.Annotations["agentteams.io/team-leader"],
		TeamAdminName:        w.Annotations["agentteams.io/team-admin"],
		TeamAdminMatrixID:    w.Annotations["agentteams.io/team-admin-id"],
		ExistingMatrixUserID: w.Status.MatrixUserID,
		ExistingRoomID:       w.Status.RoomID,
		CurrentExposedPorts:  w.Status.ExposedPorts,
		Owner:                w,
		DeployMode:           deployMode,
		ServiceEnabled:       serviceEnabled,
		Resources:            agentResourcesToBackend(resourceSpec),
		BackendRuntime:       backendRuntime,
		StatusBackendRuntime: w.Status.BackendRuntime,
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
	if state.BackendRuntime != "" {
		w.Status.BackendRuntime = state.BackendRuntime
	}
}

// reconcileWorkerSvcLabel adds or removes the worker Service name
// label on the Worker CR. Returns true if the label set was modified.
func reconcileWorkerSvcLabel(w *v1beta1.Worker, svcName string) bool {
	if svcName != "" {
		if w.Labels == nil {
			w.Labels = make(map[string]string)
		}
		if w.Labels[v1beta1.LabelWorkerSvcName] == svcName {
			return false
		}
		w.Labels[v1beta1.LabelWorkerSvcName] = svcName
		return true
	}
	// Service disabled/removed — delete label if present.
	if _, exists := w.Labels[v1beta1.LabelWorkerSvcName]; !exists {
		return false
	}
	delete(w.Labels, v1beta1.LabelWorkerSvcName)
	return true
}

// computeWorkerPhase determines the Worker status phase from the reconcile
// outcome. Delegates to the shared computeMemberPhase function.
func computeWorkerPhase(w *v1beta1.Worker, containerState string, reconcileErr error) string {
	return computeMemberPhase(w.Status.Phase, w.Status.MatrixUserID, w.Spec.DesiredState(), containerState, reconcileErr)
}

func (r *WorkerReconciler) SetupWithManager(mgr ctrl.Manager) (controller.Controller, error) {
	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Worker{})

	if r.Backend != nil {
		ctx := context.Background()
		// Watch Pods when the K8s ("pod") backend is registered & available.
		if wb, _ := r.Backend.GetBackendForType(ctx, v1beta1.BackendRuntimePod); wb != nil {
			bldr = bldr.Watches(
				&corev1.Pod{},
				handler.EnqueueRequestsFromMapFunc(WorkerPodMapFunc("")),
				builder.WithPredicates(PodLifecyclePredicates(v1beta1.LabelWorker, r.ControllerName)),
			)
		}
		// Watch Sandbox CRs and transient SandboxClaim CRs when the sandbox
		// backend is registered & available.
		if wb, _ := r.Backend.GetBackendForType(ctx, v1beta1.BackendRuntimeSandbox); wb != nil {
			if sb, ok := wb.(*backend.SandboxBackend); ok {
				bldr = bldr.Watches(
					sb.WatchObject(),
					handler.EnqueueRequestsFromMapFunc(WorkerPodMapFunc("")),
					builder.WithPredicates(SandboxLifecyclePredicates(v1beta1.LabelWorker, r.ControllerName)),
				)
				bldr = bldr.Watches(
					sb.ClaimWatchObject(),
					handler.EnqueueRequestsFromMapFunc(WorkerPodMapFunc("")),
					builder.WithPredicates(SandboxLifecyclePredicates(v1beta1.LabelWorker, r.ControllerName)),
				)
			}
		}
		// Docker / embedded mode has no watch source; reconciles for those
		// deployments are time-driven by RequeueAfter.
	}

	return bldr.Build(r)
}

// WorkerPodMapFunc returns a MapFunc for routing Pod events to Worker reconcile
// requests. If namespace is non-empty, it overrides obj.GetNamespace() — used
// for remote clusters where Pod namespace != CR namespace.
func WorkerPodMapFunc(namespace string) handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		workerName := obj.GetLabels()[v1beta1.LabelWorker]
		if workerName == "" {
			return nil
		}
		ns := namespace
		if ns == "" {
			ns = obj.GetNamespace()
		}
		return []reconcile.Request{
			{NamespacedName: client.ObjectKey{
				Name:      workerName,
				Namespace: ns,
			}},
		}
	}
}

// hashAppliedWorkerSpec computes a fnv64a hash of the WorkerSpec with selected
// config-only, lifecycle/policy-only, and service-only fields zeroed out. This
// captures only spec fields that should trigger container recreation when
// changed.
//
// Current standard-runtime coverage (fnv64a over json.Marshal with excluded
// fields zeroed):
//
//	ModelProvider, Runtime, Image, WorkerName, Identity, Soul,
//	Agents, Skills, RemoteSkills, Package, ChannelPolicy, ContainerManaged,
//	DeployMode, BackendRuntime, Labels, Env, Volumes, Mounts.
//
// Excluded (do not trigger pod recreation):
//
//	Model, McpServers — config-only (consumed by ReconcileMemberConfig)
//	AccessEntries — permission-only (resolved by credential issuance)
//	AgentIdentity, CredentialBindings — runtime credential config
//	State, IdleTimeout — lifecycle/policy
//	ServiceEnabled, Expose — service-only (consumed by ReconcileMemberService)
//
// Consumed by workerMemberContext to populate MemberContext.AppliedSpecHash,
// which owning reconcilers write to status.specHash after a successful
// reconcile. Sandbox resources no longer store this hash.
func hashAppliedWorkerSpec(spec v1beta1.WorkerSpec) string {
	spec.Model = ""          // config-only: written to openclaw.json/runtime.yaml
	spec.McpServers = nil    // config-only: written to mcporter/runtime config
	spec.AccessEntries = nil // permission-only: resolved when credentials are issued
	spec.AgentIdentity = nil // config-only: written to runtime.yaml
	spec.CredentialBindings = nil
	spec.State = nil          // exclude lifecycle state from hash
	spec.IdleTimeout = ""     // exclude controller-side autosleep policy from hash
	spec.ServiceEnabled = nil // service-only: does not affect pod
	spec.Expose = nil         // service-only: does not affect pod
	layoutVersion := workerDepsLayoutHashVersion(spec)
	if layoutVersion == "" {
		buf, err := json.Marshal(spec)
		if err != nil {
			return ""
		}
		h := fnv.New64a()
		_, _ = h.Write(buf)
		return fmt.Sprintf("%x", h.Sum64())
	}
	payload := struct {
		Spec             v1beta1.WorkerSpec `json:"spec"`
		WorkerDepsLayout string             `json:"workerDepsLayout,omitempty"`
	}{
		Spec:             spec,
		WorkerDepsLayout: layoutVersion,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return fmt.Sprintf("%x", h.Sum64())
}

func hashAppliedWorkerSpecForRuntime(spec v1beta1.WorkerSpec, runtime string) string {
	if runtime == backend.RuntimeQwenPaw {
		if spec.Runtime == "" {
			spec.Runtime = runtime
		}
		return hashQwenPawPodSpec(spec)
	}
	return hashAppliedWorkerSpec(spec)
}

func hashAppliedWorkerSpecForRuntimeAndResources(spec v1beta1.WorkerSpec, runtime string, resources *v1beta1.AgentResourceRequirements) string {
	if runtime == backend.RuntimeQwenPaw {
		if spec.Runtime == "" {
			spec.Runtime = runtime
		}
		return hashQwenPawPodSpecWithResources(spec, resources)
	}
	if resources == nil {
		return hashAppliedWorkerSpec(spec)
	}
	spec.Model = ""           // config-only: written to openclaw.json/runtime.yaml
	spec.McpServers = nil     // config-only: written to mcporter/runtime config
	spec.AccessEntries = nil  // permission-only: resolved when credentials are issued
	spec.State = nil          // exclude lifecycle state from hash
	spec.IdleTimeout = ""     // exclude controller-side autosleep policy
	spec.ServiceEnabled = nil // service-only: does not affect pod
	spec.Expose = nil         // service-only: does not affect pod
	spec.Resources = nil
	payload := struct {
		Spec             v1beta1.WorkerSpec                 `json:"spec"`
		Resources        *v1beta1.AgentResourceRequirements `json:"resources,omitempty"`
		WorkerDepsLayout string                             `json:"workerDepsLayout,omitempty"`
	}{
		Spec:             spec,
		Resources:        resources,
		WorkerDepsLayout: workerDepsLayoutHashVersion(spec),
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return fmt.Sprintf("%x", h.Sum64())
}

func workerSpecWithEffectiveBackendRuntimeForHash(spec v1beta1.WorkerSpec, backendRuntime string) v1beta1.WorkerSpec {
	if spec.BackendRuntime == nil && backendRuntime != "" {
		spec.BackendRuntime = &backendRuntime
	}
	return spec
}

func hashQwenPawPodSpec(spec v1beta1.WorkerSpec) string {
	return hashQwenPawPodSpecWithResources(spec, nil)
}

func hashQwenPawPodSpecWithResources(spec v1beta1.WorkerSpec, resources *v1beta1.AgentResourceRequirements) string {
	type qwenPawPodSpec struct {
		Runtime          string                             `json:"runtime,omitempty"`
		Image            string                             `json:"image,omitempty"`
		WorkerName       string                             `json:"workerName,omitempty"`
		ContainerManaged *bool                              `json:"containerManaged,omitempty"`
		DeployMode       *string                            `json:"deployMode,omitempty"`
		BackendRuntime   *string                            `json:"backendRuntime,omitempty"`
		Resources        *v1beta1.AgentResourceRequirements `json:"resources,omitempty"`
		Env              map[string]string                  `json:"env,omitempty"`
		Labels           map[string]string                  `json:"labels,omitempty"`
		Volumes          []v1beta1.WorkerVolumeSpec         `json:"volumes,omitempty"`
		Mounts           []v1beta1.WorkerMountSpec          `json:"mounts,omitempty"`
		WorkerDepsLayout string                             `json:"workerDepsLayout,omitempty"`
	}
	payload := qwenPawPodSpec{
		Runtime:          spec.Runtime,
		Image:            spec.Image,
		WorkerName:       spec.WorkerName,
		ContainerManaged: spec.ContainerManaged,
		DeployMode:       spec.DeployMode,
		BackendRuntime:   spec.BackendRuntime,
		Resources:        resources,
		Env:              spec.Env,
		Labels:           spec.Labels,
		Volumes:          spec.Volumes,
		Mounts:           spec.Mounts,
		WorkerDepsLayout: workerDepsLayoutHashVersion(spec),
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return fmt.Sprintf("%x", h.Sum64())
}

func workerDepsLayoutHashVersion(spec v1beta1.WorkerSpec) string {
	if len(spec.Volumes) > 0 || len(spec.Mounts) > 0 {
		return workerDepsLayoutVersion
	}
	return workerDepsLayoutVersionForBackendRuntime(spec.GetBackendRuntime())
}

func workerDepsLayoutVersionForBackendRuntime(backendRuntime string) string {
	switch backendRuntime {
	case v1beta1.BackendRuntimeSandbox:
		return workerDepsLayoutVersion
	default:
		return ""
	}
}

// PodLifecyclePredicates filters Pod events to only trigger reconciliation on
// create, delete, or relevant status transitions. A pod is considered "ours" only when
// it carries both:
//
//   - labelKey (one of the AgentTeams identity labels) with a non-empty
//     value — identifying which CR
//     kind owns the pod.
//   - agentteams.io/controller == controllerName — identifying which controller
//     instance owns the pod.
//
// The controller filter is defense-in-depth against the informer cache label
// selector configured in app.startInCluster (opts.Cache.ByObject for Pods).
// If a future watch source is wired without that cache filter, this predicate
// still prevents cross-instance reconcile when two agentteams-controller
// releases share a namespace.
func PodLifecyclePredicates(labelKey, controllerName string) predicate.Predicate {
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
			return podLifecycleSignal(oldPod) != podLifecycleSignal(newPod)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func podLifecycleSignal(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	return strings.Join([]string{
		string(pod.Status.Phase),
		podReadyConditionSignal(pod.Status.Conditions),
		podContainerStatusesSignal(pod.Status.InitContainerStatuses),
		podContainerStatusesSignal(pod.Status.ContainerStatuses),
	}, "\n")
}

func podReadyConditionSignal(conditions []corev1.PodCondition) string {
	for i := range conditions {
		cond := conditions[i]
		if cond.Type == corev1.PodReady {
			return fmt.Sprintf("%s|%s|%s", cond.Status, cond.Reason, cond.Message)
		}
	}
	return ""
}

func podContainerStatusesSignal(statuses []corev1.ContainerStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	parts := make([]string, 0, len(statuses))
	for i := range statuses {
		cs := statuses[i]
		state, reason, message := "unknown", "", ""
		switch {
		case cs.State.Waiting != nil:
			state = "waiting"
			reason = cs.State.Waiting.Reason
			message = cs.State.Waiting.Message
		case cs.State.Running != nil:
			state = "running"
		case cs.State.Terminated != nil:
			state = "terminated"
			reason = cs.State.Terminated.Reason
			message = cs.State.Terminated.Message
		}
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%t", cs.Name, state, reason, message, cs.Ready))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// SandboxLifecyclePredicates filters Sandbox CR events to only trigger
// reconciliation on create, delete, or .status.phase transitions.
// A sandbox is considered "ours" only when it carries both the given labelKey
// with a non-empty value and agentteams.io/controller == controllerName.
func SandboxLifecyclePredicates(labelKey, controllerName string) predicate.Predicate {
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
			// For unstructured objects, compare .status.phase string.
			oldPhase := extractUnstructuredPhase(e.ObjectOld)
			newPhase := extractUnstructuredPhase(e.ObjectNew)
			if oldPhase != newPhase {
				return true
			}
			// Also reconcile when the Ready condition status flips, since
			// remote sandbox backends may surface pod failures via
			// .status.conditions[type=Ready] without changing .status.phase.
			oldReady := extractUnstructuredReadyCondition(e.ObjectOld)
			newReady := extractUnstructuredReadyCondition(e.ObjectNew)
			return oldReady != newReady
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

// extractUnstructuredPhase reads .status.phase from an unstructured object.
func extractUnstructuredPhase(obj client.Object) string {
	u, ok := obj.(interface {
		UnstructuredContent() map[string]interface{}
	})
	if !ok {
		return ""
	}
	content := u.UnstructuredContent()
	status, ok := content["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	phase, _ := status["phase"].(string)
	return phase
}

// extractUnstructuredReadyCondition reads the status value of the
// .status.conditions[type=Ready] entry from an unstructured object.
// Returns an empty string when the object is not unstructured, has no
// conditions, or has no Ready condition. This keeps old/new comparisons
// stable so missing conditions do not falsely trigger reconciliation.
func extractUnstructuredReadyCondition(obj client.Object) string {
	u, ok := obj.(interface {
		UnstructuredContent() map[string]interface{}
	})
	if !ok {
		return ""
	}
	content := u.UnstructuredContent()
	status, ok := content["status"].(map[string]interface{})
	if !ok {
		return ""
	}
	conditions, ok := status["conditions"].([]interface{})
	if !ok {
		return ""
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _ := cond["type"].(string)
		if condType != "Ready" {
			continue
		}
		condStatus, _ := cond["status"].(string)
		return condStatus
	}
	return ""
}

// --- Package-level helpers ---

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
