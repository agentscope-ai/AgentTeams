### 1. Read state.json

Read state.json (local only, no sync needed). If the file does not exist, initialize it first:

```bash
bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh --action init
cat ~/state.json
```

The `active_tasks` field in state.json contains all in-progress tasks (both finite and infinite). No need to iterate over all meta.json files.

**Ensure admin notification channel is available** (used in Step 7):

1. Check `admin_dm_room_id` in state.json. If `null`, discover it now:
   - List joined rooms, find the DM room with exactly 2 members: you and `@${AGENTTEAMS_ADMIN_USER}:${AGENTTEAMS_MATRIX_DOMAIN}`
   - Persist it:
     ```bash
     bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh \
       --action set-admin-dm --room-id "<discovered-room-id>"
     ```
2. Verify the channel is resolvable:
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/resolve-notify-channel.sh
   ```
   If the output shows `"channel": "none"`, the admin DM room discovery above may have failed — retry or log a warning.
