# Message read status

An Agent reads a Matrix message when its input has been appended to the Agent's session and saved. Queue acceptance, a channel history buffer, model execution, and reply completion are separate milestones.

## Audience

New messages carry `com.agentteams.read_audience` with `version: 1` and a `members` array. Each entry has `user_id`, `display_name`, and `kind` (`agent`, `human`, or unresolved `member`). Capture joined membership before sending the message. Exclude the sender when displaying the result; later joins never expand the captured audience.

Structured `m.mentions.user_ids` restricts the display to those members. No user mentions, or `m.mentions.room: true`, uses the captured room audience. Names appearing only in message text do not count as mentions. Edits retain the original audience and require confirmation of the latest edit event.

## Exact receipts

Send an ordinary Matrix event with type and relation type `com.agentteams.session_receipt`:

```json
{
  "version": 1,
  "status": "read",
  "m.relates_to": {
    "rel_type": "com.agentteams.session_receipt",
    "event_id": "$original-message-or-latest-edit"
  }
}
```

The authenticated Matrix event sender is the acknowledging member. `received` advertises support for this message while it is still unread; `read` supersedes `received` regardless of arrival order. Missing receipt evidence from an Agent produces no status or member row. Never infer Agent reads from cumulative `m.read` markers or receipt-send timestamps.

Receipts contain identifiers and status only. Like standard Matrix receipts, they are unencrypted metadata visible to room members. They are not `m.room.message` events and must not trigger Agents, appear as conversation bubbles, advance conversation read anchors, or reorder room lists. Query the same relationship type for historical hydration, including pagination.

## QwenPaw integration

The Matrix plugin tracks the exact event IDs included in each input: the current message, retained history actually prepended, no-text/time-debounced inputs, and send-gate retriggers. Truncated or ignored messages are not added to that list.

The AgentScope `on_reply` middleware observes `REPLY_START`, which follows input validation and append and precedes reasoning. It saves the session with a pending-receipt list in `state.middle_context.agentteams_session_receipts`, then queues delivery. A failed checkpoint omits the receipt. Inference failure after this boundary does not undo a read.

Delivery uses a deterministic Matrix transaction ID derived from room, target event, and status. Failed sends retry in the background. Startup recovers pending reads from saved session files; the delivery loop never rewrites those files. Successful delivery removes the live pending entry so the runtime's normal session save can persist its removal. Crash recovery can resend the same transaction safely.

## Other Harness integrations

OpenClaw, Hermes, DeepSeek, Codex, and OpenHuman start the shared receipt worker from their Harness entrypoint. It advertises `received` using the authenticated Matrix stream and sends `read` only after observing native committed input evidence. A durable SQLite outbox retries failed deliveries using deterministic Matrix transaction IDs. The worker never reads message bodies to infer consumption and never rewrites native session files.

| Harness | Evidence that input entered its session |
| --- | --- |
| OpenClaw | Committed user transcript entries in the native SQLite store (including compressed rows), or legacy JSONL, with native `__openclaw.transport.channel` and `messageId` metadata |
| Hermes | Committed Matrix user rows and `platform_message_id` in the native session database; retained history IDs are explicitly linked to the containing input |
| DeepSeek | Native `user/message` event followed by a successful `sessions.flush`, before recording its exact source checkpoint |
| Codex | Exact turn and `userMessage` verified through `thread/read`, followed by a successful native session snapshot and journal checkpoint |
| OpenHuman | The native user input is appended, then atomically saved with its exact Matrix room/event before model execution |

Queued inputs, incomplete file writes, failed database transactions, failed snapshots, and truncated history do not establish read state. A later inference error does not revoke a successful input checkpoint. New outbound chat messages capture their audience before their native Matrix send; edits do not recapture membership.

Older clients remain compatible and have no Agent read status until they implement this contract. Historical messages without an audience have no new member-status entry. Human members use resolved Matrix read-event positions, separated by thread; a receipt's send time is not evidence of reading a later message.

Related: https://github.com/agentscope-ai/AgentTeams/issues/1619
