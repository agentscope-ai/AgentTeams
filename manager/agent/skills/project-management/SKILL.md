---
name: project-management
description: Use when admin asks to start a multi-worker project, when a Worker @mentions you with task completion in a project room, when project plan changes are needed, or when a blocked task needs resolution.
---

# Project Management

A project has: a Project Room (Matrix), a `plan.md` (single source of truth), a `meta.json`, and individual task files under `shared/tasks/{task-id}/`.

```
shared/projects/{project-id}/
├── meta.json
└── plan.md
```

## Gotchas

- **Project room MUST always include the human admin** — non-negotiable. The script handles this, but if you ever create a room manually, always invite admin
- **plan.md is the single source of truth** — all task status, assignments, and dependencies live here. Always sync to MinIO after changes
- **Do NOT proceed to next phase while REVISION_NEEDED is pending** — revision must complete first
- **"All tasks complete" step is mandatory even in YOLO mode** — always update meta.json, plan.md, and notify admin
- **plan.md had duplicate sections in the old version** — use `references/plan-format.md` as the canonical format
- **Always adapt language to admin's preferred language** when posting in rooms or DMs
- **Always read SOUL.md before composing notifications** — use the persona and language defined there

## Operation Reference

Read the relevant doc **before** executing. Do not load all of them.

| Situation | Read |
|---|---|
| Admin asks to start a new project | `references/create-project.md` |
| Need to assign a task or handle completion | `references/task-lifecycle.md` |
| Need plan.md / result.md format | `references/plan-format.md` |
| Blocked task, plan changes, mid-project onboarding, headcount request, heartbeat monitoring | `references/plan-changes.md` |
