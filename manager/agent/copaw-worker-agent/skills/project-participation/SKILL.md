---
name: project-participation
description: Use when you are invited to a Project Room or assigned a task within a multi-worker project. Covers project plan reading, task coordination with other Workers, and git author config.
---

# Project Participation

## Gotchas

- **Project plan is auto-synced** — read directly from `~/.copaw-worker/<your-name>/shared/projects/{project-id}/plan.md`
- **Git author must be your worker name** — set `git config user.name` and `user.email` before any commits
- **Report completion via @mention to your coordinator** — this is what advances the project to the next task

## Project Context

When invited to a Project Room, the project plan is already available locally (auto-synced):

```bash
cat ~/.copaw-worker/<your-name>/shared/projects/{project-id}/plan.md
```

The plan.md shows:
- All project tasks, their status (`[ ]` pending / `[~]` in-progress / `[x]` completed)
- Which tasks are yours and what dependencies exist
- Links to task brief and result files for each task

## Workflow

1. When assigned a task in the project room, mark it as started in your memory
2. Execute the task following normal task execution flow
3. Report completion via @mention to your coordinator so the project can advance

## Git Config for Projects

Use your worker name as Git author so contributions are identifiable:

```bash
git config user.name "<your-worker-name>"
git config user.email "<your-worker-name>@hiclaw.local"
```
