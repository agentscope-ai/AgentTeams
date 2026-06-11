package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/agentconfig"
	"github.com/hiclaw/hiclaw-controller/internal/auth"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	"github.com/hiclaw/hiclaw-controller/internal/executor"
	"github.com/hiclaw/hiclaw-controller/internal/gateway"
	"github.com/hiclaw/hiclaw-controller/internal/metrics"
	"github.com/hiclaw/hiclaw-controller/internal/service"
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

// Team cache field indexer keys. Registered in app.initFieldIndexers and
// consumed by the auth enricher to resolve team membership by worker name
// without enumerating every Team.
const (
	TeamLeaderNameField = "spec.leader.name"
	TeamWorkerNameField = "spec.workerNames"
)

// TeamReconciler reconciles Team resources. It directly owns the lifecycle of
// team members (leader + workers) via the shared member_reconcile helpers; no
// child Worker CRs are created.
type TeamReconciler struct {
	client.Client

	Provisioner service.WorkerProvisioner
	Deployer    service.WorkerDeployer
	Backend     *backend.Registry
	EnvBuilder  service.WorkerEnvBuilderI
	Legacy      *service.LegacyCompat // nil in incluster mode

	// DefaultRuntime is forwarded into MemberDeps.DefaultRuntime for every
	// team member this reconciler converges. Sourced from
	// HICLAW_DEFAULT_WORKER_RUNTIME (Config.DefaultWorkerRuntime) — NOT from
	// HICLAW_MANAGER_RUNTIME — because team leader and worker containers are
	// both created through backend.WorkerBackend.Create as worker-type pods.
	// Empty means "no operator preference"; backend.ResolveRuntime then falls
	// back to RuntimeOpenClaw.
	DefaultRuntime string

	AgentFSDir string // for writing inline configs to the local agent FS

	// ControllerName, when non-empty, is merged as hiclaw.io/controller
	// into the PodLabels of every team member MemberContext this reconciler
	// builds, so the resulting Pods match the owning controller instance's
	// label-scoped cache. Post-refactor (PR #666) Teams no longer create
	// child Worker CRs, so the label is applied directly to Pods via
	// MemberContext.PodLabels → backend.CreateRequest.Labels. Empty in
	// embedded mode.
	ControllerName string

	// ResourcePrefix scopes team-member ServiceAccount and Pod names per
	// HiClaw tenant instance. Forwarded into MemberDeps.ResourcePrefix so
	// createMemberContainer uses it when computing saName. Empty collapses
	// to DefaultResourcePrefix ("hiclaw-").
	ResourcePrefix auth.ResourcePrefix
	GatewayClient  gateway.Client // gateway client for modelProvider resolution
}

