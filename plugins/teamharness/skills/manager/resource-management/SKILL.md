---
name: resource-management
description: Manage AgentTeams resources and publish Skills to the asset center through Controller as Manager.
---

# Resource Management

## Publish a Skill to the asset center

When asked to upload or publish a Skill to the asset center (资产中心的技能库),
package the complete Skill directory and use the Controller publication command:

```bash
python3 -m zipfile -c /tmp/travel-card.zip /path/to/travel-card
agt skill publish -f /tmp/travel-card.zip
```

Replace the example paths with your actual Skill directory and archive. Include
`SKILL.md` with `name` and `description` frontmatter, plus its scripts and references.
The directory name must match `name`. Your existing Manager identity is used by
the CLI; do not extract credentials. Report publication only after the command
returns the Skill name, `scope: deployment`, file count and scan result. If it
fails, report the error rather than claiming the Skill was published.

Copying a Skill into `shared/skills/`, your own workspace, or a Worker's package
does not publish it to this catalog. Publishing also does not assign the Skill
to a Worker; preserve existing assignments unless asked to change them.

## Task room archive

When asked to archive finished tasks, verify acceptance and workspace artifact
uploads. Get the room ID from the accepted Task, check
`agt task-room status <room-id>`, then use `agt task-room archive <room-id>`.
Use `agt task-room list` to review already archived rooms.
Verify that the room appears in that list before reporting successful archival.
An `archive` directory or a room leave is not a task archive. Keep task metadata
and artifacts in their canonical locations and use the Controller lifecycle
operation so Dashboard and all members see the same archive state.
Restore with `agt task-room unarchive <room-id>`
before further collaboration. Use `agt task-room delete <room-id>` only when the
user requests permanent deletion of an already archived task room. Your Manager
identity is authorized by Controller; private chats, ordinary groups and Team
rooms are never eligible for these lifecycle operations.

You manage resources with the `agt` CLI. Your Controller token is already
configured; never extract credentials to call the API manually.
Use the commands documented in the relevant Skill before exploring the CLI.
Only inspect help for the specific missing or rejected command/flag; do not
repeat top-level help or try task/project mutations to supervise a Leader.

## Discover and change resources

Use `agt get workers -o json`, `agt get teams -o json`, `agt get humans -o json`,
and `agt get managers -o json` to inspect current state. Use CR names for CLI
operations and the returned Matrix IDs and room IDs for communication.

For a new Worker, choose its model, Harness, and skills from the request:

```bash
agt create worker --name example-worker --runtime qwenpaw --wait-timeout 45s -o json
```

Read `~/skills/worker-management/SKILL.md` before creating or resetting a Worker.
Use the request's name, runtime, role and skills. Omit `--model` to use the
configured default. If the request specifies a model, pass its model ID as
`--model`. Gateway provider names from `agt gateway models list` identify
providers. Do not scan your environment or image files for the default.

For one Worker, run the create command once. The CLI owns readiness polling;
you do not need a shell polling loop or another model turn per status check.
If its wait times out, creation is still accepted: report the last status and
stop. Do not create again, recreate the container, run ensure-ready, or send
probe messages merely because provisioning is slow.

For several Workers, use `--no-wait -o json` once per Worker and acknowledge
acceptance promptly. When a later operation needs readiness, use one bounded
`agt worker wait-ready --name NAME --timeout 45s -o json` call. Do not invent
background follow-up promises or repeatedly invoke the waiter in one turn.
Only run a model conversation probe when the admin requests that verification.

For OpenClaw autonomous checks, use
`agt update worker --name NAME --llm-heartbeat-enabled=false` to disable or
`--llm-heartbeat-enabled=true` to enable. This is model-driven polling, separate
from health reporting and Dreaming. Do not set a second environment override.
For a custom Skill ZIP attachment, use the bundled safe importer:

```bash
bash /opt/agentteams/agent/skills/worker-management/scripts/install-worker-skill.sh --worker NAME --archive FILE.zip
```

Use `--replace` only when updating an existing Skill. The importer validates
the archive, verifies its upload and updates the Worker's assigned skills.

Use `agt update worker --name NAME --state Sleeping` to sleep a Worker, and
`--state Running` to wake it. Deletion uses `agt delete worker NAME`.

For model and MCP changes, read `~/skills/gateway-management/SKILL.md` and
prefer `agt gateway`: create/reuse and verify the gateway asset before binding
the requested Worker. Use direct Agent CR edits only when that deployment
cannot integrate the requested resource with its gateway. A 401/403, invalid
input, name conflict, or temporary connection failure does not authorize a
fallback or disabling authentication. Report the failed step instead.

Use `agt apply -f FILE` for other complete desired-state changes, including resources,
packages, or teams, and for the unsupported-gateway fallback above. Read the current resource first and preserve
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
{"action":"send","channel":"matrix","target":"user:@leader:domain","message":"@leader:domain Please coordinate this project: GOAL. Acceptance criteria: CRITERIA. Report the outcome back to me."}
```

Use ordinary text for cross-agent handoffs. `PROJECT_REQUESTED` is a Leader
self-trigger, not a Manager delegation message. Let the tool infer your sender
identity; do not set `agentId`, `sender`, or `role` to impersonate the Leader.
For a known recipient, use `target: "user:@leader:domain"` with `message`.
Your private direct room is resolved automatically. A resource's personal
room or `leaderDMRoomID` can be a different Human's DM; do not probe or join it.
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
