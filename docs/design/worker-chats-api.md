# Worker Chats API (Read-Only Session Visibility)

The worker's qwenpaw app stores every conversation (a *chat*) per
`(user, channel)` — the Matrix room thread, the QQ direct messages, the
console sessions — and exposes it read-only on the worker's console port
(`8088`, no auth in worker context). That port is not published, so
scoped callers (L2 humans, team leaders) have no way to see a worker's
conversation history through the controller.

The Controller therefore proxies three **read-only** subpaths of the
worker's chat API. The proxy is thin and byte-transparent (the same
pattern as the worker checkpoint and channel proxies): it resolves the
worker, enforces the worker-scoped read boundary **and the
participation boundary** (an L2 human sees the worker's conversations
in the Matrix rooms they are a current member of — never the
worker's conversations in other rooms, DMs, or non-matrix channels),
and forwards to the worker's qwenpaw app over the shared docker
network. The dashboard renders the results as the worker's session
list / agent context / run status.

**Read-only by design.** There is no create / archive / delete / stream
route: the conversation lifecycle stays on the qwenpaw app and its own
channels.

## Routes

| Route | Upstream (worker qwenpaw app) | Returns |
|-------|------------------------------|---------|
| `GET /api/v1/workers/{name}/chats` | `GET /api/chats` | `list[ChatSpec]` — id, name, user_id, channel, created/updated, pinned, archived, … (L2 humans: only the chats in rooms they are a current member of) |
| `GET /api/v1/workers/{name}/chats/{chat_id}` | `GET /api/chats/{chat_id}` | `ChatHistory{messages: [Message], status}` — the saved **agent context** converted to messages (L2 humans: chats in their rooms only) |
| `GET /api/v1/workers/{name}/chats/{chat_id}/status` | `GET /api/chats/{chat_id}/status` | `ChatStatusResponse{status: "idle" \| "running"}` (QwenPaw ≥ 2.2.1; L2 humans: chats in their rooms only) |

- `{chat_id}` is the qwenpaw chat id — always a lowercase UUIDv4 (every
  chat is created with `str(uuid4())`). Anything else is rejected with
  `400` before the dial (no path injection possible).
- The list endpoint forwards the documented read-only filters
  `?user_id=`, `?channel=`, `?archived=`, `?include_app_owned=`
  (whitelist; unknown parameters → `400`, never `422`). For L2 humans
  the client-supplied filters are **dropped server-side** — the fetch is
  forced to `channel=matrix` and the response is filtered to the
  caller's rooms (a client-supplied value is a filter, never
  authorization) — see *Participation boundary*. The detail and status
  routes take no query parameters.
- 2xx responses stream verbatim (a context can be large; no body cap,
  same as the checkpoint proxy). Upstream 4xx responses pass through
  verbatim (see *QwenPaw version contract*). Upstream 5xx is wrapped in
  a `502` with a truncated body.

### Agent context versus chat history

The detail route returns the worker's **saved agent context** converted
to messages — not a guaranteed complete transcript of what was exchanged
in the channel. The context may have been compacted and may contain
tool output that was never sent to the channel. For Matrix
conversations, the messages actually exchanged in the room remain
authoritative in Matrix (subject to Matrix membership /
history-visibility rules); this proxy is a context-inspection surface,
not a second source of room history. The dashboard should label the
view "Agent context", not "Chat history".

## Authorization

### Worker scope (layer 1)

The routes ride on the standard worker `GET` authorization (L1 admin:
any worker; L2 human / team leader: own-team workers via the
`TeamMatches` scope check; standalone humans: `404`). Cross-team access
returns `404`, uniformly with "no such worker" — worker existence cannot
be probed (404-not-403, same as the other worker-scoped reads).
Embedded mode only: kube mode returns `503` uniformly.

Resource addressing versus runtime addressing: the `{name}` path segment
addresses the Worker CR and authorization keys off it (team scope,
404-not-403). The upstream dial uses the container identity instead —
`WorkerSpec.EffectiveWorkerName(worker.Name)` (`spec.workerName` when
set, the CR name otherwise) — so imported/renamed workers reach the
right container.

