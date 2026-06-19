# State Management (state.json)

Path: `~/state.json`

Single source of truth for active tasks. Heartbeat reads this instead of scanning all meta.json files.

**Always use task-management scripts to modify** — never edit manually. The scripts handle initialization, deduplication, watchdog snapshots, and atomic writes.

## Script reference

```bash
STATE_SCRIPT=/opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh
WATCHDOG_SCRIPT=/opt/hiclaw/agent/skills/task-management/scripts/check-progress-watchdog.sh
```

| When | Command |
|------|---------|
| Ensure file exists | `bash $STATE_SCRIPT --action init` |
| Assign finite task | `bash $STATE_SCRIPT --action add-finite --task-id T --title TITLE --assigned-to W --room-id R [--project-room-id P]` |
| Create infinite task | `bash $STATE_SCRIPT --action add-infinite --task-id T --title TITLE --assigned-to W --room-id R --schedule CRON --timezone TZ --next-scheduled-at ISO` |
| Finite task completed | `bash $STATE_SCRIPT --action complete --task-id T` |
| Infinite task executed | `bash $STATE_SCRIPT --action executed --task-id T --next-scheduled-at ISO` |
| Check finite task progress freshness | `bash $WATCHDOG_SCRIPT --task-id T` |
| Cache admin DM room | `bash $STATE_SCRIPT --action set-admin-dm --room-id R` |
| View active tasks | `bash $STATE_SCRIPT --action list` |

`admin_dm_room_id`: cached room ID for Manager-Admin DM. Set once via `set-admin-dm`, used by heartbeat to report to admin.

## Progress watchdog fields

`check-progress-watchdog.sh` updates the active finite task entry with:

| Field | Meaning |
|------|---------|
| `last_progress_at` | Time when you last saw a changed progress block |
| `last_progress_fingerprint` | Stable hash of the latest progress block |
| `stale_heartbeat_count` | Consecutive heartbeat checks with repeated or missing progress |
| `last_watchdog_action` | `progress_changed`, `progress_blocked`, `repeated_progress`, or `missing_progress` |
| `last_watchdog_checked_at` | Time when watchdog last checked this task |
| `last_progress_summary` | Heading of the latest progress block |

Watchdog output statuses:

| Status | Meaning | Heartbeat action |
|------|---------|------------------|
| `normal` | Latest progress block changed | Continue normal heartbeat handling |
| `blocked` | Latest progress block explicitly reports a blocker | Report the blocker to admin and ask only for missing decision/input if actionable |
| `repeated` | Latest progress block is unchanged | Ask for status on the first repeated cycle; escalate to admin when repeated again |
| `unknown` | No finite task or progress log was found | Ask the Worker to write progress or report a blocker; escalate if this repeats |

## Notification channel resolution

```bash
bash /opt/hiclaw/agent/skills/task-management/scripts/resolve-notify-channel.sh
```

Output: `{"channel": "dingtalk|matrix|none", "target": "...", "via": "primary-channel|admin-dm|none"}`

Priority: primary-channel.json (if confirmed, non-matrix) → state.json admin_dm_room_id → none.
