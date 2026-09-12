# Room Power Levels for Human Members

Status: implemented
Surfaces: `m.room.power_levels` in team / worker / project rooms; `create-project.sh --grant-admin`

## Problem

Humans are invited into worker and team rooms by the human reconciler and
into project rooms by the Manager — but nothing ever grants them a Matrix
power level. Two consequences:

1. **Rooms created before power levels accounted for humans** (and rooms
   where the human joined later) carry a `m.room.power_levels` state that
   lists only manager / leader / admin at 100 and workers at 0. The human
   sits at the implicit level 0.
2. **Rooms that never had the state set at all** (legacy) fall back to the
   homeserver's strict defaults.

Either way, a human operator gets `403` on every room operation — renaming
the room, inviting a colleague, even housekeeping. The room works for the
bots that own it and is unusable for the person it is meant for.

## Design

**Declarative grant in the human room reconcile.** The human reconciler
already walks the full desired room set on every cycle (new rooms: invite +
join; observed rooms: skip). It now additionally ensures the human's power
level in *every* desired room — new and already-observed. The
observed-room pass is the healing path: legacy rooms are fixed on the first
reconcile after deployment without any manual backfill.

- Level mapping (`humanRoomPowerLevel`): `permissionLevel 1` → 100
  (co-owner: full room control, matching the admin-equivalent scope);
  levels 2/3 → 50 (Matrix's default member authority).
- **Level-50 authority — explicitly accepted.** Matrix homeserver defaults
  gate `kick`, `ban`, and `redact` at 50, so an L2/L3 human at level 50 can
  rename the room, invite members, kick/ban members strictly below 50
  (the workers at 0 — but never the manager/leader/admin at 100), and
  redact any message in the room. They cannot change power levels (100) or
  create the room. We accept this authority rather than raising the
  `kick`/`ban`/`redact` thresholds to 100:
  - The humans at 50 are operators scoped to those rooms; the room's
    manager sits at 100 above them, so kick/ban cannot be turned against
    the team's control plane.
  - Workers are service accounts; a human kicking/banning a stuck worker
    is reversible housekeeping (the reconciler re-invites membership on
    the next cycle) and is a useful operator lever, not a security
    escalation.
  - Redact at 50 is message cleanup within a room the human is already a
    member of; the alternative (raising thresholds in every room-creation
    path) would change the security posture of all worker/team/project
    rooms — a system-wide policy change out of scope for this PR.
- The grant is a **merge**, never a replace: `Provisioner.
  EnsureRoomPowerLevel` reads the current `m.room.power_levels`
  (`matrix.Client.GetRoomState`, new — actor token with admin fallback, 404 → empty state),
  adds/raises the human's entry in `users`, preserves every other user and
  every non-user setting (`users_default`, `state_default`, `ban`, …), and
  writes back only when the level actually changed. Steady state = one GET
  per room per cycle, zero writes.
- Errors are non-fatal per the reconcile's existing error policy: a failed
  grant is logged and retried on the next cycle; the room is still recorded
  in `status.rooms`.

**Project rooms.** The Controller never creates project rooms; the Manager
does, via `create-project.sh`, which already writes a
`power_level_content_override` (manager + admin at 100, workers at 0) but
has no way to lift a human operator. New optional flag:

```
create-project.sh --id p1 --title T --workers w1,w2 --grant-admin luo,sunzong
```

`--grant-admin` accepts local parts or full Matrix IDs and adds each user at
level 100 to the creation-time override. Rooms created before this change
are healed one-time by the Manager (a `PUT m.room.power_levels` per room) —
a one-off operations task, not part of this PR.

## Authorization: actor selection and the equal-level deadlock

The homeserver enforces room-auth rules that a plain "write with the admin
token" cannot assume away (spec v8 rules; the fake in
`provisioner_team_test.go` enforces the same rules, so tests prove the
controller survives a real homeserver):

- **Strict-greater on other users' entries (rule 9.6).** A sender may
  change or remove another user's `users` entry only if the sender's level
  is **strictly greater** than the target's CURRENT level. A sender at 100
  therefore cannot demote a former L1 human who sits at 100. The sender's
  OWN entry is exempt — a self-demotion is always authorized (downward).
  On room version 12+ the creator holds an infinite level and is never
  blocked; production rooms are v1–11, where the deadlock is real.
- **Kicks (rule 4.5.4).** The kicker needs at least the `kick` level
  (50) AND the target's level strictly below the kicker's — an equal-power
  kick is rejected. A user may always leave their own room (4.5.1).
- **Membership-scoped state access.** Reading or writing room state as a
  non-member is rejected — including the homeserver admin in
  **TeamAdmin-owned rooms**, where `ProvisionTeamRooms` deliberately
  creates and reconciles as the TeamAdmin and leaves the admin out.

Consequences implemented by this PR:

