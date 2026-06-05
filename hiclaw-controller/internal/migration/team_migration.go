package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	hiclawmetrics "github.com/hiclaw/hiclaw-controller/internal/metrics"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TeamMigrator handles automatic migration of legacy Team CRs (spec.leader +
// spec.workers) to the decoupled model (spec.workerMembers referencing standalone
// Worker CRs). It is invoked by TeamReconciler during reconcile.
type TeamMigrator struct {
	Client      client.Client
	Backend     *backend.Registry
	Provisioner service.WorkerProvisioner
	Deployer    service.WorkerDeployer
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder

	Enabled   bool
	BatchSize int
}

// NeedsMigration returns true when a Team CR should be migrated:
//   - uses legacy inline spec (Leader/Workers)
//   - does not yet have WorkerMembers
//   - global migration is enabled
//   - not opted-out via annotation
func (m *TeamMigrator) NeedsMigration(t *v1beta1.Team) bool {
	if !m.Enabled {
		return false
	}
	if len(t.Spec.WorkerMembers) > 0 {
		return false
	}
	if t.Spec.Leader.Name == "" && len(t.Spec.Workers) == 0 {
		return false
	}
	if ann := t.Annotations[v1beta1.AnnotationAutoMigrate]; ann == "disabled" {
		return false
	}
	// Health gate: only migrate Teams that are Active with all members having
	// been provisioned (MatrixUserID populated). We check the actual data
	// needed by migration (seedWorkerStatus copies MatrixUserID/RoomID to
	// Worker CR) rather than relying on the Observed flag as a proxy.
	if t.Status.Phase != "Active" {
		return false
	}
	expectedMembers := 1 + len(t.Spec.Workers) // leader + workers
	if len(t.Status.Members) < expectedMembers {
		return false
	}
	for _, ms := range t.Status.Members {
		if ms.MatrixUserID == "" || ms.RoomID == "" {
			return false
		}
	}
	return true
}

// MigrationInProgress returns true when a Team is currently mid-migration.
func (m *TeamMigrator) MigrationInProgress(t *v1beta1.Team) bool {
	phase := t.Annotations[v1beta1.AnnotationMigrationPhase]
	switch phase {
	case v1beta1.MigrationPhaseWorkerCRsCreated,
		v1beta1.MigrationPhaseStatusSeeded,
		v1beta1.MigrationPhasePodsReparented,
		v1beta1.MigrationPhaseCoordinationInjected,
		v1beta1.MigrationPhaseTeamSpecPatched:
		return true
	}
	return false
}

// TryAcquire checks whether a new migration can be started (batch limiting).
// It counts Teams currently in-progress and returns true if under the limit.
func (m *TeamMigrator) TryAcquire(ctx context.Context) bool {
	var teams v1beta1.TeamList
	if err := m.Client.List(ctx, &teams); err != nil {
		return false
	}
	inProgress := 0
	for i := range teams.Items {
		if m.MigrationInProgress(&teams.Items[i]) {
			inProgress++
		}
	}
	return inProgress < m.BatchSize
}

// Step advances the migration state machine by one phase and returns an
// immediate requeue. Each step is idempotent and crash-safe — the phase
// annotation is written last.
func (m *TeamMigrator) Step(ctx context.Context, t *v1beta1.Team) (reconcile.Result, error) {
	logger := ctrl.LoggerFrom(ctx).WithValues("team", t.Name, "migration", "step")

	phase := t.Annotations[v1beta1.AnnotationMigrationPhase]
	var err error

	switch phase {
	case v1beta1.MigrationPhaseNotStarted:
		err = m.stepCreateWorkerCRs(ctx, t)
	case v1beta1.MigrationPhaseWorkerCRsCreated:
		err = m.stepSeedStatus(ctx, t)
	case v1beta1.MigrationPhaseStatusSeeded:
		err = m.stepReparentPods(ctx, t)
	case v1beta1.MigrationPhasePodsReparented:
		err = m.stepInjectCoordination(ctx, t)
	case v1beta1.MigrationPhaseCoordinationInjected:
		err = m.stepPatchTeamSpec(ctx, t)
	case v1beta1.MigrationPhaseTeamSpecPatched:
		err = m.stepFinalize(ctx, t)
	default:
		// Already migrated or unknown phase
		return reconcile.Result{}, nil
	}

	if err != nil {
		logger.Error(err, "migration step failed", "phase", phase)
		hiclawmetrics.MigrationFailures.WithLabelValues(phase).Inc()
		if m.Recorder != nil {
			m.Recorder.Eventf(t, corev1.EventTypeWarning, "MigrationPhaseFailed",
				"Migration failed at phase %q: %v", phase, err)
		}
		return reconcile.Result{RequeueAfter: 30 * time.Second}, err
	}

	return reconcile.Result{Requeue: true}, nil
}

// stepCreateWorkerCRs creates standalone Worker CRs for each Team member using
// the existing projection functions. Uses Get-before-Create for idempotency.
func (m *TeamMigrator) stepCreateWorkerCRs(ctx context.Context, t *v1beta1.Team) error {
	// Add migration finalizer to prevent Team deletion mid-migration
	if !controllerutil.ContainsFinalizer(t, v1beta1.FinalizerMigration) {
		controllerutil.AddFinalizer(t, v1beta1.FinalizerMigration)
		if err := m.Client.Update(ctx, t); err != nil {
			return fmt.Errorf("add migration finalizer: %w", err)
		}
	}

	// Create Worker CR for leader
	leaderSpec := leaderWorkerSpecForMigration(t)
	if err := m.ensureWorkerCR(ctx, t, t.Spec.Leader.Name, "team_leader", leaderSpec); err != nil {
		return fmt.Errorf("create leader Worker CR %q: %w", t.Spec.Leader.Name, err)
	}

	// Create Worker CRs for each team worker
	for _, w := range t.Spec.Workers {
		workerSpec := teamWorkerSpecToWorkerSpecForMigration(t, w)
		if err := m.ensureWorkerCR(ctx, t, w.Name, "worker", workerSpec); err != nil {
			return fmt.Errorf("create worker Worker CR %q: %w", w.Name, err)
		}
	}

	return m.setPhase(ctx, t, v1beta1.MigrationPhaseWorkerCRsCreated)
}

// stepSeedStatus populates Worker CR status from Team.Status.Members so that
// WorkerReconciler takes the "refresh" path (no re-provisioning).
func (m *TeamMigrator) stepSeedStatus(ctx context.Context, t *v1beta1.Team) error {
	memberMap := make(map[string]v1beta1.TeamMemberStatus, len(t.Status.Members))
	for _, ms := range t.Status.Members {
		memberMap[ms.Name] = ms
	}

	// Seed leader status
	if ms, ok := memberMap[t.Spec.Leader.Name]; ok {
		if err := m.seedWorkerStatus(ctx, t.Namespace, t.Spec.Leader.Name, ms); err != nil {
			return err
		}
	}

	// Seed each worker status
	for _, w := range t.Spec.Workers {
		if ms, ok := memberMap[w.Name]; ok {
			if err := m.seedWorkerStatus(ctx, t.Namespace, w.Name, ms); err != nil {
				return err
			}
		}
	}

	return m.setPhase(ctx, t, v1beta1.MigrationPhaseStatusSeeded)
}

// stepReparentPods changes Pod ownerReferences from Team CR to Worker CR.
// Only applies to K8s backend; Docker mode skips this step.
func (m *TeamMigrator) stepReparentPods(ctx context.Context, t *v1beta1.Team) error {
	if m.Backend == nil {
		// No backend registry: skip Pod reparenting
		return m.setPhase(ctx, t, v1beta1.MigrationPhasePodsReparented)
	}
	wb := m.Backend.DetectWorkerBackend(ctx)
	if wb == nil || wb.Name() != "k8s" {
		// Non-K8s backend: skip Pod reparenting
		return m.setPhase(ctx, t, v1beta1.MigrationPhasePodsReparented)
	}

	// Reparent leader Pod
	if err := m.reparentPod(ctx, t, t.Spec.Leader.Name); err != nil {
		return err
	}

	// Reparent worker Pods
	for _, w := range t.Spec.Workers {
		if err := m.reparentPod(ctx, t, w.Name); err != nil {
			return err
		}
	}

	return m.setPhase(ctx, t, v1beta1.MigrationPhasePodsReparented)
}

// stepInjectCoordination injects coordination context into each member via the
// Deployer, ensuring the agents are aware of team structure before spec switch.
func (m *TeamMigrator) stepInjectCoordination(ctx context.Context, t *v1beta1.Team) error {
	leaderRuntimeName := t.Spec.Leader.EffectiveWorkerName()
	teamRuntimeName := t.Spec.EffectiveTeamName(t.Name)

	// Build worker entries for leader coordination
	teamWorkerEntries := make([]service.TeamWorkerEntry, 0, len(t.Spec.Workers))
	for _, ms := range t.Status.Members {
		if ms.Name == t.Spec.Leader.Name {
			continue
		}
		teamWorkerEntries = append(teamWorkerEntries, service.TeamWorkerEntry{
			Name:   ms.RuntimeName,
			RoomID: ms.RoomID,
		})
	}

	// Inject leader coordination
	if err := m.Deployer.InjectCoordinationContext(ctx, service.CoordinationDeployRequest{
		LeaderName:     leaderRuntimeName,
		Role:           "team_leader",
		TeamName:       teamRuntimeName,
		TeamRoomID:     t.Status.TeamRoomID,
		LeaderDMRoomID: t.Status.LeaderDMRoomID,
		HeartbeatEvery: t.Spec.HeartbeatEvery,
		TeamWorkers:    teamWorkerEntries,
		TeamAdminID:    teamAdminMatrixID(t),
	}); err != nil {
		return fmt.Errorf("inject leader coordination: %w", err)
	}

	// Inject worker coordination
	for _, w := range t.Spec.Workers {
		runtimeName := w.EffectiveWorkerName()
		if err := m.Deployer.InjectWorkerCoordination(ctx, service.WorkerCoordinationRequest{
			WorkerName:     runtimeName,
			TeamName:       teamRuntimeName,
			TeamLeaderName: leaderRuntimeName,
			TeamAdminID:    teamAdminMatrixID(t),
		}); err != nil {
			return fmt.Errorf("inject worker %q coordination: %w", w.Name, err)
		}
	}

	return m.setPhase(ctx, t, v1beta1.MigrationPhaseCoordinationInjected)
}

// stepPatchTeamSpec writes spec.workerMembers to trigger the decoupled path.
// Legacy spec.leader/spec.workers fields are preserved (removed in v1beta2).
func (m *TeamMigrator) stepPatchTeamSpec(ctx context.Context, t *v1beta1.Team) error {
	patch := client.MergeFrom(t.DeepCopy())

	// Build workerMembers from existing leader + workers
	refs := make([]v1beta1.TeamWorkerRef, 0, 1+len(t.Spec.Workers))
	refs = append(refs, v1beta1.TeamWorkerRef{
		Name: t.Spec.Leader.Name,
		Role: "team_leader",
	})
	for _, w := range t.Spec.Workers {
		refs = append(refs, v1beta1.TeamWorkerRef{
			Name: w.Name,
			Role: "worker",
		})
	}
	t.Spec.WorkerMembers = refs

	if err := m.Client.Patch(ctx, t, patch); err != nil {
		return fmt.Errorf("patch team spec with workerMembers: %w", err)
	}

	return m.setPhase(ctx, t, v1beta1.MigrationPhaseTeamSpecPatched)
}

// stepFinalize marks migration as complete and removes the migration finalizer.
func (m *TeamMigrator) stepFinalize(ctx context.Context, t *v1beta1.Team) error {
	patch := client.MergeFrom(t.DeepCopy())

	if t.Annotations == nil {
		t.Annotations = make(map[string]string)
	}
	t.Annotations[v1beta1.AnnotationMigrationPhase] = v1beta1.MigrationPhaseMigrated
	t.Annotations[v1beta1.AnnotationMigratedAt] = time.Now().UTC().Format(time.RFC3339)
	controllerutil.RemoveFinalizer(t, v1beta1.FinalizerMigration)

	if err := m.Client.Patch(ctx, t, patch); err != nil {
		return fmt.Errorf("finalize migration: %w", err)
	}

	hiclawmetrics.MigrationPhase.WithLabelValues(t.Name, v1beta1.MigrationPhaseMigrated).Set(1)
	if m.Recorder != nil {
		m.Recorder.Event(t, corev1.EventTypeNormal, "MigrationPhaseAdvanced",
			"Migration completed successfully")
	}
	return nil
}

// --- Helpers ---

// ensureWorkerCR creates a Worker CR if it does not already exist.
//
// If a Worker with the same name already exists, it is safe to reuse only when
// it can be proven to be the same migrated member from a previous attempt, or
// when an operator explicitly marked it for adoption and the projected spec
// matches. A bare name match is not enough: legacy Team members did not have
// Worker CR identity, so a pre-existing standalone Worker may be unrelated.
func (m *TeamMigrator) ensureWorkerCR(ctx context.Context, t *v1beta1.Team, name, role string, spec v1beta1.WorkerSpec) error {
	var existing v1beta1.Worker
	key := client.ObjectKey{Name: name, Namespace: t.Namespace}
	if err := m.Client.Get(ctx, key, &existing); err == nil {
		return validateExistingWorkerForMigration(t, &existing, role, spec)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get worker %q: %w", name, err)
	}

	worker := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   t.Namespace,
			Labels:      map[string]string{},
			Annotations: migrationWorkerAnnotations(t, role),
		},
		Spec: spec,
	}
	if t.Labels != nil {
		if v, ok := t.Labels[v1beta1.LabelController]; ok {
			worker.Labels[v1beta1.LabelController] = v
		}
	}
	return m.Client.Create(ctx, worker)
}

