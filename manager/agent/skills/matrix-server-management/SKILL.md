---
name: matrix-server-management
description: Use for standalone Matrix admin requests (register users, create rooms, manage membership, upload files). Also use when you need to send a file to the admin via Matrix media upload. Do NOT use for Worker/project creation — those handle Matrix operations internally.
---

# Matrix Server Management

Manage the Tuwunel Matrix Homeserver at `${HICLAW_MATRIX_SERVER}`. User ID format: `@<username>:${HICLAW_MATRIX_DOMAIN}`.

## Gotchas

- **Workers IGNORE messages without `m.mentions`** — they have `requireMention: true`. You MUST include `m.mentions.user_ids` with the full Matrix user ID for Workers to process the message (MSC3952)
- **User IDs in body text and `m.mentions.user_ids` must match exactly** — partial or mismatched IDs cause silent failures
- **`trusted_private_chat` preset auto-joins all invited members** — no invite acceptance needed
- **Matrix accounts cannot be deleted via API** — reuse the same username on reset
- **Do NOT use this skill for Worker/project creation** — `create-worker.sh` and `create-project.sh` handle Matrix operations internally

## Operation Reference

| Situation | Read |
|---|---|
| Any Matrix API call (register, login, rooms, messages, mentions, file upload) | `references/api-reference.md` |
