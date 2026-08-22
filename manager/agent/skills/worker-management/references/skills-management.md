# Worker Skills Management

Manager centrally manages all Worker skills. Canonical definitions live in `~/worker-skills/`. Assignments are tracked in `~/workers-registry.json`.

## Commands

```bash
# Push all skills for a worker
bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh --worker <name>

# Push a skill to all workers that have it (e.g., after updating the definition)
bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh --skill <skill-name>

# Add a new skill to a worker and push
bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh --worker <name> --add-skill <skill-name>

# Remove a skill (registry only; MinIO files remain until manually removed)
bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh --worker <name> --remove-skill <skill-name>

# Skip Matrix notification (e.g., worker not yet running)
bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh --worker <name> --no-notify
```

After pushing, the script notifies affected Workers via Matrix @mention to use `file-sync`. Workers' periodic 5-minute sync is a fallback.

## Adding a New Custom Skill

1. Create `~/worker-skills/<skill-name>/SKILL.md` (must include `name`, `description`, `assign_when` frontmatter). Place scripts under `scripts/`.
2. Assign to worker:
   ```bash
   bash /opt/hiclaw/agent/skills/worker-management/scripts/push-worker-skills.sh \
     --worker <name> --add-skill <skill-name>
   ```

## Key facts

- `file-sync`, `task-progress`, `project-participation` are default skills — always included, cannot be removed
- Skills are Manager-controlled: Workers cannot modify their own skills (local→remote sync excludes `skills/**`)
- After writing any file a Worker needs, always notify them to `file-sync`
