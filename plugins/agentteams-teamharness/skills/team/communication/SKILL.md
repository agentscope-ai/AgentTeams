---
name: teamharness-communication
description: "Use for TeamHarness message delivery details after a flow has decided a message must be sent: current-session reply, NO_REPLY, cross-room/cross-channel message tool payloads, PROJECT_REQUESTED task-room handoff, and requester replyRoute reports. Do not use to choose work mode, create rooms, delegate, check, or accept tasks."
---

# Communication

This skill expands the TEAMS.md Communication Contract.

It only handles message delivery protocols. It does not choose Direct Reply,
Quick Task, or Project Work. It does not create rooms, delegate tasks, check
Worker results, or accept project state.

For ordinary direct replies and lightweight one-off answers in the current
room/session, answer directly. Use the `message` MCP tool only when a message
must leave the current runtime conversation.

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
3. Use the Task room for task-visible coordination: task request messages,
   Worker assignments, Worker completion/blocker reports, and downstream
   delegation notices.
4. If the selected destination is an external channel, another Matrix room, or
   another runtime session, use the `message` MCP tool.
5. If the destination is missing or ambiguous, do not guess. Restore project or
   task state first, or ask for the missing routing detail.

Use the Source channel / requester room for direct replies and requester-facing
reports only when the current session or recorded `reply_route` points there.
Use a project `reply_route` for requester-facing reports.

Do not treat a Task room assignment, Worker completion, Worker blocker report,
or downstream delegation as a requester report unless the recorded requester
route is exactly that current room.

## Requester Report Delivery Protocol

Use this protocol only after project state says a requester report is needed,
for example after accepted project progress or a blocker that must be surfaced
to the requester. When project state has a pending requester report, delivery
to that requester route is mandatory.

Prefer `reply_route`:

```json
{
  "channel": "dingtalk",
  "target_user": "sender_001",
  "target_session": "aaaaaaaa"
}
```

Matrix DM requester reports use the same route shape, with `targetSession`
pointing at the original DM room:

```json
{
  "channel": "matrix",
  "target_user": "@admin:matrix.local",
  "target_session": "!admin-dm:matrix.local"
}
```

For Matrix DM-originated projects, `requester=@user` is identity only. It is not
enough to deliver a requester report after the Leader wakes up in a Task room.
Use the persisted Matrix `reply_route` and send to its `target_session`.

For legacy projects without `reply_route`, parse `requester` only when it uses a
known encoding:

| Legacy requester | Delivery |
|---|---|
| `matrix:!roomid:domain` | `message` with `channel: "matrix"` and `target: "room:!roomid:domain"` |
| `dingtalk:{user_id}:{session_id}` | `message` with `channel: "dingtalk"`, `targetUser`, and `targetSession` |

Do not guess missing channel, user, room, or session values. If a legacy Matrix
requester points at the current room/session, answer directly instead of using
the `message` tool.

After successful delivery, return to `teamharness-project-management` to record
the report as sent; project-management owns project state such as
`projectflow mark_requester_report_sent`.

## Message Tool Protocol

Use the `message` MCP tool only for explicit cross-room, cross-channel, or
cross-session sends. Do not use it for normal replies in the current
room/session; answer directly instead.

## Task Room Request Message

Use this shape when a Source channel / requester room has created or reused a
Matrix task room and must hand Quick Task or Project Work to the same Leader in
that task room.

The message text should be a compact task request, not a Worker assignment and
not a full project plan. Include enough source context for the task-room Leader
to create project state and preserve the requester route:

```text
PROJECT_REQUESTED: <short task or project title>

Requester: <human/source identity>
Source: <source channel and source session or room>
ReplyRoute: <recorded requester replyRoute summary>
TaskRoom: !task-room:matrix.local

Request:
<1-5 sentence requester ask>

Expected outcome:
- <deliverable or acceptance point>
- <report/artifact expectation>
```

Send it with the `PROJECT_REQUESTED` self-trigger payload below. After the tool
returns `delivery.sent: "matrix_self_trigger"`, stop in the source session.

### PROJECT_REQUESTED Self Trigger

Use this only for same-agent Quick Task or Project Work handoff from any
requester/source session, such as DingTalk group, Matrix DM, or another Matrix
room, into a target Matrix task room. The tool sends a Matrix event with a
TeamHarness trigger marker; the QwenPaw Matrix channel accepts that marked self
event as the target room's current event.

