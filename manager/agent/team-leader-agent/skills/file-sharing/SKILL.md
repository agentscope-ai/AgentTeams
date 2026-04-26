---
name: file-sharing
description: Use before any filesync call or Leader operation involving shared files, task directories, project directories, global-shared inputs, publishing specs, refreshing Worker results, checking result.md, listing shared paths, or troubleshooting missing files. Always use this skill when the message mentions shared/, global-shared/, spec.md, meta.json, result.md, workspace/, deliverables, push, pull, stat, list, or file visibility between Leader and Workers.
---

# File Sharing

Use this skill for shared files. Do not expose storage internals to Workers.

## Local Abstraction

Use local shared paths only:

- Team work: `shared/...`
- Manager/global input: `global-shared/...`

Do not write these in chat or task specs:

- `hiclaw/hiclaw-storage/...`
- `teams/{team}/shared/...`
- `/root/hiclaw-fs/...`
- `/root/.hiclaw-worker/...`

## Team Task Files

Team task directories use:

```text
shared/tasks/{task-id}/
```

Ownership:

```text
shared/tasks/{task-id}/
├── meta.json      # Leader writes, Worker reads
├── spec.md        # Leader writes, Worker reads
├── base/          # Leader writes, Worker reads
├── result.md      # Worker taskflow writes, Leader reads
└── workspace/     # Worker writes
```

Tell Workers only local paths:

```text
Please pull shared/tasks/{task-id}/ with filesync, then read shared/tasks/{task-id}/spec.md.
```

## Publish A Leader-Written Task File

Use the `filesync` tool. Do not send remote paths to Workers.

```json
{
  "action": "push",
  "payload": {
    "path": "shared/tasks/{task-id}/"
  }
}
```

Verify `spec.md` after publishing:

```json
{
  "action": "stat",
  "payload": {
    "path": "shared/tasks/{task-id}/spec.md"
  }
}
```

## Refresh Worker-Written Task Files

Before reading Worker results, refresh the local task directory from storage:

```json
{
  "action": "pull",
  "payload": {
    "path": "shared/tasks/{task-id}/"
  }
}
```

Verify `result.md` before reading it:

```json
{
  "action": "stat",
  "payload": {
    "path": "shared/tasks/{task-id}/result.md"
  }
}
```

Use this before checking:

- `shared/tasks/{task-id}/result.md`
- `shared/tasks/{task-id}/workspace/`
- any Worker deliverable under `shared/tasks/{task-id}/`

Do not decide that a Worker result is missing until after this refresh.

## Refresh Project Files

Before using `taskflow` to inspect an existing project after restart or heartbeat, refresh the project directory:

```json
{
  "action": "pull",
  "payload": {
    "path": "shared/projects/{project-id}/"
  }
}
```

`taskflow` reads local files only, so stale project files can produce stale ready-task decisions.

## If Worker Cannot Find Files

Do not argue about absolute paths.

1. Verify the task was published.
2. Tell Worker: `Please pull shared/tasks/{task-id}/ with filesync, then read shared/tasks/{task-id}/spec.md.`
3. If it still fails after one retry, report a filesync/shared-directory issue.
