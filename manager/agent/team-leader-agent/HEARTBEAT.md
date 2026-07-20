# Team Leader Heartbeat

Use heartbeat as a periodic team check and anomaly notifier. Do not do domain work.

## Not To Do

- Do not advance project state from heartbeat: do not assign tasks, complete tasks, create tasks, modify project DAG files, plan a new DAG, or trigger the next work wave.
- Do not call `taskflow(action="delegate_task")`, `projectflow(action="plan_dag")`, `projectflow(action="plan_loop")`, `projectflow(action="record_loop_iteration")`, or `projectflow(action="complete_project")` from heartbeat.
- Use heartbeat to discover stale work, unreachable Workers, missing results, and needed follow-up. Report anomalies or the next action needed; let a normal Worker completion, requester instruction, or explicit follow-up event advance the project.

## Checklist

1. Read `AGENTS.md`.
2. Check team health.
3. Check active Project state.
4. Apply the monitoring strategy below.
5. Report only meaningful changes.

## 1. Check Team Health

Refresh current topology before relying on Worker, room, or requester state:

```bash
agt get teams <team-name> -o json
agt get workers --team <team-name> -o json
agt worker status --team <team-name>
```

Check:

- Team Room and Leader DM Room are known.
- Team Admin identity is known.
- Worker names, Matrix IDs, and runtime states are known.
- Any Worker assigned to active `[~]` work is reachable.

If a Worker with active `[~]` work is sleeping, run:

```bash
agt worker ensure-ready --name <worker> --team <team-name>
```

If required Worker metadata is missing, or the Worker cannot be made reachable, do not guess. Treat it as an anomaly and report it through the communication rules.

## 2. Check Active Projects

Use projectflow to inspect active Project plans, task state, and Worker runtime status:

```json
{
  "action": "check_active_tasks",
  "payload": {}
}
```

The tool checks only Projects whose `meta.json.status` is `active`. It reads DAG or Loop plans, inspects `[~]` delegated tasks, calls the assigned Worker's internal `/api/chats` runtime status endpoint, computes ready tasks, and reports only anomalies or recovery signals.

## 3. Apply Monitoring Strategy

Use only the `issues` returned by `projectflow(action="check_active_tasks")` to decide what needs attention:

- `task_not_running`: @mention the assigned Worker in the Team Room and ask it to continue the exact task from `shared/tasks/{task-id}/spec.md` and existing `workspace/`, or publish `result.md` / a blocker if it is done or stuck.
- `task_result_pending_check`: report that a submitted result is waiting for normal task checking. Do not call `check_task` from heartbeat unless the current heartbeat prompt explicitly asks for recovery.
- `invalid_task_result`: @mention the assigned Worker to fix the `result.md` protocol.
- `missing_task_meta`: report the task directory/state inconsistency; do not recreate the task from heartbeat.
- `worker_runtime_unknown`: report that the assigned Worker's runtime status could not be checked; do not assume the task stopped.
- `ready_tasks_pending`: report that existing ready tasks are waiting for the normal Leader scheduling loop. Include the ready task IDs and assigned Workers. Do not delegate them from heartbeat.
- `project_completion_pending`: report that the Project appears ready for normal result aggregation and completion. Do not call `complete_project` from heartbeat.
- `loop_iteration_decision_pending`: report that the current Loop iteration needs a normal Leader decision. Do not call `record_loop_iteration` from heartbeat.

Heartbeat follow-up is a trigger, not a DAG reset. Do not move `[~]` tasks back to `[ ]`, do not create bare tasks, and do not reassign a task unless the project DAG explicitly requires a new task.

## Quiet Rules

- Do not send "thanks", "got it", or encouragement-only @mentions.
- If no action is needed, stay quiet.
- If two rounds produce no new task/question/decision, stop replying.
