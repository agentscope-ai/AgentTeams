---
name: file-sync
description: Sync files with centralized storage. Use when your coordinator or another Worker notifies you of file updates (config changes, task files, shared data, collaboration artifacts).
---

# File Sync (CLI-harness Worker)

## Pull config and skill updates

When your coordinator notifies you that your config has been updated (model switch, skill update), trigger an immediate pull:

```bash
python3 ~/skills/file-sync/scripts/cli-sync.py
```

This pulls `openclaw.json`, skills, and `shared/` from MinIO into your workspace. `SOUL.md` changes are picked up on your next task automatically (it is injected into every prompt).

**Automatic background sync:**
- Pulls run every 300 seconds (5 minutes); pushes of your workspace files run every few seconds
- Most config changes arrive without manual sync — use the script when told files changed urgently

## Sync task / shared files

The `shared/` directory is auto-mirrored from MinIO. Task and project files are at:

| Local path (auto-synced) |
|---|
| `shared/tasks/{task-id}/` |
| `shared/projects/{project-id}/` |

```bash
# Read the spec (already synced locally)
cat shared/tasks/{task-id}/spec.md

# Push your results back to MinIO (push is manual)
mc mirror ~/shared/tasks/{task-id}/ ${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/{task-id}/ --overwrite --exclude "spec.md" --exclude "base/"
```

**When to use:**
- When you finish work: push results back to MinIO
- When told files were updated urgently: run `cli-sync.py` for an immediate pull

Always confirm to the sender after push completes.

**`base/` is read-only** — never push to it; always exclude it from mirror commands.
