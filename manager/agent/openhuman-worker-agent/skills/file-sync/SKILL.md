---
name: file-sync
description: Sync files with centralized storage. Use when your coordinator or another Worker notifies you of file updates (config changes, task files, shared data, collaboration artifacts).
---

# File Sync (OpenHuman Worker)

## Sync agent config files

When your coordinator notifies you that your config has been updated (e.g., skill update, model switch), pull the latest from MinIO:

```bash
mc mirror --overwrite "${HICLAW_STORAGE_PREFIX}/agents/${HICLAW_WORKER_NAME}/" /home/openhuman/.openhuman/agent-config/
```

**Hot reload caveats:**
- OpenHuman Core reloads `SOUL.md` on the next message — no restart needed
- Skill changes in `/home/openhuman/.openhuman/skills/` are picked up on the next message as well
- Changes that affect `config.toml` (Matrix channel, LLM provider) require a container restart — your coordinator handles this when needed

**Automatic background sync:**
- Background sync runs every 60 seconds as a fallback
- Most file changes are picked up without manual sync

## Sync task / shared files

The `shared/` directory is automatically mirrored from MinIO at startup and every sync cycle. No manual pull is needed.

Task and project files are at:

| Local path (auto-synced) |
|---|
| `shared/tasks/{task-id}/` |
| `shared/projects/{project-id}/` |

```bash
# Read the spec (already synced locally)
cat /home/openhuman/.openhuman/shared/tasks/{task-id}/spec.md

# Push your results back to MinIO (push is still manual)
bash /home/openhuman/.openhuman/skills/file-sync/scripts/push-shared.sh tasks/{task-id}/ --exclude "spec.md" --exclude "base/"
```

The `push-shared.sh` script automatically detects your team and pushes to the correct MinIO path.

**When to use:**
- When you finish work: push results back to MinIO using `push-shared.sh`
- When told files have been updated urgently: run the manual mc mirror command above

Always confirm to the sender after push completes.

**Example workflow:**
```bash
# Coordinator assigns task: "New task [st-01]. Please file-sync and read shared/tasks/st-01/spec.md"
# Pull latest (if needed)
mc mirror --overwrite "${HICLAW_STORAGE_PREFIX}/agents/${HICLAW_WORKER_NAME}/" /home/openhuman/.openhuman/agent-config/

# Read the spec
cat /home/openhuman/.openhuman/shared/tasks/st-01/spec.md

# ... do the work ...

# Push results
bash /home/openhuman/.openhuman/skills/file-sync/scripts/push-shared.sh tasks/st-01/ --exclude "spec.md" --exclude "base/"

# Confirm to coordinator
"Task complete. Results pushed to MinIO."
```