func migrationWorkerAnnotations(t *v1beta1.Team, role string) map[string]string {
	return map[string]string{
		v1beta1.AnnotationMigrationOwned:     "true",
		v1beta1.AnnotationMigratedFromTeam:   t.Name,
		v1beta1.AnnotationMigratedMemberRole: role,
	}
}

func validateExistingWorkerForMigration(t *v1beta1.Team, existing *v1beta1.Worker, expectedRole string, expected v1beta1.WorkerSpec) error {
	if existing == nil {
		return fmt.Errorf("existing worker is nil")
	}
	if !reflect.DeepEqual(existing.Spec, expected) {
		return fmt.Errorf("worker %q already exists but spec does not match projected Team member spec", existing.Name)
	}
	ann := existing.Annotations
	if ann[v1beta1.AnnotationMigrationOwned] == "true" &&
		ann[v1beta1.AnnotationMigratedFromTeam] == t.Name &&
		ann[v1beta1.AnnotationMigratedMemberRole] == expectedRole {
		return nil
	}
	if ann[v1beta1.AnnotationAllowMigrationAdopt] == "true" {
		return nil
	}
	return fmt.Errorf("worker %q already exists and is not owned by this migration; delete/rename it or annotate %s=true after verifying it should be adopted",
		existing.Name, v1beta1.AnnotationAllowMigrationAdopt)
}

