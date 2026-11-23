---
name: teamharness-task-management
description: "Use the common Task protocol to plan, execute, review and complete tasks by Task and artifact IDs."
---

# Task Collaboration

In a runtime-backed turn, call the native Task tool using its exact name in
the callable tool list, including any namespace. For example, use
`teamharness__task` when the list shows that name. If a call reports an unknown
tool name, correct the name from that list. Use `agt task` in a direct CLI
session. Do not run it through a shell tool to operate on a managed Task.
Your identity and permissions come from your runtime. Do not supply a
different role, Project, source room, recipient or storage path. The task
system preserves the original request, Human conversation and first dispatch
Agent identity.

## Manager intake

When you receive a business request, match the requested outcome against the
registered Team roster before using any tool to do the work yourself. A
suitable Team must receive the request through `task create` with its registered
`leader` and a concise `title`. Verify its Task receipt before moving to the next input.
The Leader chooses the plan and assignees. The task system delivers the accepted
result to the original conversation.

A request to reply here, attach a file in this conversation, or make the result
downloadable here specifies where to deliver the result. For delegated work,
the Task system fulfills that request in this conversation. Create the Task
for the suitable Team and let that delivery return here. A file you previously
created in your own workspace does not satisfy a new independent request.

A request to count or transform supplied data and produce a file is a business
deliverable. The size of the input, the ease of calculation, and your ability
to produce the file do not make it a question you can answer directly. When
the roster includes a Team that handles this work, register the Task before
calculating, writing files, running commands, or sending an attachment.

You may answer questions that require only a conversational answer and use your
management tools for AgentTeams administration. You may perform business work
yourself after comparing it with the roster and verifying that none of the
registered Teams can take it. Resolve an unavailable roster first.

When you receive Human input, interpret each supplied message in the conversation.
A complete work request asks you to perform work; a question, amendment, answer
or withdrawal can require no new Task. Determine that meaning before operating.

Decide from the current Human input first. If it asks for a deliverable, match it
to the roster and call `task create` when a Team can take it. Do this before
opening old Task records or starting the work yourself. A clear, complete
request for another run needs no clarification when the only apparent ambiguity
is that its body equals an earlier input.

Your framework identifies transport copies by their source event identity.
Each distinct input that independently requests work needs its own creation
receipt, even if another input requested the same work. Use a previous Task
only for an actual question, correction, cancellation, or other follow-up
about that Task. An earlier delivery does not fulfill a new source event.
Combine independent requests only when the Human asks you to do so.
Use only the Task operations available to your role.
If the user withdraws a request before you have started creating its Task, do not
create one merely to cancel it. For an existing Task, identify the intended Task
and call `cancel` with its main `taskId` and an explicit `reason` when your role
permits it; do not cancel unrelated work or claim
that a running tool has stopped without confirmation. The framework supplies and
checks input freshness; do not poll message history to reconstruct it yourself.

When the framework supplies multiple input references, choose the original
request's `inputId` (CLI: `--input-id`) for each Task you decide to create.
Confirm a request as registered only with a Task creation receipt for its exact
source event. A Task from another input cannot satisfy the current request.
Your framework maps stable `inputId` references to the verified messages shown
as `[Message ...]`. Use the supplied references; do not compute or renumber them.
Message references do not imply a required Task count. Before finishing your turn, address every genuinely independent request
still pending in the supplied inputs. Creating one Task does not settle the others.
Interpret their relationships first; do not bind all creations to the
first message or turn a supplement/withdrawal into another Task.
When later inputs amend the same request, keep the original `inputId` and
supply `relatedInputIds` in conversation order (CLI: repeat `--related-input-id`).
You can reference only supplied inputs from the same Human conversation. The
system captures all selected original bodies; later Human corrections override
earlier conflicting requirements. Use relatedInputIds only for a Human amendment
or supplement to the selected request; equal text is not such a relationship.
Do not turn an amendment into a separate Task.

An `outcome: input_changed` control result means no new Task was registered: new Human input
arrived before admission. Your framework adds the verified new input as a native Human user message
and preserves its reference in the control receipt. It saves that context before
acknowledging the input or continuing inference. Interpret it together with the earlier
request before choosing an operation. This is a normal update, not a failed task; do not ask
the Human to resubmit merely because new input arrived. A withdrawal may require zero Tasks. Do
not blindly retry an obsolete create. The framework handles later queued copies
of inputs you already interpreted.

You do not need to copy the current request: the task system verifies its
original Matrix message and preserves the complete text. Use optional `spec`
only to supplement necessary earlier context. Original Human requirements take
precedence over supplementary context and execution assignments.

## Plan as Leader

Preserve the main task already registered by Manager. Call `plan` once with
`taskId` and the complete `nodes` array. Each node has a local `id`, registered
`assignedTo`, full `spec` and explicit `dependsOn` local IDs. Use `[]` only for
independent work. If a node consumes another node's outputs or verifies its
result, include that upstream ID; mentioning the dependency in prose does not
delay dispatch or prepare inputs. Check every required edge before planning.
Preserve explicit requirements for independent work with distinct Worker identities.
When the requester asks for a synthesis or decision based on those results, make
the aggregation node depend on every required upstream result. Choose the
smallest plan that meets these requirements, the original scope, any explicit
node count, and the acceptance criteria. Keep closely related implementation,
tests and documentation together when one Worker can meet those requirements.
Add a separate QA node where the requirements or verification benefit justify
an independent handoff. Choose execution assignees from the supplied Worker roster. Your own
Leader identity is unavailable for execution; keep ownership of planning and
acceptance. When the Task system reports a no-effect assignee rejection, correct
the assignment in your plan and submit it again. The task system creates the task room,
allocates execution IDs and dispatches ready steps. End your turn after planning.

