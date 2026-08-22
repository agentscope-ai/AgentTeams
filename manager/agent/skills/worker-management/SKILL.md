---
name: worker-management
description: Use when admin requests creating/resetting a worker, starting/stopping a worker, managing worker skills, enabling peer mentions, or opening a CoPaw console.
---

# Worker Management

## Quick Create (2 steps)

```bash
# 1. Write SOUL.md (REQUIRED)
mkdir -p /root/hiclaw-fs/agents/<NAME>
# Write SOUL.md with AI Identity + Role + Security sections (see references/create-worker.md)

# 2. Run create script
bash /opt/hiclaw/agent/skills/worker-management/scripts/create-worker.sh \
  --name <NAME> --skills <skill1>,<skill2>
# Add --runtime copaw for Python workers
# Add --remote for admin-managed deployment
```

> Full creation workflow (runtime selection, SOUL.md template, skill matching, post-creation greeting): read `references/create-worker.md`

## Gotchas

- **Worker name must be lowercase and > 3 characters** — Tuwunel stores usernames in lowercase; short names cause registration failures
- **`--remote` means "remote from Manager"** — which is actually LOCAL from the admin's perspective. Use it when admin says "local mode" / "run on my machine"
- **`file-sync`, `task-progress`, `project-participation` are default skills** — always included, cannot be removed
- **Never run `create-worker.sh` for imported workers** — `hiclaw-import.sh` already sets up everything, just start the container
- **Peer mentions cause loops if not briefed** — after enabling, explicitly tell Workers to only @mention peers for blocking info, never for acknowledgments
- **Always notify Workers to `file-sync` after writing files they need** — the 5-minute periodic sync is fallback only
- **Workers are stateless** — all state is in centralized storage. Reset = recreate config files
- **Matrix accounts persist in Tuwunel** (cannot be deleted via API) — reuse same username on reset

## Operation Reference

Read the relevant doc **before** executing. Do not load all of them.

| Admin wants to... | Read | Key script |
|---|---|---|
| Create a new worker | `references/create-worker.md` | `scripts/create-worker.sh` |
| Start/stop/check idle workers | `references/lifecycle.md` | `scripts/lifecycle-worker.sh` |
| Push/add/remove skills | `references/skills-management.md` | `scripts/push-worker-skills.sh` |
| Open/close CoPaw console | `references/console.md` | `scripts/enable-worker-console.sh` |
| Enable direct @mentions between workers | `references/peer-mentions.md` | `scripts/enable-peer-mentions.sh` |
| Get remote worker install command | `references/lifecycle.md` | `scripts/get-worker-install-cmd.sh` |
| Reset a worker | `references/create-worker.md` | `rm -rf` config dir + re-run `create-worker.sh` |
