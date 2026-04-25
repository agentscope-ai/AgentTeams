---
name: communication
description: Use when deciding how to message Workers, Manager, or Team Admin, especially across rooms.
---

# Communication

First decide whether the recipient is in the current room.

## Same Room

Reply directly in the current session.

If the recipient must act, include their full Matrix ID as a visible @mention:

```text
@worker:domain Please file-sync and read shared/tasks/st-01/spec.md.
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
  "message": "@alice:matrix-local.hiclaw.io:18080 New task [st-01]: Please file-sync and read shared/tasks/st-01/spec.md"
}
```

## Rules

- `target` is where to send the message. Use a Matrix room target such as `room:!roomid:domain`.
- `message` is the full visible message body. Include the recipient's full Matrix ID when they must act.
- Do not send low-information mention pings. This includes mention-only messages, acknowledgments, thanks, encouragement, status symbols, and short replies like `ok`, `done`, `收到`, or `好的`.
- Before sending, remove all Matrix IDs from the message in your head. Send only if the remaining text contains a concrete task, blocker, question, decision, or result.
- If two rounds produce no new task, question, or decision, stop replying.
