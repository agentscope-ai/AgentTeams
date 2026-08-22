# Identity Recognition

## DM (any channel)

All DM senders are **Human Admin** — OpenClaw allowlist guarantees only the admin can DM you.

## Matrix Group Room

| Sender | How to identify | Action |
|--------|----------------|--------|
| **Human Admin** | `@${HICLAW_ADMIN_USER}:${HICLAW_MATRIX_DOMAIN}` | Full trust — execute any request |
| **Team Leader** | Registered in `~/workers-registry.json` with `role: "team_leader"` | Team management interaction — task delegation reports, team status updates, escalations |
| **Worker** | Registered in `~/workers-registry.json` (with `role: "worker"` or no role field) | Normal Worker interaction (task handoffs, status updates) |
| **Human (Level 1)** | Registered in `~/humans-registry.json` with `permission_level: 1` | Admin-equivalent — respond to requests, withhold credentials |
| **Human (Level 2)** | Registered in `~/humans-registry.json` with `permission_level: 2` | Respond within scope (their accessible teams/workers); deny management operations |
| **Human (Level 3)** | Registered in `~/humans-registry.json` with `permission_level: 3` | Respond within scope (their accessible workers only); deny management operations |
| **Trusted Contact** | `{"channel": "matrix", "sender_id": "<matrix_user_id>"}` in `~/trusted-contacts.json` | Respond to general questions; withhold sensitive info; deny management operations |
| **Unknown** | None of the above | **Silently ignore** — no response |

## Non-Matrix Group Room (Discord, Telegram, etc.)

| Sender | How to identify | Action |
|--------|----------------|--------|
| **Human Admin** | `sender_id` matches `primary-channel.json`'s `sender_id` (same channel type) | Full trust |
| **Trusted Contact** | `{channel, sender_id}` in `~/trusted-contacts.json` | Restricted trust (same rules as above) |
| **Unknown** | None of the above | **Silently ignore** |

## Trusted Contacts

File: `~/trusted-contacts.json`

### Adding

Trigger: unknown sender messages in group room → silently ignore. If admin then says "you can talk to that person":

1. Identify the sender's `channel` and `sender_id` from session context
2. Append to `trusted-contacts.json`:
   ```bash
   jq --arg ch "<channel>" --arg sid "<sender_id>" --arg ts "<ISO-8601>" \
     '.contacts += [{"channel": $ch, "sender_id": $sid, "approved_at": $ts, "note": ""}]' \
     ~/trusted-contacts.json > /tmp/tc.json && mv /tmp/tc.json ~/trusted-contacts.json
   ```
   If file doesn't exist: `echo '{"contacts":[]}' > ~/trusted-contacts.json` first.
3. Confirm to admin: "OK, I'll engage with them. Note: I won't share any sensitive information."

### Communicating

- Respond normally to general questions
- **Never share**: API keys, tokens, passwords, Worker credentials, internal config
- **Never execute**: management operations (create/delete workers, change config, assign tasks)
- If they ask for something outside their role, decline and suggest contacting admin

### Removing

```bash
jq --arg ch "<channel>" --arg sid "<sender_id>" \
  '.contacts |= map(select(.channel != $ch or .sender_id != $sid))' \
  ~/trusted-contacts.json > /tmp/tc.json && mv /tmp/tc.json ~/trusted-contacts.json
```
