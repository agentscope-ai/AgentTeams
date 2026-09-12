package controller

import (
	"context"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/matrix"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// reconcileHumanRooms drives Status.Rooms toward the desired set built
// from Spec.AccessibleWorkers / AccessibleTeams. Single declarative
// path — the pre-refactor controller had separate handleCreate /
// handleUpdate branches; those are unified here.
//
// Additions: invite via admin (always), then /join as the user (only
// when a user access token is obtainable — see ensureUserToken). When
// the user token cannot be obtained (login failed / stale password), we
// skip /join and record the invite as pending by not appending to
// Status.Rooms; a later reconcile will retry once the user re-establishes
// a password we know about.
//
// Removals: kick via admin. On failure, keep the room in Status.Rooms
// so the next reconcile retries rather than dropping state.
//
// Errors from individual invite/join/kick operations are never returned
// — the function always runs to completion so partial failures don't
// block unrelated rooms.
func (r *HumanReconciler) reconcileHumanRooms(ctx context.Context, s *humanScope) {
	logger := log.FromContext(ctx)
	h := s.human

	desired := buildDesiredHumanRooms(ctx, r.Client, h)

	observed := make(map[string]struct{}, len(h.Status.Rooms))
	for _, rid := range h.Status.Rooms {
		observed[rid] = struct{}{}
	}

	matrixUserID := h.Status.MatrixUserID
	if matrixUserID == "" {
		matrixUserID = s.identity.MatrixUserID
	}

	// Start with currently-observed rooms; we'll prune removals below.
	next := make([]string, 0, len(h.Status.Rooms)+len(desired))
	next = append(next, h.Status.Rooms...)

	powerLevel := humanRoomPowerLevel(h.Spec.PermissionLevel)

	for rid, origin := range desired {
		alreadyMember := false
		if _, ok := observed[rid]; ok {
			alreadyMember = true
		}
		if !alreadyMember {
			if err := r.Provisioner.InviteToRoom(ctx, rid, matrixUserID); err != nil {
				logger.Error(err, "failed to invite human to room", "room", rid)
				continue
			}
			// Acquire a user token lazily — only on the first new-room
			// addition of this reconcile. Steady-state passes (desired ==
			// observed) and revoke-only passes never reach this call, so
			// Matrix Login is not issued on every 5-minute requeue.
			token := r.ensureUserToken(ctx, s)
			if token == "" {
				logger.V(1).Info("user token unavailable; invite-only this cycle",
					"room", rid, "human", h.Name, "username", s.username)
				continue
			}
			if err := r.Provisioner.JoinRoomAs(ctx, rid, token); err != nil {
				logger.Error(err, "failed to join room as human", "room", rid)
				continue
			}
			next = append(next, rid)
		}
		// Grant the human their power level in every room they should be
		// in — new rooms and already-observed ones alike. The existing-room
		// pass is the healing path: legacy rooms were created before power
		// levels accounted for human members, leaving them at the implicit
		// level 0 and 403 on room operations (rename, invite). Non-fatal
		// per this file's error policy; the next cycle retries.
		//
		// The grant must run as an actor actually authorized in the room:
		// TeamAdmin-owned rooms do not include the homeserver admin, so
		// the default admin identity would be rejected with M_FORBIDDEN.
		actorToken := r.roomActorToken(ctx, origin, h.Namespace)
		grantErr := r.Provisioner.EnsureRoomPowerLevel(ctx, rid, matrixUserID, powerLevel, actorToken, "")
		if matrix.IsForbidden(grantErr) {
			// An M_FORBIDDEN on the actor write is the homeserver's
			// strict-greater rule: the actor's level is not above the
			// human's CURRENT level, i.e. an equal-level demotion (a
			// former L1 human at 100 being lowered to 50). The sender's
			// OWN entry is exempt from that rule, so retry with the
			// human's own token — lazily, so steady-state cycles never
			// issue a Matrix Login.
			if token := r.ensureUserToken(ctx, s); token != "" {
				if retryErr := r.Provisioner.EnsureRoomPowerLevel(ctx, rid, matrixUserID, powerLevel, actorToken, token); retryErr == nil {
					grantErr = nil
				} else {
					logger.Error(retryErr, "failed to ensure human power level via self-write", "room", rid, "level", powerLevel)
				}
			}
		}
		if grantErr != nil {
			logger.Error(grantErr, "failed to ensure human power level", "room", rid, "level", powerLevel)
		}
	}

	// Removals: in-place filter. A failed revocation keeps the room so the
	// next reconcile retries, matching pre-refactor behavior.
	//
	// Revocation chain — each stage covers what the previous cannot:
	//  1. kick as the homeserver admin: works for rooms the admin is a
	//     member of when the target's level is below the admin's;
	//  2. self-leave with the human's own token: always authorized for a
	//     joined member regardless of power levels (spec room-auth rule:
	//     a user may leave their own room) — the only in-band revocation
	//     for an equal-level (100) human, and the only one that works in
	//     rooms the admin is not in (TeamAdmin-owned rooms);
	//  3. the Tuwunel admin bot force-leave: last resort when the human
	//     token is unavailable (stale password). Like the team-reconcile
	//     usage, a confirmed command delivery is treated as resolved.
	deferUnknown, knownRoomIDs := teamRoomRevocationLag(ctx, r.Client, h)
	kept := next[:0]
	for _, rid := range next {
		if _, ok := desired[rid]; ok {
			kept = append(kept, rid)
			continue
		}
		if deferUnknown {
			if _, known := knownRoomIDs[rid]; !known {
				// Origin unresolved while the human holds a team membership
				// claim whose room is not yet visible: this may be that
				// team's brand-new room (status-lag window right after
				// team provisioning). Defer the kick to the next cycle
				// instead of evicting the team admin from their own team
				// room — that deadlock would surface as join-403 on every
				// later team reconcile (CI test-19).
				logger.V(1).Info("deferring kick: room origin unresolved, team room claim pending", "room", rid)
				kept = append(kept, rid)
				continue
			}
		}
		if err := r.Provisioner.KickFromRoom(ctx, rid, matrixUserID, "access revoked"); err == nil {
			continue // kicked, or the user was already out
		} else {
			logger.V(1).Info("admin kick rejected; trying revocation fallbacks", "room", rid, "err", err.Error())
		}
		removed := false
		if token := r.ensureUserToken(ctx, s); token != "" {
			if lerr := r.Provisioner.LeaveRoomAs(ctx, rid, token); lerr == nil {
				removed = true
			} else {
				logger.Error(lerr, "self-leave failed", "room", rid)
			}
		}
		if !removed {
			if ferr := r.Provisioner.ForceLeaveRoom(ctx, matrixUserID, rid); ferr == nil {
				removed = true
			} else {
				logger.Error(ferr, "force-leave failed", "room", rid)
			}
		}
		if !removed {
			kept = append(kept, rid) // keep for the next cycle's retry
		}
	}

	h.Status.Rooms = kept
}

// roomActorToken returns the access token of the actor authorized to read
// and write state in the room described by origin. Teams with a TeamAdmin
// configured own their team room (the homeserver admin is deliberately not
// a member), so grants there must run as that admin; every other room
// (worker DM rooms, teams without an admin) keeps the default
// homeserver-admin actor (""). An unavailable actor (admin human not
// provisioned, login failed) degrades to the default — the grant then 403s
// and is retried next cycle rather than silently skipping.
func (r *HumanReconciler) roomActorToken(ctx context.Context, origin humanRoomOrigin, namespace string) string {
	if origin.teamName == "" {
		return ""
	}
	var team v1beta1.Team
	if err := r.Client.Get(ctx, client.ObjectKey{Name: origin.teamName, Namespace: namespace}, &team); err != nil {
		return ""
	}
	if team.Spec.Admin == nil {
		return ""
	}
	actor, err := resolveTeamAdminActor(ctx, r.Client, r.Provisioner, &team)
	if err != nil {
		log.FromContext(ctx).V(1).Info("team admin actor unavailable; power grant uses the default admin and may be rejected",
			"team", origin.teamName, "err", err.Error())
		return ""
	}
	return actor.Token
}

// humanRoomPowerLevel maps the Human CR permission level to the Matrix power
// level granted in rooms the human belongs to. Level 1 (admin equivalent)
// co-owns the rooms (full control); levels 2/3 (team/worker scoped) get
// level 50 — Matrix's default member authority: rename, invite, kick, ban
// and redact (the homeserver defaults all sit at 50), but not power-level
// changes, and only against members strictly below 50 — the manager/leader
// at 100 can never be kicked or banned by the human. This authority is
// accepted and documented in docs/design/room-power-levels.md.
func humanRoomPowerLevel(permissionLevel int) int {
	if permissionLevel == 1 {
		return 100
	}
	return 50
}

// ensureUserToken returns a Matrix access token for the human,
// acquiring one via Login on first call per reconcile and caching it
// in the scope. Returns "" when login fails — callers degrade to
// admin-only invite behavior without surfacing the error (stale
// passwords are an expected condition, not a reconcile failure).
//
// The lazy acquisition is critical: every Login call creates a new
// device session on Tuwunel (the homeserver does not reuse sessions
// when the caller omits device_id), so issuing a Login on every
// 5-minute requeue would accumulate ~288 orphan devices per human per
// day. By gating the Login behind "we actually have a new room to
// /join", a Human whose spec is quiescent triggers zero Logins
// regardless of requeue cadence.
func (r *HumanReconciler) ensureUserToken(ctx context.Context, s *humanScope) string {
	if s.userToken != "" {
		return s.userToken
	}
	// Fresh provisioning set userToken directly; only steady-state
	// reconciles fall through to here. Without a stored password we
	// cannot log in, so return empty and let the caller fall back to
	// admin-only invite.
	if s.identity.Source == nil {
		return ""
	}
	if s.identity.ManagesInitialPassword && !r.Provisioner.MatrixAppServiceEnabled() && s.human.Status.InitialPassword == "" {
		return ""
	}

	token, err := s.identity.Source.EnsureUserToken(ctx, &s.human.Spec, &s.human.Status, s.human.Name)
	if err != nil {
		log.FromContext(ctx).Info("human login with stored password failed; continuing with admin-only room management",
			"name", s.human.Name, "err", err.Error())
		return ""
	}
	s.userToken = token
	return token
}