Set top-level `channel` to `"matrix"` because the trigger target is the Matrix
task room. Put the original requester/source channel in `sender.session.channel`.
Set `sender.agent` and `agentId` to the current runtime Matrix user id, for
example `@prod-observe-oncall-bot:at-cn-rpg4um9o601`. Do not use role names
such as `leader`, worker names, or workspace names such as `default`.
Pass the requester route as structured `replyRoute`. Do not write the route only
inside `message.text`; the tool rejects `PROJECT_REQUESTED` without structured
`replyRoute` and does not send the Matrix trigger.
Also include the route values that the task-room Leader must pass to
`projectflow` in `message.text`. For Matrix routes, include
`replyRoute.targetSession`. For external routes such as DingTalk or Feishu,
include `replyRoute.channel`, `replyRoute.targetUser`, and
`replyRoute.targetSession`.

Pass `message` as a JSON object. Do not serialize it into a JSON string. The
object form lets the tool attach the TeamHarness trigger marker; a plain Matrix
send will be ignored by the target room as the Leader's own ordinary message.

```json
{
  "action": "send",
  "channel": "matrix",
  "sender": {
    "agent": "@prod-observe-oncall-bot:at-cn-rpg4um9o601",
    "session": {
      "channel": "dingtalk",
      "id": "ding-group-session-001"
    }
  },
  "target": "room:!task-room:matrix.local",
  "replyRoute": {
    "channel": "dingtalk",
    "targetUser": "sender_001",
    "targetSession": "ding-group-session-001",
    "mentionSender": true
  },
  "message": {
    "type": "PROJECT_REQUESTED",
    "text": "PROJECT_REQUESTED: <request summary>\nReplyRoute: dingtalk/sender_001/ding-group-session-001\nTask room: !task-room:matrix.local"
  },
  "agentId": "@prod-observe-oncall-bot:at-cn-rpg4um9o601"
}
```

Expected result:

```json
{
  "delivery": {"sent": "matrix_self_trigger"},
  "context": {"via": "matrix_current_event"},
  "trigger": {"status": "sent"}
}
```

After this trigger, stop durable project work in the source session. The target
task-room Leader session should copy the visible PROJECT_REQUESTED `replyRoute`
into `projectflow create_quick_project` or `create_project` as
`payload.replyRoute`; `projectflow` then persists it in the existing project
`meta.json.reply_route`.

## Requester Report Message

Use `teamharness-project-management` to build the requester-facing report
content from project state. This skill only handles delivery shape and message
tool payloads.

Deliver the report produced by `teamharness-project-management` with the
recorded `replyRoute`:

Requester reports are usually informational. Do not use Matrix mention syntax
for descriptive owners, executors, reviewers, or participants in the report
body. Use plain names unless the report intentionally asks that member to
respond or take action.

```json
{
  "action": "send",
  "replyRoute": {
    "channel": "dingtalk",
    "targetUser": "sender_001",
    "targetSession": "aaaaaaaa"
  },
  "text": "<project-management requester report markdown>"
}
```

Keep the returned Matrix `messageId` when the requester report is sent to a
Matrix room. Use that id as `parentEventId` for any immediately published
report files so the files attach to the report message.

For DingTalk, Feishu, WeChat, or other non-Matrix requester reports, do not
describe workspace files as chat attachments or visible file cards. The
`artifact` tool publishes Matrix room files only. If report files exist, include
the important `shared/...` paths in the report text and say they are available
from the shared workspace or platform object-storage view.

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

For Matrix DM requester reports, pass the recorded Matrix route:

```json
{
  "action": "send",
  "replyRoute": {
    "channel": "matrix",
    "targetUser": "@admin:matrix.local",
    "targetSession": "!admin-dm:matrix.local"
  },
  "text": "Project A is ready."
}
```

For DingTalk requester reports that should notify the original sender, add
`mentionSender: true`. Use this only with the recorded DingTalk requester route;
the message tool will use stored session metadata to mention the sender. If the
recorded DingTalk session webhook or sender metadata is missing, the tool
returns `ok: false` and does not fall back to normal delivery. Do not mark the
requester report as sent after that failure.

```json
{
  "action": "send",
  "replyRoute": {
    "channel": "dingtalk",
    "targetUser": "sender_001",
    "targetSession": "aaaaaaaa"
  },
  "mentionSender": true,
  "text": "Project A is ready."
}
```

For cross-room Matrix sends, pass a channel target:

```json
{
  "action": "send",
  "channel": "matrix",
  "target": "room:!source-room:matrix.local",
  "text": "Project A has moved to the task room. I will report back here when accepted."
}
```

Do not use this for Worker assignment messages in the current Task room. In the
Task room, send the `TASK_ASSIGNED` line as a normal current-session reply so
the runtime emits one formatted Matrix event and the Worker is triggered once.

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

Pass a channel target or `replyRoute`, not an object-storage path.
