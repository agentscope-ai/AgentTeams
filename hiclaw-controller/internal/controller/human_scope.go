package controller

import (
	"context"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// humanScope carries cross-phase state for a single Reconcile pass.
// Fields set by earlier phases are consumed by later phases; keeping them
// in one place avoids threading extra return values through every phase
// function and mirrors the scope pattern used by managerScope.
type humanScope struct {
	human     *v1beta1.Human
	patchBase client.Patch

	// userToken is the Human's own Matrix access token for this reconcile
	// pass, obtained either from EnsureHumanUser (first-time) or
	// LoginAsHuman (steady-state). Empty when login failed (e.g. the user
	// changed their password in Element); rooms phase then degrades to
	// admin-only invite without /join.
	userToken string
}

// computeHumanPhase derives the observable Phase from reconcile outcome.
//
// Behavior matches the pre-refactor controller:
//   - success → "Active" once MatrixUserID is set; otherwise "Pending"
//   - error → "Failed" when no user has been provisioned yet
//     (reconcile is stuck before it can report a real state), or the
//     previous Phase otherwise (transient errors keep us in "Active").
func computeHumanPhase(h *v1beta1.Human, reconcileErr error) string {
	if reconcileErr != nil {
		if h.Status.MatrixUserID == "" {
			return "Failed"
		}
		if h.Status.Phase == "" {
			return "Pending"
		}
		return h.Status.Phase
	}
	if h.Status.MatrixUserID == "" {
		return "Pending"
	}
	return "Active"
}

// buildDesiredHumanRooms resolves Spec.AccessibleWorkers / AccessibleTeams
// into the set of Matrix room IDs the human should currently be a member
// of. Workers/Teams that don't exist or haven't finished provisioning
// (empty Status.RoomID / TeamRoomID) are simply skipped — they'll be
// picked up on a later reconcile once their rooms materialize.
//
// Returned as a set (map-to-empty-struct) rather than a slice because
// the reconciler does membership comparisons against the observed
// Status.Rooms set.
func buildDesiredHumanRooms(ctx context.Context, c client.Client, h *v1beta1.Human) map[string]struct{} {
	desired := make(map[string]struct{})

	// Handle workers (standalone + team)
	for _, workerName := range h.Spec.AccessibleWorkers {
		// Check standalone worker first
		var worker v1beta1.Worker
		if err := c.Get(ctx, client.ObjectKey{Name: workerName, Namespace: h.Namespace}, &worker); err == nil {
			if worker.Status.RoomID != "" {
				desired[worker.Status.RoomID] = struct{}{}
			}
		}
		// Also check if this is a team worker
		if roomID := findTeamWorkerRoomID(ctx, c, h.Namespace, workerName); roomID != "" {
			desired[roomID] = struct{}{}
		}
	}

	// Handle teams
	for _, teamName := range h.Spec.AccessibleTeams {
		var team v1beta1.Team
		if err := c.Get(ctx, client.ObjectKey{Name: teamName, Namespace: h.Namespace}, &team); err != nil {
			continue
		}
		if team.Status.TeamRoomID != "" {
			desired[team.Status.TeamRoomID] = struct{}{}
		}
	}
	return desired
}

// findTeamWorkerRoomID looks up a team worker's room ID from Team.Status.Members
func findTeamWorkerRoomID(ctx context.Context, c client.Client, ns, workerName string) string {
	var teamList v1beta1.TeamList
	if err := c.List(ctx, &teamList, client.InNamespace(ns)); err != nil {
		return ""
	}
	for _, t := range teamList.Items {
		for _, member := range t.Status.Members {
			if member.Name == workerName {
				return member.RoomID
			}
		}
	}
	return ""
}
