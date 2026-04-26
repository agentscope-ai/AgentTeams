# Team Leader Heartbeat

Use heartbeat to keep active team work moving. Do not do domain work.

## Checklist

1. Read `AGENTS.md`.
2. Read `team-state.json`.
3. Refresh current topology:
   ```bash
   hiclaw get teams <team-name> -o json
   hiclaw get workers --team <team-name> -o json
   hiclaw worker status --team <team-name>
   ```
4. Run the recovery loop below.
5. Report only meaningful changes.

## Restart / Recovery Loop

Use this loop after restart and on every heartbeat. `team-state.json` is only an index; shared project and task files are the source of truth.

1. For each active project in `team-state.json`, refresh project files before using `taskflow`:
   ```json
   {
     "action": "pull",
     "path": "shared/projects/{project-id}/"
   }
   ```
   Then read:
   - `shared/projects/{project-id}/meta.json`
   - `shared/projects/{project-id}/plan.md`
2. Resolve project state with `taskflow`:
   ```json
   {
     "action": "ready_tasks",
     "payload": {
       "projectId": "{project-id}"
     }
   }
   ```
3. For each returned ready DAG item, assign it with `taskflow` and `action=assign_task`, then publish `shared/tasks/{task-id}/` and `shared/projects/{project-id}/`.
4. For each in-progress `[~]` DAG item:
   - Pull `shared/tasks/{task-id}/` with `filesync` before reading Worker-written files.
   - If `filesync(action="stat", path="shared/tasks/{task-id}/result.md")` succeeds and `result.md` is successful, call `taskflow` with `action=complete_task`, update `team-state.json` as an index if needed, publish Leader-owned files, and assign the next ready wave.
   - If `result.md` says `REVISION_NEEDED` or `BLOCKED`, do not call `complete_task`; pause the DAG item and decide the next Leader action.
   - If no result exists and the Worker is sleeping, run `hiclaw worker ensure-ready --name <worker> --team <team-name>`.
   - If no progress for too long, follow up with a concrete question or blocker check.
5. When all project tasks are `[x]`, aggregate task results into `shared/projects/{project-id}/result.md`, complete the project in `team-state.json`, publish, and report to the original requester.

Do not create bare tasks during recovery. All task assignment must come from a project `plan.md`.

## Quiet Rules

- Do not send "thanks", "got it", or encouragement-only @mentions.
- If no action is needed, stay quiet.
- If two rounds produce no new task/question/decision, stop replying.
