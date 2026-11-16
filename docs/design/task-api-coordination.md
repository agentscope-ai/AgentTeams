# Task API coordination (opt-in)

Task/project records remain the authoritative business state. The Controller
resolves the registered recipient and delivers scoped control notifications through
the Agent API or the opt-in Matrix transport. Matrix also carries progress,
artifacts, approval cards and final reports; progress projections do not start
another Agent turn.

The initial runtime adapter supports QwenPaw teams. Other runtimes retain their
existing Matrix transport unless a compatible native adapter is implemented.
Enable this path only for a team whose participating runtimes support it:

```text
TEAMHARNESS_COORDINATION_URL=http://<controller>:8090/api/v1/coordination
```

The runtime forwards this variable to the builtin TeamHarness MCP child process.
`POST /api/v1/coordination/events` uses the caller's existing scoped credential.
Its `202` response means durable queue admission, not task acceptance or delivery
to the Human. `GET /api/v1/coordination/events/{id}` exposes a receipt to the
originating Agent, the actual receiving Worker/Leader, and administrators. It includes the final Matrix event ID only
once the original user conversation has actually received the report.

## Event flow

- `handoff`: Manager to a registered team Leader. The original requester and
  source conversation are preserved independently of the coordinator route.
- `task_assigned`: Leader to the assignee in the current task record. The
  delegation identity must match. A prepared assignment waits for its committed
  assigned state before execution.
- `task_submitted`: Worker to its registered Leader, fenced by the current
  submission identity. Submission alone never releases dependent tasks.
- `project_ready`: after all execution leaves are accepted, the Controller
  notifies the Leader once if the coordinating parent/project still needs
  closure. Loop iterations are not treated as finite completed plans.
- `project_completed`: the registered Leader notifies the saved Manager route.
  The Manager validates the completed state and registered Leader, then relays
  the saved Leader report without another model call. The Controller projects
  that response to the saved original user conversation. A direct Human
  requester retains the existing direct Matrix report path. This is a completion
  notification, not a stop/cancel call against a Manager session.

The Leader can store a complete finite plan with `start_project` and
`dispatchMode: automatic`. Accepting a checked result dispatches newly ready
stored leaf specs without another routing decision. A matching durable
acceptance ends the review turn without another model call only for automatic
finite plans. Canonical plan type and dispatch mode accompany the callback;
manual DAG and Loop acceptance retains its existing iteration/delegation turn
instead of being stopped after one result. The Controller
owns the `project_ready` transition; the Leader handles whole-plan acceptance
only in that closure turn. `complete_project` with a
Leader-approved `report` closes coordinating parents only after every execution
leaf is accepted and artifact-backed, saves the report, and notifies the source.
It cannot clear a cancelled, blocked or unresolved task.

## Delivery semantics

The outbox is separate from task state and uses strict object-store conditional
writes and leases. Restart resumes a saved native execution receipt. Repeated
admission with the same identity reuses the receipt; changing its payload is
rejected. Paused plans wait, while cancelled or superseded execution identities
do not start another run.

Paused deliveries do not expire by elapsed queue time. Native executions use
the runtime's configured execution budget; the transport does not replace it
with a shorter model timeout. HTTP request timeouts still bound each individual
admission or receipt lookup.

The native console task API does not provide a durable idempotent admission
lookup. If admission outcome is unknown, the Controller records failure for
operator recovery rather than replaying a potentially side-effecting run. A
missing native execution receipt is also a visible terminal delivery failure;
it is not retried forever or treated as proof of business completion. This
transport does not claim exactly-once execution of external tools.

Failed deliveries are projected with an idempotent receipt ID into the task
room (or the source room for a handoff). Projection retries never replay the
Agent execution. The receipt API exposes both failure and final-user event IDs.

## Current validation boundary

The native end-to-end samples cover a finite plan with one coordinating main
task and four execution subtasks per project, using QwenPaw. Task-level final
notifications for several independent main tasks in a shared project, optional
projects, cross-team projects and other runtime adapters still require their
own contracts and end-to-end validation. Do not treat project completion as
proof that these broader task models have been validated.

## Experimental Matrix coordination

`AGENTTEAMS_COORDINATION_TRANSPORT=matrix` selects Controller-originated Matrix mentions for assignment, submission acceptance and whole-plan closure. The default HTTP transport remains available; the initial Manager-to-Leader handoff retains HTTP. The Controller relays the saved final report to the original Human conversation as the registered Manager after rechecking completed state, without a Manager execution request or another model call. This does not automatically accept submissions: the Leader still makes the acceptance decision from the supplied result and independent QA evidence.

The Controller sends assignments as the registered Leader and acceptance notices as the actual submitting Worker, with the recipient in `m.mentions`, a fixed description, and a structured coordination packet. Closure is a self-addressed Leader notice. The packet retains the existing bounded spec, original requester route and per-delegation session. The Worker checks the current task fence and its own authenticated dispatch receipt; it does not need permission to list the team roster. Ordinary room history is not prepended to this control request.

Matrix send retries use the same transaction ID. A Worker is marked started only after the task's real ACK changes its authoritative state, not merely after Matrix accepts the message. The outbox continues monitoring task submission and approval resolutions until execution ends. Delivery transport is stored with the outbox entry; changing the local transport setting does not rewrite admitted entries. A cancelled or superseded assignment is ignored.

Workers and Leaders use isolated control sessions and validate their own durable Controller receipt before inference. A self-addressed Leader closure notification also requires that receipt and accepted leaf state. Submission messages remain pending until the authoritative task records an acceptance decision; closure remains pending until the project actually completes.

The Worker uses the normal Matrix streaming output path for visible tool calls, execution progress and results. Detailed model reasoning is suppressed for these structured control turns. Responses and edits carry `org.agentteams.progress_only` so visible feedback cannot reopen another Agent execution alongside the authoritative task callback.

Validation scope remains QwenPaw finite task plans. Other Matrix runtime adapters and native Loop transport have not been validated by this experiment.

## Task-room Human membership

After creating or reusing a task room, the shared `roomflow` calls the
Controller to accept existing invitations for registered team Humans. The
Controller verifies the authenticated Leader's canonical team, the room's
`room.meta` task kind/team/Leader, and current invited membership. Only the
configured team admin, Human members and Humans with access to that team are
eligible; no caller-supplied user list or Human token is accepted. Ordinary
invites outside that scope remain unchanged. Joining uses the existing identity
protocol without registering users, resetting passwords or changing room access.
The operation is idempotent. Older Controllers retain the invite workflow and
return a compatibility marker rather than preventing room creation.
