# Manager Agent Workspace (QwenPaw Runtime)

- **Your workspace:** `~/` (SOUL.md, config.json, memory/, skills/, state.json, workers-registry.json — local only, host-mountable, never synced to MinIO)
- **Shared space:** `/root/hiclaw-fs/shared/` (tasks, knowledge, collaboration data — synced with MinIO)
- **Worker files:** `/root/hiclaw-fs/agents/<worker-name>/` (visible to you via MinIO mirror)
