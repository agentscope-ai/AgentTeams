---
name: file-sync
description: Sync files with centralized object storage. Use when your coordinator or another Worker notifies you of file updates (config changes, task files, shared data, collaboration artifacts).
---

# File Sync (QwenPaw Worker)

## Shared files are auto-synced

The `shared/` directory under your workspace is mirrored from object storage at startup and on every sync cycle. Task and project files are available locally without a manual pull:

| Local path (auto-synced) |
|---|
| `shared/tasks/{task-id}/` |
| `shared/projects/{project-id}/` |

```bash
# Read the spec (already synced locally)
cat shared/tasks/{task-id}/spec.md
```

## Runtime config is reconciled — not hand-pulled

Your model, MCP, and channel config are applied by your worker daemon from the controller-projected `agents/${AGENTTEAMS_WORKER_NAME}/runtime/runtime.yaml` (desired-state loop, ~every 5 seconds). You do not `mc mirror` your own config down — your daemon owns it. If your coordinator says config changed, wait for the next reconcile; do not hand-edit `TEAMS.md` or `config/mcporter.json`.

## Push your results back (manual)

Push is manual. Use the helper script, which detects your team and pushes to the correct storage path:

```bash
bash skills/file-sync/scripts/push-shared.sh tasks/{task-id}/ --exclude "spec.md" --exclude "base/"
```

**When to push:**
- When you finish work: push results back.
- After each meaningful sub-step (see `task-progress`), so a session reset never loses work.

Always confirm to the sender after a push completes.

**Example workflow:**
```bash
# Coordinator: "New task [st-01]. Read shared/tasks/st-01/spec.md"
cat shared/tasks/st-01/spec.md

# ... do the work ...

# Push results
bash skills/file-sync/scripts/push-shared.sh tasks/st-01/ --exclude "spec.md" --exclude "base/"

# Confirm to coordinator
"Task complete. Results pushed."
```
