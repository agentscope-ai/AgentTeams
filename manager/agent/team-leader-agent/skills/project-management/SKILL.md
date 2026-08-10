---
name: project-management
description: Use before any projectflow call or Team Leader workflow involving Project state, Project lifecycle, DAG planning, Loop planning, ready node checks, pause/resume/complete, project recovery, heartbeat project checks, or project-level result aggregation. Always use team-coordination first when the question is how to organize the work.
---

# Project Management

You manage Project files, Project lifecycle, and Project execution plans. Use this skill as the Project state layer. Use `team-coordination` to decide the strategy, and use `task-management` to delegate or check individual Worker tasks.

Project state is tool-owned. Do not create, edit, delete, or repair `shared/projects/**` with shell commands, heredocs, direct file writes, `rm`, `mkdir`, `cp`, or Python module execution. Use `projectflow` actions only. If `projectflow` fails or returns inconsistent state, stop and report the blocker instead of manually patching files.

## Scope

Use this skill for:

- `projectflow` calls
- Project metadata and lifecycle state
- DAG and Loop execution plans
- ready-node resolution
- Project pause, resume, and completion
- heartbeat and recovery checks
- Project-level result aggregation

Do not use this skill to decide whether the work should be DAG or Loop; use `team-coordination` first. Do not use this skill to create Worker task files or check `result.md`; use `task-management`.

## Project Files

Project files live under:

```text
shared/projects/{project-id}/
```

Use:

- `meta.json` for Project metadata and lifecycle status
- `plan.md` for the current execution plan
- `result.md` for the final Project result

The execution plan type is stored in `plan.md` as `**Plan Type**: dag` or `**Plan Type**: loop`. Do not put Project lifecycle status in `plan.md`. Do not hand-edit `meta.json.status`; use `projectflow`.

Use `filesync` before reading remote Project files and after changing local Project files. Use directory paths with a trailing slash, such as `shared/projects/{project-id}/`. `projectflow` reads and writes local shared files only.

If pulling `shared/projects/{project-id}/` fails, stop before any Project state mutation. Do not pull only `meta.json` or `plan.md` and continue to mark nodes, resolve ready nodes, record Loop iterations, or complete the Project.

## IDs

Use safe IDs only. Project and task IDs must match:

```text
[A-Za-z0-9_-]+
```

Use these formats:

- `projectId`: `{short-description}-YYYYMMDD-HHMMSS`
- DAG task ID: `{projectId}-{seq}`
- Loop task ID: `{projectId}-i{iteration}-{seq}`

Do not use colons, dots, slashes, spaces, or short standalone IDs such as `st-01`.

## Projectflow Actions

Use `projectflow` for:

- `create_project`
- `plan_dag`
- `ready_nodes`
- `plan_loop`
- `ready_loop_nodes`
- `record_loop_iteration`
- `pause_project`
- `resume_project`
- `complete_project`

`projectflow` does not push or pull files. It also does not send Matrix messages, stop Workers, accept task results, or write Worker task specs.

## Create Project

Create the Project before planning DAG or Loop work:

```json
{
  "action": "create_project",
  "payload": {
    "projectId": "{project-id}",
    "title": "{title}",
    "source": "{optional source}",
    "requester": "{requester Matrix id or room marker}",
    "parentTaskId": "{optional parent task id}"
  }
}
```

This creates `meta.json` and an initial DAG `plan.md`. After creation, publish `shared/projects/{project-id}/`.

## Lifecycle

Lifecycle actions only change Project lifecycle state. They do not change the graph, accept task results, resolve ready nodes, delegate work, stop Workers, or aggregate final output.

Pause:

1. Use only after `team-coordination` has selected a pause or interruption path, or when a recovery path needs to close scheduling.
2. Run `projectflow(action="pause_project")`.
3. Publish `shared/projects/{project-id}/`.
4. Do not issue new work while paused.

Resume:

1. Confirm the requester explicitly wants scheduling to continue.
2. Pull `shared/projects/{project-id}/`.
3. Run `projectflow(action="resume_project")`.
4. Publish `shared/projects/{project-id}/`.
5. Continue through normal result checking, plan refresh if needed, and ready-node resolution.

Complete:

1. Confirm delivery criteria are satisfied.
2. Write `shared/projects/{project-id}/result.md`.
3. Run `projectflow(action="complete_project")`.
4. Publish `shared/projects/{project-id}/`.
5. Report the final result to the requester recorded on the Project.

## DAG Operations

