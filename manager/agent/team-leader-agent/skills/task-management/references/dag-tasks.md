# DAG Tasks

Use this reference when assigning one Worker execution unit from a project DAG.

## Creating A Task

1. Start from `shared/projects/{project-id}/plan.md`. Do not create bare tasks directly from external requests.
2. Select a ready DAG item returned by `taskflow` with `action=ready_tasks`.
3. Use the DAG item ID as the task ID: `st-01`, `st-02`, etc.
4. Create the task directory and Leader-owned files with `taskflow` and `action=assign_task`:

   ```json
   {
     "action": "assign_task",
     "payload": {
       "projectId": "{project-id}",
       "taskId": "{task-id}",
       "roomId": "room:!team-room:domain",
       "spec": "# Task: <title>\n\n## Context\n...\n\n## Expected Result\nKeep deliverables under shared/tasks/{task-id}/ and submit status, summary, and deliverables through taskflow(action=submit_task)."
     }
   }
   ```

5. Publish and verify the task files with the `filesync` tool. Do not put remote storage paths in chat or specs.
6. Publish the updated project plan after `taskflow` marks `[ ]` to `[~]`.
7. @mention the Worker in the Team Room, telling them to pull `shared/tasks/{task-id}/` with `filesync` and read `shared/tasks/{task-id}/spec.md`.
8. Update `team-state.json` only as an index if needed.

## Completion

When the Worker @mentions you with completion:

1. Pull the task directory with `filesync(action="pull")`.
2. Verify `shared/tasks/{task-id}/result.md` with `filesync(action="stat")`.
3. Read `shared/tasks/{task-id}/result.md`. Worker-side `taskflow` should generate the `STATUS: <value>` line.
4. For `SUCCESS` or `SUCCESS_WITH_NOTES`, call `taskflow` with `action=complete_task`:

   ```json
   {
     "action": "complete_task",
     "payload": {
       "projectId": "{project-id}",
       "taskId": "{task-id}"
     }
   }
   ```

5. For `REVISION_NEEDED` or `BLOCKED`, stop and decide the next Leader action; do not mark complete.
6. Update `team-state.json` only as an index if needed.
7. Publish updated Leader-owned files.
8. Assign any `readyTasks` returned by `taskflow`.
9. If all project tasks are done, aggregate results and report to the original requester.

Current `taskflow` does not mark revision or blocked states. Those outcomes require a Leader decision before the graph advances.
