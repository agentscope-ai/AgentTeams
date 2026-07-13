---
name: communication
description: Use before sending or suppressing any Leader message to Workers, Manager, or Team Admin. Always use this skill for cross-room Matrix messages, @mention decisions, task assignment notifications, structured status reports, completion reports, blocker/revision messages, questions, requester updates, or when deciding whether a same-room reply is enough.
---

# Communication

## Routing Gate

Before any message, decide the target recipient and target room.

1. If the target recipient is in the current room, reply directly in the current session.
2. If the target room is the current room, reply directly in the current session.
3. Use the `message` tool only when the target room is different from the current room, or when the workflow explicitly requires a different room.

Room names such as Team Room, Leader Room, or Leader DM describe where the recipient should see the message. They do not by themselves mean you should call the `message` tool.

Hard rule: do not call the `message` tool to send a message back into the current room.

## Task Assignment Room

Send normal task assignment notifications to the team room, not to a Worker's private room, Leader DM, or Leader Room. Include the assigned Worker's full Matrix ID as a visible @mention so the Worker is addressed while the assignment context stays visible to the team.

If the current room is Leader DM or Leader Room, the Team Room assignment is cross-room. Use the `message` tool with `target` set to `room:<Team Room ID>` from your team context. Do not directly reply in the current room for Worker task assignment just because the Worker Matrix ID appears in the message text.

Use a Worker private room only for exceptional follow-up that should not be team-visible, such as sensitive clarification or direct recovery/debugging.

## Requester Reports

Route reports by the project source:

| Source | Target Room | Report To |
|--------|----------------|-----------|
| Manager | Leader Room @mention | Manager |
| Team Admin | Leader DM | Team Admin |

Determine requester from the current notification message `sender`, and report back to the requester recorded on the project:

- If `sender` is Team Admin, the target room is Leader DM. If the current room is that Leader DM, reply directly in the current session.
- If `sender` is Manager, the target room is Leader Room. If the current room is that Leader Room, reply directly in the current session and @mention Manager when action is needed.
- If the recorded requester is missing or does not match the original event sender, stop and fix project metadata before reporting.

Only use the `message` tool for a requester report when the target room is not the current room.

After task handling changes Project state, notifying the requester is mandatory. If the requester is Team Admin, this means a Leader DM report to the DM admin. Team Room coordination, Worker assignment messages, and downstream task notices do not count as the requester report.

Do not copy team-room coordination logs into requester DM. Summarize the state.

For project-shaped Team Admin requests received in Leader DM, do not send DAG plans, analysis, "let me..." progress notes, or other interim project narration back to Leader DM before the first Team Room assignment has been posted. While reading skills, checking organization, planning, creating the Project, or delegating the first task, your same-room Leader DM reply must be exactly `NO_REPLY`. The first visible non-`NO_REPLY` message for that request must be either the Team Room assignment sent with `message target=room:<Team Room ID>`, or a blocker/question to the Team Admin when assignment cannot proceed. After the Team Room assignment, send one concise requester update if needed.

Use `project-management` to determine project report content and the DAG or Loop Project Status Report shape. Use this skill to decide where the report should be delivered and whether to reply directly or use the `message` tool.

All human-facing message text must use the language selected by `AGENTS.md` Response Language. This includes headings, field labels, table headers, state labels, summaries, next steps, notes, and deliverable descriptions.

Matrix rendering supports headings, lists, dividers, Markdown tables, and fenced code blocks. Keep requester reports concise. The requester wants current state and outcomes, not internal command logs.

## Same Room

Reply directly in the current session.

If the recipient must act, include their full Matrix ID as a visible @mention:

```text
@worker:domain New task [todo-api-20260429-130052-01]: Please read shared/tasks/todo-api-20260429-130052-01/spec.md and follow your Worker task participation skills. Publish shared/tasks/todo-api-20260429-130052-01/result.md when complete, then @mention me with the outcome.
```

Do not use the `message` tool for same-room replies.

## Cross-Room

Use the `message` tool only when the target room is not the current room, or when the workflow must continue in a different room.

Resolve the recipient Matrix ID and target room from `hiclaw` CLI immediately before sending.

```json
{
  "action": "send",
  "channel": "matrix",
  "target": "room:!roomid:matrix-local.agentteams.io:18080",
  "message": "@alice:matrix-local.agentteams.io:18080 New task [todo-api-20260429-130052-01]: Please read shared/tasks/todo-api-20260429-130052-01/spec.md and follow your Worker task participation skills. Publish shared/tasks/todo-api-20260429-130052-01/result.md when complete, then @mention me with the outcome."
}
```

## Rules

- `target` is where to send the message. Use a Matrix room target such as `room:!roomid:domain`.
- `message` is the full visible message body. Include the recipient's full Matrix ID when they must act.
- Do not bypass this skill with raw Matrix API calls, direct `curl`, or remembered channel commands. Raw Matrix sends can lose formatted HTML and structured mentions, causing Workers not to wake.
- Do not send low-information mention pings. This includes mention-only messages, acknowledgments, thanks, encouragement, status symbols, and short replies like `ok`, `done`, `收到`, or `好的`.
- Before sending, remove all Matrix IDs from the message in your head. Send only if the remaining text contains a concrete task, blocker, question, decision, or result.
- If two rounds produce no new task, question, or decision, stop replying.
