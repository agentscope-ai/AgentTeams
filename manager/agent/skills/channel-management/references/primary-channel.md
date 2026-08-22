# Primary Channel & Messaging

## Primary Channel State

File: `~/primary-channel.json`

- `confirmed`: `true` = use this channel for proactive notifications; `false` = Matrix DM fallback
- `channel`: channel identifier for the `message` tool (e.g. `"discord"`, `"telegram"`, `"slack"`, `"feishu"`)
- `to`: recipient identifier for the `message` tool's `target` parameter. Format varies:
  - Discord DM: `user:USER_ID`
  - Feishu DM: `ou_` 开头的 open_id；群聊用 `oc_` 开头的 chat_id
  - Telegram: chat ID (e.g. `123456789`)
  - WhatsApp/Signal: phone number (e.g. `+15551234567`)
- `sender_id`: admin's raw ID on that channel (for identity recognition)
- `channel_name`: human-readable name (e.g. `"Discord"`, `"飞书"`)

### Script commands

```bash
SCRIPT=/opt/hiclaw/agent/skills/channel-management/scripts/manage-primary-channel.sh

# Show current state
bash $SCRIPT --action show

# Confirm primary channel
bash $SCRIPT --action confirm --channel "<ch>" --to "<to>" \
  --sender-id "<sid>" --channel-name "<Name>"

# Reset to Matrix DM fallback
bash $SCRIPT --action reset
```

## Sending Messages

Use the built-in `message` tool for proactive notifications (reminders, heartbeat, task updates, escalations).

1. Read `primary-channel.json` via `--action show`
2. If `confirmed: true` and channel is not `"matrix"`:
   - `channel` → `.channel` from file
   - `target` → `.to` from file
   - `message` → your notification text
3. If `confirmed: false`, channel is `"matrix"`, or file missing → fall back to Matrix DM

The `message` tool is built-in to OpenClaw — no HTTP calls needed. When calling from a Matrix session, you MUST explicitly set `channel` and `target`, otherwise the message goes to the current Matrix room.

## First-Contact Protocol

Trigger: admin sends DM from a channel that doesn't match `primary-channel.json`.

1. Read current state via `--action show`
2. Respond to admin's message normally
3. Ask (in admin's language): "Would you like to set [Channel Name] as your primary channel for notifications?"
4. On "yes" → `--action confirm`; on "no" → `--action reset`; no reply → leave unchanged

## Changing Primary Channel

When admin requests a switch: read current state, update with `--action confirm`, confirm in admin's language.

## Cross-Channel Escalation

When blocked on an admin decision while working in a Matrix room:

1. Resolve notification channel:
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/resolve-notify-channel.sh
   ```
2. If non-Matrix primary channel confirmed, use `message` tool to send the question
3. Note the pending escalation in memory to connect the reply back when it arrives
4. When admin replies, continue the blocked workflow in the original Matrix room and @mention relevant workers

Fallback: if no primary channel, @mention admin in the current Matrix room.
