---
name: file-sync
description: Sync files with centralized storage. Use when your coordinator or another Worker notifies you of file updates (config changes, task files, shared data, collaboration artifacts).
---

# File Sync (CoPaw Worker)

## Sync agent config files

When your coordinator notifies you that your config has been updated (e.g., model switch, skill update), trigger an immediate sync:

```bash
copaw-sync
```

This pulls `openclaw.json`, `SOUL.md`, `AGENTS.md`, and skills from MinIO and re-bridges the config. CoPaw automatically hot-reloads config changes within ~2 seconds.

**Automatic background sync (AUTO ALL):**
- Background sync runs every 60 seconds automatically
- On every cycle the **full worker prefix** is mirrored from MinIO — any new file your coordinator places in your directory is pulled automatically, no `copaw-sync` call needed
- Config files (`openclaw.json`, `config/mcporter.json`) and skills are force-updated with `--overwrite` on each cycle so coordinator changes always land
- The `shared/` directory is also fully mirrored every cycle

## Sync task / shared files

The `shared/` directory is automatically mirrored from MinIO at startup and every sync cycle to `~/.copaw-worker/<your-name>/shared/`. No manual pull is needed.

Task and project files are at:

| Local path (auto-synced) |
|---|
| `~/.copaw-worker/<your-name>/shared/tasks/{task-id}/` |
| `~/.copaw-worker/<your-name>/shared/projects/{project-id}/` |

```bash
# Read the spec (already synced locally)
cat ~/.copaw-worker/<your-name>/shared/tasks/{task-id}/spec.md

# Push your results back to MinIO (push is still manual)
mc mirror ~/.copaw-worker/<your-name>/shared/tasks/{task-id}/ ${HICLAW_STORAGE_PREFIX}/shared/tasks/{task-id}/ \
  --overwrite --exclude "spec.md" --exclude "base/"
```

**When to use:**
- When you finish work: push results back to MinIO
- When told files have been updated urgently: run `copaw-sync` to trigger an immediate pull (background auto-sync already runs every 60s, so explicit sync is rarely needed)

Always confirm to the sender after push completes.

**Example workflow:**
```bash
# Coordinator assigns task: "New task [task-20260309-120000]. Pull spec from MinIO."
# shared/ is already synced — just read the spec
cat ~/.copaw-worker/<your-name>/shared/tasks/task-20260309-120000/spec.md

# ... do the work ...

# Push results
mc mirror ~/.copaw-worker/<your-name>/shared/tasks/task-20260309-120000/ \
  ${HICLAW_STORAGE_PREFIX}/shared/tasks/task-20260309-120000/ \
  --overwrite --exclude "spec.md" --exclude "base/"

# Confirm to coordinator
"Task complete. Results pushed to MinIO."
```