type teamAdminActor struct {
	MatrixUserID string
	Token        string
	Username     string
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
		if controllerutil.ContainsFinalizer(&team, finalizerName) {
			if err := r.handleDelete(ctx, &team); err != nil {
				logger.Error(err, "failed to delete team", "name", team.Name)
				return reconcile.Result{RequeueAfter: 30 * time.Second}, err
			}
			controllerutil.RemoveFinalizer(&team, finalizerName)
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

func (r *TeamReconciler) resolveTeamAdminActor(ctx context.Context, t *v1beta1.Team) (teamAdminActor, error) {
	if t.Spec.Admin == nil {
		return teamAdminActor{}, nil
	}
	if strings.TrimSpace(t.Spec.Admin.Name) == "" {
		return teamAdminActor{}, fmt.Errorf("team admin human name is required")
	}

	var human v1beta1.Human
	key := client.ObjectKey{Name: t.Spec.Admin.Name, Namespace: t.Namespace}
	if err := r.Get(ctx, key, &human); err != nil {
		return teamAdminActor{}, fmt.Errorf("load team admin human %s/%s: %w", key.Namespace, key.Name, err)
	}

	username := human.Spec.EffectiveUsername(human.Name)
	matrixUserID := r.Provisioner.MatrixUserID(username)
	if t.Spec.Admin.MatrixUserID != "" && t.Spec.Admin.MatrixUserID != matrixUserID {
		return teamAdminActor{}, fmt.Errorf("team admin matrixUserId %q does not match Human %s/%s matrix user %q",
			t.Spec.Admin.MatrixUserID, key.Namespace, key.Name, matrixUserID)
	}
	if human.Status.InitialPassword == "" {
		return teamAdminActor{}, fmt.Errorf("team admin human %s/%s has no initial password; cannot obtain Matrix token",
			key.Namespace, key.Name)
	}

	token, err := r.Provisioner.LoginAsHuman(ctx, username, human.Status.InitialPassword)
	if err != nil {
		return teamAdminActor{}, fmt.Errorf("login as team admin human %s/%s: %w", key.Namespace, key.Name, err)
	}
	return teamAdminActor{
		MatrixUserID: matrixUserID,
		Token:        token,
		Username:     username,
	}, nil
}

// reconcileTeamNormal drives one convergence pass over a Team CR:
//  1. Provision team-level infra (rooms, shared storage)
//  2. Write local inline configs
//  3. Clean up stale members (in Status.Members but no longer desired)
//  4. Reconcile each desired member (leader + workers) via the shared phases
//     4.5. Inject leader coordination context + SOUL.md template (after package deploy)
//  5. Registry updates + summarise backend readiness and patch Team.Status
func (r *TeamReconciler) reconcileTeamNormal(ctx context.Context, t *v1beta1.Team) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	patchBase := client.MergeFrom(t.DeepCopy())
	if t.Status.Phase == "" {
		t.Status.Phase = "Pending"
		if err := r.Status().Patch(ctx, t, patchBase); err != nil {
			return reconcile.Result{}, err
		}
		patchBase = client.MergeFrom(t.DeepCopy())
	}

	workerNames := make([]string, 0, len(t.Spec.Workers))
	workerRuntimeNames := make([]string, 0, len(t.Spec.Workers))
	for _, w := range t.Spec.Workers {
		workerNames = append(workerNames, w.Name)
		workerRuntimeNames = append(workerRuntimeNames, w.EffectiveWorkerName())
	}
	if err := validateTeamRuntimeNames(t); err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}
	if err := r.validateNoStandaloneWorkerRuntimeConflicts(ctx, t); err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}
	teamRuntimeName := t.Spec.EffectiveTeamName(t.Name)
	leaderRuntimeName := t.Spec.Leader.EffectiveWorkerName()
	adminActor, err := r.resolveTeamAdminActor(ctx, t)
	if err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}
	derivedTeam := t
	if adminActor.MatrixUserID != "" {
		derivedTeam = t.DeepCopy()
		if derivedTeam.Spec.Admin == nil {
			derivedTeam.Spec.Admin = &v1beta1.TeamAdminSpec{}
		}
		derivedTeam.Spec.Admin.MatrixUserID = adminActor.MatrixUserID
	}

	// --- Step 1: Team-level infrastructure ---
	rooms, err := r.Provisioner.ProvisionTeamRooms(ctx, service.TeamRoomRequest{
		TeamName:             teamRuntimeName,
		LeaderName:           leaderRuntimeName,
		LeaderCredentialName: t.Spec.Leader.Name,
		WorkerNames:          workerRuntimeNames,
		AdminSpec:            derivedTeam.Spec.Admin,
		HumanMembers:         t.Spec.HumanMembers,
		TeamAdminActorToken:  adminActor.Token,
		TeamAdminActorName:   adminActor.Username,
	})
	if err != nil {
		return r.failTeam(ctx, t, patchBase, fmt.Sprintf("provision team rooms: %v", err))
	}
	t.Status.TeamRoomID = rooms.TeamRoomID
	t.Status.LeaderDMRoomID = rooms.LeaderDMRoomID

	if err := r.Deployer.EnsureTeamStorage(ctx, teamRuntimeName); err != nil {
		logger.Error(err, "team shared storage init failed (non-fatal)", "name", t.Name, "teamName", teamRuntimeName)
	}

	// --- Step 2: Write local inline configs (shared FS with agents) ---
	if err := r.writeInlineConfigs(t); err != nil {
		return r.failTeam(ctx, t, patchBase, err.Error())
	}

	// --- Step 3: Stale cleanup ---
	desiredMembers := buildDesiredMembers(derivedTeam, r.ControllerName)
	if r.GatewayClient != nil {
		for i := range desiredMembers {
			mp := desiredMembers[i].Spec.ModelProvider
			if mp == "" {
				continue
			}
			info, err := r.GatewayClient.ResolveModelProvider(ctx, mp)
			if err != nil {
				return r.failTeam(ctx, t, patchBase, fmt.Sprintf("resolve model provider %q for %s: %v", mp, desiredMembers[i].Name, err))
			}
			desiredMembers[i].ModelProviderInfo = info
		}
	}
	desiredNames := make(map[string]struct{}, len(desiredMembers))
	for _, m := range desiredMembers {
		desiredNames[m.Name] = struct{}{}
	}
	deps := MemberDeps{
		Provisioner:    r.Provisioner,
		Deployer:       r.Deployer,
		Backend:        r.Backend,
		EnvBuilder:     r.EnvBuilder,
		ResourcePrefix: r.ResourcePrefix,
		DefaultRuntime: r.DefaultRuntime,
		GatewayClient:  r.GatewayClient,
	}
	// staleCtx.Spec is intentionally left zero. The original TeamWorkerSpec
	// has already been removed from t.Spec.Workers, and we never persisted a
	// per-member snapshot. ReconcileMemberDelete -> DeprovisionWorker ->
	// DeleteConsumer relies on the consumer key (derived from Name) to cascade
	// removal of all authorizations attached to this worker (including MCP
	// server grants), so not forwarding Spec.McpServers is acceptable.
	for i := range t.Status.Members {
		ms := &t.Status.Members[i]
		if _, keep := desiredNames[ms.Name]; keep {
			continue
		}
		runtimeName := ms.RuntimeName
		if runtimeName == "" {
			runtimeName = ms.Name
		}
		role := RoleTeamWorker
		if ms.Role == RoleTeamLeader.String() || ms.Name == t.Spec.Leader.Name {
			role = RoleTeamLeader
		}
		staleCtx := MemberContext{
			Name:                ms.Name,
			RuntimeName:         runtimeName,
			Namespace:           t.Namespace,
			Role:                role,
			TeamName:            teamRuntimeName,
			TeamLeaderName:      leaderRuntimeName,
			ExistingRoomID:      ms.RoomID,
			CurrentExposedPorts: ms.ExposedPorts,
		}
		if err := ReconcileMemberDelete(ctx, deps, staleCtx); err != nil {
			logger.Error(err, "failed to remove stale team member (non-fatal)", "name", ms.Name)
		}
		r.removeLegacyMember(ctx, runtimeName)
	}
	pruneMembers(&t.Status, desiredNames)

	// --- Step 4: Reconcile each desired member (leader first) ---
	//
	// ms.Observed flips to true the moment ReconcileMemberInfra returns nil —
	// a failure in a later phase (Config/Container/Expose) does NOT revoke
	// observed status, because infra success means the Matrix user already
	// exists and its access token has been persisted. Dropping observed back
	// to false would force the next reconcile down the Provision path
	// (IsUpdate=false), which re-invokes matrix.EnsureUser's Login fallback
	// and mints a new access token — a rotation that triggers an openclaw
	// gateway restart. Only a member whose very first ReconcileMemberInfra
	// fails stays Observed=false, mirroring WorkerReconciler's
	// Status.MatrixUserID check.
	//
	// ms.SpecHash, in contrast, is updated only when ALL phases succeed, so
	// a partial failure keeps memberSpecChanged=true and retries container
	// recreation on the next pass.
	perMemberErrors := make([]string, 0)
	for i := range desiredMembers {
		m := desiredMembers[i]
		ms := memberStatus(&t.Status, m.Name, m.Role)
		if len(ms.ExposedPorts) > 0 {
			m.CurrentExposedPorts = ms.ExposedPorts
		}
		if err := r.reconcileMember(ctx, deps, m, ms); err != nil {
			logger.Error(err, "team member reconcile failed", "name", m.Name)
			perMemberErrors = append(perMemberErrors, fmt.Sprintf("%s: %v", m.Name, err))
			continue
		}
		// Record the hash only after a full reconcile success so a failed
		// mid-phase attempt on the next pass still sees SpecChanged=true
		// and retries the container recreation. ms.Observed was already
		// updated inside reconcileMember as soon as infra succeeded.
		ms.SpecHash = hashMemberSourceSpec(t, m.Role, m.Name)

		// Publish this member into workers-registry.json. Positioned after
		// SpecHash update for the same reason WorkerReconciler writes legacy
		// only after ReconcileMemberExpose succeeded: a fully converged
		// member is what the Manager-side tooling expects to find there.
		r.reconcileLegacyMember(ctx, t, m, ms)
	}

	// --- Step 4.5: Leader coordination context + SOUL.md template ---
	// Runs AFTER member reconciliation so that package deploy (seed-only)
	// writes the leader's package AGENTS.md and SOUL.md to OSS first.
	// InjectCoordinationContext then reads the package content from OSS and
	// overlays coordination context on top; renderAndPushSoulTemplate is
	// seed-only and correctly skips when the package already provided SOUL.md.
	if err := r.Deployer.InjectCoordinationContext(ctx, service.CoordinationDeployRequest{
		LeaderName:         leaderRuntimeName,
		Role:               RoleTeamLeader.String(),
		TeamName:           teamRuntimeName,
		TeamRoomID:         rooms.TeamRoomID,
		LeaderDMRoomID:     rooms.LeaderDMRoomID,
		HeartbeatEvery:     leaderHeartbeatEvery(t),
		WorkerIdleTimeout:  t.Spec.Leader.WorkerIdleTimeout,
		TeamWorkers:        workerRuntimeNames,
		TeamAdminID:        teamAdminMatrixID(derivedTeam),
		TeamCoordinatorIDs: teamCoordinatorIDs(derivedTeam),
		LeaderSoul:         t.Spec.Leader.Soul,
	}); err != nil {
		logger.Error(err, "leader coordination context injection failed (non-fatal)")
	}

	// --- Step 5: Registry updates ---
	if r.Legacy != nil && r.Legacy.Enabled() {
		leaderMatrixID := r.Legacy.MatrixUserID(leaderRuntimeName)
		if err := r.Legacy.UpdateManagerGroupAllowFrom(leaderMatrixID, true); err != nil {
			logger.Error(err, "failed to update Manager groupAllowFrom for team leader (non-fatal)")
		}
		if err := r.Legacy.UpdateTeamsRegistry(service.TeamRegistryEntry{
			Name:           teamRuntimeName,
			Leader:         t.Spec.Leader.Name,
			Workers:        workerNames,
			TeamRoomID:     rooms.TeamRoomID,
			LeaderDMRoomID: rooms.LeaderDMRoomID,
			Admin:          teamAdminRegistryEntry(derivedTeam.Spec.Admin),
			Members:        teamMemberRegistryEntries(t.Spec.HumanMembers),
		}); err != nil {
			logger.Error(err, "teams-registry update failed (non-fatal)")
		}
	}

	// --- Step 6: Summarise backend readiness and patch status ---
	leaderReady, readyWorkers := r.summarizeBackendReadiness(ctx, t, desiredMembers)
	sortMembers(&t.Status)
	t.Status.TotalWorkers = len(t.Spec.Workers)
	t.Status.LeaderReady = leaderReady
	t.Status.ReadyWorkers = readyWorkers

	switch {
	case len(perMemberErrors) > 0:
		t.Status.Phase = "Degraded"
		t.Status.Message = strings.Join(perMemberErrors, "; ")
	case leaderReady && readyWorkers == t.Status.TotalWorkers:
		t.Status.Phase = "Active"
		t.Status.Message = ""
	default:
		t.Status.Phase = "Pending"
		t.Status.Message = ""
	}

	if err := r.Status().Patch(ctx, t, patchBase); err != nil {
		logger.Error(err, "failed to patch team status (non-fatal)")
	}

	requeue := reconcileInterval
	if len(perMemberErrors) > 0 {
		requeue = reconcileRetryDelay
	}
	logger.Info("team reconciled",
		"name", t.Name,
		"phase", t.Status.Phase,
		"leaderReady", leaderReady,
		"readyWorkers", readyWorkers,
		"members", observedMemberNames(&t.Status))
	return reconcile.Result{RequeueAfter: requeue}, nil
}

