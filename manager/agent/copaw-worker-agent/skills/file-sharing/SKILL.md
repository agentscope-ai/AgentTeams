---
name: file-sharing
description: Use before direct filesync calls, reading non-task shared files, pushing mid-task progress, or troubleshooting missing shared files. Do not use for normal task acceptance or submission; taskflow ack_task and submit_task handle lifecycle sync internally.
---

# File Sharing

Use local shared paths only. Do not expose storage internals.

## Local Paths

Use:

- `shared/tasks/{task-id}/`
- `shared/projects/{project-id}/` only for read-only project context

Directory sync paths must end with `/`. If a directory pull fails, report the filesync result instead of creating local placeholder directories or switching to absolute paths.

Do not use in chat, task outputs, or normal reasoning:

- `agentteams/agentteams-storage/...`
- `teams/{team}/shared/...`
- `/root/agentteams-fs/...`
- `/root/agentteams-fs/agents/...`

## Task Lifecycle Sync

`taskflow(action="ack_task")` and `taskflow(action="submit_task")` handle all task lifecycle file sync internally (pull, push, stat). Do not call `filesync` separately for these operations.

## Non-Task Shared Files

For shared files outside the task lifecycle (project context, reference materials), call `filesync` before reading:

```json
{
  "action": "pull",
  "payload": {
    "path": "shared/projects/{project-id}/"
  }
}
```

To push mid-task progress or non-result files that the coordinator needs before submission:

```json
{
  "action": "push",
  "payload": {
    "path": "shared/tasks/{task-id}/progress/",
    "exclude": ["spec.md", "base/"]
  }
}
```

## If You Cannot Find Files

1. Call `filesync` with `action="pull"` for `shared/tasks/{task-id}/`.
2. Check `pwd`, then check the local relative path from the task message:

   ```bash
   pwd
   ls -la
   ls -la shared/tasks/{task-id}/
   ```

3. If still missing, @mention your coordinator with the `filesync` outcome and the exact local path you checked:

```text
@coordinator:domain BLOCKED: I pulled shared/tasks/{task-id}/ but cannot find shared/tasks/{task-id}/spec.md.
```

Do not search random container absolute paths or create the missing task directory yourself.
