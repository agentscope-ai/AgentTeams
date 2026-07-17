---
name: task-progress
description: Use when executing a task (progress logging, plan updates), when resuming a task after session reset, or when managing task history. Covers progress log format and resume flow.
---

# Task Progress

## Gotchas

- **Push progress after every meaningful action** — don't batch updates; session resets can lose unpushed work.
- **Resume flow reads progress/ latest-first** — keep filenames as `YYYY-MM-DD.md` for correct sort order.

## Progress Log

After every meaningful action (completing a sub-step, hitting a problem, making a decision), append to:

```
shared/tasks/{task-id}/progress/YYYY-MM-DD.md
```

Format (append, don't overwrite):

```markdown
## HH:MM — {brief action title}

- What was done: ...
- Current state: ...
- Issues encountered: ...
- Next step: ...
```

Push the task directory after each update:

```bash
bash skills/file-sync/scripts/push-shared.sh tasks/{task-id}/ --exclude "spec.md" --exclude "base/"
```

## plan.md Template

Create `shared/tasks/{task-id}/plan.md` before starting work:

```markdown
# Task Plan: {task title}

**Task ID**: {task-id}
**Assigned to**: {your name}
**Started**: {ISO datetime}

## Steps

- [ ] Step 1: {description}
- [ ] Step 2: {description}

## Notes

(running notes — decisions, findings, blockers)
```

Update checkboxes immediately as you complete each step, and push after each update.

## Resume Flow

When asked to resume a task after a session reset:

1. Task files are already in `shared/tasks/{task-id}/` (auto-synced).
2. Read `spec.md`, `plan.md`, and the recent `progress/` files (latest first).
3. Continue work and append to today's `progress/YYYY-MM-DD.md`.