// reconcileMember runs the shared member phases for one team member and
// writes the resulting runtime state into ms. The leader never has
// ExposedPorts (the Leader phase always produces zero ports), so that field
// stays nil for RoleTeamLeader entries.
//
// ms.Observed is flipped to true the instant ReconcileMemberInfra succeeds —
// see the Step 4 comment in reconcileTeamNormal for why post-infra failures
// must not revoke observed status (token-rotation hazard).
func (r *TeamReconciler) reconcileMember(ctx context.Context, deps MemberDeps, m MemberContext, ms *v1beta1.TeamMemberStatus) error {
	state := &MemberState{}

	// Pre-populate ExistingMatrixUserID when we've already provisioned the
	// member before, forcing the Refresh path instead of Provision.
	if m.IsUpdate {
		m.ExistingMatrixUserID = r.Provisioner.MatrixUserID(m.RuntimeName)
	}

	if _, err := ReconcileMemberInfra(ctx, deps, m, state); err != nil {
		return err
	}
	if err := EnsureModelProviderAuth(ctx, deps, m, state); err != nil {
		return err
	}
	ms.Observed = true
	if state.RoomID != "" {
		ms.RoomID = state.RoomID
	}
	if state.MatrixUserID != "" {
		ms.MatrixUserID = state.MatrixUserID
	}
	ms.RuntimeName = m.RuntimeName
	if err := EnsureMemberServiceAccount(ctx, deps, m); err != nil {
		return err
	}
	if err := ReconcileMemberConfig(ctx, deps, m, state); err != nil {
		return err
	}
	if _, err := ReconcileMemberContainer(ctx, deps, m, state); err != nil {
		return err
	}
	_ = ReconcileMemberExpose(ctx, deps, m, state)

	if m.Role == RoleTeamWorker {
		ms.ExposedPorts = state.ExposedPorts
	} else {
		ms.ExposedPorts = nil
	}
	return nil
}

// summarizeBackendReadiness queries each member's pod/container status from
// the backend and writes ms.Ready per member. Used instead of reading Worker
// CR status because team members no longer have Worker CRs.
//
// On a backend-unreachable path (Backend == nil or DetectWorkerBackend nil)
// this preserves any previously-recorded ms.Ready value — callers should NOT
// treat a false/true gap across reconciles as a transition, since a transient
// backend outage would otherwise flap Phase=Active back to Pending.
func (r *TeamReconciler) summarizeBackendReadiness(ctx context.Context, t *v1beta1.Team, members []MemberContext) (leaderReady bool, readyWorkers int) {
	if r.Backend == nil {
		return false, 0
	}
	wb := r.Backend.DetectWorkerBackend(ctx)
	if wb == nil {
		return false, 0
	}
	for _, m := range members {
		result, err := wb.Status(ctx, m.Name)
		if err != nil {
			continue
		}
		ready := result.Status == backend.StatusRunning || result.Status == backend.StatusReady
		if ms := t.Status.MemberByName(m.Name); ms != nil {
			ms.Ready = ready
		}
		if m.Role == RoleTeamLeader {
			leaderReady = ready
			continue
		}
		if ready {
			readyWorkers++
		}
	}
	return leaderReady, readyWorkers
}

