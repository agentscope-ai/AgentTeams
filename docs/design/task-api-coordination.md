## Task-first execution boundary

Ordinary single-team work uses `taskflow start_task`, not start_project.
Create a Project only at an explicit user request. Do not infer Project creation
from a request's complexity, number of Workers or a possible cross-team need;
automatic cross-team Project creation is deferred.

Both coordinating and executable work are Tasks in the existing task store:

- `task_kind: main`: owns one request, `team_id`, its execution graph in `tasks`,
  its task room, original reply routes and accepted final report.
- `task_kind: execution`: independently executable Worker assignment. It records
  its `team_id`, `main_task_id` and `parent_task_id`. Parentage is containment;
  `depends_on` is execution ordering. Do not infer kind from a missing parent.
- `project_id` is optional membership. An existing Project aggregates only the
  main Task. Several main Tasks in one Project have independent task rooms,
  completion fences and reports. Completing one never completes the Project.

`start_task` stores all execution contracts and dispatches ready leaves;
`taskflow accept_task_result` reuses the existing submission identity, digest,
review and acceptance gates, then advances this main Task only. Workers update
their own execution records; only the Leader writes the coordinating plan.
Parent node updates merge against fresh Task metadata under strict conditional
writes so concurrent reviews do not erase each other or reopen cancellation.

Controller notifications carry `main_task_id`; `main_task_ready` and
`main_task_completed` read the main Task rather than Project metadata. Final
report delivery retains the original Manager/Human route, with no extra Manager
model turn in Matrix mode. The task index reads Task-owned plans, including
planned children which do not yet have execution records.

Cross-main dependencies, cross-team Task references and their admission/dispatch
contract are deferred. Future Project-level dispatch must use explicit Task kind
and team-qualified Task references, and wait for accepted main Task completion;
it must not treat a coordinating main Task as a Worker execution leaf. Current
start_task rejects dependencies outside its own execution graph. Existing
explicit Project DAG/Loop operations remain compatible.

# Task API coordination (opt-in)

Task/project records remain the authoritative business state. The Controller
resolves the registered recipient and delivers scoped control notifications through
the existing Matrix ingress. Matrix also carries progress,
artifacts, approval cards and final reports; progress projections do not start
another Agent turn.

Task notifications use the existing Matrix ingress of QwenPaw, OpenClaw, Hermes,
DeepSeek Harness, OpenHuman and Codex. Other Harnesses do not gain a new HTTP
notification server or a QwenPaw console API. The shared receipt validator reads
canonical input and current task identity before native admission; progress
projections do not start peer turns. Updated Harness images are required.
QwenPaw retains its fixed ACK/review prefetch hooks; these are not requirements
on other Harnesses. New deliveries, including Manager handoff, use Matrix.
Existing HTTP outbox receipts retain their recovery path, without adding
notification HTTP endpoints to any Harness.
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
- `main_task_ready` (legacy explicit Project plans: `project_ready`): after all execution leaves are accepted, the Controller
  notifies the Leader once if the coordinating parent/project still needs
  closure. Loop iterations are not treated as finite completed plans.
- `main_task_completed` (legacy explicit Project plans: `project_completed`):
  the Controller validates the completed state and registered Leader, then projects
  that response to the saved original user conversation. A direct Human
  requester retains the existing direct Matrix report path. This is a completion
  notification, not a stop/cancel call against a Manager session.

The Leader stores an ordinary finite main-task plan with `taskflow start_task`.
Accepting a checked result dispatches newly ready execution specs without another
routing decision. The Controller owns `main_task_ready`; the Leader verifies the
whole request and calls `complete_task` with the final report. Completion requires
accepted, artifact-backed leaves and cannot clear cancellation or unresolved work.

Existing explicit Project plans retain `start_project` with `dispatchMode:
automatic`, `project_ready` and `complete_project`. A matching durable acceptance
ends review without another model call only for automatic finite plans. Manual DAG
and Loop acceptance retains its existing continuation turn. Project compatibility
does not make Project creation part of ordinary Task intake.

## Delivery semantics

The outbox is separate from task state and uses strict object-store conditional
writes and leases. Restart resumes the saved delivery receipt. Repeated
admission with the same identity reuses the receipt; changing its payload is
rejected. Paused plans wait, while cancelled or superseded execution identities
do not start another run.

Paused deliveries do not expire by elapsed queue time. Native executions use
the runtime's configured execution budget; the transport does not replace it
with a shorter model timeout. HTTP request timeouts still bound each individual
admission or receipt lookup.

For pre-upgrade HTTP outbox entries only, the native console task API does not provide a durable idempotent admission
lookup. If admission outcome is unknown, the Controller records failure for
operator recovery rather than replaying a potentially side-effecting run. A
missing native execution receipt is also a visible terminal delivery failure;
it is not retried forever or treated as proof of business completion. This
transport does not claim exactly-once execution of external tools.

Failed deliveries are projected with an idempotent receipt ID into the task
room (or the source room for a handoff). Projection retries never replay the
Agent execution. The receipt API exposes both failure and final-user event IDs.

## Current validation boundary

Earlier real-model timing samples used QwenPaw Project plans and are not
Task-first performance evidence. Main-task state/acceptance tests exercise a
Project-free lifecycle and optional Project aggregation. Native ingress tests
cover receipt validation, isolated sessions, self-addressed closure and progress
echo suppression. These are not six full real-model development runs.
Cross-main/cross-team dependency dispatch and native Loop notifications remain
outside this change.