1. **Actor selection.** `GetRoomState` / `SetRoomState` accept an explicit
   token ("" = homeserver admin, as before). The human reconciler
   annotates each desired room with its origin; a team room of a team with
   `spec.admin` is granted as that TeamAdmin (token resolved from the
   admin Human via the shared `resolveTeamAdminActor`), every other room
   keeps the default admin actor.
2. **Equal-level demotion → self-write fallback.** `EnsureRoomPowerLevel`
   writes as the actor; on `M_FORBIDDEN` it retries with the human's own
   token (`selfToken`), whose own entry is exempt from 9.6. The reconciler
   fetches that token **lazily** (only after the 403), so steady-state
   cycles still issue no Matrix Login.
3. **Revocation chain** (removal from the desired set), each stage covering
   what the previous cannot:
   1. kick as the homeserver admin (rooms it is in, target below 100);
   2. **self-leave** with the human's own token (always authorized, any
      room, any level — the only in-band path for an equal-level 100);
   3. the Tuwunel admin-bot force-leave (token unavailable / stale
      password). A confirmed command delivery is treated as resolved,
      matching the team-reconcile convention.
4. **Kick idempotency fix.** `KickFromRoomWithToken` used to swallow a
   403 `cannot kick ...` as success, which made an equal-power kick look
   like a removal: the room was dropped from `status.rooms` while the
   user stayed in it. Only a 404 / "not in room" answer is idempotent;
   every other 403 is returned as a decodable `M_FORBIDDEN`
   (`matrix.APIError` / `matrix.IsForbidden`) so callers can fall back.
5. **Login-token cache (steady-state logins).** `TuwunelClient` caches
   `/login` access tokens per user for 30 minutes (password and
   AppService-impersonation logins alike), so the per-cycle TeamAdmin
   actor resolution — and the human's own token resolution — issue no
   Matrix Login in steady state. In-band token invalidators clear the
   entry: password reset (orphan recovery, `SetPasswordAsAdmin`) and
   account deactivation; out-of-band invalidation (server-side revoke,
   logout-everywhere) self-heals on TTL expiry. Logins that double as an
   account-liveness check (the existing-account fallbacks in
   `EnsureUser` / `EnsureAppServiceUser`, which drive orphan recovery)
   always go to the homeserver, so a cached dead token can never
   short-circuit the recovery flow.
6. **The human desired-room set recognizes team membership.**
   `buildDesiredHumanRooms` includes, beyond `spec.accessibleTeams`, the
   team rooms of teams where the human is `spec.admin` or appears in
   `spec.humanMembers`. Load-bearing: `syncTeamRoomHumanStatuses` (team
   reconciler) writes the team room into the admin's / members'
   `status.rooms` WITHOUT touching their `spec.accessibleTeams`; a
   human-side desired set built from `accessibleTeams` alone would let
   the access-revocation path kick the team admin out of their own team
   room, and the team would then fail on join
   (`M_FORBIDDEN: cannot join a room that is not public` — the admin is
   deliberately excluded from the team-room invite list by the
   creator-join design) on every reconcile: a permanent deadlock.
   Regression tests: `TestHumanReconciler_TeamAdminRoomNotRevoked` /
   `TestHumanReconciler_HumanMemberRoomNotRevoked`.
7. **The revocation path never kicks a room whose origin it cannot
   resolve while a team membership claim is pending.** Item 6 closes the
   steady-state case (room visible in the team status). A residual
   status-lag window remained: right after team provisioning,
   `syncTeamRoomHumanStatuses` writes the new team room into the admin's
   `status.rooms` BEFORE the team's `status.teamRoomID` is visible in the
   human reconciler's cache (informer lag across objects). In that window
   the room is in `status.rooms` but no visible Team/Worker claims it —
   an UNKNOWN origin — and the revocation path kicked it anyway,
   evicting the team admin from their own team room; every later team
   reconcile then failed on join (`M_FORBIDDEN: cannot join a room that
   is not public`) until the 180s test-19 timeout (CI SHARD_C 4/5).
   `teamRoomRevocationLag` detects the window (human is `spec.admin` /
   `spec.humanMembers` of a team whose room is not yet visible) and
   defers the kick of unknown-origin rooms for one cycle — by then the
   team status is visible and the origin resolves: still belonging →
   desired (kept), genuinely revoked → kicked. Known-origin revocations
   (visible team/worker rooms the human no longer belongs to) stay
   prompt. Regression tests:
   `TestHumanReconciler_RevocationDeferredWhileTeamRoomUnresolved` /
   `TestHumanReconciler_RevocationProceedsForKnownOriginTeamRoom`.

Known limitations (documented, all non-fatal / retry or documented-stuck):

1. **A level-100 human whose Matrix password is unavailable cannot be
   demoted.** The actor write is rejected (9.6, equal level) and there is
   no self token, so the demotion is retried every cycle without effect.
   Matrix provides no out-of-band equal-level demotion. *Removal* from
   the room is unaffected (admin-bot force-leave still works).