// writeInlineConfigs persists leader + worker inline identity/soul/agents
// strings to the shared agent FS. No-op for members that don't supply any.
func (r *TeamReconciler) writeInlineConfigs(t *v1beta1.Team) error {
	if t.Spec.Leader.Identity != "" || t.Spec.Leader.Soul != "" || t.Spec.Leader.Agents != "" {
		agentDir := fmt.Sprintf("%s/%s", r.AgentFSDir, t.Spec.Leader.Name)
		if err := executor.WriteInlineConfigs(agentDir, "copaw", t.Spec.Leader.Identity, t.Spec.Leader.Soul, t.Spec.Leader.Agents); err != nil {
			return fmt.Errorf("write leader inline configs: %w", err)
		}
	}
	for _, w := range t.Spec.Workers {
		if w.Identity == "" && w.Soul == "" && w.Agents == "" {
			continue
		}
		agentDir := fmt.Sprintf("%s/%s", r.AgentFSDir, w.Name)
		runtime := w.Runtime
		if runtime == "" {
			runtime = "copaw"
		}
		if err := executor.WriteInlineConfigs(agentDir, runtime, w.Identity, w.Soul, w.Agents); err != nil {
			return fmt.Errorf("write worker %s inline configs: %w", w.Name, err)
		}
	}
	return nil
}

func (r *TeamReconciler) handleDelete(ctx context.Context, t *v1beta1.Team) error {
	logger := log.FromContext(ctx)
	logger.Info("deleting team", "name", t.Name)
	teamRuntimeName := t.Spec.EffectiveTeamName(t.Name)

	deps := MemberDeps{
		Provisioner:    r.Provisioner,
		Deployer:       r.Deployer,
		Backend:        r.Backend,
		EnvBuilder:     r.EnvBuilder,
		ResourcePrefix: r.ResourcePrefix,
		DefaultRuntime: r.DefaultRuntime,
		GatewayClient:  r.GatewayClient,
	}

	// Union of Status.Members and desired members to guarantee cleanup even
	// when reconcile failed before writing Status.Members.
	names := make(map[string]MemberRole)
	for _, ms := range t.Status.Members {
		if ms.Role == RoleTeamLeader.String() || ms.Name == t.Spec.Leader.Name {
			names[ms.Name] = RoleTeamLeader
		} else {
			names[ms.Name] = RoleTeamWorker
		}
	}
	if t.Spec.Leader.Name != "" {
		names[t.Spec.Leader.Name] = RoleTeamLeader
	}
	for _, w := range t.Spec.Workers {
		names[w.Name] = RoleTeamWorker
	}

	errs := make([]error, 0)
	for name, role := range names {
		var exposed []v1beta1.ExposedPortStatus
		var existingRoomID string
		if ms := t.Status.MemberByName(name); ms != nil {
			exposed = ms.ExposedPorts
			existingRoomID = ms.RoomID
		}
		mctx := MemberContext{
			Name:                name,
			RuntimeName:         name,
			Namespace:           t.Namespace,
			Role:                role,
			TeamName:            teamRuntimeName,
			TeamLeaderName:      t.Spec.Leader.EffectiveWorkerName(),
			ExistingRoomID:      existingRoomID,
			CurrentExposedPorts: exposed,
		}
		if ms := t.Status.MemberByName(name); ms != nil && ms.RuntimeName != "" {
			mctx.RuntimeName = ms.RuntimeName
		}
		if role == RoleTeamLeader {
			mctx.Spec = leaderWorkerSpec(t)
			mctx.RuntimeName = t.Spec.Leader.EffectiveWorkerName()
		} else {
			// For "observed but no longer in Spec.Workers" entries (stale),
			// the inner loop finds no match and mctx.Spec stays zero. The
			// same rationale as the stale cleanup in reconcileTeamNormal
			// applies: deleting gateway and runtime resources here does not
			// need Spec.McpServers, which only feeds mcporter config writes.
			for _, w := range t.Spec.Workers {
				if w.Name == name {
					mctx.Spec = teamWorkerSpecToWorkerSpec(t, w)
					break
				}
			}
		}
		if err := ReconcileMemberDelete(ctx, deps, mctx); err != nil {
			logger.Error(err, "member cleanup failed (non-fatal)", "name", name)
			errs = append(errs, err)
		}
		r.removeLegacyMember(ctx, mctx.RuntimeName)
	}

	if r.Legacy != nil && r.Legacy.Enabled() {
		if t.Spec.Leader.Name != "" {
			leaderMatrixID := r.Legacy.MatrixUserID(t.Spec.Leader.EffectiveWorkerName())
			if err := r.Legacy.UpdateManagerGroupAllowFrom(leaderMatrixID, false); err != nil {
				logger.Error(err, "failed to revoke Manager groupAllowFrom (non-fatal)")
			}
		}
		if err := r.Legacy.RemoveFromTeamsRegistry(ctx, teamRuntimeName); err != nil {
			logger.Error(err, "failed to remove team from registry (non-fatal)")
		}
	}

	// Release the Matrix aliases that tied this Team to its rooms. The rooms
	// themselves are preserved (they still hold chat history and members can
	// leave on their own schedule), but a fresh Team CR with the same name
	// must get a clean alias so CreateRoom resolves a new room instead of
	// reattaching to the old one.
	if err := r.Provisioner.DeleteTeamRoomAliases(ctx, teamRuntimeName, t.Spec.Leader.EffectiveWorkerName()); err != nil {
		logger.Error(err, "failed to delete team room aliases (non-fatal)")
	}

	if len(errs) > 0 {
		// Errors are non-fatal individually but we return the aggregate so the
		// caller logs one consolidated message; finalizer removal still
		// proceeds at the Reconcile level to avoid stuck CRs.
		return kerrors.NewAggregate(errs)
	}
	return nil
}

