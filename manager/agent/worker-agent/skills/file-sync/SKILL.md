---
name: file-sync
description: Sync files with centralized storage. Use when your coordinator or another Worker notifies you of file updates (config changes, task files, shared data, collaboration artifacts).
---

# File Sync

When your coordinator or another Worker notifies you that files have been updated in centralized storage (e.g., config changes, task briefs, shared data, collaboration artifacts), run:

```bash
hiclaw-sync
```

This pulls the latest files from centralized storage to your local workspace. OpenClaw automatically detects config changes and hot-reloads within ~300ms.

**When to use**: any time you are told that new files are available, configs have changed, or another agent has written something you need to read.

Always confirm to the sender after sync completes.

