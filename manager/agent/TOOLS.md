# Management Skills — Quick Reference

Each skill has a full `SKILL.md` in `skills/<name>/`. The `description` field in each SKILL.md tells the system when to load it.

Available skills: `task-management`, `task-coordination`, `git-delegation-management`, `worker-management`, `project-management`, `channel-management`, `matrix-server-management`, `mcp-server-management`, `model-switch`, `worker-model-switch`, `file-sync-management`

## Cross-Skill Combos

These workflows span multiple skills. Load them together when you hit these scenarios:

| Scenario | Skill chain | Why |
|----------|-------------|-----|
| Worker sends `git-request:` | `git-delegation` → `task-coordination` → `file-sync` | Must create `.processing` marker before git ops, remove after, then sync to MinIO |
| Admin gives a task | `task-management` → `worker-management` (check status) → `file-sync` (push spec) | If Worker is stopped, wake it first; after writing spec, push to MinIO and notify |
| Admin starts a multi-worker project | `project-management` → `task-management` → `worker-management` | Create project room, then assign tasks — check each Worker's status before assigning |
| Switch a Worker's model | `worker-model-switch` → `worker-management` (recreate container) | Script updates config, but container must be recreated to apply |
| Set up MCP server for Workers | `mcp-server-management` → `worker-management` (notify) | After server is live and verified, notify relevant Workers to file-sync |
| Worker unresponsive, admin wants recovery | `matrix-server-management` (create new room) → `worker-management` | Create fresh 3-person room to give Worker clean context |

> **Model switch cheat sheet:** Manager model → `model-switch`. Worker model → `worker-model-switch`. Never mix them up. Both require a restart after running the script.
>
> **⚠️ MANDATORY:** When switching any model, you MUST use the corresponding skill script. Do NOT call Higress API directly or manually edit config files. The scripts handle gateway testing, config patching, registry updates, and Worker notification.

---

Add local notes below — SSH aliases, API endpoints, environment-specific details that don't belong in SKILL.md.