// reconcileLegacyMember upserts a team member (leader or worker) into the
// legacy workers-registry.json. This is the TeamReconciler counterpart to
// WorkerReconciler.reconcileLegacy — both must emit entries with identical
// field semantics (role, team_id, runtime, skills, image) so that
// manager-side tooling (find-worker.sh, push-worker-skills.sh,
// update-worker-config.sh, etc.) can treat standalone workers and team
// members uniformly.
//
// m.Role drives the role string: RoleTeamLeader -> "team_leader",
// RoleTeamWorker -> "worker". ms is the Team.Status member entry populated by
// reconcileMember; RoomID/MatrixUserID on it are the source of truth for the
// registry row, but MatrixUserID is re-derived via r.Legacy.MatrixUserID to
// stay deterministic (mirrors WorkerReconciler which uses
// r.Provisioner.MatrixUserID(w.Name)).
//
// Non-fatal: any OSS error is logged but does not fail the reconcile pass,
// matching the legacy contract in WorkerReconciler.
func (r *TeamReconciler) reconcileLegacyMember(ctx context.Context, t *v1beta1.Team, m MemberContext, ms *v1beta1.TeamMemberStatus) {
	if r.Legacy == nil || !r.Legacy.Enabled() {
		return
	}
	logger := log.FromContext(ctx)
	runtimeName := m.RuntimeName
	if runtimeName == "" {
		runtimeName = m.Name
	}

	roomID := ""
	if ms != nil {
		roomID = ms.RoomID
	}

	entry := service.WorkerRegistryEntry{
		Name:         runtimeName,
		MatrixUserID: r.Legacy.MatrixUserID(runtimeName),
		RoomID:       roomID,
		Runtime:      m.Spec.Runtime,
		Deployment:   "local",
		Skills:       m.Spec.Skills,
		Role:         m.Role.String(),
		TeamID:       nilIfEmpty(t.Spec.EffectiveTeamName(t.Name)),
		Image:        nilIfEmpty(m.Spec.Image),
	}
	if err := r.Legacy.UpdateWorkersRegistry(entry); err != nil {
		logger.Error(err, "workers-registry update failed (non-fatal)", "name", m.Name, "runtimeName", runtimeName)
	}
}

// removeLegacyMember deletes a team member from workers-registry.json. Used
// by both the stale-member cleanup in reconcileTeamNormal and the full team
// deletion in handleDelete. No-op when Legacy is disabled.
func (r *TeamReconciler) removeLegacyMember(ctx context.Context, runtimeName string) {
	if r.Legacy == nil || !r.Legacy.Enabled() {
		return
	}
	if runtimeName == "" {
		return
	}
	if err := r.Legacy.RemoveFromWorkersRegistry(runtimeName); err != nil {
		log.FromContext(ctx).Error(err, "workers-registry remove failed (non-fatal)", "runtimeName", runtimeName)
	}
}

func (r *TeamReconciler) failTeam(ctx context.Context, t *v1beta1.Team, patchBase client.Patch, msg string) (reconcile.Result, error) {
	t.Status.Phase = "Failed"
	t.Status.Message = msg
	if err := r.Status().Patch(ctx, t, patchBase); err != nil {
		log.FromContext(ctx).Error(err, "failed to patch team status after failure (non-fatal)")
	}
	return reconcile.Result{RequeueAfter: reconcileRetryDelay}, fmt.Errorf("%s", msg)
}

// --- helpers ---

// memberStatus returns a pointer to the entry for name in s.Members,
// creating a zero-value entry (tagged with the given role) when absent. The
// returned pointer remains valid for in-place mutation across the reconcile
// pass because the caller treats Members as append-only for the duration of
// reconcileTeamNormal (pruneMembers runs once up front, before the per-
// member loop); no subsequent call re-slices the underlying array, so a
// pointer obtained here will not be invalidated by later memberStatus
// appends.
func memberStatus(s *v1beta1.TeamStatus, name string, role MemberRole) *v1beta1.TeamMemberStatus {
	if existing := s.MemberByName(name); existing != nil {
		if existing.Role == "" {
			existing.Role = role.String()
		}
		return existing
	}
	s.Members = append(s.Members, v1beta1.TeamMemberStatus{Name: name, Role: role.String()})
	return &s.Members[len(s.Members)-1]
}

// pruneMembers removes entries from s.Members whose names are not present in
// keep. Called exactly once per reconcile (Step 3) so the memberStatus
// pointer-stability invariant above holds.
func pruneMembers(s *v1beta1.TeamStatus, keep map[string]struct{}) {
	if len(s.Members) == 0 {
		return
	}
	filtered := s.Members[:0]
	for _, ms := range s.Members {
		if _, ok := keep[ms.Name]; ok {
			filtered = append(filtered, ms)
		}
	}
	// Zero out the trailing tail to release references to dropped entries
	// (important when ExposedPorts holds domain strings).
	for i := len(filtered); i < len(s.Members); i++ {
		s.Members[i] = v1beta1.TeamMemberStatus{}
	}
	s.Members = filtered
}

// sortMembers orders Members by Name for stable status patches and
// deterministic test assertions. Kubernetes merge-patch compares the full
// array by index, so an unstable order would cause spurious patch churn and
// unnecessary informer events.
func sortMembers(s *v1beta1.TeamStatus) {
	sort.Slice(s.Members, func(i, j int) bool {
		return s.Members[i].Name < s.Members[j].Name
	})
}

// observedMemberNames returns the sorted names of members with Observed=true.
// Used only for logging ("team reconciled … members=[…]"). Unexported so the
// log key stays controller-internal and tests do not lock it in.
func observedMemberNames(s *v1beta1.TeamStatus) []string {
	names := make([]string, 0, len(s.Members))
	for _, ms := range s.Members {
		if ms.Observed {
			names = append(names, ms.Name)
		}
	}
	sort.Strings(names)
	return names
}

