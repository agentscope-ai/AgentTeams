package controller

import (
	"context"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/controller/humanidentity"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// humanScope carries cross-phase state for a single Reconcile pass.
// Fields set by earlier phases are consumed by later phases; keeping them
// in one place avoids threading extra return values through every phase
// function and mirrors the scope pattern used by managerScope.
type humanScope struct {
	human     *v1beta1.Human
	username  string
	patchBase client.Patch
	identity  humanidentity.ResolvedIdentity

	// userToken is the Human's own Matrix access token for this reconcile
	// pass, obtained either from the identity source's EnsurePrecreated
	// (first-time) or EnsureUserToken (steady-state, e.g. LoginWithPassword
	// for legacy_password). Empty when login failed (e.g. the user changed
	// their password in Element); rooms phase then degrades to admin-only
	// invite without /join.
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
	if h.Status.Phase == "Degraded" && h.Status.Message != "" {
		return "Degraded"
	}
	if reconcileErr != nil {
		if h.Status.Phase == "Degraded" {
			return "Degraded"
		}
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

// humanRoomOrigin records where a desired room came from so the room phase
// can pick an actor authorized to write that room's state: a TeamAdmin
// owns the team room of a team that configures an Admin (the homeserver
// admin is deliberately not a member of those rooms), while worker DM
// rooms and teams without an Admin keep the default homeserver-admin actor.
type humanRoomOrigin struct {
	teamName   string // non-empty for team rooms (team.Status.TeamRoomID)
	workerName string // non-empty for worker DM rooms (worker.Status.RoomID)
}

// buildDesiredHumanRooms resolves Spec.AccessibleWorkers / AccessibleTeams
// plus team membership (spec.admin / spec.humanMembers) into the set of
// Matrix room IDs the human should currently be a member of, annotated
// with each room's origin. Workers/Teams that don't exist or
// haven't finished provisioning (empty Status.RoomID / TeamRoomID) are
// simply skipped — they'll be picked up on a later reconcile once their
// rooms materialize.
//
// Returned as a map (roomID -> origin) rather than a slice because the
// reconciler does membership comparisons against the observed Status.Rooms
// set and needs the origin to choose the room-state actor.
func buildDesiredHumanRooms(ctx context.Context, c client.Client, h *v1beta1.Human) map[string]humanRoomOrigin {
	desired := make(map[string]humanRoomOrigin)
	for _, workerName := range h.Spec.AccessibleWorkers {
		var worker v1beta1.Worker
		if err := c.Get(ctx, client.ObjectKey{Name: workerName, Namespace: h.Namespace}, &worker); err != nil {
			continue
		}
		if worker.Status.RoomID != "" {
			desired[worker.Status.RoomID] = humanRoomOrigin{workerName: workerName}
		}
	}
	// Team rooms: a human belongs to a team's room when the team names
	// them (spec.admin / spec.humanMembers) or the human names the team
	// (spec.accessibleTeams). The team-membership leg is load-bearing:
	// syncTeamRoomHumanStatuses writes the team room into the admin's and
	// human members' Status.Rooms WITHOUT touching their AccessibleTeams,
	// so a desired set built from AccessibleTeams alone would let the
	// access-revocation path kick the team admin out of their own team
	// room — and, because the admin is deliberately excluded from the
	// team-room invite list (creator-join design in ProvisionTeamRooms),
	// the team would then fail on join (M_FORBIDDEN: cannot join a room
	// that is not public) on every reconcile.
	var teams v1beta1.TeamList
	if err := c.List(ctx, &teams, client.InNamespace(h.Namespace)); err == nil {
		for i := range teams.Items {
			tm := &teams.Items[i]
			if tm.Status.TeamRoomID == "" {
				continue
			}
			belongs := containsString(h.Spec.AccessibleTeams, tm.Name)
			if !belongs && tm.Spec.Admin != nil && tm.Spec.Admin.Name == h.Name {
				belongs = true
			}
			if !belongs {
				for _, m := range tm.Spec.HumanMembers {
					if m.Name == h.Name || (m.MatrixUserID != "" && m.MatrixUserID == h.Status.MatrixUserID) {
						belongs = true
						break
					}
				}
			}
			if belongs {
				desired[tm.Status.TeamRoomID] = humanRoomOrigin{teamName: tm.Name}
			}
		}
	}
	return desired
}

// teamRoomRevocationLag reports whether the revocation path must defer
// kicks of rooms whose origin cannot currently be resolved, and returns
// the set of room IDs currently visible in the cache (team
// Status.TeamRoomID + worker Status.RoomID).
//
// Right after team provisioning, syncTeamRoomHumanStatuses writes the new
// team room into the admin's / human members' Status.Rooms BEFORE the
// team's Status.TeamRoomID is visible in this reconciler's cache
// (informer lag across objects). While that window is open, a room in
// Status.Rooms that no visible Team/Worker claims has an UNKNOWN origin:
// it is the new team's room, not an orphan. Kicking it would evict the
// team admin from their own team room — and, because the admin is
// deliberately excluded from the team-room invite list (creator-join
// design in ProvisionTeamRooms), every later team reconcile would then
// fail on join (M_FORBIDDEN: cannot join a room that is not public): a
// permanent deadlock (CI test-19). So while the human holds a team
// membership claim (spec.admin / spec.humanMembers) against a team whose
// room is not yet visible, unknown-origin rooms are kept for one more
// cycle. By then the team status is visible and the origin resolves:
// still belonging -> the room is desired (kept); genuinely revoked ->
// kicked as usual. Known-origin rooms (visible team/worker rooms the
// human no longer belongs to) are kicked immediately, even inside the
// window, so access revocation stays prompt.
func teamRoomRevocationLag(ctx context.Context, c client.Client, h *v1beta1.Human) (deferUnknown bool, knownRoomIDs map[string]struct{}) {
	knownRoomIDs = make(map[string]struct{})
	var teams v1beta1.TeamList
	if err := c.List(ctx, &teams, client.InNamespace(h.Namespace)); err != nil {
		return false, knownRoomIDs
	}
	unresolvedClaim := false
	for i := range teams.Items {
		tm := &teams.Items[i]
		if tm.Status.TeamRoomID != "" {
			knownRoomIDs[tm.Status.TeamRoomID] = struct{}{}
			continue
		}
		if tm.Spec.Admin != nil && tm.Spec.Admin.Name == h.Name {
			unresolvedClaim = true
		} else {
			for _, m := range tm.Spec.HumanMembers {
				if m.Name == h.Name || (m.MatrixUserID != "" && m.MatrixUserID == h.Status.MatrixUserID) {
					unresolvedClaim = true
					break
				}
			}
		}
	}
	var workers v1beta1.WorkerList
	if err := c.List(ctx, &workers, client.InNamespace(h.Namespace)); err == nil {
		for i := range workers.Items {
			if workers.Items[i].Status.RoomID != "" {
				knownRoomIDs[workers.Items[i].Status.RoomID] = struct{}{}
			}
		}
	}
	return unresolvedClaim, knownRoomIDs
}
