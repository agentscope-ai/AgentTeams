# Worker Lifecycle Management

The Manager automatically stops idle Workers and wakes them when assigning tasks. State is persisted in `~/worker-lifecycle.json` (local only, never synced to MinIO).

## Commands

```bash
# Sync all Worker container statuses
bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh --action sync-status

# Check for idle Workers and auto-stop those exceeding timeout
bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh --action check-idle

# Ensure a Worker is ready (auto-start if stopped, auto-recreate if missing)
bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh --action ensure-ready --worker <name>

# Manually stop/start
bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh --action stop --worker <name>
bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh --action start --worker <name>
```

## start vs create

| Situation | Command |
|-----------|---------|
| Container stopped | `lifecycle-worker.sh --action start` — restarts existing container |
| Container not found | `create-worker.sh` — full registration flow |
| Worker needs reset | `create-worker.sh` — removes old, rebuilds |
| Remote worker | Admin runs install command on target machine |

## Changing Idle Timeout

Default: 720 minutes (12 hours). Change via:
```bash
jq '.idle_timeout_minutes = 60' ~/worker-lifecycle.json > /tmp/lc.json && mv /tmp/lc.json ~/worker-lifecycle.json
```

## Get Remote Worker Install Command

```bash
bash /opt/hiclaw/agent/skills/worker-management/scripts/get-worker-install-cmd.sh --worker <name>
```

Provide the `install_cmd` **verbatim in a code block** — do NOT redact any values.

## Heartbeat Check (automated every 15 minutes)

1. Scan `/root/hiclaw-fs/shared/tasks/*/meta.json` for `"status": "assigned"` tasks
2. Ask each assigned Worker for status in their Room
3. If Worker confirms completion, update meta.json: `"status": "completed"`, fill `completed_at`
4. Assess capacity vs pending tasks
