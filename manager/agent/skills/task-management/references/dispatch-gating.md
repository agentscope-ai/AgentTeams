# Dispatch Gating

## Overview

Capacity-controlled dispatch prevents overwhelming the AI gateway by limiting concurrent worker tasks. The `dispatch-gate.sh` script implements:

- **Max concurrent workers**: Limit total workers with active tasks (0 = unlimited)
- **Max tasks per worker**: Limit tasks assigned to a single worker
- **Circuit breaker**: Per-worker failure tracking with automatic cooldown

## Configuration

Config file: `~/dispatch-config.json` (created on first use with defaults):

```json
{
  "max_concurrent_workers": 0,
  "max_tasks_per_worker": 2,
  "circuit_breaker_threshold": 3,
  "circuit_breaker_cooldown_min": 30
}
```

| Setting | Default | Description |
|---------|---------|-------------|
| `max_concurrent_workers` | 0 (unlimited) | Max workers with active tasks simultaneously |
| `max_tasks_per_worker` | 2 | Max concurrent tasks per worker |
| `circuit_breaker_threshold` | 3 | Failures before circuit opens |
| `circuit_breaker_cooldown_min` | 30 | Cooldown period after circuit opens |

## When to Call the Gate

**Before every task assignment** in the finite-tasks workflow (Step 0):

```bash
bash /opt/hiclaw/agent/skills/task-management/scripts/dispatch-gate.sh \
  --action check --worker {worker-name}
```

Parse the JSON output:
- If `allowed: true` → proceed with task assignment
- If `allowed: false` → defer the task (see Deferred Tasks below)

## YOLO Mode Behavior

In YOLO mode (`AGENTTEAMS_YOLO=1` or `~/yolo-mode` exists), the gate is **advisory only**:
- Log a warning if dispatch is denied
- Proceed with assignment anyway
- The admin has delegated full authority and is unreachable

## Circuit Breaker Semantics

The circuit breaker is **per-worker**, not global:

1. Each failed task assignment increments the worker's failure count
2. When count reaches `circuit_breaker_threshold`, the circuit opens
3. While open, dispatch to that worker is denied
4. After `circuit_breaker_cooldown_min`, the circuit auto-resets
5. Successful task completion resets the failure count

Record failures when:
- Worker container fails to start (`ensure-ready` returns `failed`)
- Worker repeatedly reports `[BLOCKED:CRITICAL]` on the same task
- Task times out without any progress

Reset failures when:
- Task completes successfully
- Admin explicitly confirms worker is healthy

```bash
# Record a failure
bash /opt/hiclaw/agent/skills/task-management/scripts/dispatch-gate.sh \
  --action record-failure --worker {worker}

# Reset after success
bash /opt/hiclaw/agent/skills/task-management/scripts/dispatch-gate.sh \
  --action reset-failures --worker {worker}
```

## Deferred Tasks

When dispatch is denied, defer the task:

1. Add to `state.json` with `"deferred": true`:
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/manage-state.sh \
     --action add-finite --task-id {task-id} --title "{title}" \
     --assigned-to {worker} --room-id {room-id}
   # Then manually add "deferred": true to the entry (or track in memory)
   ```

2. During heartbeat Step 5c, check if deferred tasks can now be dispatched:
   ```bash
   bash /opt/hiclaw/agent/skills/task-management/scripts/dispatch-gate.sh \
     --action check --worker {worker}
   ```
   If now allowed, proceed with the original assignment flow.

## Status Check

View current dispatch state:

```bash
bash /opt/hiclaw/agent/skills/task-management/scripts/dispatch-gate.sh --action status
```

Output:
```json
{
  "config": {
    "max_concurrent_workers": 4,
    "max_tasks_per_worker": 2,
    "circuit_breaker_threshold": 3,
    "circuit_breaker_cooldown_min": 30
  },
  "current": {
    "total_active_workers": 3,
    "capacity_available": 1
  },
  "circuit_breakers": [
    {"worker": "bob", "count": 3, "last_failure": "2026-07-19T10:00:00Z"}
  ]
}
```

## Configuration Changes

```bash
# View current config
bash /opt/hiclaw/agent/skills/task-management/scripts/dispatch-gate.sh --action config --show

# Set max concurrent workers
bash /opt/hiclaw/agent/skills/task-management/scripts/dispatch-gate.sh \
  --action config --set-max-concurrent 5

# Set max tasks per worker
bash /opt/hiclaw/agent/skills/task-management/scripts/dispatch-gate.sh \
  --action config --set-max-per-worker 3
```

## Relationship to Controller (Phase 2)

This script provides Manager-side dispatch gating. Phase 2 may add controller-level enforcement via a `DispatchGovernor` that:
- Tracks active worker count authoritatively
- Provides `POST /api/v1/dispatch/acquire` for hard enforcement
- Enables Prometheus metrics for dispatch denials

Until Phase 2, this script is the primary dispatch control mechanism.