Use DAG when `team-coordination` decided the work is finite and dependencies can be planned now.

`plan_dag` receives the complete graph you want to keep:

```json
{
  "action": "plan_dag",
  "payload": {
    "projectId": "{project-id}",
    "tasks": [
      {
        "taskId": "{project-id}-01",
        "title": "Short outcome title",
        "assignedTo": "<matrix-localpart>",
        "dependsOn": []
      }
    ]
  }
}
```

Each node uses:

- `taskId`
- `title`
- `assignedTo`
- `dependsOn`

For `assignedTo`, use the Worker's **Matrix localpart** (the part between `@` and `:` in `matrixUserID`). Extract it mechanically — never guess, strip, or transform.

**Lookup steps (mandatory before every `plan_dag` / `plan_loop` call):**
1. Run `agt get workers --team "$TEAM_CR" -o json`
2. For each Worker, extract the localpart from `.matrixUserID`: e.g. `@worker-issue-resolver:domain` → `worker-issue-resolver`
3. Use that localpart verbatim as `assignedTo`

❌ Do NOT use CLI `.name` field directly (it may include deployment prefixes like `magic-cn-...-worker-issue-resolver`)
❌ Do NOT strip prefixes yourself — you will incorrectly remove legitimate name components
❌ Do NOT infer worker names from memory, AGENTS.md, or display names

| CLI `.name` | `matrixUserID` | ✅ `assignedTo` | ❌ Wrong |
|---|---|---|---|
| `magic-cn-x0a4t4pr201-worker-issue-resolver` | `@worker-issue-resolver:domain` | `worker-issue-resolver` | `issue-resolver` |
| `magic-cn-plt4s29va0r-worker-dev-worker` | `@dev-worker:domain` | `dev-worker` | `worker` |

Do not use `worker`, `owner`, `dependencies`, or standalone short IDs.

When you call `plan_dag`, send the full graph you want to keep:

- keep accepted nodes that still matter
- omit obsolete nodes
- add new nodes for changed scope, fixes, verification, or follow-up work

`plan_dag` preserves existing node status for nodes with the same `taskId`. It does not resume a paused Project, accept task results, or delegate work.

Call `ready_nodes` only when the Project is active. It returns pending DAG nodes whose dependencies are satisfied by accepted `[x]` nodes. Delegate returned nodes with `task-management`.

See `references/dag-execution.md` for DAG node and dependency design.

## Loop Operations

Use Loop when `team-coordination` decided the work should repeat until a stop condition, quality gate, evidence threshold, or maximum iteration count.

`plan_loop` receives the complete Loop plan you want to keep:

```json
{
  "action": "plan_loop",
  "payload": {
    "projectId": "{project-id}",
    "goal": "{loop goal}",
    "stopCondition": "{condition for stopping}",
    "iterationTemplate": "{how each iteration should run}",
    "maxIterations": 5,
    "currentIteration": 1,
    "tasks": [
      {
        "taskId": "{project-id}-i001-01",
        "title": "Short outcome title",
        "assignedTo": "<matrix-localpart>",
        "dependsOn": []
      }
    ]
  }
}
```

Required Loop inputs:

- `goal`
- `stopCondition`
- `iterationTemplate`
- `maxIterations`

Optional inputs:

- `currentIteration`
- `status`
- `tasks`

For every Loop task, `assignedTo` follows the same rule as DAG tasks: extract the Matrix localpart from `agt get workers` output. Never strip or transform.

Use `ready_loop_nodes` to find pending nodes in the current iteration whose dependencies are satisfied by accepted `[x]` nodes. Delegate returned nodes with `task-management`.

After evaluating an iteration, use `record_loop_iteration`:

```json
{
  "action": "record_loop_iteration",
  "payload": {
    "projectId": "{project-id}",
    "iteration": 1,
    "decision": "continue",
    "summary": "{what you learned}",
    "nextAction": "{optional next step}"
  }
}
```

Allowed decisions:

- `continue`
- `replan`
- `ask_user`
- `stop_success`
- `stop_blocked`

`record_loop_iteration` records your decision and updates Loop status. It does not create the next iteration's tasks by itself; call `plan_loop` when you need a new current-iteration task set.

## Result Gate

When a Worker result arrives:

1. Use `task-management` to pull and check the task result.
2. Pull `shared/projects/{project-id}/`.
3. Read Project `meta.json` and `plan.md`.
4. If the Project is paused, stop. Do not accept results, mark `[x]`, resolve ready nodes, or delegate work.
5. If active, use `team-coordination` to decide whether to accept, verify, repair, replan, ask the requester, continue Loop, or complete.