// buildDesiredMembers translates a Team spec into MemberContexts for leader
// and each worker. Every member is tagged with PodLabel hiclaw.io/team=<name>
// so the Team controller can watch their pod lifecycle via a shared predicate.
//
// SpecChanged is computed per member via hashMemberSourceSpec, which digests
// only the user-authored fields that govern a member container. Derived
// context (peer list inflated into ChannelPolicy, admin Matrix injections)
// is intentionally excluded so adding/removing a peer does NOT recreate the
// other members — only the newly added member gets a fresh container.
func buildDesiredMembers(t *v1beta1.Team, controllerName string) []MemberContext {
	teamRuntimeName := t.Spec.EffectiveTeamName(t.Name)
	isObserved := func(name string) bool {
		if ms := t.Status.MemberByName(name); ms != nil {
			return ms.Observed
		}
		return false
	}
	// memberLabels returns the base PodLabels for a team member stamped
	// with the owning controller's identity. The four layers merged here
	// (low-to-high): Team.metadata.labels (team-wide defaults), the
	// per-member spec.labels (perMemberLabels, overrides team-wide on
	// collision), then the controller-forced system labels (highest).
	// Controller system labels deliberately come last so any reserved
	// key a user writes is silently overridden rather than rejected.
	// In in-cluster mode controllerName is always non-empty (see
	// Config.validate), so no defensive check is needed.
	memberLabels := func(role MemberRole, perMemberLabels map[string]string) map[string]string {
		return mergeLabels(
			t.ObjectMeta.Labels,
			perMemberLabels,
			map[string]string{
				"hiclaw.io/team":        t.Name,
				"hiclaw.io/role":        role.String(),
				v1beta1.LabelController: controllerName,
			},
		)
	}
	members := make([]MemberContext, 0, 1+len(t.Spec.Workers))

	leaderSpec := leaderWorkerSpec(t)
	leaderObserved := isObserved(t.Spec.Leader.Name)
	var leaderHeartbeat *agentconfig.HeartbeatConfig
	if t.Spec.Leader.Heartbeat != nil && t.Spec.Leader.Heartbeat.Enabled {
		every := t.Spec.Leader.Heartbeat.Every
		leaderHeartbeat = &agentconfig.HeartbeatConfig{
			Enabled: true,
			Every:   every,
		}
	}
	members = append(members, MemberContext{
		Name:               t.Spec.Leader.Name,
		RuntimeName:        t.Spec.Leader.EffectiveWorkerName(),
		Namespace:          t.Namespace,
		Role:               RoleTeamLeader,
		Spec:               leaderSpec,
		Generation:         t.Generation,
		SpecChanged:        memberSpecChanged(t, RoleTeamLeader, t.Spec.Leader.Name),
		IsUpdate:           leaderObserved,
		TeamName:           teamRuntimeName,
		TeamLeaderName:     "",
		TeamAdminMatrixID:  teamAdminMatrixID(t),
		TeamCoordinatorIDs: teamCoordinatorIDs(t),
		PodLabels:          memberLabels(RoleTeamLeader, t.Spec.Leader.Labels),
		Owner:              t,
		Heartbeat:          leaderHeartbeat,
	})

	for _, w := range t.Spec.Workers {
		workerObserved := isObserved(w.Name)
		spec := teamWorkerSpecToWorkerSpec(t, w)
		members = append(members, MemberContext{
			Name:               w.Name,
			RuntimeName:        w.EffectiveWorkerName(),
			Namespace:          t.Namespace,
			Role:               RoleTeamWorker,
			Spec:               spec,
			Generation:         t.Generation,
			SpecChanged:        memberSpecChanged(t, RoleTeamWorker, w.Name),
			IsUpdate:           workerObserved,
			TeamName:           teamRuntimeName,
			TeamLeaderName:     t.Spec.Leader.EffectiveWorkerName(),
			TeamAdminMatrixID:  teamAdminMatrixID(t),
			TeamCoordinatorIDs: teamCoordinatorIDs(t),
			PodLabels:          memberLabels(RoleTeamWorker, w.Labels),
			Owner:              t,
		})
	}
	return members
}

// memberSpecChanged returns true only when the member has been observed
// before AND its source-level spec hash now differs from the recorded one.
// A missing stored hash (brand-new member, or pre-upgrade state) returns
// false: "SpecChanged" is reserved for "the user edited this member's
// spec", not "we haven't seen this one yet". Initial container creation
// is handled by the StatusNotFound branch in ReconcileMemberContainer
// independently of SpecChanged, so returning false here is safe and avoids
// a transient race: between the first reconcile that creates the container
// and the second reconcile that observes it Running/Starting, the Status
// patch carrying the fresh hash may not yet have propagated to the
// informer cache — a stored-hash-empty-means-changed policy would Delete
// the just-created container on that intervening pass.
func memberSpecChanged(t *v1beta1.Team, role MemberRole, name string) bool {
	ms := t.Status.MemberByName(name)
	if ms == nil || ms.SpecHash == "" {
		return false
	}
	return ms.SpecHash != hashMemberSourceSpec(t, role, name)
}

