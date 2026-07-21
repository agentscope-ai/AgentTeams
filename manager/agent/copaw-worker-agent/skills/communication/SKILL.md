---
name: communication
description: Use before sending @mentions, deciding whether to reply, TASK_COMPLETED, BLOCKED, QUESTION, direct answers, loop prevention, or suppressing low-information acknowledgements.
---

# Communication

## Routing

Always reply directly in the current room. Do not use the `message` tool for cross-room sends — Workers do not communicate across rooms.

## Coordinator Identity

Your coordinator is the sender of the current task assignment message. Use that Matrix ID directly for @mentions. Do not call `organization` or `agt` CLI to look up your coordinator during standard task flows.

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

Exception: when a new assigned task arrives, `task-management` requires you to directly say in the current room that you received the message before task acceptance work starts. Do not turn it into a progress thread, and do not send repeated acknowledgements for the same task.

Before sending any @mention, remove all Matrix IDs from the message in your head. Send only if the remaining text contains a concrete completion, blocker, question, requested answer, or decision.

## History Context

If your message includes a history section, treat it as context only. Act on the current message section.

## Loop Safeguard

If two rounds of replies produce no new task, question, or decision, stop replying.
