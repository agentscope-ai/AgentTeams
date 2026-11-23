# Team Leader Heartbeat

Use heartbeat to inspect current work and report actionable anomalies.
Do not implement Worker work, create tasks or Projects, change task state,
replan, delegate, accept results or complete work from a heartbeat.

Read the current team and task state with `agt get teams <team> -o json`,
`agt get workers --team <team> -o json`, and `agt get tasks --team <team> -o json`.
Use `task-diagnosis` when an actual failure or stall needs investigation.
Inspect an existing Project only when the work belongs to that Project;
ordinary main Tasks do not require one.

Report task identity, observed state and the specific next action needed.
Manager or platform Admin owns member lifecycle and resource changes.
Do not wake, sleep, reconfigure or create members yourself.

Resume coordination on a normal completion, blocker, decision or requester
message. Preserve the existing main Task ID and requester/coordinator routes.
Keep quiet when nothing changed; do not send acknowledgement-only @mentions.
