# Team Contract

This file describes stable collaboration rules for the team.

It is not a status database. Do not store live room IDs, credentials, worker
phase, task status, current model values, package versions, or transient project
facts here. Query live state through runtime config, Matrix, shared workspace
files, or TeamHarness tools.

## Roles

- The Leader plans work, delegates ready tasks, checks results, updates project
  state, and reports accepted outcomes to the requester.
- Workers execute assigned tasks, keep task outputs inside their task
  directory, submit structured results, and report blockers or completion to
  the assigning coordinator.
- Remote members participate as humans or local coding agents. They should
  claim work only when directly assigned or explicitly invited.
- The Manager handles control-plane operations such as team, worker, human,
  model, MCP, package, and lifecycle management.

## Runtime Agent Boundary

The TeamHarness roster is the authority for team collaboration. Runtime-native
agents, built-in QA agents, spawned subagents, or tool-level agent lists are
implementation details of the current runtime. They are not TeamHarness team
members unless they are explicitly listed in the team roster or assigned through
TeamHarness team state.

- Do not present runtime-native agent tools such as `chat_with_agent`,
  `submit_to_agent`, `spawn_subagent`, or `list_agents` as the normal
  TeamHarness delegation path.
- Do not claim that a runtime-native QA agent, default agent, or spawned
  subagent is a team Worker.
- For TeamHarness work, delegate through the team room or a dedicated
  assignment room, project/task flow, requester reply routes, and the members
  described by this file.
- Only discuss runtime-native agents when the requester explicitly asks about
  runtime internals. When answering team questions, explain the TeamHarness
  roles and member roster instead.

## Rooms

- Use the team room for normal team-visible coordination.
- For project tasks that originate from DingTalk, Feishu, WeChat, or another
  external requester channel, call `roomflow` `create_task_room` with the
  channel `source` and the current channel's stable `sourceRoomId` before task
  delegation. Reuse the returned assignment room for later tasks from the same
  source room.
- Never invent, transform, or suffix `sourceRoomId` for task type, worker,
  retry, or project purpose. For the same DingTalk group, pass exactly the same
  `sourceRoomId` every time; otherwise TeamHarness treats it as a different
  external room and may create a different Matrix assignment room.
- Before assigning work in a reused external assignment room, call `roomflow`
  again with the same `sourceRoomId` and the complete Matrix user ids for all
  Workers needed in that room so missing members are invited before the task
  message is sent.
- Use the Leader DM for requester-facing private updates when the requester is
  the team admin.
- Use project reply routes for requester reports that originated from an
  external channel or another runtime session.
- Use worker private rooms only for exceptional recovery or sensitive follow-up.
- Do not copy every team-room event into a DM. Summarize decisions and outcomes.

## Communication Contract

Communication rules apply before selecting a request mode and at every outgoing
message point in Direct Reply, Quick Task, and Project Work.

Use `teamharness-communication` when the TEAMS.md communication contract needs
one of these detailed protocols:

- Channel / Room Selection Protocol
- Requester Report Delivery Protocol
- Message Tool Protocol

Always apply these rules:

- Answer in the current session for ordinary Direct Reply.
- Use `NO_REPLY` for low-information acknowledgements, self echoes, and
  non-actionable mention-only messages.
- Do not let acknowledgements create ping-pong between rooms or agents.
- Use the Team Room or assignment room for team-visible coordination, not as a
  requester-facing report destination unless the recorded requester route is
  exactly that room.
- Use project `reply_route` for requester-facing reports after accepted project
  state changes.
- Use the `message` MCP tool only for cross-room, cross-channel, or
  cross-session sends.
- Do not treat Worker assignment, Worker completion, or Team Room status as the
  requester report unless the recorded requester route is exactly that room.

## Request Modes

Choose the lightest mode that can safely satisfy the current message.

- Direct Reply: ordinary questions, clarifications, readiness checks, useful
  acknowledgements, explicit requests for a short answer, or synchronous
  single-agent checks. Do not involve a Worker or create project/task state.
- Quick Task: one bounded Worker-owned action with one owner, one expected
  result, no DAG, and no Loop. Use quick project/task state so the Leader can
  recover the result, accept or reject it, and report back to the requester.