## Matrix coordination

Registered managed Harnesses use Controller-originated Matrix mentions for handoff, assignment, submission acceptance and whole-plan closure. The legacy transport setting does not override a managed Harness's Matrix ingress. Old HTTP outbox entries retain their existing recovery semantics. The Controller relays the saved final report to the original Human conversation as the registered Manager after rechecking completed state, without a Manager execution request or another model call. This does not automatically accept submissions: the Leader still makes the acceptance decision from the supplied result and independent QA evidence.

The Controller sends assignments as the registered Leader and acceptance notices as the actual submitting Worker, with the recipient in `m.mentions`, a fixed description, and a structured coordination packet. Closure is a self-addressed Leader notice. The packet retains the existing bounded spec, original requester route and per-delegation session. The Worker checks the current task fence and its own authenticated dispatch receipt; it does not need permission to list the team roster. Ordinary room history is not prepended to this control request.

Matrix send retries use the same transaction ID. A Worker is marked started only after the task's real ACK changes its authoritative state, not merely after Matrix accepts the message. The outbox continues monitoring task submission and approval resolutions until execution ends. Delivery transport is stored with the outbox entry; changing the local transport setting does not rewrite admitted entries. A cancelled or superseded assignment is ignored.

Workers and Leaders use isolated control sessions and validate their own durable Controller receipt before inference. A self-addressed Leader closure notification also requires that receipt and accepted leaf state. Submission messages remain pending until the authoritative task records an acceptance decision; closure remains pending until the main Task (or legacy explicit Project plan) actually completes.

The Worker uses the normal Matrix streaming output path for visible tool calls, execution progress and results. Detailed model reasoning is suppressed for these structured control turns. Responses and edits carry `org.agentteams.progress_only` so visible feedback cannot reopen another Agent execution alongside the authoritative task callback.

Real-model latency evidence remains QwenPaw-only. Other Matrix adapters have ingress/contract regressions; no six-Harness real-model latency SLA is claimed. Native Loop transport is not validated.

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

## Prepared protocol context and combined report submission

For opted-in QwenPaw control turns, the runtime supplies the registered team
roster at Human intake and executes the fixed ACK or submission-check operation
before model inference. These operations use the normal scoped tools and
state fences; they do not decide task acceptance. Preparation tool spans remain
part of the measured total. Missing or failed preparation falls back to the
normal tool protocol. Manual DAG and Loop continuation remain explicit.

Shared `submit_task` accepts optional `resultMarkdown`: the Worker's complete
report is saved as its own task's `result.md` in the submission operation.
Optional `resultPath` identifies a report already written under that task, or
the destination for `resultMarkdown`. Submission, shared-storage synchronization,
artifact publication, prerequisite previews and review use that single canonical
file rather than generating another completion document. Omitting both fields
preserves the existing canonical report, defaulting to `result.md`.
Submitted retries require the same path and report text; invalid input cannot
change the report or task state. Protocol/input files and symbolic links are
not valid report destinations. This
combines report persistence with submission, not execution with acceptance.

The argument compatibility layer preserves explicit report fields whether they
arrive in `payload` or at the supported top level; an explicit payload wins.
Automatic completion requires a supplied or previously saved final report
before committing terminal state. A completion without a report must fail
visibly in the tool call, rather than leave a completed plan whose requester
notification cannot be delivered.

`start_project.spec` can carry the shared original contract once. It is
materialized with each leaf's own assignment in the persisted execution spec,
so all runtimes receive the same full request through their existing task
protocol. Leaf ownership, spec validation, dependency gates and retry-plan
identity remain unchanged; no extra context fetch or runtime-specific path is
introduced.

Latency is not an acceptance gate that can replace delivery quality. Independent
QA retains implementation review, required acceptance cases and relevant
boundary/failure checks; batching calls must not remove necessary work.
Parallel analysis/implementation is appropriate only for a fixed original
contract. A material ambiguity or contradictory result must be resolved before
acceptance and downstream completion, rather than hidden to meet a timing target.

## Managed Matrix adapters

The Controller authenticates the actor, recipient, Team and main-task scope.
`GET /api/v1/coordination/events/{id}` returns the canonical control packet only
to its existing authorized readers. Each Matrix adapter keeps native room/event
deduplication and checks the receipt before allowing a self-addressed closure.
The common Python validator is shipped through the existing shared library
build context, including for Node and Rust Harnesses. Each native bridge retains
its own execution/session lifecycle; no second runtime scheduler is introduced.
Main-task sessions use the canonical team-scoped receipt identity or its actual
Matrix notification event/thread, so equal Task IDs in different teams cannot
share execution/review/closure context. Opaque receipt session strings are never
used as Matrix transport event IDs.

OpenHuman binds the shared server through its existing native MCP stdio client.
Its Task workspace is inside the native sandbox, and the orchestrator exposes
`mcp_list_tools`/`mcp_call_tool` with role-filtered skills. This is required for
ACK/submission/acceptance; receiving a Matrix event alone is insufficient.

Main Task creation by arbitrary Workers is not enabled by changing task kind.
The current automatic planning tool remains a Leader operation; creator access
and coordinating-plan write authority are separate admission concerns.
