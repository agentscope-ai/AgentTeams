---
name: task-progress
description: Track task progress with daily logs and a task history registry. Use when working on assigned tasks so your coordinator can see status even between messages.
---

# Task Progress (CLI-harness Worker)

You wake up fresh for every task message. Progress files are how continuity works.

## Daily progress log

Maintain `~/progress/YYYY-MM-DD.md` (append, do not rewrite):

```markdown
## HH:MM — {task-id or topic}
- did what
- decided what, why
- next step
```

The harness pushes workspace files to MinIO every few seconds, so logs survive restarts and your coordinator can read them.

## Task history registry

Keep `~/.cli-harness/task-history.json` (create if missing):

```json
[
  {"task_id": "st-01", "status": "in_progress", "started": "2026-01-01T09:00:00Z"},
  {"task_id": "st-01", "status": "completed", "started": "...", "finished": "...", "summary": "one line"}
]
```

Statuses: `in_progress` → `completed` | `blocked` | `cancelled`.

## Rules

- Update the daily log after every meaningful sub-step — before pushing results
- Mark a task `blocked` in the registry the moment you report a blocker; add the reason to the daily log
- Keep `plan.md` checkboxes in the task directory current so anyone reading the task sees status without asking you