- Project Work: multi-member work, durable shared state, deliverables,
  dependencies, acceptance gates, or follow-up tracking. Use project and task
  flow only after this mode is selected. Choose DAG for finite dependency work
  and Loop for iterative work with a stop condition.

## Standard Flow Index

Use this section as the execution index only: pick the large step, load the
listed skill, then follow the listed skill step for exact tool usage, payload
shape, and cautions. Do not execute project, task, room, or cross-channel
actions from this index alone.

Direct Reply mode does not need a dedicated flow. Answer directly in the
current conversation. If routing is unclear or the reply must leave the current
session, load `teamharness-communication`
`## Channel / Room Selection Protocol` before replying.

After any task delegation step, do not continuously poll for the Worker result
or keep calling check tools in the same Leader turn. The Worker will report
completion or blockers in the assignment room recorded for that task. When that
room message or task event reaches the Leader, resume from the submitted-result
check step in the relevant flow.

### Quick Task Flow

Use for Quick Task mode.

| Role | Purpose | Skill | Skill step |
| --- | --- | --- | --- |
| Leader | If the message has task/project/recovery context, restore project state before deciding whether to create new work. | `teamharness-project-management` | `## Project Files` |
| Leader | Confirm the request is exactly one Worker-owned task. | `teamharness-team-coordination` | `## Choose Execution Mode`, `## Good Task Boundaries` |
| Leader | Create the quick project and single delegated task state. | `teamharness-project-management` | `## Create Quick Project` |
| Leader | Send the Worker assignment in the assignment room. | `teamharness-task-delegation` | `## Assignment Message` |
| Worker | Execute and submit the assigned result. | `teamharness-task-execution` | `## Acknowledge`, `## Execute`, `## Submit`, `## Completion Message` |
| Leader | Check the submitted result before project acceptance. | `teamharness-task-delegation` | `## Check Submitted Task`, `## Result Contract` |
| Leader | Accept or reject the checked result. | `teamharness-project-management` | `## Accepting Worker Results` |
| Leader | Report the accepted result or blocker through the project requester source and reply route. | `teamharness-communication` | `## Requester Report Delivery Protocol` |

Do not use Quick Task for multi-node dependencies, parallel Workers, or Loop
iterations. Switch to Project Work before continuing if those appear.

### Project Work Flow

Use for Project Work mode.

| Role | Purpose | Skill | Skill step |
| --- | --- | --- | --- |
| Leader | If the message has task/project/recovery context, restore project state before planning or delegating new work. | `teamharness-project-management` | `## Project Files` |
| Leader | Choose DAG or Loop and define task boundaries. | `teamharness-team-coordination` | `## Choose Execution Mode`, `## Good Task Boundaries` |
| Leader | Create project state and plan DAG or Loop. | `teamharness-project-management` | `## Create Project`, `## Plan DAG`, `## Plan Loop` |
| Leader | Resolve ready work before assignment. | `teamharness-project-management` | `## Ready Nodes` |
| Leader | Delegate only ready nodes and send the assignment message. | `teamharness-task-delegation` | `## Delegate Only Ready Nodes`, `## Delegate Task`, `## Assignment Message` |
| Worker | Execute the assigned task. | `teamharness-task-execution` | `## Acknowledge`, `## Execute`, `## Submit`, `## Completion Message` |
| Leader | Check submitted Worker results before project acceptance. | `teamharness-task-delegation` | `## Check Submitted Task`, `## Result Contract` |
| Leader | Accept/reject results, advance the plan, and find new ready work. | `teamharness-project-management` | `## Accepting Worker Results`, `## Ready Nodes` |
| Leader | Report accepted progress or final result through the project requester source and reply route. | `teamharness-communication` | `## Requester Report Delivery Protocol` |

The project owns durable context and requester routing. The task owns one
bounded Worker execution unit. The Leader must accept or reject submitted
results before project dependencies advance.

## Shared Workspace

- Use local shared paths in messages and task specs.
- Use `shared/projects/{project-id}/` for project files.
- Use `shared/tasks/{task-id}/` for task files and deliverables.
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

Do not guess stale state from memory. Query current state before assigning work,
sending cross-room messages, accepting results, or changing lifecycle.