// seedWorkerStatus patches a Worker's status subresource with data from
// Team.Status.Members, causing WorkerReconciler to take the refresh path.
func (m *TeamMigrator) seedWorkerStatus(ctx context.Context, namespace, name string, ms v1beta1.TeamMemberStatus) error {
	var worker v1beta1.Worker
	key := client.ObjectKey{Name: name, Namespace: namespace}
	if err := m.Client.Get(ctx, key, &worker); err != nil {
		return fmt.Errorf("get worker %q for status seed: %w", name, err)
	}

	// Skip if status already seeded (idempotent)
	if worker.Status.MatrixUserID != "" && worker.Status.RoomID != "" {
		return nil
	}

	patch := client.MergeFrom(worker.DeepCopy())
	worker.Status.MatrixUserID = ms.MatrixUserID
	worker.Status.RoomID = ms.RoomID
	worker.Status.Phase = "Running"

	return m.Client.Status().Patch(ctx, &worker, patch)
}

// reparentPod changes a Pod's controller ownerReference from Team CR to Worker CR.
func (m *TeamMigrator) reparentPod(ctx context.Context, t *v1beta1.Team, memberName string) error {
	// Find the Pod for this member
	var podList corev1.PodList
	if err := m.Client.List(ctx, &podList, client.InNamespace(t.Namespace), client.MatchingLabels{
		"hiclaw.io/worker": memberName,
	}); err != nil {
		return fmt.Errorf("list pods for member %q: %w", memberName, err)
	}

	if len(podList.Items) == 0 {
		// No Pod found — may have been deleted or not yet created; skip
		return nil
	}

	// Get the Worker CR for the new owner
	var worker v1beta1.Worker
	key := client.ObjectKey{Name: memberName, Namespace: t.Namespace}
	if err := m.Client.Get(ctx, key, &worker); err != nil {
		return fmt.Errorf("get worker %q for reparent: %w", memberName, err)
	}

	// Build new ownerReference pointing to Worker CR
	gvk := v1beta1.SchemeGroupVersion.WithKind("Worker")
	newOwnerRef := metav1.OwnerReference{
		APIVersion:         gvk.GroupVersion().String(),
		Kind:               gvk.Kind,
		Name:               worker.Name,
		UID:                worker.UID,
		Controller:         boolPtr(true),
		BlockOwnerDeletion: boolPtr(true),
	}

	for i := range podList.Items {
		pod := &podList.Items[i]

		// Replace ownerReferences using JSON merge patch. Strategic merge
		// merges ownerReferences by identity and would leave the old Team
		// controller reference in place.
		ownerRefs := []metav1.OwnerReference{newOwnerRef}
		patchData, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": ownerRefs,
			},
		})
		if err != nil {
			return fmt.Errorf("marshal reparent patch for pod %q: %w", pod.Name, err)
		}

		if err := m.Client.Patch(ctx, pod, client.RawPatch(types.MergePatchType, patchData)); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("reparent pod %q: %w", pod.Name, err)
		}
	}

	return nil
}

