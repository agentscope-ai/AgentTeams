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

## Two-layer model (chat-flow vs Project CRD)

This skill's `meta.json`/`plan.md` are the **execution layer** — chat-flow tracking of phases,
tasks, and assignments, always created by `create-project.sh`. There is a **second, federated
layer**: the `Project` CRD (`hiclaw-controller`'s `/api/v1/projects`), which is **repo/access
provisioning** — which team owns the project, which Gitea repos are attached, and at what access
level (`rw`/`ro`). The two are linked only by the shared project id; there is no schema merge
between them.

Create the CRD layer by adding `--team <TEAM_NAME>` and one or more repeatable
`--repo <URL>:<rw|ro>` flags to the same `create-project.sh` call (`references/create-project.md`
Step 1b). Omit both flags when the project has no repo to provision (e.g. pure coordination/chat
work) — this remains the default and produces byte-identical output to a project with no CRD.
The CRD is applied via `hiclaw apply -f`, which POSTs/PUTs to the controller's Project REST
routes; the Manager container bundles the `hiclaw` CLI.

## Gotchas

- **Check YOLO mode BEFORE drafting the plan** — `[ "${AGENTTEAMS_YOLO:-}" = "1" ] || [ -f ~/yolo-mode ]`. In YOLO mode you MUST auto-confirm in `create-project.md` Step 1c; never post a "please confirm" question, the admin will not reply and the project stalls forever. See `references/create-project.md` Step 0.
- **Project room MUST always include the human admin** — non-negotiable. The script handles this, but if you ever create a room manually, always invite admin
- **plan.md is the single source of truth** — all task status, assignments, and dependencies live here. Always sync to MinIO after changes
- **Do NOT proceed to next phase while REVISION_NEEDED is pending** — revision must complete first
- **"All tasks complete" step is mandatory even in YOLO mode** — always update meta.json, plan.md, and notify admin
- **plan.md had duplicate sections in the old version** — use `references/plan-format.md` as the canonical format
- **Always adapt language to admin's preferred language** when posting in rooms or DMs
- **Always read SOUL.md before composing notifications** — use the persona and language defined there
- **`--team`/`--repo` only add the Project CRD, never replace chat-flow tracking** — `plan.md`/`meta.json` remain the source of truth for task status regardless; the CRD is repo/access provisioning only, not a second place to track phases
- **Cross-project CRD dependencies block assignment** — when a Project CR lists `spec.dependsOn`, check `status.dependencies` (via `GET /api/v1/projects/{name}` or list) before assigning the first task or advancing phases. Do not assign work while any dependency has `satisfied: false`. Satisfied means the dependency project phase is `Ready`, `Completed`, or `Archived`. This is CRD-level visibility only — it does not auto-wire into `plan.md` task DAGs.

## Operation Reference

Read the relevant doc **before** executing. Do not load all of them.

| Situation | Read |
|---|---|
| Admin asks to start a new project | `references/create-project.md` |
| Need to assign a task or handle completion | `references/task-lifecycle.md` |
| Need plan.md / result.md format | `references/plan-format.md` |
| Blocked task, plan changes, mid-project onboarding, headcount request, heartbeat monitoring | `references/plan-changes.md` |
