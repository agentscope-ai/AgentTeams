---
name: file-sync-management
description: Use when you need to push files to MinIO for Workers to access, pull Worker output from MinIO, or when a Worker reports they've uploaded files. Also use when notifying Workers to file-sync after writing files they need.
---

# File Sync Management

## Gotchas

- **Local `/root/hiclaw-fs/` is NOT real-time synced** — you must explicitly pull from MinIO
- **After writing to `/root/hiclaw-fs/`, immediately push to MinIO** + notify Worker to file-sync via @mention
- **When Worker says they pushed files, pull before reading** — never assume local copy is current
- **`mc mirror` uses `--overwrite`; single file uses `mc cp`**

## Operation Reference

| Situation | Read |
|---|---|
| Pull/push files, sync commands, notification workflow | `references/sync-guide.md` |
