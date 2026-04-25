# Worker Selection (Team Scope)

## Finding Available Team Workers

```bash
bash ./skills/team-task-management/scripts/find-team-worker.sh
```

Output includes per-worker: name, availability (idle/busy), role, skills, active tasks.

## Decision Flow

1. **Idle workers exist** → pick best match by role/skills
2. **All busy** → wait for current tasks to complete, or split work differently
3. **Worker unresponsive** → escalate to Manager

## Assignment Rules

- Always assign to idle workers first
- Match worker skills to sub-task requirements
- Distribute work evenly when possible
- Never assign to workers outside your team
