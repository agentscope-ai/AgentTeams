---
name: file-sharing
description: Use when creating, publishing, refreshing, or troubleshooting shared files.
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
├── plan.md        # Worker writes
├── result.md      # Worker writes, Leader reads
└── workspace/     # Worker writes
```

Tell Workers only local paths:

```text
Please file-sync and read shared/tasks/{task-id}/spec.md.
```

## Publish A Leader-Written Task File

Use the storage implementation only inside commands. Do not send remote paths to Workers.

```bash
mc cp shared/tasks/{task-id}/meta.json ${HICLAW_STORAGE_PREFIX}/teams/{team}/shared/tasks/{task-id}/meta.json
mc cp shared/tasks/{task-id}/spec.md ${HICLAW_STORAGE_PREFIX}/teams/{team}/shared/tasks/{task-id}/spec.md
mc stat ${HICLAW_STORAGE_PREFIX}/teams/{team}/shared/tasks/{task-id}/spec.md
```

## Refresh Worker-Written Task Files

Before reading Worker results, refresh the local task directory from storage:

```bash
mkdir -p shared/tasks/{task-id}
mc mirror ${HICLAW_STORAGE_PREFIX}/teams/{team}/shared/tasks/{task-id}/ shared/tasks/{task-id}/ --overwrite
```

Use this before checking:

- `shared/tasks/{task-id}/result.md`
- `shared/tasks/{task-id}/plan.md`
- `shared/tasks/{task-id}/workspace/`
- any Worker deliverable under `shared/tasks/{task-id}/`

Do not decide that a Worker result is missing until after this refresh.

## If Worker Cannot Find Files

Do not argue about absolute paths.

1. Verify the task was published.
2. Tell Worker: `Please run file-sync, then read shared/tasks/{task-id}/spec.md.`
3. If it still fails after one retry, report a file-sync/shared-directory issue.
