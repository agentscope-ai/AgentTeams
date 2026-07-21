# Worker Selection (Step 0)

**Trigger**: Admin gives a task without naming a Worker or Team.

## Check for matching Team first

```bash
# List all teams
bash /opt/agentteams/agent/skills/team-management/scripts/manage-teams-registry.sh --action list
```

If the task matches a Team's domain/expertise, delegate to the Team Leader instead of an individual Worker. See `team-management/references/team-task-delegation.md`.

## Find available workers

```bash
# All workers with full availability info
bash /opt/agentteams/agent/skills/task-management/scripts/find-worker.sh

# Filter by required skills
bash /opt/agentteams/agent/skills/task-management/scripts/find-worker.sh --skills github-operations,git-delegation

# Filter by team membership
bash /opt/agentteams/agent/skills/task-management/scripts/find-worker.sh --team alpha-team
```

Output includes `summary` (idle/busy/stopped/unavailable counts) and `workers` array with: `availability`, `role` (from SOUL.md), `worker_role` (worker/team_leader), `team_id`, `skills`, `finite_tasks`, `infinite_tasks`, `active_tasks`, `container_status`.

## Decision flow (delegation-first)

1. **Task matches a Team** → delegate to Team Leader (preferred for complex, multi-skill tasks)
2. **Idle standalone workers exist** → pick best match by role + skills, present as Option A (strongly recommended)
3. **Only busy workers** → present workload info, suggest Option B (import/create a new Worker) or wait
4. **No workers at all** → suggest Option B. Only fall back to Option C if admin explicitly requests it

Options:
- **Option A** (preferred) — Assign to idle Worker
- **Option B** — Import a matching Worker template first; if none fits, create a new Worker (suggest name/role/skills/model)
- **Option C** (last resort) — Handle yourself. Only when admin explicitly says "do it yourself" or task is within your management skills (worker-management, mcp-server-management, model-switch)

Act on choice: A → ensure container ready then assign; B → create Worker then assign; C → work directly (no task directory needed).

**Skip Step 0 when**: admin names a Worker, says "do it yourself", or it's a heartbeat-triggered infinite task. In YOLO mode, the admin is unavailable — autonomously pick the best Worker or create one without asking.

## Before assigning: container readiness

The `find-worker.sh` output already includes `container_status` and `availability`:

- `idle` or `busy` → container running, assign directly
- `stopped` → wake up first: `lifecycle-worker.sh --action ensure-ready --worker <name>`
- `unavailable` → try `ensure-ready` first (attempts recreate); if `status=failed`, notify admin to recreate via `create-worker.sh`

If you already ran `find-worker.sh`, do NOT run a separate container check. Only run standalone check when assigning to an explicitly named Worker (Step 0 was skipped).

## When Option B means "import a template"

If the admin's requirement sounds reusable, or they explicitly mention templates, import, registry, or a direct package URI such as `nacos://...`, use `hiclaw-find-worker` before hand-creating a Worker:

```bash
bash /opt/agentteams/agent/skills/hiclaw-find-worker/scripts/hiclaw-find-worker.sh \
  --query "<admin requirement>" --limit 3 --json
```

If the admin already gave you a full package URI, skip the search and install it through `hiclaw-find-worker` after confirmation. Otherwise, recommend the best 1-3 matches, wait for confirmation, then install the chosen template via `hiclaw-find-worker`.

## Skills API URL (only when hand-creating a new Worker)

Check registry URL: `echo "${AGENTTEAMS_SKILLS_API_URL:-https://skills.sh}"`

`find-skills` is a builtin skill (always available). You can optionally ask admin for a custom registry URL and pass it to `create-worker.sh` via `--skills-api-url`.