// setPhase updates the migration phase annotation on the Team CR and emits
// a Kubernetes event + Prometheus gauge update.
func (m *TeamMigrator) setPhase(ctx context.Context, t *v1beta1.Team, phase string) error {
	patch := client.MergeFrom(t.DeepCopy())
	if t.Annotations == nil {
		t.Annotations = make(map[string]string)
	}
	t.Annotations[v1beta1.AnnotationMigrationPhase] = phase
	if err := m.Client.Patch(ctx, t, patch); err != nil {
		return err
	}

	hiclawmetrics.MigrationPhase.WithLabelValues(t.Name, phase).Set(1)
	if m.Recorder != nil {
		m.Recorder.Eventf(t, corev1.EventTypeNormal, "MigrationPhaseAdvanced",
			"Migration advanced to phase %q", phase)
	}
	return nil
}

// --- Projection helpers (simplified for migration — no ChannelPolicy merge) ---

// leaderWorkerSpecForMigration projects a LeaderSpec into a standalone WorkerSpec.
// Unlike the runtime leaderWorkerSpec, it does NOT merge ChannelPolicy (that is
// handled by reconcileTeamDecoupled after migration).
func leaderWorkerSpecForMigration(t *v1beta1.Team) v1beta1.WorkerSpec {
	return v1beta1.WorkerSpec{
		Model:         t.Spec.Leader.Model,
		Runtime:       "copaw",
		WorkerName:    t.Spec.Leader.WorkerName,
		Identity:      t.Spec.Leader.Identity,
		Soul:          t.Spec.Leader.Soul,
		Agents:        t.Spec.Leader.Agents,
		Package:       t.Spec.Leader.Package,
		RemoteSkills:  t.Spec.Leader.RemoteSkills,
		McpServers:    t.Spec.Leader.McpServers,
		State:         t.Spec.Leader.State,
		AccessEntries: t.Spec.Leader.AccessEntries,
		Env:           t.Spec.Leader.Env,
		Labels:        t.Spec.Leader.Labels,
	}
}

// teamWorkerSpecToWorkerSpecForMigration projects a TeamWorkerSpec into a standalone
// WorkerSpec. ChannelPolicy is NOT merged (handled by reconcileTeamDecoupled).
func teamWorkerSpecToWorkerSpecForMigration(t *v1beta1.Team, w v1beta1.TeamWorkerSpec) v1beta1.WorkerSpec {
	return v1beta1.WorkerSpec{
		Model:         w.Model,
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
		State:         w.State,
		AccessEntries: w.AccessEntries,
		Env:           w.Env,
		Labels:        w.Labels,
	}
}

// teamAdminMatrixID extracts the admin's MatrixUserID from TeamSpec.Admin.
func teamAdminMatrixID(t *v1beta1.Team) string {
	if t.Spec.Admin != nil {
		return t.Spec.Admin.MatrixUserID
	}
	return ""
}

func boolPtr(b bool) *bool { return &b }
