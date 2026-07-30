---
name: file-sharing
description: Use before filesync calls for project directories, global-shared inputs, non-task shared files, listing shared paths, or troubleshooting missing files. Do not use for task-level sync; taskflow delegate_task and check_task handle task directory sync internally.
---

# File Sharing

Use this skill for shared files. Do not expose storage internals to Workers.

## Local Abstraction

Use local shared paths only:

- Team work: `shared/...`
- Manager/global input: `global-shared/...`

Directory sync paths must end with `/`, especially `shared/projects/{project-id}/` and `shared/tasks/{task-id}/`. If a directory pull fails, stop and report the sync failure; do not fall back to partial file pulls and then modify Project state.

Do not write these in chat or task specs:

- `agentteams/agentteams-storage/...`
- `teams/{team}/shared/...`
- `/root/agentteams-fs/...`
- `/root/agentteams-fs/agents/...`

## Team Task Files

Team task directories use:

```text
shared/tasks/{task-id}/
```

Ownership:

```text
shared/tasks/{task-id}/
├── meta.json      # Leader creates
├── spec.md        # Leader writes, Worker reads
├── base/          # Leader writes, Worker reads
├── result.md      # Worker writes through its runtime protocol, Leader reads
└── workspace/     # Worker writes
```

Tell Workers only local paths:

```text
Please read shared/tasks/{task-id}/spec.md and follow your Worker task participation skills.
```

## Task Directory Sync

`taskflow` handles task directory sync internally:

- `delegate_task` auto-pushes `shared/tasks/{task-id}/` after creation.
- `check_task` auto-pulls `shared/tasks/{task-id}/` before reading the result.

Do not call `filesync push/pull/stat` for task directories during delegation or result checking.

## Refresh Project Files

Before using `projectflow` to inspect an existing project after restart or heartbeat, refresh the project directory:

```json
{
  "action": "pull",
  "payload": {
    "path": "shared/projects/{project-id}/"
  }
}
```

`projectflow` reads local files only, so stale project files can produce stale ready-task decisions.

If this project directory pull fails, stop. Do not pull only `meta.json` or `plan.md` and continue to update DAG or Loop state.

## If Worker Cannot Find Files

Do not argue about absolute paths.

1. Verify the task was published.
2. Tell Worker: `Please pull shared/tasks/{task-id}/ with filesync, then read shared/tasks/{task-id}/spec.md.`
3. If it still fails after one retry, report a filesync/shared-directory issue.