Only accepted `[x]` plan nodes satisfy dependencies for `ready_nodes` and `ready_loop_nodes`. A Worker `SUCCESS` is not enough by itself.

After you accept a DAG or Loop task result, update the corresponding plan node to `[x]`, publish Project files, then resolve ready nodes. Use the Project Status Reports template below for the report content, then use `communication` to deliver it to the requester.

## Project Status Reports

Build Project status report content here because it describes Project execution state. Use `communication` only for routing, target room, @mention, and same-room versus cross-room delivery.

Before writing a requester report:

1. Pull and read `shared/projects/{project-id}/meta.json` and `plan.md`.
2. Identify the plan type from `plan.md`.
3. For DAG, report graph progress through dependencies and next ready waves.
4. For Loop, report iteration progress through the current iteration, iteration decision, and next iteration path.
5. Write the report in the language selected by `AGENTS.md` Response Language, then use `communication` to deliver it to the requester recorded on the Project.

Status emoji for task and Project state. Keep the emoji stable, but localize the visible state label to the report language:

| State | Emoji |
|---|---|
| Completed | `✅` |
| In Progress | `🔄` |
| Pending | `⏳` |
| Blocked | `❌` |
| Revision | `🔁` |

Keep the report envelope consistent across DAG and Loop:

````markdown
---

## <localized project status report heading>

**<localized project name label>**: <name>
**<localized project ID label>**: <project-id>
**<localized execution mode label>**: <DAG or Loop>
**<localized project status label>**: <emoji + localized state label>
**<localized current focus label>**: <DAG wave/current node, or Loop iteration n / max and topic>

**<localized summary label>**: <1-3 sentences about what changed and what happens next>

**<localized task status label>**:
| <localized task ID label> | <localized task title label> | <localized owner label> | <localized status label> | <localized context label> |
|---|---|---|---|---|
| <task-id> | <title> | <worker> | <emoji + localized state label> | <dependencies, iteration role, result note, or -> |

**<localized execution progress label>**:
```
<DAG dependency flow or Loop iteration line; choose the matching shape below>
```

**<localized deliverables label>**:
- `<path>` - <what it contains>

**<localized next steps label>**:
1. <next DAG transition, next Loop decision, or requester action>

**<localized notes label>**: <blocker, risk, or decision needed; omit section if none>
````

Use this execution progress shape for DAG reports:

```text
<task-id> <emoji> -> <task-id> <emoji> -> <task-id> <emoji>
                         ^
<task-id> <emoji> -------|
```

Use the DAG progress section to show the dependency path that changed, newly unblocked nodes, or the next ready wave. If the graph is large, show only the relevant changed path and summarize the rest in the task table.

Use this execution progress shape for Loop reports:

```text
Iteration 1 ✅ -> Iteration 2 🔄 -> Iteration 3 ⏳ ... Max <n>
                  Current: <current iteration topic>
                  Decision: <continue, replan, ask_user, stop_success, stop_blocked, or pending evaluation>
```

Use the Loop progress section to show current iteration number, maximum iterations, current iteration topic, and the latest Leader decision. Do not present future Loop rounds as if they were pre-planned DAG tasks. Show only the current iteration's task statuses in the task table unless prior iteration context is necessary.

For intermediate updates, include the Project header, summary, task status table, and next steps. Omit the deliverables section unless new deliverables are available, and omit the execution progress section only when neither DAG flow nor Loop iteration state changed.

## Heartbeat And Recovery

Do not call `check_active_tasks` for now.

Team Leader heartbeat is temporarily disabled. Follow `HEARTBEAT.md`: do not probe Worker runtime, inspect active tasks, or send anomaly reports from scheduled heartbeat runs.

Worker runtime probes are disabled because Kubernetes Team Workers currently have no per-Worker Service. Hostname probes such as `http://agentteams-worker-<worker>:8088/api/chats` can misreport healthy Workers as unreachable.

For recovery, act only on explicit room messages, requester instructions, or Project files you were directly asked to inspect.

## References

Read only when needed:

- `references/dag-execution.md` - DAG input shape, node design, dependency design, dynamic replanning, and ready nodes.

## Patch Rules

Do not:

- treat task status as Project lifecycle state
- continue dispatching while the Project is paused
- use `plan_dag` or `plan_loop` as a substitute for `resume_project`
- use lifecycle actions as the whole pause, stop, or interruption workflow
- call ready-node resolution before pulling current Project files
