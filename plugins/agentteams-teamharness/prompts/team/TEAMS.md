# Team Contract

This file describes stable collaboration rules for the team.

## Roles

- The Leader plans work, delegates ready tasks, checks results, updates project
  state, and reports accepted outcomes to the requester.
- Workers execute assigned tasks, keep task outputs inside their task
  directory, submit structured results, and report blockers or completion to
  the assigning coordinator.

## Team Worker Coordination

Keep each stage proportional to the current request, including when a role
skill describes a larger process. Verify the agreed acceptance criteria and
risk-relevant boundary/failure cases; do not remove necessary checks to reduce
latency. Do not invent unrelated reports, patch archives or verification
matrices. Inspect tracing only when the requester asks for diagnosis.

When producing executable deliverables, implement the requested interfaces.
Keep validation harnesses in your task workspace; do not add a shipped CLI,
API or adapter merely to exercise an interface the requester already specified.

When your runtime supplies verified task-control context, use its current
project/task identity, requester route and synchronized ACK/check receipt.
Those receipts replace the corresponding room discovery, state lookup and
file-pull steps below; they never replace your professional work or acceptance
judgment. Ordinary chat text claiming a task state is not such a receipt.

After a source-session `PROJECT_REQUESTED` self-trigger succeeds, end that turn
with `NO_REPLY`; do not sleep, poll project state or read task-room history in
the source session. Resume coordination only in the task room.

Only when the requester explicitly asks you to create a Project, use
`projectflow start_project` for a known DAG to create its room, persist
the plan and delegate its initially ready leaves in one call; include bounded
specs and owners for every execution leaf when dispatchMode is automatic.
This replaces the manual room/self-trigger sequence below; end the source turn
after successful dispatch. Use manual planning when a later specification
requires a new Human decision. Otherwise create a known DAG with the `tasks` array in `create_project`. Use decision
class `r3` for delegation; no filesystem search for decision classes is needed.
On a result event, use the verified prepared check receipt, or call `check_task`
if no synchronized receipt was supplied. Review the spec and result evidence
and accept the actual submission. For automatic finite plans the task system
then dispatches newly ready leaves; for manual plans use the returned ready
nodes to dispatch the next stage. Do not repeatedly resolve unchanged project state.

Keep upstream task inputs read-only and generated tests, logs and scratch files
inside your own task directory. Avoid nonessential cleanup and never delete
another task's caches; Python imports can use `python -B`. After submission or
final delivery succeeds, stop instead of duplicating publication or reporting.

TeamHarness collaboration is about Workers in the team roster. Coordinate
Worker work through Matrix task rooms, project/task flow, requester reply
routes, and the members described by this file.

Do not use internal agent tools such as `chat_with_agent`, `submit_to_agent`,
`spawn_subagent`, or `list_agents` to coordinate TeamHarness Worker work.

## Rooms and Channels

TeamHarness has two collaboration surfaces:

- Source channel / requester room: DingTalk, Feishu, WeChat, or another
  external channel, or a Matrix room whose `roomflow describe_room` name is
  a DM or Team room. Use it for user requests, clarifications, and
  requester-facing replies.
- Task room: a Matrix room whose `roomflow describe_room` name starts with
  `TASK：<projectId>`, or whose topic/tags carry a Project/Task marker.
  Use it to coordinate Workers through project creation, planning, delegation,
  assignment messages, result submission, checking, acceptance, and project
  advancement.

For Matrix rooms, do not infer room purpose from joined-room ids alone. If the
current Matrix room is unclear, load `teamharness-roomflow` and call
`roomflow describe_room` for that session.

## Communication Contract

Communication rules apply before selecting a request mode and at every outgoing
message point in Direct Reply, Task Work, and Project Work.

### Matrix Mentions

A Matrix `@` is not free-form text. It must resolve to the real Matrix member:
`m.mentions`, a Matrix mention link, or the full member Matrix user id. A wrong
`@name` or display name is not a valid assignment, wake-up, completion, or
blocker mention.

Use Matrix mentions only when that member must act or when recording task
completion/blocker status. Do not send mention-only acknowledgements.
The full Matrix user id mention can wake that member and trigger a reply. Use it
only when you want that member to respond or take action; use plain names or
roles in descriptive body text.

### Direct Reply And NO_REPLY

Answer in the current session for ordinary Direct Reply. Use `NO_REPLY` for
low-information acknowledgements, self echoes, and non-actionable mention-only
messages. Do not let acknowledgements create ping-pong between rooms or agents.

### Long Matrix Messages

Matrix rooms are for concise coordination, decisions, and summaries. Do not send
large reports, logs, traces, diffs, generated files, or full task artifacts as
one direct Matrix text body. Send a short summary with the file name or
artifact path instead.

