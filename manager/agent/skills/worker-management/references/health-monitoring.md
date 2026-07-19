# Worker Health Monitoring

## Overview

Health classification derives worker states from existing data sources without new infrastructure. The `worker-health-report.sh` script reads from:

- `worker-lifecycle.json` — container status, idle timestamps
- `state.json` — active tasks per worker, blocked status
- `hiclaw get workers -o json` — phase, lastHeartbeat, lastActiveAt from CRD status

## Health States

| State | Condition | Recommended Action |
|-------|-----------|-------------------|
| `working` | Has active tasks, container running, lastActiveAt < 30min | None — normal operation |
| `stalled` | Has active tasks, container running, lastActiveAt > 60min | Nudge with progress question |
| `zombie` | Container running but no heartbeat for >15min and no active tasks | Attempt ensure-ready; if still zombie, flag for admin |
| `idle` | Container running, no tasks, within idle timeout | None — will auto-stop via idle timeout |
| `blocked` | Has tasks with `status=blocked` in state.json | Check escalation; relay admin response |
| `stopped` | Container not running (expected or unexpected) | If unexpected, attempt ensure-ready |

## Thresholds

Default thresholds (configurable via script flags):

- **Stalled**: 60 minutes without activity while having active tasks
- **Zombie**: 15 minutes without heartbeat while having no tasks

These align with the AGENTS.md guidance: "Worker 30-minute timeout — be patient."

```bash
# Custom thresholds
bash /opt/hiclaw/agent/skills/worker-management/scripts/worker-health-report.sh \
  --stalled-threshold-min 90 --zombie-threshold-min 20
```

## Heartbeat Integration (Step 5b)

During heartbeat, after capacity assessment (Step 5):

```bash
bash /opt/hiclaw/agent/skills/worker-management/scripts/worker-health-report.sh
```

Parse the JSON output and act on each state:

### For `stalled` workers:

1. Check if already nudged this heartbeat cycle (avoid duplicate nudges)
2. Ensure container is running:
   ```bash
   bash /opt/hiclaw/agent/skills/worker-management/scripts/lifecycle-worker.sh \
     --action ensure-ready --worker {worker}
   ```
3. Send nudge message to Worker's room:
   ```
   @{worker}:{domain} Your task {task-id} appears stalled (no activity for {N} minutes). Are you blocked? Please report status or use [BLOCKED:<severity>] format if you need help.
   ```

### For `zombie` workers:

1. Attempt ensure-ready (may restart container)
2. If status is `recreated` or `failed`, flag for admin in Step 7 report
3. Do NOT assign new tasks to zombie workers

### For `blocked` workers:

1. Check if an escalation exists for the blocked task
2. If no escalation, raise one (see escalation-management skill)
3. If admin has responded, relay the answer to the Worker

## Output Format

```json
{
  "workers": {
    "alice": {
      "health": "working",
      "since": "2026-07-19T10:30:00Z",
      "detail": "2 active task(s), active 5min ago",
      "active_tasks": 2,
      "blocked_tasks": 0,
      "mins_since_active": 5
    },
    "bob": {
      "health": "stalled",
      "since": "2026-07-19T09:00:00Z",
      "detail": "1 active task(s), no activity for 90min",
      "active_tasks": 1,
      "blocked_tasks": 0,
      "mins_since_active": 90
    }
  },
  "summary": {
    "total": 4,
    "working": 2,
    "stalled": 1,
    "zombie": 0,
    "idle": 1,
    "blocked": 0,
    "stopped": 0
  }
}
```

## Relationship to Controller Health (Phase 2)

This script provides Manager-side health classification using available data. Phase 2 adds controller-level `HealthState` to Worker CRD status, which will:

- Provide authoritative health state without Manager polling
- Enable Prometheus metrics for health transitions
- Power the dashboard health panel

Until Phase 2 is deployed, this script is the primary health classification mechanism.