// hashMemberSourceSpec digests the user-authored fields that govern a
// member's container. The decisive rule is: only fields the user explicitly
// types into the Team CR count. Derived values — the team-wide peer list
// inflated into ChannelPolicy.GroupAllowExtra, the admin Matrix ID
// injections — are excluded. Concretely:
//   - leader hash covers Team.Spec.Leader (the whole LeaderSpec, incl. its
//     own ChannelPolicy) + team-level ChannelPolicy
//   - worker hash covers the matching TeamWorkerSpec (incl. its own
//     ChannelPolicy, Expose, MCPServers, Skills, Image, State, etc.) +
//     team-level ChannelPolicy + PeerMentions toggle (flipping the toggle
//     *does* rewrite every worker's policy, so it must be in the hash)
//
// Returning "" on marshal error is safe: memberSpecChanged treats an empty
// stored hash as "changed", and an empty current hash as not-equal to any
// non-empty stored hash, so either direction errs on the side of recreation.
//
// Stability contract for LeaderSpec / TeamWorkerSpec evolution:
//
// Because the payload embeds the whole LeaderSpec / TeamWorkerSpec, adding a
// field to either struct without a json:",omitempty" tag will introduce a new
// key in every marshaled payload — even when the user never sets that field.
// All existing Teams' MemberSpecHashes would then diverge from the stored
// value on the first reconcile after upgrade, triggering container recreation
// for every member of every Team simultaneously.
//
// To preserve hash stability, every new field on LeaderSpec or
// TeamWorkerSpec MUST:
//  1. carry a json:",omitempty" tag, AND
//  2. have a zero value that is semantically equivalent to the old
//     (pre-field) behavior.
//
// This is a load-bearing invariant, not a style suggestion. If a future
// field genuinely needs to cause recreation, that migration should be
// explicit (e.g. bump a dedicated schema version) rather than implicit via
// JSON key churn.
func hashMemberSourceSpec(t *v1beta1.Team, role MemberRole, name string) string {
	type leaderInput struct {
		Leader     v1beta1.LeaderSpec         `json:"leader"`
		TeamPolicy *v1beta1.ChannelPolicySpec `json:"teamPolicy,omitempty"`
	}
	type workerInput struct {
		Worker       v1beta1.TeamWorkerSpec     `json:"worker"`
		TeamPolicy   *v1beta1.ChannelPolicySpec `json:"teamPolicy,omitempty"`
		PeerMentions *bool                      `json:"peerMentions,omitempty"`
	}
	var payload any
	switch role {
	case RoleTeamLeader:
		payload = leaderInput{Leader: t.Spec.Leader, TeamPolicy: t.Spec.ChannelPolicy}
	case RoleTeamWorker:
		var ws v1beta1.TeamWorkerSpec
		found := false
		for _, w := range t.Spec.Workers {
			if w.Name == name {
				ws = w
				found = true
				break
			}
		}
		if !found {
			return ""
		}
		payload = workerInput{
			Worker:       ws,
			TeamPolicy:   t.Spec.ChannelPolicy,
			PeerMentions: t.Spec.PeerMentions,
		}
	default:
		return ""
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return fmt.Sprintf("%x", h.Sum64())
}

// leaderWorkerSpec projects a LeaderSpec into WorkerSpec with merged channel
// policy (team leader can @ all workers + human coordinators).
func leaderWorkerSpec(t *v1beta1.Team) v1beta1.WorkerSpec {
	policy := mergeChannelPolicy(t.Spec.ChannelPolicy, t.Spec.Leader.ChannelPolicy)
	workerNames := make([]string, 0, len(t.Spec.Workers))
	for _, w := range t.Spec.Workers {
		workerNames = append(workerNames, w.EffectiveWorkerName())
	}
	policy = appendGroupAllowExtra(policy, workerNames...)
	if coordinatorIDs := teamCoordinatorIDs(t); len(coordinatorIDs) > 0 {
		policy = appendGroupAllowExtra(policy, coordinatorIDs...)
	}
	if teamAdminID := teamAdminMatrixID(t); teamAdminID != "" {
		policy = appendDmAllowExtra(policy, teamAdminID)
	}
	return v1beta1.WorkerSpec{
		Model:         t.Spec.Leader.Model,
		ModelProvider: t.Spec.Leader.ModelProvider,
		Runtime:       "copaw",
		WorkerName:    t.Spec.Leader.WorkerName,
		Identity:      t.Spec.Leader.Identity,
		Soul:          t.Spec.Leader.Soul,
		Agents:        t.Spec.Leader.Agents,
		Package:       t.Spec.Leader.Package,
		RemoteSkills:  t.Spec.Leader.RemoteSkills,
		McpServers:    t.Spec.Leader.McpServers,
		ChannelPolicy: policy,
		State:         t.Spec.Leader.State,
		Resources:     t.Spec.Leader.Resources,
		Env:           t.Spec.Leader.Env,
	}
}

// teamWorkerSpecToWorkerSpec projects a TeamWorkerSpec into WorkerSpec with
// the policy merge rules:
//   - leader is always on the worker's groupAllow
//   - team admin and coordinator members are on the worker's groupAllow
//   - if Team.Spec.PeerMentions is true (default), all peers are groupAllow too
//
// Runtime is passed through from TeamWorkerSpec. An empty value is intentional
// and means "no per-member preference" — backend.ResolveRuntime resolves it
// against TeamReconciler.DefaultRuntime (sourced from HICLAW_DEFAULT_WORKER_RUNTIME
// via MemberDeps.DefaultRuntime → backend.CreateRequest.RuntimeFallback),
// falling back to RuntimeOpenClaw if neither is set. This matches the
// resolution chain used by standalone Worker CRs.
func teamWorkerSpecToWorkerSpec(t *v1beta1.Team, w v1beta1.TeamWorkerSpec) v1beta1.WorkerSpec {
	policy := mergeChannelPolicy(t.Spec.ChannelPolicy, w.ChannelPolicy)
	policy = appendGroupAllowExtra(policy, t.Spec.Leader.EffectiveWorkerName())
	if coordinatorIDs := teamCoordinatorIDs(t); len(coordinatorIDs) > 0 {
		policy = appendGroupAllowExtra(policy, coordinatorIDs...)
	}
	peerMentions := t.Spec.PeerMentions == nil || *t.Spec.PeerMentions
	if peerMentions {
		for _, peer := range t.Spec.Workers {
			if peer.Name != w.Name {
				policy = appendGroupAllowExtra(policy, peer.EffectiveWorkerName())
			}
		}
	}
	return v1beta1.WorkerSpec{
		Model:         w.Model,
		ModelProvider: w.ModelProvider,
		Runtime:       w.Runtime,
		WorkerName:    w.WorkerName,
		Image:         w.Image,
		Identity:      w.Identity,
		Soul:          w.Soul,
		Agents:        w.Agents,
		Skills:        w.Skills,
		RemoteSkills:  w.RemoteSkills,
		McpServers:    w.McpServers,
		Package:       w.Package,
		Expose:        w.Expose,
		ChannelPolicy: policy,
		State:         w.State,
		Resources:     w.Resources,
		Env:           w.Env,
	}
}

func validateTeamRuntimeNames(t *v1beta1.Team) error {
	seen := map[string]string{}
	leaderRuntime := t.Spec.Leader.EffectiveWorkerName()
	seen[leaderRuntime] = "leader[" + t.Spec.Leader.Name + "]"
	for _, w := range t.Spec.Workers {
		runtimeName := w.EffectiveWorkerName()
		if owner, ok := seen[runtimeName]; ok {
			return fmt.Errorf("duplicate team runtime workerName %q between %s and worker[%s]", runtimeName, owner, w.Name)
		}
		seen[runtimeName] = "worker[" + w.Name + "]"
	}
	return nil
}

func (r *TeamReconciler) validateNoStandaloneWorkerRuntimeConflicts(ctx context.Context, t *v1beta1.Team) error {
	if r.Client == nil {
		return nil
	}

	desiredNames := map[string]string{
		t.Spec.Leader.Name: "leader[" + t.Spec.Leader.Name + "]",
	}
	desiredRuntimeNames := map[string]string{
		t.Spec.Leader.EffectiveWorkerName(): "leader[" + t.Spec.Leader.Name + "]",
	}
	for _, w := range t.Spec.Workers {
		desiredNames[w.Name] = "worker[" + w.Name + "]"
		desiredRuntimeNames[w.EffectiveWorkerName()] = "worker[" + w.Name + "]"
	}

	var workers v1beta1.WorkerList
	if err := r.List(ctx, &workers, client.InNamespace(t.Namespace)); err != nil {
		return fmt.Errorf("list standalone workers: %w", err)
	}
	for _, worker := range workers.Items {
		if owner, ok := desiredNames[worker.Name]; ok {
			return fmt.Errorf("team member %s name %q conflicts with existing standalone Worker %s/%s", owner, worker.Name, worker.Namespace, worker.Name)
		}
		runtimeName := worker.Spec.EffectiveWorkerName(worker.Name)
		if owner, ok := desiredRuntimeNames[runtimeName]; ok {
			return fmt.Errorf("team member %s runtime workerName %q conflicts with existing standalone Worker %s/%s", owner, runtimeName, worker.Namespace, worker.Name)
		}
	}
	return nil
}

func teamAdminMatrixID(t *v1beta1.Team) string {
	if t.Spec.Admin == nil {
		return ""
	}
	return t.Spec.Admin.MatrixUserID
}

func teamCoordinatorIDs(t *v1beta1.Team) []string {
	ids := make([]string, 0, 1+len(t.Spec.HumanMembers))
	if adminID := teamAdminMatrixID(t); adminID != "" {
		ids = append(ids, adminID)
	}
	for _, member := range t.Spec.HumanMembers {
		if !teamMemberIsCoordinator(member) {
			continue
		}
		switch {
		case member.MatrixUserID != "":
			ids = append(ids, member.MatrixUserID)
		case member.Name != "":
			ids = append(ids, member.Name)
		}
	}
	return uniqueTeamStrings(ids)
}

func teamMemberIsCoordinator(member v1beta1.TeamMemberSpec) bool {
	return member.Role == "" || member.Role == "coordinator"
}

func uniqueTeamStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (r *TeamReconciler) SetupWithManager(mgr ctrl.Manager) error {
	bldr := ctrl.NewControllerManagedBy(mgr).For(&v1beta1.Team{})

	if r.Backend != nil {
		if wb := r.Backend.DetectWorkerBackend(context.Background()); wb != nil && wb.Name() == "k8s" {
			bldr = bldr.Watches(
				&corev1.Pod{},
				handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
					teamName := obj.GetLabels()["hiclaw.io/team"]
					if teamName == "" {
						return nil
					}
					return []reconcile.Request{
						{NamespacedName: client.ObjectKey{
							Name:      teamName,
							Namespace: obj.GetNamespace(),
						}},
					}
				}),
				builder.WithPredicates(podLifecyclePredicates("hiclaw.io/team", r.ControllerName)),
			)
		}
	}

	return bldr.Complete(r)
}

