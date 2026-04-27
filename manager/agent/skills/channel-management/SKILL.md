---
name: channel-management
description: Use when determining sender identity in any room, managing trusted contacts, configuring the admin's primary notification channel, handling first-contact from a new channel, or escalating to admin across channels.
assign_when: Not assigned to workers — this is a manager-only capability
---

# Channel Management

Manages communication channels, admin identity recognition, trusted contacts, and primary channel configuration.

## Gotchas

- **Primary channel cannot be set to "matrix"** — Matrix DM is the default fallback. Use `--action reset` to revert to it
- **Unknown senders in group rooms must be silently ignored** — no response at all, until admin explicitly approves them as trusted contacts
- **Trusted contacts must never receive sensitive info** — no API keys, tokens, passwords, Worker credentials, or internal config. No management operations either
- **When calling `message` tool from a Matrix session, you MUST explicitly set `channel` and `target`** — otherwise the message goes to the current Matrix room instead of the primary channel
- **`to` field in primary-channel.json maps to `target` parameter in `message` tool** — pass the value directly, no transformation needed
- **First-contact protocol: always ask in admin's language** — match the language they used in their message
- **Task dispatch must go to Worker Room, not admin DM** — when assigning tasks to Workers, use the task-management skill's send protocol (runtime-aware: `hiclaw get managers -o json` for runtime, then message tool or `copaw channels send` per finite/infinite references). Never embed @worker task assignments in admin DM replies.

## Operation Reference

Read the relevant doc **before** executing. Do not load all of them.

| Situation | Read |
|---|---|
| Need to identify who sent a message (admin, worker, trusted contact, unknown) | `references/identity-and-contacts.md` |
| Add/remove trusted contacts | `references/identity-and-contacts.md` |
| Configure primary channel, send notifications, first-contact protocol, cross-channel escalation | `references/primary-channel.md` |
