---
name: teamharness-task-management
description: "Use the common Task protocol to plan, execute, review and complete tasks by Task and artifact IDs."
---

# Task Collaboration

Use the common `task` tool, or `agt task` when a CLI is available. Your identity
and permissions come from your runtime. Do not supply a different role, Project,
source room, recipient or storage path. The task system preserves the original
request, Human conversation and first dispatch Agent identity.

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

Call `submit` with `taskId`, a truthful `summary`, optional detailed `report`,
and `files` relative to your output directory. Use the actual result `status`;
do not claim success when validation failed. Submission is a candidate, not
Leader acceptance. End after a successful durable submission. The task system
notifies your Leader; do not send another completion message or poll.

## Review as Leader

Use the prepared public candidate or call `get` with the execution Task ID.
Check the task spec, `candidate.id`, business status, report and exact artifact
references. Read more only when evidence is truncated, missing or inconsistent.
A prepared candidate snapshot does not prove business acceptance. Review the
independent QA evidence; do not repeat execution, edit outputs or rerun accepted
checks. Call `accept` or `reject` with the exact `submissionId` and your evidence
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

On `main_task_ready`, verify the whole original request against accepted results.
Call `complete` with the main `taskId` and final Human report. By default the task
system includes all accepted outputs; use `artifacts` only to select exact accepted
IDs. It saves the final report and returns it with downloadable files to the
original Human conversation using the first dispatch Agent identity.

Framework-generated receipts enter your context automatically. Queued notification
means pending delivery, not recipient acceptance. Do not send duplicate reports,
forge delivery facts, inspect private session files or wake another model to relay
results. Use legacy taskflow/projectflow only for a verified legacy plan that
explicitly requires those operations, never for a `task/v1` event.
