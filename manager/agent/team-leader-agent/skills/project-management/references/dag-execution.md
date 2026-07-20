# DAG Execution

Use this reference to design the input for `projectflow(action="plan_dag")`.

## Goal

Build a DAG that turns the requester goal into independently executable Worker tasks with explicit dependencies.

The DAG is the current execution plan. It can be replaced when results, blockers, or requester changes make a different graph better.

## Input Shape

`plan_dag` receives the complete desired graph:

```json
{
  "action": "plan_dag",
  "payload": {
    "projectId": "{project-id}",
    "tasks": [
      {
        "taskId": "{project-id}-01",
        "title": "Short task title",
        "assignedTo": "<matrix-localpart>",
        "dependsOn": []
      },
      {
        "taskId": "{project-id}-02",
        "title": "Follow-up task title",
        "assignedTo": "<matrix-localpart>",
        "dependsOn": ["{project-id}-01"]
      }
    ]
  }
}
```

Each task node uses:

- `taskId`: stable node ID. Use `{projectId}-{seq}`.
- `title`: short human-readable work unit.
- `assignedTo`: Worker's **Matrix localpart** (the part between `@` and `:` in `matrixUserID`). Extract mechanically from `agt get workers --team "$TEAM_CR" -o json` output. Never strip, guess, or transform.
  - ❌ Do NOT use CLI `.name` field directly (may include deployment prefixes)
  - ❌ Do NOT strip prefixes yourself
  - Example: `@worker-issue-resolver:domain` → `worker-issue-resolver`
- `dependsOn`: list of task IDs that must produce accepted upstream results first.

Do not use `worker`, `owner`, `dependencies`, or short standalone IDs like `st-01`.

## Node Design

A good DAG node is:

- Owned by one Worker.
- Small enough to finish and report a clear result.
- Large enough to produce a useful artifact or decision.
- Written as an outcome, not a vague activity.
- Independent from sibling nodes unless `dependsOn` says otherwise.

Prefer titles like:

- `API contract design`
- `Backend CRUD implementation`
- `Frontend task list UI`
- `Integration test`

Avoid titles like:

- `Work on backend`
- `Help with frontend`
- `Continue project`

## Dependency Design

Use dependencies only when a downstream node needs an upstream result.

Common shapes:

- Sequential: design -> implementation -> test
- Fan-out: design -> backend and frontend in parallel
- Fan-in: backend + frontend -> integration test
- Verifier loop: implementation -> verification -> fix task if needed

Do not add dependencies just to control conversation order. Extra dependencies reduce parallelism and make recovery harder.

## Dynamic Replanning

When calling `plan_dag`, always send the complete graph you want to keep.

- Keep accepted nodes that still matter.
- Omit obsolete nodes.
- Add new nodes for changed scope, fixes, verification, or follow-up work.
- Do not mutate old submitted tasks; create new nodes for new work.

`plan_dag` does not resume a paused Project, accept task results, or delegate work. Use lifecycle, result gate, and task-management actions for those steps.

## Ready Nodes

After planning, call `ready_nodes` only when the Project is active.

`ready_nodes` returns pending nodes whose dependencies are satisfied. Delegate returned nodes through `task-management`.
