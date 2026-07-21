# Management Skills — Quick Reference

Each skill has a full `SKILL.md` in `skills/<name>/`. The `description` field in each SKILL.md tells the system when to load it.

Available skills: `task-management`, `task-coordination`, `git-delegation-management`, `worker-management`, `agentteams-find-worker`, `project-management`, `channel-management`, `matrix-server-management`, `mcp-server-management`, `model-switch`, `worker-model-switch`, `file-sync-management`

## Mandatory Routing

- Load `agentteams-find-worker` when the admin is assigning work but has not specified which existing Worker should handle it, and you need to search Nacos for a suitable Worker to import.
- Load `agentteams-find-worker` when the admin explicitly says to import a Worker from the market, or gives a direct `nacos://...` package URI.
- Do not route ordinary Worker creation into `agentteams-find-worker` just because the admin mentions a template-like role. If the admin is simply asking you to create a Worker, use `worker-management` unless they explicitly want market import.
- Once the admin confirms a specific market/Nacos Worker to import, do not hand-create a replacement through `worker-management` unless search failed before confirmation or the admin explicitly asks for fallback.

## Skill Boundary

- `worker-management` is the primary skill for Worker lifecycle management: create, reset, start, stop, update skills, and other day-to-day Worker operations.
- `agentteams-find-worker` is an auxiliary skill for `worker-management`, used only when you need Nacos-backed Worker discovery or market import.
- The normal order is: decide whether the admin needs an ordinary Worker operation or a Nacos-backed import. Ordinary operations stay in `worker-management`; discovery/import scenarios temporarily load `agentteams-find-worker`.
- After a successful import, control returns to `worker-management` for later lifecycle operations on that Worker.

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
