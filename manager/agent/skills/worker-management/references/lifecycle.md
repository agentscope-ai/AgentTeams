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

# Delete a worker (stop + remove container + clean up lifecycle state)
bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh --action delete --worker <name>
```

## Delete Completion Boundary

After a delete, verify with one `hiclaw get workers` or `hiclaw get workers -o json`.
If the target Worker is absent, the deletion task is complete. Do not continue
probing Matrix room lists or trying ad hoc room cleanup commands. A room that is
still visible in Element/Matrix after the Worker is gone is usually client
history or cache; tell the admin to leave/hide it from the client UI if needed.

If verification is inconclusive, retry the same diagnostic command at most once.
After two empty, identical, or malformed results, stop running tools and report
the confirmed state instead of continuing a loop.

## start vs create

| Situation | Command |
|-----------|---------|
| Container stopped | `lifecycle-worker.sh --action start` — restarts existing container |
| Container not found | `create-worker.sh` — full registration flow |
| Worker needs reset | `create-worker.sh` — removes old, rebuilds |
| Worker permanently removed | `lifecycle-worker.sh --action delete` — stops, removes container, cleans lifecycle state |

## Changing Idle Timeout

Default: 720 minutes (12 hours). Change via:
```bash
jq '.idle_timeout_minutes = 60' ~/worker-lifecycle.json > /tmp/lc.json && mv /tmp/lc.json ~/worker-lifecycle.json
```

## Heartbeat Check (automated every 15 minutes)

1. Scan `/root/hiclaw-fs/shared/tasks/*/meta.json` for `"status": "assigned"` tasks
2. Ask each assigned Worker for status in their Room
3. If Worker confirms completion, update meta.json: `"status": "completed"`, fill `completed_at`
4. Assess capacity vs pending tasks
