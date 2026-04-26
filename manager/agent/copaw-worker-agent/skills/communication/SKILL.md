---
name: communication
description: Use before sending or suppressing any Worker reply to a coordinator, human admin, Team Leader, Manager, or another Worker. Always use this skill for @mention decisions, TASK_COMPLETED, BLOCKED, QUESTION, direct answers, cross-room updates, NO_REPLY decisions, loop prevention, or deciding whether a short acknowledgement should be suppressed.
---

# Communication

You usually reply in the current room/session.

## @Mention Rules

Use a full Matrix ID when the recipient must act.

Mention your coordinator only for:

- Task completion: `@coordinator:domain TASK_COMPLETED: <summary>`
- Blocker: `@coordinator:domain BLOCKED: <what is blocking you>`
- Question: `@coordinator:domain QUESTION: <your question>`
- Direct answer to a coordinator question

Do not @mention for:

- "Got it"
- "Thanks"
- "Working on it"
- Encouragement-only replies
- Status symbols such as green dots or check marks
- Short acknowledgments such as `ok`, `done`, `收到`, or `好的`
- Mid-task progress that requires no decision

Before sending any @mention, remove all Matrix IDs from the message in your head. Send only if the remaining text contains a concrete completion, blocker, question, requested answer, or decision. Otherwise send `NO_REPLY`.

## History Context

If your message includes a history section, treat it as context only. Act on the current message section.

## NO_REPLY

`NO_REPLY` is a standalone complete response.

- If you have content, send only the content.
- If you have nothing to say, send only `NO_REPLY`.
- Never append `NO_REPLY` to content.

## Loop Safeguard

If two rounds of replies produce no new task, question, or decision, stop replying.
