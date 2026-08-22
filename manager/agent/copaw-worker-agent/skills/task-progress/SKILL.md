---
name: task-progress
description: Use when executing a task (progress logging, plan updates), when resuming a task after session reset, or when managing task history. Covers progress log format, task-history.json, and resume flow.
---

# Task Progress

## Gotchas

- **Push progress log after every meaningful action** — don't batch updates; session resets can lose unpushed work
- **task-history.json is LRU top 10** — overflow goes to `~/.copaw-worker/<your-name>/.copaw/history-tasks/{task-id}.json`
- **Resume flow reads progress/ latest-first** — keep filenames as `YYYY-MM-DD.md` for correct sort order

## Progress Log

After every meaningful action (completing a sub-step, hitting a problem, making a decision), append to:

```
~/.copaw-worker/<your-name>/shared/tasks/{task-id}/progress/YYYY-MM-DD.md
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
mc mirror ~/.copaw-worker/<your-name>/shared/tasks/{task-id}/ ${HICLAW_STORAGE_PREFIX}/shared/tasks/{task-id}/ --overwrite --exclude "spec.md" --exclude "base/"
```

## Task History (LRU Top 10)

File: `~/.copaw-worker/<your-name>/.copaw/task-history.json`

```json
{
  "updated_at": "2026-02-21T15:00:00Z",
  "recent_tasks": [
    {
      "task_id": "task-20260221-100000",
      "brief": "One-line description of the task",
      "status": "in_progress",
      "task_dir": "~/.copaw-worker/<your-name>/shared/tasks/task-20260221-100000",
      "last_worked_on": "2026-02-21T15:00:00Z"
    }
  ]
}
```

Rules:
- **New task assigned**: add to head of `recent_tasks`
- **Exceeds 10 entries**: move oldest to `~/.copaw-worker/<your-name>/.copaw/history-tasks/{task-id}.json`
- **Status changes**: update `status` field in `recent_tasks`

## Resume Flow

When your coordinator or admin asks you to resume a task after session reset:

1. Read `task-history.json`; if not there, check `history-tasks/{task-id}.json`
2. Get `task_dir` from the entry
3. Task files are already in `~/.copaw-worker/<your-name>/shared/tasks/{task-id}/` (auto-synced). If you need the very latest, run `copaw-sync`
4. Read `{task_dir}/spec.md`, `{task_dir}/plan.md`, and recent `{task_dir}/progress/` files (latest first)
5. Continue work and append to today's `progress/YYYY-MM-DD.md`
