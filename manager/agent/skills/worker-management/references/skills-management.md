# Worker Skills Management

Manager centrally manages all Worker skills. Canonical definitions live in `~/worker-skills/`. Worker status is available via `agt get workers`.

## Commands

```bash
# Push all skills for a worker
bash /opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh --worker <name>

# Push a skill to all workers that have it (e.g., after updating the definition)
bash /opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh --skill <skill-name>

# Add a new skill to a worker and push
bash /opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh --worker <name> --add-skill <skill-name>

# Remove a skill from Worker.spec.skills
bash /opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh --worker <name> --remove-skill <skill-name>

# Skip Matrix notification (e.g., worker not yet running)
bash /opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh --worker <name> --no-notify
```

After pushing, the script notifies affected Workers via Matrix @mention to use `file-sync`. Workers' periodic 5-minute sync is a fallback.

## Adding a New Custom Skill

1. Create `~/worker-skills/<skill-name>/SKILL.md` (must include `name`, `description`, `assign_when` frontmatter). Place scripts under `scripts/`.
2. Assign to worker:
   ```bash
   bash /opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh \
     --worker <name> --add-skill <skill-name>
   ```

### From a chat attachment

When the admin sends a Worker Skill as a ZIP attachment, the attachment is available to you as a local file. Do not search the entire filesystem for it: use the local path in the incoming `FileContent`. If that path is unavailable, report that the attachment could not be read and ask the admin to resend it.

Before assigning the Skill:

1. Inspect the ZIP without extracting it. Reject absolute paths, `..` traversal, symlinks, or archives containing more than one Skill root.
2. Extract into a temporary directory, never directly into `~/worker-skills/`.
3. Locate `SKILL.md`, validate its `name`, `description`, and `assign_when` frontmatter, and require the Skill name to match `^[A-Za-z0-9][A-Za-z0-9._-]*$`.
4. Copy the complete validated Skill root, including optional `scripts/` and `references/`, to `~/worker-skills/<skill-name>/`.
5. Run `push-worker-skills.sh --worker <name> --add-skill <skill-name>`, then query the Worker and report the final assignment.

Never install an attached archive into the Manager's own `~/skills/` directory.

## Key facts

- `file-sync`, `task-progress`, `project-participation` are default skills — always included, cannot be removed
- Skills are Manager-controlled: Workers cannot modify their own skills (local→remote sync excludes `skills/**`)
- After writing any file a Worker needs, always notify them to `file-sync`