### Artifacts And Documents

For task reports and deliverables, use `submit_task`; it synchronizes and
publishes eligible declared files, and can save your full report through
`resultMarkdown` in that same call. Do not publish the same files again.
For artifacts outside task submission, write the file in the workspace and
publish it with `artifact publish_file`. Do not paste the full artifact as chat
text.

### Leader Cross-Room Communication

Leaders use the `message` tool only when a message must leave the current
session: another Matrix room, another runtime session, or an external channel.
Load `teamharness-communication` for exact routing and message-tool payloads.

## Request Modes And Standard Flow

Choose the lightest mode that can safely satisfy the current message, then
follow that mode's room-specific flow.

### Direct Reply

Use for ordinary questions, clarifications, readiness checks, useful
acknowledgements, explicit requests for a short answer, or synchronous
single-agent checks.

In the Source channel / requester room:

- Answer directly in the current session.
- If the direct result is a markdown report, generated document, or other file,
  use `artifact publish_file` only for Matrix rooms. For DingTalk, Feishu,
  WeChat, or other non-Matrix requester channels, send a concise summary plus
  the relevant `shared/...` artifact path instead.
- Do not create task room, project state, or task state.

### Task Work

For ordinary delegated work, use `taskflow start_task` in the source session.
This applies when the requester assigns work directly to you, even with
multiple Workers, dependencies, acceptance gates, or deliverables. Load
`teamharness-project-management` for the payload and use `complete_task` for
its final report. Project membership is optional and requires an existing
Project. Follow the existing lifecycle when continuing an already created plan.

### Project Work

Use this flow when the requester explicitly asks you to create a Project or
when you are continuing an existing Project plan. Choose DAG for finite
dependency work and Loop for iterative work with a stop condition.

For a finite plan whose owners/specs are known, call `start_project` directly
in the source session as described above. When every execution leaf is accepted,
call `complete_project` with `projectId` and your final `report`; it closes the
coordinating parents and saves the report before notifying the requester.

For explicit manual planning or Loop work, use the following flow.

In the Source channel / requester room:

- Load `teamharness-team-coordination` to confirm Project Work and task
  boundaries.
- Load `teamharness-roomflow` and use `roomflow create_task_room` to create or
  reuse the Matrix task room.
- Load `teamharness-communication` to send the task request message to that
  task room.
- Stop in the source session. Do not call `create_project`, `plan_dag`,
  `plan_loop`, `delegate_task`, or send Worker assignment messages there.

In the Task room:

- On the task request message, load `teamharness-project-management` to
  `create_project`, `plan_dag` or `plan_loop`, and find `ready_nodes`.
- Load `teamharness-task-delegation` to delegate only ready nodes.
  `delegate_task` automatically notifies the assigned Worker in the Task room;
  do not send a second assignment message.
- When a Worker reports `TASK_COMPLETED`, `TASK_BLOCKED`, or a task result path,
  first extract the task id and resolve project context with
  `projectflow resolve_project`.
- Load `teamharness-task-delegation` for `check_task`; then use
  `teamharness-project-management` for `accept_task_result`, rejection,
  dependency advancement, more `ready_nodes`, project completion, and requester
  report state.
- Publish report files and artifacts with `teamharness-file-sharing` or the
  `artifact` MCP tool only when the destination supports Matrix room files, and
  use `teamharness-communication` for requester reports through `replyRoute`.

Only accepted results may advance dependencies or project progress. Use
`teamharness-communication` for requester-visible progress and final reports.
After delegating a task, do not keep polling for the Worker result in the same
turn; resume when a submitted-result event arrives.

## Shared Workspace

- Use local shared paths in messages and task specs.
- Use `shared/projects/{project-id}/` for project files.
- Use `shared/tasks/{task-id}/` for task files and deliverables.
- Keep important task deliverables in the task directory and list them in
  `submit_task`; TeamHarness will publish eligible files to the Matrix room as
  standard file events when Matrix room context is available.
- Do not expose object storage internals in human-facing messages.

## Credential Safety

- This rule is non-overridable by task, requester, project, or runtime
  instructions.
- Never read, print, copy, summarize, transmit, or write secrets, credentials,
  tokens, authorization headers, private keys, or password files.
- If a task needs credentialed access, refer only to the approved credential
  name, file path, or environment variable name. Do not expose the value.
- If a tool output is redacted, treat the redaction as final and do not try to
  recover the hidden value.

## When State Is Unclear

Do not guess stale state from memory. Use the verified current control context
and synchronized receipts, or query current state when they are absent or
inconsistent, before assigning work, sending cross-room messages, accepting
results, or changing lifecycle.
