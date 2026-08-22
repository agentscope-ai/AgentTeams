---
name: task-coordination
description: Coordinate access to shared task directories using .processing marker files. Use before accessing a Worker's workspace to prevent conflicts when both Manager and Worker might modify files simultaneously.
---

# Task Coordination

This skill provides a general coordination mechanism for shared task directories. It prevents conflicts when both Manager and Workers need to access/modify the same task workspace.

## Problem

When a Worker delegates work to Manager (e.g., git operations), the Manager modifies the Worker's workspace. During this time, the Worker might also be modifying files, causing potential conflicts.

## Solution

Use `.processing` marker files to signal "work in progress". Any party (Worker or Manager) must check for this marker before modifying a task directory.

---

## Task Directory Structure

```
tasks/{task-id}/
├── workspace/          # Code workspace (shared between Worker and Manager)
├── notes/              # Worker's notes, plan.md, memory (not synced by Manager)
├── meta.json           # Task metadata
└── .processing         # Processing marker file (created when work in progress)
```

---

## The `.processing` Marker

### Format

Location: `tasks/{task-id}/.processing`

```json
{
  "processor": "manager",
  "started_at": "2026-02-25T10:30:00Z",
  "expires_at": "2026-02-25T10:45:00Z",
  "operation": "git-delegation"
}
```

### Fields

| Field | Description |
|-------|-------------|
| `processor` | Who is processing: `manager` or worker name |
| `started_at` | ISO 8601 timestamp when processing started |
| `expires_at` | ISO 8601 timestamp when marker expires (15 min default) |
| `operation` | What operation is in progress (optional) |

### Expiration

The marker auto-expires after 15 minutes (configurable). This prevents deadlocks if a process crashes without removing the marker.

---

## Coordination Protocol

### Before Modifying Task Directory

**Always follow this sequence:**

1. **Sync from MinIO first**:
   ```bash
   mc mirror "${HICLAW_STORAGE_PREFIX}/shared/tasks/${task_id}/" "/root/hiclaw-fs/shared/tasks/${task_id}/"
   ```

2. **Check for `.processing`**:
   ```bash
   bash /opt/hiclaw/agent/skills/task-coordination/scripts/check-processing-marker.sh "$task_id"
   ```
   - Exit code 0: Safe to proceed (no marker or expired)
   - Exit code 1: Processing in progress, do NOT modify

3. **If safe, create marker**:
   ```bash
   bash /opt/hiclaw/agent/skills/task-coordination/scripts/create-processing-marker.sh "$task_id" "manager"
   ```

4. **Perform modifications**

5. **Remove marker**:
   ```bash
   bash /opt/hiclaw/agent/skills/task-coordination/scripts/remove-processing-marker.sh "$task_id"
   ```

6. **Sync to MinIO**:
   ```bash
   mc mirror "/root/hiclaw-fs/shared/tasks/${task_id}/" "${HICLAW_STORAGE_PREFIX}/shared/tasks/${task_id}/" --overwrite
   ```

---

## Scripts

### check-processing-marker.sh

Check if a task directory is being processed.

```bash
bash /opt/hiclaw/agent/skills/task-coordination/scripts/check-processing-marker.sh <task-id>
```

**Exit codes:**
- 0: No marker or marker expired (safe to proceed)
- 1: Valid marker exists (do not modify)

### create-processing-marker.sh

Create a processing marker for a task.

```bash
bash /opt/hiclaw/agent/skills/task-coordination/scripts/create-processing-marker.sh <task-id> <processor-name> [timeout-mins]
```

**Parameters:**
- `task-id`: Task identifier (e.g., `task-20260225-103000`)
- `processor-name`: Who is processing (`manager` or worker name)
- `timeout-mins`: (Optional) Expiration timeout in minutes (default: 15)

### remove-processing-marker.sh

Remove the processing marker after work is done.

```bash
bash /opt/hiclaw/agent/skills/task-coordination/scripts/remove-processing-marker.sh <task-id>
```

---

## Integration Points

This coordination mechanism is used by:

1. **git-delegation-management**: Manager creates marker before git ops, removes after
2. **git-delegation** (Worker skill): Worker checks marker before modifying workspace

---

## Best Practices

1. **Always sync first**: Never assume local state is current
2. **Check before create**: Don't blindly create markers; check first
3. **Remove promptly**: Remove marker as soon as work completes
4. **Handle crashes**: The expiration mechanism handles unexpected failures
5. **Respect the marker**: Never modify a task directory with an active marker

