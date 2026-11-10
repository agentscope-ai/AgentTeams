---
name: resource-management
description: Manage AgentTeams resources through the Controller as Manager.
---

# Resource Management

You manage resources with the `agt` CLI. Your Controller token is already
configured; never extract credentials to call the API manually.

## Discover and change resources

Use `agt get workers -o json`, `agt get teams -o json`, `agt get humans -o json`,
and `agt get managers -o json` to inspect current state. Use CR names for CLI
operations and the returned Matrix IDs and room IDs for communication.

For a new Worker, choose its model, Harness, and skills from the request:

```bash
agt create worker --name example-worker --runtime qwenpaw --model MODEL --no-wait
```

Use `agt create worker --help` or `agt update worker --help` for supported flags.
Poll the returned resource until ready when a subsequent operation needs it.
Use `agt update worker --name NAME --state Sleeping` to sleep a Worker, and
`--state Running` to wake it. Deletion uses `agt delete worker NAME`.

Use `agt apply -f FILE` for complete desired-state changes, including resources,
MCP servers, packages, or teams. Read the current resource first and preserve
fields outside the requested change. Check `agt apply --help` and the installed
CRD schema when a field is unfamiliar; do not invent flags or fields.

Create Workers before referencing them in a Team. The Team's `workerMembers`
list assigns exactly one `team_leader`; other members use `worker`. A Worker's
`spec.runtime` selects its Harness independently of its team role.
Check that the Team Admin has a Human CR before creating the Team. A Matrix
login account alone is not a Human CR. Use `agt create human --help` for required
fields; permission levels are 1, 2, or 3 and must follow the admin's request.

## Communicate and coordinate

Use the TeamHarness `message` tool with the destination room ID and full Matrix
mentions. A message in your admin DM does not dispatch work to a Worker room.
For example, call `message` with:

```json
{"action":"send","channel":"matrix","target":"room:!leader-room:domain","message":"@leader:domain Please coordinate this project: GOAL. Acceptance criteria: CRITERIA. Report the outcome back to me."}
```

Use ordinary text for cross-agent handoffs. `PROJECT_REQUESTED` is a Leader
self-trigger, not a Manager delegation message. Let the tool infer your sender
identity; do not set `agentId`, `sender`, or `role` to impersonate the Leader.
After sending, end your turn and wait for the Leader's report. Do not take over
projectflow/taskflow or run polling loops while the team works.
Reply to the admin once per turn. For a team project, forward its goal and
acceptance criteria to the Leader and let the Leader manage task state.
If your Harness has no native `message` tool, use `agt message --room ROOM_ID
--text 'MESSAGE WITH FULL MATRIX MENTIONS'`. Your current credentials determine
the sender; never extract tokens or use another agent's identity.

Use TeamHarness `filesync` and `artifact` tools for shared deliverables. Do not
edit project/task state or native runtime configuration as a management shortcut.

## Preserve identity and scope

Your Manager CR is commonly `default`, while your Matrix/runtime name is
`manager`. Keep these identities distinct. A requested Harness change recreates
the container and can interrupt active work; schedule it when idle. Preserve
existing workspaces, rooms, and credentials. Ask before destructive operations
or irreversible external side effects unless the admin already authorized them.
