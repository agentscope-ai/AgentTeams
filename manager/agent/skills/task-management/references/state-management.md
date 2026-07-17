# State Management (state.json)

Path: `~/state.json`

Single source of truth for active tasks. Heartbeat reads this instead of scanning all meta.json files.

**Always use `manage-state.sh` or `hiclaw manager-state` to modify** — never edit manually. The script delegates to `hiclaw manager-state` when the CLI is available; set `HICLAW_MANAGER_STATE_IMPL=shell` to force the shell fallback.

## Script reference

```bash
STATE_SCRIPT=/opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh
# Preferred (same actions, shared implementation):
# hiclaw manager-state --action list
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
| Verify deliverables | `bash $STATE_SCRIPT --action verify --task-id T` |

`verify` is shell-only: it runs `verify-output.sh` against the local task directory and prints JSON. It does not modify `state.json` (unlike other actions, it is not implemented in `hiclaw manager-state`).

See `references/finite-tasks.md` for default checks and the optional `verifiable_claims` schema in task `meta.json`.

`admin_dm_room_id`: cached room ID for Manager-Admin DM. Set once via `set-admin-dm`, used by heartbeat to report to admin.

## Notification channel resolution

```bash
bash /opt/hiclaw/agent/skills/task-management/scripts/resolve-notify-channel.sh
```

Output: `{"channel": "dingtalk|matrix|none", "target": "...", "via": "primary-channel|admin-dm|none"}`

Priority: primary-channel.json (if confirmed, non-matrix) → state.json admin_dm_room_id → none.
