# Worker Skills Management

You centrally manage all Worker Skills. Canonical definitions live in `~/worker-skills/`. Worker status is available via `agt get workers`.

## Commands

```bash
# Safely import a new Skill ZIP and explicitly assign it to one Worker
bash /opt/agentteams/agent/skills/worker-management/scripts/install-worker-skill.sh \
  --worker <name> --archive <local-attachment-path.zip>

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

1. Create `~/worker-skills/<skill-name>/SKILL.md`. The `name` and `description` frontmatter fields are required. `assign_when` is optional and only makes the Skill eligible for automatic role matching during Worker creation. Place scripts under `scripts/`.
2. Assign to worker:
   ```bash
   bash /opt/agentteams/agent/skills/worker-management/scripts/push-worker-skills.sh \
     --worker <name> --add-skill <skill-name>
   ```

### From a chat attachment

When the admin sends a Worker Skill as a ZIP attachment, the attachment is available to you as a local file. Do not search the entire filesystem for it: use the local path in the incoming `FileContent`. If that path is unavailable, report that the attachment could not be read and ask the admin to resend it.

Pass that path directly to the supported installer:

```bash
bash /opt/agentteams/agent/skills/worker-management/scripts/install-worker-skill.sh \
  --worker <name> \
  --archive <local-path-from-FileContent>
```

The installer rejects unsafe paths, symlinks, special files, oversized archives, multiple Skill roots, missing `name` or `description`, and name/directory mismatches. It stages the validated complete Skill under `~/worker-skills/`, then delegates upload, Worker CR reconciliation, verification, and notification to `push-worker-skills.sh`.

An explicit assignment does not require `assign_when`. A missing `assign_when` means only that you must not select that Skill automatically from a Worker's role. If the field is present, the installer preserves it and reports that it is available for automatic matching.

Use `--replace` only when the admin explicitly asks you to replace an existing canonical Skill with the attached version. If the installer fails, report its error; do not manually extract, copy, upload, or update the Worker CR as a fallback.

Never install an attached archive into the Manager's own `~/skills/` directory.

## Key facts

- `file-sync`, `task-progress`, `project-participation` are default skills — always included, cannot be removed
- Skills are Manager-controlled: Workers cannot modify their own skills (local→remote sync excludes `skills/**`)
- After writing any file a Worker needs, always notify them to `file-sync`
