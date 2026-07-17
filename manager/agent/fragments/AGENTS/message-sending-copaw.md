## Message Sending Rules

**CRITICAL**: When sending messages to Workers or admin during task execution:

- ✅ **ALWAYS USE**: `copaw channels send` CLI via shell tool
- ❌ **NEVER USE**: Direct `curl` to Matrix API (`/_matrix/client/v3/rooms/.../send/m.room.message`)

**Why**: Direct Matrix API calls bypass CoPaw's message formatting layer, resulting in messages without proper HTML rendering (`formatted_body`). The `copaw channels send` CLI ensures markdown is converted to HTML and mentions are properly structured.

**Example**:
```bash
copaw channels send \
  --agent-id default \
  --channel matrix \
  --target-user "@alice:matrix-local.agentteams.io:18080" \
  --target-session "!SQ2a5Er8Qtq9mM7RRR:matrix-local.agentteams.io:18080" \
  --text "@alice:matrix-local.agentteams.io:18080 Task assigned: Create README.md. Please file-sync to get task files."
```

**Note**: Your agent-id is always `default`.

**IMPORTANT - No Duplicate Messages to Admin**:
- ❌ **DO NOT** use `copaw channels send` to notify admin when you're **in an admin DM session**
- ✅ **ONLY** report to admin in your final reply (the message body you return)
- **Why**: When in admin DM, messages sent via CLI during thinking create duplicates - admin sees both the CLI message AND your final reply
- **Exception**: When processing Worker messages in a Worker/Project room, you MUST use `copaw channels send` with `resolve-notify-channel.sh` to notify admin in DM — your final reply goes to the Worker room, not admin

**Note**: The `matrix-server-management` skill's API reference shows raw Matrix API calls for administrative operations only. Do NOT use those examples for sending messages to Workers or admin.
