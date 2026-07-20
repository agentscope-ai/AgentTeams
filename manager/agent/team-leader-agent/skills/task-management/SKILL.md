---
name: task-management
description: Use before any Leader taskflow call or task-level workflow involving delegating a ready DAG or Loop node, writing task meta/spec, checking a Worker task result, handling result statuses, task directory ownership, or Worker result contracts. Always use this skill when the request mentions delegate_task, check_task, task result.md, task spec.md, BLOCKED, REVISION_NEEDED, INTERRUPTED, SUCCESS, or assigning/delegating a Worker task.
---

# Task Management

You manage individual Worker task delegation and result checks. Use this skill as the task execution layer. Use `team-coordination` for work organization strategy and `project-management` for Project state, DAG, Loop, lifecycle, and ready-node operations.

Task state is tool-owned. Do not create, edit, delete, or repair `shared/tasks/**` with shell commands, heredocs, direct file writes, `rm`, `mkdir`, `cp`, or Python module execution. Use `taskflow` actions only. If `taskflow` fails or returns inconsistent state, stop and report the blocker instead of manually patching files.

## Scope

Use this skill for:

- `taskflow` calls
- creating Leader-owned task files
- delegating one ready DAG or Loop node
- checking one submitted Worker result
- enforcing task file ownership
- enforcing Worker result contracts

Do not use this skill to choose DAG vs Loop, design the project graph, pause/resume/complete a Project, or aggregate the final Project result.

## Ownership

You own:

```text
shared/tasks/{task-id}/meta.json
shared/tasks/{task-id}/spec.md
```

The Worker owns:

```text
shared/tasks/{task-id}/workspace/
shared/tasks/{task-id}/<deliverables>
shared/tasks/{task-id}/result.md
```

Do not ask Workers to edit Project-level files:

```text
shared/projects/{project-id}/meta.json
shared/projects/{project-id}/plan.md
shared/projects/{project-id}/result.md
```

## Tool Boundary

Use `taskflow` for Leader task actions:

- `delegate_task`
- `check_task`

`taskflow` is a QwenPaw/CoPaw MCP tool exposed in your tool list. It is not a shell command, CLI binary, Python module, or HTTP endpoint. Call the tool directly; do not search for a binary or manually edit task files if the tool is unavailable.

Worker runtimes use their own task actions:

- `ack_task`
- `submit_task`

Use `project-management` for:

- `create_project`
- `plan_dag`
- `ready_nodes`
- `plan_loop`
- `ready_loop_nodes`
- `record_loop_iteration`
- `pause_project`
- `resume_project`
- `complete_project`

Do not call `check_active_tasks` from heartbeat or routine recovery checks for now; Kubernetes Team Workers do not expose per-Worker `/api/chats` Services, so runtime probes can misreport healthy Workers as unreachable.

`taskflow` handles file sync internally: `delegate_task` auto-pushes the task directory, `check_task` auto-pulls the task directory. Use `filesync` separately only for project-level files or non-task shared files.

delegate_task does not send Matrix messages. A task is not actually assigned to the Worker until you send a visible Team Room message that @mentions the assigned Worker's full Matrix ID. Do not start polling the task, tell the requester that the Worker is working, or wait for results before this Team Room notification has been sent.

Mandatory next action after `delegate_task`: use `communication`, then send the Team Room assignment with the `message` tool. Do not output a same-room sentence describing your intent to delegate, such as "I need to delegate the first ready node" or "I will assign this to the dev worker". That text is not a Worker notification and leaves the task unassigned from the Worker's perspective.

## Task Spec Language

Write task specs in the language selected by `AGENTS.md` Response Language.

Localize:

- prose
- headings
- task titles
- context
- deliverable descriptions
- constraints
- next-step wording

Keep machine-facing identifiers and protocol tokens unchanged:

- task IDs
- paths
- `result.md`
- `STATUS`
- `SUMMARY`
- `DELIVERABLES`
- `SUCCESS`
- `SUCCESS_WITH_NOTES`
- `REVISION_NEEDED`
- `BLOCKED`
- `INTERRUPTED`

## Worker Name Canonicality

`assigned_to`, Matrix @mention, and all task tracking fields must use the Worker's **Matrix localpart** (the part between `@` and `:` in `matrixUserID`). Extract it mechanically — never guess, strip, or transform.

### Lookup (mandatory before assigning any task)

```bash
# 1. Resolve team CR name
TEAM_CR="$(agt get workers "${AGENTTEAMS_WORKER_CR_NAME:-$AGENTTEAMS_WORKER_NAME}" -o json | jq -r '.team')"

# 2. Get all team workers
agt get workers --team "$TEAM_CR" -o json

# 3. Extract localpart from matrixUserID for each worker
#    @worker-issue-resolver:domain → worker-issue-resolver
```

Use the extracted localpart verbatim everywhere:

- `manage-team-state.sh --assigned-to <localpart>`
- Matrix @mention: `@<localpart>:<domain>`
- `meta.json` `assigned_to` field

### Common mistake

CLI `.name` may include a deployment prefix (e.g. `magic-cn-x0a4t4pr201-worker-issue-resolver`). **Do NOT use `.name` directly and do NOT manually strip prefixes.** Always extract the localpart from `.matrixUserID` instead.

| CLI `.name` | `matrixUserID` | ✅ `assigned_to` | ❌ Wrong |
|---|---|---|---|
| `magic-cn-...-worker-issue-resolver` | `@worker-issue-resolver:domain` | `worker-issue-resolver` | `issue-resolver` |
| `magic-cn-...-dev-worker` | `@dev-worker:domain` | `dev-worker` | `worker` |

## Delegate A Ready Node

Delegate only nodes returned by:

- `projectflow(action="ready_nodes")` for DAG plans
- `projectflow(action="ready_loop_nodes")` for Loop plans

Do not create bare tasks directly from external requests. Start from the Project plan.

Before delegation:

1. Pull `shared/projects/{project-id}/`.
2. Read Project `meta.json` and `plan.md`.
3. Confirm the Project is active.
4. Resolve ready nodes with `project-management`.
5. Select one ready node.

Create the task with `taskflow(action="delegate_task")`:

```json
{
  "action": "delegate_task",
  "payload": {
    "projectId": "{project-id}",
    "taskId": "{task-id}",
    "roomId": "room:!team-room:domain",
    "spec": "# <localized task heading>: <title>\n\n## <localized context heading>\n...\n\n## <localized expected result heading>\n<localized instructions. Keep deliverables under shared/tasks/{task-id}/ and publish result.md with STATUS, SUMMARY, and DELIVERABLES.>"
  }
}
```

`roomId` is required. Set it to the Matrix room where this task was assigned and where the Worker must report completion. For Team work, use the Team Room. For external delegation, use that external assignment room.

`delegate_task` writes:

```text
shared/tasks/{task-id}/meta.json
shared/tasks/{task-id}/spec.md
```

It also updates the plan marker from `[ ]` to `[~]`. Treat `[~]` as delegated/assigned from your perspective, not proof that the Worker is actively executing.

After delegation:

1. `delegate_task` auto-pushes `shared/tasks/{task-id}/`. Do not call `filesync push` for the task directory.
2. Publish `shared/projects/{project-id}/`, because the plan marker changed.
3. Use `communication`, then call `message` to @mention the assigned Worker in the assignment room. For Team work, this must be the Team Room, not the Leader DM and not the Worker's private room.
4. Include only the task ID, title, and instruction to start.

Do not prescribe Worker-internal acknowledgement, push, submit, or planning steps.

## Check A Submitted Task

When a Worker reports completion:

1. In the current room, directly say that you received the message.
2. Check the submitted task:

   ```json
   {
     "action": "check_task",
     "payload": {
       "taskId": "{task-id}"
     }
   }
   ```

   `check_task` auto-pulls `shared/tasks/{task-id}/` and returns the task meta (`task` field with `project_id`, `room_id`, assigned Worker) and the validated result (`result` field with status, summary, deliverables). Do not call `filesync pull` or `read_file` for task files separately.

3. Pull `shared/projects/{project-id}/`.
4. Read Project `meta.json` and `plan.md`.
5. If the Project is paused, stop. Do not check or accept the result, mark `[x]`, resolve ready nodes, call `plan_dag`, call `plan_loop`, or delegate follow-up work. Tell the requester that a Worker result arrived while the Project is paused and wait for explicit direction.
6. Return to `team-coordination` and decide whether the result satisfies the Project delivery criteria.
7. Return to `project-management` and apply the Project-level decision:
    - If accepted, update the plan node to `[x]`, publish Project files, and resolve ready nodes.
    - If revision, repair, verification, replanning, or blocker handling is needed, do not mark `[x]`; create or replace plan nodes as needed.
8. Delegate any ready downstream nodes through the normal ready-node delegation flow.
9. Close the completion loop with requester visibility:
    - Read `project-management` and use the Project Status Reports template for the report content.
    - Read `communication`.
    - Use the Requester Reports routing rules.
    - Report to the requester recorded on the Project.
    - If the requester is Team Admin, notify the Team Admin in Leader DM.
    - If the Worker completion arrived in the Team Room, do not treat the Team Room reply or downstream task assignment as the requester update.
10. You may batch multiple Worker completions and downstream delegations into one requester report, but do not omit Step 9 when Project state changed. Task handling is not complete until the requester or DM admin has been notified.

Project directory pulls in these flows are blocking. If `shared/projects/{project-id}/` cannot be pulled as a directory, stop and report the sync failure. Do not switch to pulling only `meta.json` or `plan.md` and then modify the plan.

## Result Contract

Worker `result.md` is parsed with this contract:

```text
STATUS: SUCCESS
SUMMARY: Short summary of the result

DELIVERABLES:
- shared/tasks/{task-id}/path/to/output

NOTES:
- Optional note
```

Valid statuses:

| Status | Meaning |
|---|---|
| `SUCCESS` | Candidate dependency input; you must accept it before it can unblock downstream work |
| `SUCCESS_WITH_NOTES` | Candidate dependency input with notes; you must accept it before it can unblock downstream work |
| `REVISION_NEEDED` | Worker task ended; you must decide the next graph shape |
| `BLOCKED` | Worker task ended; you must escalate, ask, or replan |
| `INTERRUPTED` | Runtime/control-plane interruption ended the task; do not accept it by default |

Deliverable paths must be under:

```text
shared/tasks/{task-id}/
```

`SUCCESS` and `SUCCESS_WITH_NOTES` are effective only as candidate results. Dependencies advance only after you accept the result and mark the plan node `[x]`.

Submitting a result ends the Worker task. Do not resume, rewrite, or mutate the old task. Use `project-management` to plan a new DAG or Loop node if more work is needed.

## Plan Markers

Project plans use these task markers:

| Marker | Meaning |
|---|---|
| `[ ]` | pending |
| `[~]` | delegated |
| `[x]` | completed / accepted by you |
| `[!]` | blocked |
| `[→]` | revision |

Use the Unicode arrow marker for revision in `plan.md`; the parser expects that single marker character.

Only `[x]` satisfies dependencies for `ready_nodes` and `ready_loop_nodes`.

## References

Read only when needed:

- `references/dag-tasks.md` - detailed task creation and completion workflow for DAG or Loop plan nodes.

## Patch Rules

Do not:

- put organization strategy into `task-management`
- create tasks that were not returned by ready-node resolution
- mark completion before checking the task result with `check_task`
- mark `[x]` just because the Worker wrote `SUCCESS`
- ask Workers to edit Project-level files