## Execute as Worker

Use the framework-prepared `begin` context. If none is supplied, call `begin`
with your execution Task ID. Use its spec, input previews and actual local
paths. Do not ACK again, reconstruct paths, or download supplied inputs again.
Inputs are read-only. Write deliverables in `outputDirectory` and temporary work
in `scratchDirectory`. Use an isolated test directory. Remove only the exact
scratch paths you created. Do not use wildcard cleanup in a shared temporary
directory or delete upstream files.
An input with `purpose: revisionBase` is the exact rejected candidate, with its
saved rejection in `review`. Use it as the base for the requested correction;
it is not an accepted dependency. Keep this supplied base read-only and publish
your corrected files from your own output directory.

When your task requires a self-contained runnable project, publish unchanged
copies of required accepted input modules alongside your app. Load dependencies
from the delivered project, not hardcoded runtime, Task or storage paths. Validate
a clean snapshot containing only the files you will submit.
When verifying such a project, use only its declared accepted input files; do
not fetch undeclared producer-workspace files to hide a missing dependency.

If a tool returns a running session handle, use that session's wait operation
until it reports a final result. Wait before starting dependent work, submitting,
or ending your turn. Do not rerun the original command to obtain its result.

Call the native Task tool with `action: submit`, `taskId`, a truthful
`summary`, optional detailed `report` and `files` as a JSON array of paths
relative to your output directory, even for one file. Keep Task mutations on
this tool during a managed Task turn. Use the actual result `status`;
do not claim success when validation failed. Submission is a candidate, not
Leader acceptance. End after a successful durable submission. The task system
notifies your Leader; do not send another completion message or poll.

## Review as Leader

Use the prepared public candidate or call `get` with the execution Task ID.
Check the task spec, `candidate.id`, business status, report and exact artifact
references. Read more only when evidence is truncated, missing or inconsistent.
A prepared candidate snapshot does not prove business acceptance. Check the
published files of a runnable project for its required dependencies; producer
workspace files and hardcoded runtime paths are not delivery evidence. Review the
available test evidence through static inspection; execution belongs to Workers.
Accepting this step releases its dependencies, not final acceptance of the whole
request. Do not wait for downstream QA that depends on this step. Read missing
evidence once; do not download files to assemble a test environment, repeat
unchanged reads, edit outputs or rerun checks. This is a decision-only phase:
use common Task operations, not batch execution, subagents or another workflow.
If required proof is missing or inconsistent, reject with explicit correction
requirements instead of executing the work yourself.
Distinguish the Worker's recorded test results from your own artifact review;
do not claim you ran checks that you only read. Call `accept` or `reject` with
the execution Task ID and your evidence in `note`. In a prepared native review,
omit `submissionId` to use the candidate bound by the framework. When using
the CLI, pass the exact `submissionId` you reviewed. Use `reject` only for a
business review decision. If the same step needs correction, provide explicit `replacementSpec`
in that rejection. The task system keeps the rejected attempt terminal, creates
one replacement execution under the same main task and rewires unstarted
consumers. Omit it when no replacement is intended. Do not reopen the rejected
execution, replace the main task or bypass required Human approval.

After acceptance the task system dispatches ready dependencies automatically.
End this turn; do not assign them again or wait for their results.

## Read artifacts

Use `files_read` for bounded text and `files_get` for a verified local path.
Use the owning Task ID with the `artifactId` and `version` from the candidate or
accepted input. For a child candidate, pass its execution Task ID as `taskId`.
For a truncated report, use `candidate.reportFile`. Continue text reads with the
returned version and next offset. Use `files_put` only for deliberate intermediate sharing;
normal submission publishes the named output files. Never guess storage keys.

## Complete as Leader

On `main_task_ready`, use the prepared `acceptedResults` from `get` to verify
whole-request coverage. Each result includes its actual execution `id`, exact
candidate, saved review, report and artifact versions. Do not reread unchanged
accepted files or rerun checks. For a concrete evidence gap, use the result's
execution ID; a plan step alias and main Task ID do not own child files.
Call `complete` with the main `taskId` and final Human report. By default the task
system includes all accepted outputs; use `artifacts` only to select exact accepted
IDs. It saves the final report and returns it with downloadable files to the
original Human conversation using the first dispatch Agent identity.

After a valid durable plan, submit, accept/reject or complete receipt,
your framework records the fact and ends this phase without another model call.
Do not repeat that operation or checkpoint after submission. Pending projection
and delivery remain the task system's responsibility.
A create receipt confirms only its selected request was registered; continue
interpreting any other pending Human inputs before ending your turn.

Framework-generated receipts enter your context automatically. Queued notification
means pending delivery, not recipient acceptance. Do not send duplicate reports,
forge delivery facts, inspect private session files or wake another model to relay
results. Use legacy taskflow/projectflow only for a verified legacy plan that
explicitly requires those operations, never for a `task/v1` event.
