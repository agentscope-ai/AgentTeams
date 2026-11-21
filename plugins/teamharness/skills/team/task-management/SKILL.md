---
name: teamharness-task-management
description: "Use the common Task protocol to plan, execute, review and complete tasks by Task and artifact IDs."
---

# Task Collaboration

Use the common `task` tool, or `agt task` when a CLI is available. Your identity
and permissions come from your runtime. Do not supply a different role, Project,
source room, recipient or storage path. The task system preserves the original
request, Human conversation and first dispatch Agent identity.

As Manager, call `create` with the registered `leader` and a concise `title`.
You do not need to copy the current request: the task system verifies its
original Matrix message and preserves the complete text. Use optional `spec`
only to supplement necessary earlier context. Original Human requirements take
precedence over supplementary context and execution assignments.

## Plan as Leader

Preserve the main task already registered by Manager. Call `plan` once with
`taskId` and the complete `nodes` array. Each node has a local `id`, registered
`assignedTo`, full `spec` and optional `dependsOn` local IDs. Keep the original
scope and acceptance criteria. Assign execution and independent QA to Workers;
do not perform their work yourself. The task system creates the task room,
allocates execution IDs and dispatches ready steps. End your turn after planning.

## Execute as Worker

Use the framework-prepared `begin` context. If none is supplied, call `begin`
with your execution Task ID. Use its spec, input previews and actual local
paths. Do not ACK again, reconstruct paths, or download supplied inputs again.
Inputs are read-only. Write deliverables in `outputDirectory` and temporary work
in `scratchDirectory`. Use an isolated test directory; do not delete upstream files.

When your task requires a self-contained runnable project, publish unchanged
copies of required accepted input modules alongside your app. Load dependencies
from the delivered project, not hardcoded runtime, Task or storage paths. Validate
a clean snapshot containing only the files you will submit.
When verifying such a project, use only its declared accepted input files; do
not fetch undeclared producer-workspace files to hide a missing dependency.

Call `submit` with `taskId`, a truthful `summary`, optional detailed `report`,
and `files` relative to your output directory. Use the actual result `status`;
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
independent QA evidence; do not repeat execution, edit outputs or rerun accepted
checks. This is a decision-only phase: use common Task operations, not batch
execution, subagents or another workflow. If required proof is missing, reject
with explicit correction requirements instead of executing the work yourself.
Distinguish the Worker's recorded test results from your own artifact review;
do not claim you ran checks that you only read. Call `accept` or `reject` with the exact `submissionId` and your evidence
in `note`. If the same step needs correction, provide explicit `replacementSpec`
in that rejection. The task system keeps the rejected attempt terminal, creates
one replacement execution under the same main task and rewires unstarted
consumers. Omit it when no replacement is intended. Do not reopen the rejected
execution, replace the main task or bypass required Human approval.

After acceptance the task system dispatches ready dependencies automatically.
End this turn; do not assign them again or wait for their results.

## Read artifacts

Use `files_read` for bounded text and `files_get` for a verified local path.
Prefer `artifactId` plus `version` from the candidate or accepted input. For a
truncated report, use `candidate.reportFile`. Continue text reads with the returned
version and next offset. Use `files_put` only for deliberate intermediate sharing;
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

After a valid durable create, plan, submit, accept/reject or complete receipt,
your framework records the fact and ends this phase without another model call.
Do not repeat that operation or checkpoint after submission. Pending projection
and delivery remain the task system's responsibility.

Framework-generated receipts enter your context automatically. Queued notification
means pending delivery, not recipient acceptance. Do not send duplicate reports,
forge delivery facts, inspect private session files or wake another model to relay
results. Use legacy taskflow/projectflow only for a verified legacy plan that
explicitly requires those operations, never for a `task/v1` event.
