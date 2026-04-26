---
name: file-sharing
description: Use before any filesync call or Worker operation involving shared task files, shared project context, task specs, task metadata, workspace files, deliverables, result verification, missing files, or publishing outputs. Always use this skill when the message mentions shared/, shared/tasks, shared/projects, spec.md, meta.json, result.md, workspace/, deliverables, pull, push, stat, list, missing spec, or file visibility between Worker and Leader.
---

# File Sharing

Use local shared paths only. Do not expose storage internals.

## Local Paths

Use:

- `shared/tasks/{task-id}/`
- `shared/projects/{project-id}/` only for read-only project context

Do not use in chat, task outputs, or normal reasoning:

- `hiclaw/hiclaw-storage/...`
- `teams/{team}/shared/...`
- `/root/hiclaw-fs/...`
- `/root/.hiclaw-worker/...`

## Pull Latest Files

When your coordinator assigns a task or mentions any `shared/...` path, call the `filesync` tool before reading files:

```json
{
  "action": "pull",
  "payload": {
    "path": "shared/tasks/{task-id}/"
  }
}
```

Then read local paths. For a task, the first file to read is always:

```bash
cat shared/tasks/{task-id}/spec.md
```

Do not decide that `shared/` or `spec.md` is missing until after `filesync(action="pull")` finishes.

## Push Task Results

After meaningful updates, push only your task directory:

```json
{
  "action": "push",
  "payload": {
    "path": "shared/tasks/{task-id}/",
    "exclude": ["spec.md", "meta.json", "base/"]
  }
}
```

After `taskflow(action=submit_task)` writes `result.md`, push the task directory and verify it exists remotely before reporting completion:

```json
{
  "action": "stat",
  "payload": {
    "path": "shared/tasks/{task-id}/result.md"
  }
}
```

Do not report `TASK_COMPLETED` until `stat` returns `ok=true`.

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