### Participation boundary (layer 2 — L2 humans, room level)

Passing the worker scope is necessary but not sufficient for an L2
human: they may converse with the worker, but they may only **view the
worker's conversations in the Matrix rooms they are a current member
of** — not the worker's conversations in other rooms, in DMs with
other users, or on non-matrix channels. The boundary is room-level
(deliberate, not a simplification): in the AgentTeams workflow a
human's work with a team happens in the team's Matrix rooms, and the
diagnostic value of this surface is exactly the agent-driven
conversations in those rooms (manager/leader delegating, workers
reporting) — the human's own `@`-messages are the rare case, and
sender-level "my own chats only" would hide the evidence a
diagnostician needs while still being trivially gameable (any team
mate's MXID). Room level is the literal "who is in the room" check:
it matches Matrix's own visibility model, covers DMs correctly
(two-member rooms stay private to their participants), and needs no
stored membership table. The boundary is enforced server-side on all
three routes:

- **Anchor.** The caller's OWN Matrix access token — the one the
  authenticator just validated via whoami — is re-read from the request
  and passed to the homeserver's `GET
  /_matrix/client/v3/joined_rooms` (`matrix.Client.ListJoinedRooms`
  with a user token; there is no admin-substitute view of "which rooms
  is THIS user in"). No privilege escalation, no Human CR dependency
  on this path (the Human CR is still what the identity layer uses for
  team membership).
- **Chat → room mapping.** The matrix channel keys every chat by room:
  `session_id = "matrix:{room_id}"` — identically in BOTH group-session
  modes of the channel (`share_session_in_group`, AgentTeams decision
  #7001, 2026-09-05). Only the per-chat `user_id` differs between
  modes: the sender's MXID when sessions are isolated per sender (the
  AgentTeams default — `share_session_in_group: false`), the room ID
  itself in shared/legacy mode. So a chat is in the caller's room set
  iff `matrixRoomID(chat.session_id)` is a room the caller is in —
  which admits, per room, every `(room, sender)` session (isolated
  mode) or the single room session (shared mode). Non-matrix channels
  (qq/console/cron), app-owned chats, and subagent sessions have no
  `"!"`-prefixed room namespace: `matrixRoomID` yields `""` and they
  are never visible to an L2 human (fail closed, no per-channel
  allowlist).
- **List.** The client-supplied filters are dropped; the controller
  fetches `GET /api/chats?channel=matrix` and returns only the items
  whose session resolves to one of the caller's rooms, re-encoded
  byte-transparent per item (an empty result is `[]`, never `null`).
- **Detail / status.** A participation precheck (the worker's matrix
  chat list filtered to the caller's rooms must contain the chat id)
  runs before the dial; an absent chat returns a **uniform 404 in the
  upstream's own not-found shape**
  (`{"detail":"Chat not found: {id}"}`) — indistinguishable from a
  genuinely missing chat, so neither other rooms' chat existence nor
  content can be probed, and the upstream detail endpoint is never
  dialed for a denied request. A precheck upstream failure returns
  `502`, not a false 404: participation that cannot be proven must not
  silently hide a healthy worker.
- **Fail closed.** If the request carries no bearer token, the Matrix
  source is unavailable (no Matrix client wired), or the
  `joined_rooms` call fails, the whole surface hides for that caller
  (uniform `404`, no upstream dial) — an unprovable anchor must not
  leak a conversation view.
- **L3 (worker-scoped) humans** have no chats access in v1 — consistent
  with #1277, which deliberately keeps the other read surfaces
  (checkpoints, skills, …) team-scoped and lists extensions as
  follow-ups. Their `WorkerReadable` leg does not apply to this route.

Full-view callers (L1 admin, manager SA, team leader SA) skip layer 2
entirely — they see the complete list (with client filters) and dial
detail/status directly.

**Data sensitivity.** An agent context is the worker's saved
conversation context and may contain tool output or fragments of stored
configuration. Access is bounded by worker scope AND room
participation — no new credential or capability is introduced (the
membership query rides on the caller's own already-validated token),
and callers outside the team cannot even confirm the worker exists.

## Status mapping

| Upstream / situation | Controller response |
|----------------------|---------------------|
| worker name / chat id fails validation | `400` |
| worker not found | `404` `{"message":"worker not found"}` |
| scoped caller outside the worker's team | `404` (same body as unknown worker) |
| L3 (worker-scoped) human | `404` (no chats access in v1) |
| L2 human, no token / no Matrix source / `joined_rooms` fails | `404` (uniform, fail closed, no upstream dial) |
| L2 human, chat not in their rooms (detail/status) | `404` `{"detail":"Chat not found: {id}"}` (upstream's own shape) |
| L2 human, precheck upstream call fails | `502` (participation unprovable is not a 404) |
| kube mode | `503` |
| upstream unreachable (5 s timeout) | `502` `{"message":"worker unreachable"}` |
| upstream 2xx | `200`, body + `Content-Type` verbatim |
| upstream 4xx | passed through verbatim (status + body) |
| upstream 5xx | `502`, body truncated to 128 bytes |

## Example

```bash
# L2 human (alice) lists a worker's sessions in the rooms she is in —
# agent-driven (manager/leader) sessions included; the worker's chats in
# other rooms, DMs, and non-matrix channels never appear.
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/chats \
  -H "Authorization: Bearer $AGENTTEAMS_MATRIX_TOKEN"
# → 200 [{"id":"0d9f…","name":"matrix:…","session_id":"matrix:!…","user_id":"@manager:…","channel":"matrix",…}, …]

# open the agent context of a session in one of her rooms
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/chats/0d9f3d6e-…-0e1f \
  -H "Authorization: Bearer $AGENTTEAMS_MATRIX_TOKEN"
# → 200 {"messages":[{"type":"message","role":"user","content":[…],…}], "status":"idle"}

# a chat in a room she is not in → uniform 404, indistinguishable from absent
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/chats/{other-room-chat-id} \
  -H "Authorization: Bearer $AGENTTEAMS_MATRIX_TOKEN"
# → 404 {"detail":"Chat not found: {other-user-chat-id}"}

# is the worker currently replying in that session? (QwenPaw ≥ 2.2.1)
curl -s http://127.0.0.1:8090/api/v1/workers/daily-carol/chats/0d9f3d6e-…-0e1f/status \
  -H "Authorization: Bearer $AGENTTEAMS_MATRIX_TOKEN"
# → 200 {"status":"running"}   (always 200 on 2.2.1+; 404 on older builds)

# L1 admin sees the full list (client filters apply as written)
curl -s "http://127.0.0.1:8090/api/v1/workers/daily-carol/chats?user_id=@bob:…" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
# → 200 [ …all of the worker's chats matching the filter… ]
```

## Notes

- **Single-agent workers.** Without an `X-Agent-Id` header the worker's
  qwenpaw app resolves the active agent from its config; in a
  single-profile worker container that is the worker's own agent, so the
  global (non-agent-scoped) upstream path targets the right agent
  without header plumbing (same as the channel proxy).
- **Addressing.** Upstream is dialed as
  `http://{containerPrefix}{name}:{AGENTTEAMS_CONSOLE_PORT}` (default
  `8088`, system-wins env resolution — the same chain the container is
  created with).
