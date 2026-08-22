---
name: task-management
description: Use when admin gives a task to delegate to a Worker, when a Worker reports task completion, when managing recurring scheduled tasks, or when you need to check worker availability.
---

# Task Management

## Gotchas

- **Don't let Workers hallucinate on unfamiliar domains** — when a task involves niche frameworks, complex APIs, or domain-specific workflows that the Worker likely lacks knowledge about, include a suggestion to use `find-skills` to search for relevant skills first. Skipping this leads to hallucinated code and wasted iterations
- **Delegation-first** — always prefer assigning to a Worker over doing it yourself. Only self-execute when admin explicitly says "do it yourself" or the task is within your management skills
- **Never @mention a Worker after recording infinite task execution** — this creates a rapid-fire loop (execute → report → trigger → execute → ...) that burns tokens continuously. Triggering happens only during heartbeat
- **Always use `manage-state.sh` to modify state.json** — never edit manually with jq. The script handles atomicity, deduplication, and initialization
- **Every task assigned to a Worker MUST be registered in state.json** — this includes coordination, research, review, and management tasks, not just coding tasks. If a task is missing from state.json, the Worker's container will be auto-stopped by idle timeout while still working
- **Always push task files to MinIO before notifying Worker** — Worker needs to file-sync to get the spec
- **Always pull task directory from MinIO before reading results** — Worker pushes results there
- **Read SOUL.md before composing notifications** — use the persona and language defined there
- **Infinite task status is always "active", never "completed"** — they repeat until explicitly cancelled

## Operation Reference

Read the relevant doc **before** executing. Do not load all of them.

| Situation | Read |
|---|---|
| Admin gives task, no Worker specified | `references/worker-selection.md` |
| Assign a one-off task or handle completion | `references/finite-tasks.md` |
| Create or manage a recurring scheduled task | `references/infinite-tasks.md` |
| Need to update state.json or resolve notification channel | `references/state-management.md` |