// --- Policy helpers (preserved from prior implementation) ---

func leaderHeartbeatEvery(team *v1beta1.Team) string {
	if team.Spec.Leader.Heartbeat == nil {
		return ""
	}
	return team.Spec.Leader.Heartbeat.Every
}

func mergeChannelPolicy(teamPolicy, individualPolicy *v1beta1.ChannelPolicySpec) *v1beta1.ChannelPolicySpec {
	if teamPolicy == nil && individualPolicy == nil {
		return nil
	}
	merged := &v1beta1.ChannelPolicySpec{}
	if teamPolicy != nil {
		merged.GroupAllowExtra = append(merged.GroupAllowExtra, teamPolicy.GroupAllowExtra...)
		merged.GroupDenyExtra = append(merged.GroupDenyExtra, teamPolicy.GroupDenyExtra...)
		merged.DmAllowExtra = append(merged.DmAllowExtra, teamPolicy.DmAllowExtra...)
		merged.DmDenyExtra = append(merged.DmDenyExtra, teamPolicy.DmDenyExtra...)
	}
	if individualPolicy != nil {
		merged.GroupAllowExtra = append(merged.GroupAllowExtra, individualPolicy.GroupAllowExtra...)
		merged.GroupDenyExtra = append(merged.GroupDenyExtra, individualPolicy.GroupDenyExtra...)
		merged.DmAllowExtra = append(merged.DmAllowExtra, individualPolicy.DmAllowExtra...)
		merged.DmDenyExtra = append(merged.DmDenyExtra, individualPolicy.DmDenyExtra...)
	}
	return merged
}

func appendGroupAllowExtra(policy *v1beta1.ChannelPolicySpec, names ...string) *v1beta1.ChannelPolicySpec {
	if len(names) == 0 {
		return policy
	}
	if policy == nil {
		policy = &v1beta1.ChannelPolicySpec{}
	}
	existing := make(map[string]bool, len(policy.GroupAllowExtra))
	for _, v := range policy.GroupAllowExtra {
		existing[v] = true
	}
	for _, n := range names {
		if n != "" && !existing[n] {
			policy.GroupAllowExtra = append(policy.GroupAllowExtra, n)
			existing[n] = true
		}
	}
	return policy
}

func appendDmAllowExtra(policy *v1beta1.ChannelPolicySpec, names ...string) *v1beta1.ChannelPolicySpec {
	if len(names) == 0 {
		return policy
	}
	if policy == nil {
		policy = &v1beta1.ChannelPolicySpec{}
	}
	existing := make(map[string]bool, len(policy.DmAllowExtra))
	for _, v := range policy.DmAllowExtra {
		existing[v] = true
	}
	for _, n := range names {
		if n != "" && !existing[n] {
			policy.DmAllowExtra = append(policy.DmAllowExtra, n)
			existing[n] = true
		}
	}
	return policy
}

func teamAdminRegistryEntry(admin *v1beta1.TeamAdminSpec) *service.TeamAdminEntry {
	if admin == nil {
		return nil
	}
	return &service.TeamAdminEntry{
		Name:         admin.Name,
		MatrixUserID: admin.MatrixUserID,
	}
}

func teamMemberRegistryEntries(members []v1beta1.TeamMemberSpec) []service.TeamMemberEntry {
	if len(members) == 0 {
		return nil
	}
	entries := make([]service.TeamMemberEntry, 0, len(members))
	for _, member := range members {
		entries = append(entries, service.TeamMemberEntry{
			Name:         member.Name,
			MatrixUserID: member.MatrixUserID,
			Role:         member.Role,
		})
	}
	return entries
}
