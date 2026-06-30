---
name: teamharness-communication
description: "Use when TEAMS.md Communication Contract needs detailed routing: Channel / Room Selection Protocol, Requester Report Delivery Protocol, or Message Tool Protocol. It resolves current session vs Team Room vs assignment room vs requester reply_route vs external channel, and explains when to use the message MCP tool. Do not use it to choose Quick Task or Project Work, create rooms, delegate tasks, check results, or accept project state."
---

# Communication

This skill expands the TEAMS.md Communication Contract.

It only handles message delivery protocols. It does not choose Direct Reply,
Quick Task, or Project Work. It does not create rooms, delegate tasks, check
Worker results, or accept project state.

For ordinary direct replies and other lightweight one-off answers in the
current room/session, answer directly. Use the `message` MCP tool only when a
message must leave the current runtime conversation.

Use `NO_REPLY` exactly when the current event is a low-information
acknowledgement, self echo, or non-actionable mention-only message. Do not add
extra explanation to `NO_REPLY`.

Prevent ping-pong loops. Do not let acknowledgements, mention-only messages, or
same-room status echoes create repeated cross-room or cross-agent replies.

## Channel / Room Selection Protocol

Use this protocol after another TeamHarness flow has already decided that a
message should be sent.

1. If the reply belongs in the current runtime session, answer directly.
2. If a project requester report is needed, prefer the project's `reply_route`.
3. If the selected destination is the Team Room or assignment room, send there
   only for team-visible coordination.
4. If the selected destination is an external channel, another Matrix room, or
   another runtime session, use the `message` MCP tool.
5. If the destination is missing or ambiguous, do not guess. Restore project or
   task state first, or ask for the missing routing detail.

Use the Team Room for normal team-visible coordination. Use the assignment room
recorded on the task for task-visible coordination. Use a project `reply_route`
for requester-facing reports.

Do not treat a Team Room assignment, Worker completion, Worker blocker report,
or downstream delegation as a requester report unless the recorded requester
route is exactly that current room.

## Requester Report Delivery Protocol

Use this protocol only after project state says a requester report is needed,
for example after accepted project progress or a blocker that must be surfaced
to the requester.

When project state records a pending requester report after an accepted state
change, delivery is mandatory.

Prefer `reply_route`:

```json
{
  "channel": "dingtalk",
  "target_user": "sender_001",
  "target_session": "aaaaaaaa"
}
```

For legacy projects without `reply_route`, parse `requester` only when it uses a
known encoding:

| Legacy requester | Delivery |
|---|---|
| `matrix:!roomid:domain` | `message` with `channel: "matrix"` and `target: "room:!roomid:domain"` |
| `dingtalk:{user_id}:{session_id}` | `message` with `channel: "dingtalk"`, `targetUser`, and `targetSession` |

Do not guess missing channel, user, room, or session values. If a legacy Matrix
requester points at the current room/session, answer directly instead of using
the `message` tool.

After successful delivery, call `projectflow` `mark_requester_report_sent` when
the project recorded a pending requester report.

## Message Tool Protocol

Use the `message` MCP tool only for explicit cross-room, cross-channel, or
cross-session sends. Do not use it for normal replies in the current
room/session; answer directly instead.

For cross-session requester reports, pass the recorded `replyRoute`:

```json
{
  "action": "send",
  "replyRoute": {
    "channel": "dingtalk",
    "targetUser": "sender_001",
    "targetSession": "aaaaaaaa"
  },
  "text": "Project A is ready."
}
```

For Matrix room sends, pass a channel target:

```json
{
  "action": "send",
  "channel": "matrix",
  "target": "room:!team-room:matrix.local",
  "text": "@worker-a:matrix.local TASK_ASSIGNED: task-001 - Please start. Spec: shared/tasks/task-001/spec.md"
}
```

For external channel sends, pass the channel-specific target fields:

```json
{
  "action": "send",
  "channel": "dingtalk",
  "targetUser": "sender_001",
  "targetSession": "aaaaaaaa",
  "text": "Project A is ready."
}
```

Pass a channel target or `replyRoute`, not an object-storage path. Matrix
mentions are message formatting details: mention a Worker or Leader only when
the message asks that member to act or records task completion/blocker status.
Do not send mention-only acknowledgements such as `@worker ok`.
