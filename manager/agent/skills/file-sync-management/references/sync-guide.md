# File Sync Guide

Workers push their output to MinIO. Your local `/root/agentteams-fs/` is NOT automatically synced — you must pull explicitly.

## Pull task directory (after Worker reports completion)

```bash
mc mirror ${AGENTTEAMS_STORAGE_PREFIX}/shared/tasks/{task-id}/ /root/agentteams-fs/shared/tasks/{task-id}/ --overwrite
cat /root/agentteams-fs/shared/tasks/{task-id}/result.md
```

## Pull single file (Worker gave you a path)

```bash
mc cp ${AGENTTEAMS_STORAGE_PREFIX}/<path-worker-gave-you> /root/agentteams-fs/<same-path>
```

## Pull directory

```bash
mc mirror ${AGENTTEAMS_STORAGE_PREFIX}/<dir>/ /root/agentteams-fs/<dir>/ --overwrite
```

## Push after writing files Workers need

```bash
# Single file
mc cp /root/agentteams-fs/<path> ${AGENTTEAMS_STORAGE_PREFIX}/<path>

# Directory
mc mirror /root/agentteams-fs/<dir>/ ${AGENTTEAMS_STORAGE_PREFIX}/<dir>/ --overwrite
```

Then notify the target Worker via Matrix @mention to run their file-sync skill.

## Rules

1. When you write files to `/root/agentteams-fs/`, always push to MinIO immediately, then @mention the target Worker to file-sync
2. When a Worker tells you they've pushed files, always pull from MinIO before reading — never assume local is up to date
3. If a local file is missing or stale after a Worker notification, pull from MinIO directly — do not wait for background sync
