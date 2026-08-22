# Matrix API Reference

All calls use `MATRIX_URL="${HICLAW_MATRIX_SERVER}"`.

## User Registration

Tuwunel uses single-step registration with a token (no UIAA flow).

```bash
# Register
curl -X POST ${MATRIX_URL}/_matrix/client/v3/register \
  -H 'Content-Type: application/json' \
  -d '{
    "username": "<USERNAME>",
    "password": "<PASSWORD>",
    "auth": {
      "type": "m.login.registration_token",
      "token": "'"${HICLAW_REGISTRATION_TOKEN}"'"
    }
  }'
# Response: { "user_id": "...", "access_token": "..." }

# Login
curl -X POST ${MATRIX_URL}/_matrix/client/v3/login \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "m.login.password",
    "identifier": {"type": "m.id.user", "user": "<USERNAME>"},
    "password": "<PASSWORD>"
  }'
```

## Room Management

### Create room (3-party: Human + Manager + Worker)

Use `trusted_private_chat` for auto-join. Override power levels: Admin + Manager = 100, Workers = 0.

```bash
curl -X POST ${MATRIX_URL}/_matrix/client/v3/createRoom \
  -H "Authorization: Bearer ${MANAGER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Worker: <NAME>",
    "topic": "Communication channel for <NAME>",
    "invite": [
      "@'"${HICLAW_ADMIN_USER}"':'"${HICLAW_MATRIX_DOMAIN}"'",
      "@<NAME>:'"${HICLAW_MATRIX_DOMAIN}"'"
    ],
    "preset": "trusted_private_chat",
    "power_level_content_override": {
      "users": {
        "@manager:'"${HICLAW_MATRIX_DOMAIN}"'": 100,
        "@'"${HICLAW_ADMIN_USER}"':'"${HICLAW_MATRIX_DOMAIN}"'": 100,
        "@<NAME>:'"${HICLAW_MATRIX_DOMAIN}"'": 0
      }
    }
  }'
```

### Send message (no mention)

```bash
curl -X PUT "${MATRIX_URL}/_matrix/client/v3/rooms/<ROOM_ID>/send/m.room.message/$(date +%s)" \
  -H "Authorization: Bearer ${MANAGER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"msgtype": "m.text", "body": "Hello..."}'
```

### Send message with @mention

Workers have `requireMention: true` — without `m.mentions`, they receive but IGNORE the message.

```bash
# Single mention
curl -X PUT "${MATRIX_URL}/_matrix/client/v3/rooms/<ROOM_ID>/send/m.room.message/$(date +%s)" \
  -H "Authorization: Bearer ${MANAGER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{
    "msgtype": "m.text",
    "body": "@<WORKER>:'"${HICLAW_MATRIX_DOMAIN}"' Your task: ...",
    "m.mentions": {
      "user_ids": ["@<WORKER>:'"${HICLAW_MATRIX_DOMAIN}"'"]
    }
  }'

# Multiple mentions
# Add all user IDs to both body text and m.mentions.user_ids array
```

### Upload and send a file

```bash
# 1. Upload
curl -X POST "${MATRIX_URL}/_matrix/media/v3/upload?filename=<FILENAME>" \
  -H "Authorization: Bearer ${MANAGER_TOKEN}" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @/path/to/file
# Response: {"content_uri": "mxc://<SERVER>/<MEDIA_ID>"}

# 2. Send as m.file message
curl -X PUT "${MATRIX_URL}/_matrix/client/v3/rooms/<ROOM_ID>/send/m.room.message/$(date +%s)" \
  -H "Authorization: Bearer ${MANAGER_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{
    "msgtype": "m.file",
    "body": "<FILENAME>",
    "url": "mxc://<SERVER>/<MEDIA_ID>"
  }'
```

Then reply in conversation with: `MEDIA: mxc://<SERVER>/<MEDIA_ID>`

Use `text/plain` for text files, `application/octet-stream` as safe fallback. The `mxc://` URI is permanent and accessible to all room members.

### List joined rooms

```bash
curl -s ${MATRIX_URL}/_matrix/client/v3/joined_rooms \
  -H "Authorization: Bearer ${MANAGER_TOKEN}" | jq
```

### Get room messages

```bash
curl -s "${MATRIX_URL}/_matrix/client/v3/rooms/<ROOM_ID>/messages?dir=b&limit=20" \
  -H "Authorization: Bearer ${MANAGER_TOKEN}" | jq
```
