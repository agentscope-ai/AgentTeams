# Plan Tasks

Use this reference when assigning one Worker execution unit from a project DAG or Loop current iteration.

## Creating A Task

1. Start from `shared/projects/{project-id}/plan.md`. Do not create bare tasks directly from external requests.
2. Select a ready item returned by `projectflow`: use `ready_nodes` for DAG plans and `ready_loop_nodes` for Loop plans.
3. Use the planned node task ID exactly as returned by `projectflow`.
4. Create the task directory and Leader-owned files with `taskflow` and `action=delegate_task`. This is the Leader task delegation action for a ready plan node:

   Write `spec` in the language selected by `AGENTS.md` Response Language. Localize the headings and instructions; keep task IDs, paths, `result.md`, `STATUS`, `SUMMARY`, and `DELIVERABLES` unchanged.

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

   `roomId` is required. Set it to the assignment room where the Worker must report completion: the Team Room for Team DAG work, or the external assignment room for externally delegated work. Do not omit it.

5. Publish and verify the task files with the `filesync` tool. Do not put remote storage paths in chat or specs.
6. Publish the updated project plan after `taskflow` marks `[ ]` to `[~]`. Treat `[~]` as delegated/assigned from the Leader perspective.
7. @mention the Worker in the Team Room with the task ID, title, and `shared/tasks/{task-id}/spec.md` path. Do not prescribe Worker-internal tools or acknowledgement steps.

## Completion

When the Worker @mentions you with completion:

1. Pull the task directory with `filesync(action="pull")` using `shared/tasks/{task-id}/`.
2. Read task `meta.json` to identify `project_id`.
3. Pull `shared/projects/{project-id}/` and read project `meta.json` plus `plan.md`.
4. If the project is paused, stop and wait for requester direction. Do not check/accept the result, mark `[x]`, call `plan_dag`, call `ready_nodes`, or delegate follow-up work.
5. Verify `shared/tasks/{task-id}/result.md` with `filesync(action="stat")`.
6. Read `shared/tasks/{task-id}/result.md`. It must contain `STATUS: <value>`, produced by the Worker's own runtime protocol or result template.
7. Check the submitted task result:

   ```json
   {
     "action": "check_task",
     "payload": {
       "taskId": "{task-id}"
     }
   }
   ```

8. For `SUCCESS` or `SUCCESS_WITH_NOTES`, inspect the result against the project delivery criteria. If accepted, mark the DAG node `[x]` in `shared/projects/{project-id}/plan.md`, publish project files, then call `projectflow` with `action=ready_nodes`:

   ```json
   {
     "action": "ready_nodes",
     "payload": {
       "projectId": "{project-id}"
     }
   }
   ```

9. Do not mark the DAG node `[x]` just because the Worker wrote `SUCCESS`; `[x]` means Leader accepted the result.
10. For `REVISION_NEEDED`, `BLOCKED`, or `INTERRUPTED`, do not mark complete. The task has ended; use `projectflow(action="plan_dag")` if more work is needed. `INTERRUPTED` is produced by runtime/control-plane interruption, not by normal Worker task execution.
11. Publish updated Leader-owned files.
12. Delegate any `readyNodes` returned by `projectflow(action="ready_nodes")`.
13. If all project tasks are done, aggregate results and report to the original requester.

Current `projectflow(action="ready_nodes")` treats only Leader-accepted `[x]` plan nodes as satisfied dependencies. `SUCCESS` and `SUCCESS_WITH_NOTES` are not automatic DAG progress; they become effective only after the Leader accepts the result and marks the node `[x]`. `REVISION_NEEDED`, `BLOCKED`, and runtime/control-plane `INTERRUPTED` results require a Leader decision and a new `projectflow(action="plan_dag")` update before the graph advances.

Directory pulls are blocking. If `shared/tasks/{task-id}/` or `shared/projects/{project-id}/` cannot be pulled as a directory, stop and report the sync failure before checking results or changing the plan.
