package controller

import (
	"context"
	"fmt"
	"sort"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/service"
	"github.com/hiclaw/hiclaw-controller/internal/slicesx"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

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
func removeString(values []string, target string) []string {
	filtered := values[:0]
	for _, value := range values {
		if value != target {
			filtered = append(filtered, value)
		}
	}
	return filtered
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
func (r *TeamReconciler) runtimeConfigTeamMembers(t *v1beta1.Team, desiredMembers []MemberContext) []service.RuntimeConfigTeamMember {
	roster := make([]service.RuntimeConfigTeamMember, 0, len(desiredMembers)+len(t.Spec.HumanMembers))
	for _, member := range desiredMembers {
		entry := service.RuntimeConfigTeamMember{
			Name:        member.Name,
			RuntimeName: member.RuntimeName,
			Role:        member.Role.String(),
		}
		if ms := t.Status.MemberByName(member.Name); ms != nil {
			entry.MatrixUserID = ms.MatrixUserID
			entry.PersonalRoomID = ms.RoomID
		}
		if entry.MatrixUserID == "" && r.Provisioner != nil && entry.RuntimeName != "" {
			entry.MatrixUserID = r.Provisioner.MatrixUserID(entry.RuntimeName)
		}
		roster = append(roster, entry)
	}
	for _, human := range t.Spec.HumanMembers {
		role := human.Role
		if role == "" {
			role = "coordinator"
		}
		roster = append(roster, service.RuntimeConfigTeamMember{
			Name:         human.Name,
			Role:         role,
			MatrixUserID: human.MatrixUserID,
		})
	}
	return roster
}
func teamAdminMatrixID(t *v1beta1.Team) string {
	if t.Spec.Admin == nil {
		return ""
	}
	return t.Spec.Admin.MatrixUserID
}
func teamAdminName(t *v1beta1.Team) string {
	if t.Spec.Admin == nil {
		return ""
	}
	return t.Spec.Admin.Name
}
func teamCoordinatorIDs(t *v1beta1.Team) []string {
	ids := make([]string, 0, 1+len(t.Spec.HumanMembers))
	if adminID := teamAdminMatrixID(t); adminID != "" {
		ids = append(ids, adminID)
	}
	for _, member := range t.Spec.HumanMembers {
		if !v1beta1.TeamMemberIsCoordinator(member) {
			continue
		}
		switch {
		case member.MatrixUserID != "":
			ids = append(ids, member.MatrixUserID)
		case member.Name != "":
			ids = append(ids, member.Name)
		}
	}
	return slicesx.UniqueNonEmpty(ids)
}
func (r *TeamReconciler) workerToTeamRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	var teamList v1beta1.TeamList
	if err := r.List(ctx, &teamList,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{TeamWorkerMembersField: obj.GetName()},
	); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(teamList.Items))
	for _, t := range teamList.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: client.ObjectKey{Name: t.Name, Namespace: t.Namespace},
		})
	}
	return reqs
}

// workerStatusChangePredicate triggers only on Worker status subresource
// changes (Phase, MatrixUserID, RoomID) and delete events.
func workerStatusChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldW, ok1 := e.ObjectOld.(*v1beta1.Worker)
			newW, ok2 := e.ObjectNew.(*v1beta1.Worker)
			if !ok1 || !ok2 {
				return false
			}
			return oldW.Generation != newW.Generation ||
				oldW.Status.ObservedGeneration != newW.Status.ObservedGeneration ||
				oldW.Status.Phase != newW.Status.Phase ||
				oldW.Status.MatrixUserID != newW.Status.MatrixUserID ||
				oldW.Status.RoomID != newW.Status.RoomID
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
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
