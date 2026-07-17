## QwenPaw Message CLI Reference

For QwenPaw runtime, use the following CLI command format to send messages:

```bash
copaw channels send \
  --agent-id default \
  --channel matrix \
  --target-user "<user_id for mentions>" \
  --target-session "<room_id>" \
  --text "<message content>"
```

Key parameters:
- `--agent-id`: Always `default` for Manager agent
- `--channel`: Always `matrix` for Matrix protocol
- `--target-user`: The Matrix user ID for @mentions (e.g., `@worker:matrix.domain`)
- `--target-session`: The Matrix room ID (e.g., `!roomid:matrix.domain`)
- `--text`: The message content

To query available sessions:
```bash
copaw chats list --agent-id default --channel matrix
```
