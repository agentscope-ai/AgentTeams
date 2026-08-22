# plan.md Format

```markdown
# Project: {title}

**ID**: {project-id}
**Status**: planning | active | completed
**Room**: {room-id}
**Created**: {ISO date}
**Confirmed**: {ISO date or "pending"}

## Team

- @manager:{domain} — Project Manager
- @{worker1}:{domain} — {role description}
- @{worker2}:{domain} — {role description}

## Task Plan

### Phase 1: {phase name}

- [ ] {task-id} — {task title} (assigned: @{worker}:{domain})
  - Spec: /root/hiclaw-fs/shared/tasks/{task-id}/spec.md
  - Result: /root/hiclaw-fs/shared/tasks/{task-id}/result.md

### Phase 2: {phase name}

- [ ] {task-id} — {task title} (assigned: @{worker}:{domain}, depends on: {task-id})
  - Spec: /root/hiclaw-fs/shared/tasks/{task-id}/spec.md
  - Result: /root/hiclaw-fs/shared/tasks/{task-id}/result.md
  - **On REVISION_NEEDED**: return to {task-id} | reassign to @{worker}

## Change Log

- {ISO datetime}: Project initiated
- {ISO datetime}: Plan confirmed by human
```

## Task status markers

- `[ ]` — pending (not yet started)
- `[~]` — in-progress (Worker is working)
- `[x]` — completed
- `[!]` — blocked (needs attention)
- `[→]` — revision in progress

**task-id** format: `task-YYYYMMDD-HHMMSS`

## On REVISION_NEEDED directive

For tasks that may require rework (reviews, QA, approvals), specify what happens:

| Directive | Meaning |
|-----------|---------|
| `return to {task-id}` | Revision task assigned to original task's assignee |
| `reassign to @{worker}` | Revision task assigned to specified worker |

## result.md Format

```markdown
# Task Result: {title}

**Task ID**: {task-id}
**Completed**: {ISO datetime}

## Outcome

**Status**: SUCCESS | SUCCESS_WITH_NOTES | REVISION_NEEDED | BLOCKED

## Summary

{Brief summary of what was done}

## Deliverables

{List of completed deliverables}

## Notes

{Any notes, issues, or suggestions}
```

| Status | When to use |
|--------|-------------|
| `SUCCESS` | Fully completed, no issues |
| `SUCCESS_WITH_NOTES` | Completed with suggestions for improvement |
| `REVISION_NEEDED` | Issues found requiring rework |
| `BLOCKED` | Cannot complete due to external blocker |
