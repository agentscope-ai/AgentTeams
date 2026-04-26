---
name: communication
description: Use before sending or suppressing any Leader message to Workers, Manager, or Team Admin. Always use this skill for cross-room Matrix messages, @mention decisions, task assignment notifications, structured status reports, completion reports, blocker/revision messages, questions, requester updates, NO_REPLY decisions, or when deciding whether a same-room reply is enough.
---

# Communication

First decide whether the recipient is in the current room.

## Task Assignment Room

Send normal task assignment notifications to the team room, not to a Worker's private room. Include the assigned Worker's full Matrix ID as a visible @mention so the Worker is addressed while the assignment context stays visible to the team.

Use a Worker private room only for exceptional follow-up that should not be team-visible, such as sensitive clarification or direct recovery/debugging.

## Requester Reports

Requester DM is for event-based project status, blockers that need a decision, questions, and final delivery. It is not for internal polling, waiting, routine checks, or unchanged state.

Route reports by the project source:

| Source | Report Channel | Report To |
|--------|----------------|-----------|
| Manager | Leader Room @mention | Manager |
| Team Admin | Leader DM | Team Admin |

Determine requester from the current notification message `sender`, and report back to the requester recorded on the project:

- If `sender` is Team Admin, report to Team Admin in Leader DM.
- If `sender` is Manager, report to Manager in Leader Room.
- If the recorded requester is missing or does not match the original event sender, stop and fix project metadata before reporting.

Send requester updates when one of these is true:

- The project is complete.
- A DAG node changes status: assigned, in progress, completed, blocked, or revision.
- A completed DAG node unblocks or starts the next wave of work.
- A blocker requires requester or admin action.
- A requirement is ambiguous and needs an answer.
- An exception, timeout, or recovery issue needs escalation.

Batch same-turn changes into one report. For example, if one completed task unblocks two newly assigned tasks, send one status report covering all three node changes.

Do not send requester updates for unchanged polling results, repeated waiting, routine verification, or "still not done" checks.

Do not copy team-room coordination logs into requester DM. Summarize the state.

Use a Matrix-friendly plain-text structure for final or node-status reports. Do not use Markdown tables; Matrix may render them as raw pipe text.

```text
Project: <name>
Status: <IN_PROGRESS | BLOCKED | REVISION_NEEDED | COMPLETED>

Summary:
<1-3 sentences about what changed and what happens next>

Task Status:
- <task-id> <title>
  Owner: <worker/role>
  Status: <Pending/In Progress/Completed/Blocked/Revision>
  Result: <short result or next action>

Deliverables:
- <name/path>: <what it contains>

Notes:
- <important dependency, blocker, risk, or next step; omit if none>
```

For intermediate node updates, omit `Deliverables` unless new deliverables are available. Keep reports concise. Prefer task status and deliverables over process narration. The requester wants current state and outcomes, not internal command logs.

## Same Room

Reply directly in the current session.

If the recipient must act, include their full Matrix ID as a visible @mention:

```text
@worker:domain Please pull shared/tasks/st-01/ with filesync, then read shared/tasks/st-01/spec.md.
```

Do not use the `message` tool for same-room replies.

## Cross-Room

Use the `message` tool only when the recipient is not in the current room, or when the workflow must continue in a different room.

Resolve the recipient Matrix ID and target room from `hiclaw` CLI immediately before sending.

```json
{
  "action": "send",
  "channel": "matrix",
  "target": "room:!roomid:matrix-local.hiclaw.io:18080",
  "message": "@alice:matrix-local.hiclaw.io:18080 New task [st-01]: Please pull shared/tasks/st-01/ with filesync, then read shared/tasks/st-01/spec.md"
}
```

## Rules

- `target` is where to send the message. Use a Matrix room target such as `room:!roomid:domain`.
- `message` is the full visible message body. Include the recipient's full Matrix ID when they must act.
- Do not send low-information mention pings. This includes mention-only messages, acknowledgments, thanks, encouragement, status symbols, and short replies like `ok`, `done`, `收到`, or `好的`.
- Before sending, remove all Matrix IDs from the message in your head. Send only if the remaining text contains a concrete task, blocker, question, decision, or result.
- If two rounds produce no new task, question, or decision, stop replying.
