# State Management (state.json)

Path: `~/state.json`

Single source of truth for active tasks. Heartbeat reads this instead of scanning all meta.json files.

**Always use `manage-state.sh` to modify** — never edit manually. The script handles initialization, deduplication, and atomic writes.

## Script reference

```bash
STATE_SCRIPT=/opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh
```

| When | Command |
|------|---------|
| Ensure file exists | `bash $STATE_SCRIPT --action init` |
| Assign finite task | `bash $STATE_SCRIPT --action add-finite --task-id T --title TITLE --assigned-to W --room-id R [--project-room-id P]` |
| Create infinite task | `bash $STATE_SCRIPT --action add-infinite --task-id T --title TITLE --assigned-to W --room-id R --schedule CRON --timezone TZ --next-scheduled-at ISO` |
| Finite task completed | `bash $STATE_SCRIPT --action complete --task-id T` |
| Infinite task executed | `bash $STATE_SCRIPT --action executed --task-id T --next-scheduled-at ISO` |
| Cache admin DM room | `bash $STATE_SCRIPT --action set-admin-dm --room-id R` |
| View active tasks | `bash $STATE_SCRIPT --action list` |

`admin_dm_room_id`: cached room ID for Manager-Admin DM. Set once via `set-admin-dm`, used by heartbeat to report to admin.

## Notification channel resolution

```bash
bash /opt/hiclaw/agent/skills/task-management/scripts/resolve-notify-channel.sh
```

Output: `{"channel": "dingtalk|matrix|none", "target": "...", "via": "primary-channel|admin-dm|none"}`

Priority: primary-channel.json (if confirmed, non-matrix) → state.json admin_dm_room_id → none.
