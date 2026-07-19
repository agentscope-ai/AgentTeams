---
name: session-recovery
description: Use on Manager session start for rapid context restoration, or when a Worker is recreated and needs predecessor context injected.
---

# Session Recovery

Rapid context restoration for Manager sessions and Worker recreation.

## Manager Session Recovery

On every Manager session start, after reading SOUL.md and memory files (see AGENTS.md "Every Session"), also read the session snapshot:

```bash
cat ~/last-session-snapshot.json 2>/dev/null || echo "no snapshot"
```

The snapshot provides instant operational context without re-deriving:
- Active task count and list
- Open escalations (severity, summary)
- Worker health summary
- Deferred tasks awaiting dispatch
- Timestamp of last heartbeat

**If the snapshot exists and is recent (< 1 hour old)**: use it as your starting context. You still need to verify current state, but the snapshot tells you what to check first.

**If the snapshot is stale (> 1 hour) or missing**: fall back to the normal startup flow (read state.json, check workers, etc.).

## Worker Recreation Recovery

When a Worker is recreated (detected via `lifecycle-worker.sh --action ensure-ready` returning `recreated`):

1. The Worker's previous session context is lost
2. Check if the Worker had active tasks in state.json
3. If yes, re-send the task assignment with full context:
   ```
   @{worker}:{domain} Your container was recreated. You were working on task {task-id}: {title}. Please file-sync to get the spec: shared/tasks/{task-id}/spec.md. Resume from where you left off — check progress/ directory for your previous notes.
   ```
4. The Worker's MinIO data persists (spec.md, progress notes, partial results) — only container-local state is lost

## Session Snapshot

The snapshot is written automatically at the end of each heartbeat cycle by `session-snapshot.sh`. You don't need to call it manually — heartbeat handles it.

If you need to force a snapshot (e.g., before a risky operation):

```bash
bash /opt/hiclaw/agent/skills/session-recovery/scripts/session-snapshot.sh
```

## Gotchas

- **Snapshot is a cache, not source of truth** — always verify against state.json and worker status before acting
- **Don't skip normal startup** — snapshot supplements SOUL.md and memory files, doesn't replace them
- **Worker MinIO data survives recreation** — only container-local state (in-memory session, caches) is lost
- **Stale snapshots are ignored** — if the snapshot is > 1 hour old, treat it as missing