2. **Removing `spec.admin` from a team whose room already exists** leaves
   that room owned by the former TeamAdmin (the homeserver admin is not a
   member): actor selection falls back to the admin identity, the grant
   403s, and is retried every cycle without effect. Revocation is
   unaffected (self-leave / force-leave still work).
3. **The revocation chain starts from a homeserver-admin kick.** A
   removed room is by definition no longer in the desired set, and its
   origin is not recorded in `status`, so an actor-scoped kick
   (`KickFromRoomAs`) cannot be chosen yet; it is in place for when
   origin tracking lands in status.

## What is not changed

- Worker / team / DM room creation keeps its existing power levels
  (manager / admin / leader at 100, workers at 0).
- Worker service accounts still cannot manage rooms (level 0 unchanged).
- No CRD change; the mapping is derived from the existing
  `spec.permissionLevel`.

## Tests

- `internal/matrix/client_test.go` — `TestGetRoomState`: returns the state
  **content** (not the event envelope) with the admin token; missing state
  → `(nil, nil)`, not an error. `TestGetRoomState_WithUserToken` (explicit
  token authenticates as that token, not the admin),
  `TestGetRoomState_Forbidden` / `TestSetRoomState_Forbidden` (non-member /
  rejected writes surface a decodable `M_FORBIDDEN` via
  `matrix.IsForbidden`), `TestKickFromRoom_EqualPowerForbidden` (403
  `cannot kick` is an error, not a silent success — the old swallowing
  branch is gone), `TestLeaveRoom_IdempotentNotFound`,
  `TestLogin_TokenCachedPerUser` (per-user cache: second login is served
  from cache, no HTTP; different user still goes to the homeserver),
  `TestLogin_TokenCacheExpires` (TTL expiry → fresh login),
  `TestLogin_AppServiceTokenCached`, `TestInvalidateUserToken_FreshLogin`,
  `TestEnsureUser_OrphanRecovery_IgnoresStaleCachedToken` (a stale cached
  token does NOT short-circuit orphan recovery — liveness-check logins
  bypass the cache and the reset-password flow completes).
- `internal/service/provisioner_power_test.go` (run against the
  **authorization-aware** fake, which enforces the spec rules above — a
  permissive double would not have caught either P1): legacy room → write
  with the user's level; existing users merged and untouched; extension
  fields (`events`, `invite`, `notifications`) preserved through the write
  — only the target users entry is mutated; exact-match level → no write;
  read error propagates with no write; state without a `users` map
  handled; second grant preserves the first (JSON round-trip semantics);
  **equal-level demotion** — `TestEnsureRoomPowerLevel_DemotionRevokesLevel`
  (actor 100 vs human 100: the actor write is rejected by enforced 9.6 and
  the self-write with the human's own token completes the 100 → 50),
  `TestEnsureRoomPowerLevel_EqualLevelDemotionWithoutSelfTokenFails`
  (no self token → `M_FORBIDDEN` surfaced, state unchanged — no silent
  success), `TestEnsureRoomPowerLevel_SimpleGrantIsSingleActorWrite`
  (0 → 50 is a plain actor write, one attempt),
  `TestEnsureRoomPowerLevel_TeamAdminOwnedRoom` (admin read of a
  TeamAdmin-owned room → `M_FORBIDDEN`; team-admin actor reads and writes
  the grant), `TestEnsureRoomPowerLevel_TeamAdminRoomEqualLevelDemotion`
  (same 9.6 wall as the team-admin actor, self fallback completes it).
- `internal/controller/human_controller_test.go`:
  `TestHumanReconciler_PowerLevelMapping` (level 1 → 100 in both a new room
  and an already-observed room; grant targets the human's Matrix ID),
  `TestHumanReconciler_PowerLevelL2GetsDefault` (level 2 → 50),
  `TestHumanReconciler_PowerLevelErrorNonFatal` (grant failure does not
  block the reconcile; room still recorded),
  `TestHumanReconciler_PowerGrantUsesTeamAdminActor` (team room granted
  with the TeamAdmin's token, worker room with the default admin; admin
  token resolved via login as the admin human),
  `TestHumanReconciler_EqualLevelDemotionSelfWriteFallback` (403 → retry
  with the human's own token; login issued lazily, exactly once),
  `TestHumanReconciler_PowerGrantNoLoginOnSuccess` (no 403 → no login),
  `TestHumanReconciler_RevocationSelfLeaveFallback` (kick rejected →
  self-leave with the human token → room dropped),
  `TestHumanReconciler_RevocationForceLeaveLastResort` (stale password,
  no self token → admin-bot force-leave → room dropped),
  `TestHumanReconciler_RevocationDeferredWhileTeamRoomUnresolved`
  (team names the human as admin, room in `status.rooms`, team
  `status.teamRoomID` not yet visible → kick deferred, room kept),
  `TestHumanReconciler_RevocationProceedsForKnownOriginTeamRoom`
  (claim pending on another team, but the revoked room's origin IS
  visible → kick proceeds immediately).