- **Standalone workers** (not a member of any team) hide as `404` for
  scoped callers — they have no team to scope to.

## Tests (`internal/server/worker_chats_test.go`)

- Full-view passthrough: `TestChatList_ForwardsVerbatim`,
  `TestChatList_AllWhitelistedParamsForwarded`,
  `TestChatDetail_ForwardsVerbatim`, `TestChatStatus_Forwards` — admin
  queries forwarded (sorted), bodies verbatim, large bodies streamed.
- **Participation boundary (room level):**
  - `TestChat_L2RoomParticipation_ScopeCheckStillApplies` — in-scope
    L2 human resolves 200; the team-scope check still applies
    (findTeamMember's second return is the member name, not the team
    name).
  - `TestChat_L2RoomParticipation_ListFiltersToCallerRooms` — the list
    keeps exactly the chats in the caller's rooms (an AGENT-DRIVEN
    session, `user_id` = the manager's MXID, included); other rooms and
    non-matrix channels excluded; client filters dropped (upstream
    forced to `channel=matrix`); `joined_rooms` called with the
    caller's OWN token.
  - `TestChat_L2RoomParticipation_InRoomAgentDrivenDetail200` — a chat
    in the caller's room whose `user_id` is the manager's MXID passes
    the precheck and streams verbatim (the behavior that changed from
    sender-level v1).
  - `TestChat_L2RoomParticipation_OtherRoomDetail404` — a chat in a
    room the caller is not in 404s in the upstream's own shape; the
    upstream detail endpoint is **never dialed** (no content, no
    existence probe).
  - `TestChat_L2RoomParticipation_NonMatrixChatDetail404` —
    console/qq chats (no `"!"`-prefixed room namespace) 404, never
    dialed (fail closed, no per-channel allowlist).
  - `TestChat_L2RoomParticipation_SharedSessionModeVisible` — the
    shared `share_session_in_group` mode (chat `user_id` = the room
    id) is visible to every current room member.
  - `TestChat_L2RoomParticipation_StatusSameBoundary` — the status
    route runs the same precheck (other room 404 / in-room 200).
  - `TestChat_L2RoomParticipation_JoinedRoomsFailure404` — a failing
    `joined_rooms` call: uniform 404, **zero** upstream dials (fail
    closed).
  - `TestChat_L2RoomParticipation_NoToken404` /
    `TestChat_L2RoomParticipation_NoMatrixSource404` — no bearer token
    / no Matrix client wired: uniform 404, no dial.
  - `TestChat_L2RoomParticipation_PrecheckUpstreamFailure502` — a
    failing precheck list call 502s (unprovable participation ≠ 404).
  - `TestChat_L3HumanDenied` — L3 (worker-scoped) human: 404, upstream
    never dialed (v1: no chats access, #1277 posture).
- Scope layer: `TestChat_TeamLeaderCrossTeamDenied`,
  `TestChat_StandaloneHumanDenied` (both unchanged — 404, no probe).
- Version gate: `TestChatStatus_Upstream404IsTheVersionGate` (older
  builds' 404 passed through verbatim).
- Robustness: validation 400s, kube-mode 503, unknown worker 404,
  bounded 502 bodies, unreachable-worker 502, prefix/port resolution.

## QwenPaw version contract

The proxy is **version-agnostic**: it forwards to fixed, prefixed paths
(`/api/chats…`) and contains no version logic. The version gate is by
pass-through: an upstream 4xx is returned verbatim, so clients can
distinguish "this worker build predates the route" from a real failure
and hide the feature accordingly (the skills proxy's pattern).

The three routes were verified directly against the official PyPI
release wheels (hash-checked):

| QwenPaw release | `GET /api/chats` | `GET /api/chats/{id}` | `GET /api/chats/{id}/status` |
|---|---|---|---|
| 2.0.1 (2026-07-24) | present (`user_id` / `channel` / `archived` filters) | present | **absent** → upstream `404` passed through |
| 2.2.0 (2026-09-03) | present | present | **absent** → upstream `404` passed through |
| 2.2.1 (2026-09-11) | present (`+include_app_owned`) | present (`+include_app_owned`) | present — always `200 {status}` (queries the run tracker, not chat persistence) |

So list + detail work unchanged on any 2.0.1 → 2.2.1 build; the status
route is a 2.2.1+ feature that older builds surface as their own `404`
`{"detail":"Not Found"}`, which dashboards should treat as
"status not available on this worker build" (hide the indicator), not
as an error. `include_app_owned` forwarded to older builds is silently
ignored by their router (unknown query parameters are not rejected).
