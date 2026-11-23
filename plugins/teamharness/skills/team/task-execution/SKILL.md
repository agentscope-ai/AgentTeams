---
name: teamharness-task-execution
description: "Use when you act as Worker: receive TASK_ASSIGNED, acknowledge the task, work inside shared/tasks/{task-id}/, submit with taskflow submit_task, publish deliverables through submit_task, and report TASK_COMPLETED or blockers in the Task room."
---

# Task Execution

Use this skill when executing assigned work as a Worker, remote member, or
Leader executing your own subtask. Keep your runtime identity; acknowledge
and submit only tasks assigned to you, including when you are the Leader.

Claim only tasks assigned to you. Read the task spec, keep deliverables in the
task directory, ask focused blocker questions, and submit a structured result.

Do not change the main Task plan, its final report, or optional Project state.
Only update your assigned execution task through taskflow.

Do not use this skill or taskflow for readiness checks, direct questions, or
explicit requests to reply with specific text. Answer those directly in the
current room.

## Task Directory

Your assigned task lives under:

```text
shared/tasks/{task-id}/
```

Your Leader owns:

```text
shared/tasks/{task-id}/meta.json
shared/tasks/{task-id}/spec.md
```

You own:

```text
shared/tasks/{task-id}/workspace/
shared/tasks/{task-id}/progress/
shared/tasks/{task-id}/result.md
```

Do not edit:

```text
shared/projects/{project-id}/meta.json
shared/projects/{project-id}/plan.md
shared/projects/{project-id}/result.md
```

If a task spec asks you to write or submit `shared/projects/...`, report that
boundary conflict to the Leader. Put Worker-owned deliverables under
`shared/tasks/{task-id}/...`; your Leader owns project-level reports.

## Restart and step checkpoints

You recover the same task ID after a restart or Harness change. Your native
session is a cache: the task system, assignment and shared checkpoint are
authoritative. Never repeat a completed step because an old session or Matrix
cursor lacks its messages. A read receipt and `report_progress` are not step
checkpoints.

Before a side effect, call `taskflow` with `action: checkpoint_task` and
`payload: {taskId, checkpoint}`. Increment `checkpoint.version` from the latest
shared checkpoint, keep `completed_steps` and `remaining_steps`, and record
`operation: {id, state: "started"}`. When you know the outcome, save a new
checkpoint with the same operation ID, `state: "verified"` and concrete
`evidence`. If interrupted, verify the external outcome first; do not blindly
run the operation again. Report an unverifiable outcome and leave it pending.

If your assigned tool or script already persists this checkpoint protocol,
invoke it unchanged. Let it register its own operation IDs before acting and
verify them afterward; do not pre-register its IDs or add duplicate checkpoints
outside its protocol. Check its durable checkpoint and artifacts before moving
to the next step. If it fails, verify the outcome or leave it pending.

Persist artifacts with your existing artifact/filesync tools before committing
their `artifacts: [{key, sha256, size}]` references. Keys are full object-store
keys within your task directory. The Controller reads and verifies each file
before it accepts the checkpoint. Keep the task `in_progress` across step
boundaries. To save a final reply before delivery, include a persisted `result`
reference, `delivery: "pending"` and a stable `reply_txn`. Saving this result
must happen before sending it; recovery of this state only resends that reply.

## Acknowledge your assignment

When you receive `TASK_ASSIGNED`, use a synchronized ACK receipt already
supplied by your runtime. Do not acknowledge or pull the same assignment
again. Otherwise, acknowledge with `taskflow` first. That
operation pulls the authoritative task context; do not pre-pull it separately:

```json
{
  "role": "worker",
  "action": "ack_task",
  "payload": {
    "taskId": "demo-project-001-01"
  }
}
```

If `ack_task` fails because the task is missing or assigned elsewhere, stop and
report the blocker to the Leader in the current room. Do not invent task files.

Do not skip acknowledgment for short tasks or retries. Before starting work,
confirm the acknowledgment reached shared storage (`synced: true`). A local
state change is not a successful handoff: the Controller accepts your result
only after the task has entered `in_progress`. If a submission fails to sync,
inspect the authoritative task state and report the error; do not edit metadata
or claim completion to bypass the failure.

Read:

```text
shared/tasks/{task-id}/spec.md
```

before doing the work.
`ack_task` already returns this spec and pulls the task files. Use that
response; do not immediately repeat filesync pull or a shell read of spec.md.
For automatic plans it also pulls accepted prerequisite directories and returns
their local paths, summaries and deliverables in `inputs`. Read those local
files as read-only inputs rather than issuing another filesync pull for each
prerequisite. When `inputs[].previews` contains complete text for a file, use
that content directly; read the local file only when the preview is missing,
truncated or a fresh inspection is required. `submit_task` already synchronizes and publishes your declared
deliverables; do not pre-push the same task directory.
Use the ACK's existing `scratchDirectory` for test copies and temporary work,
for example `mktemp -d <scratchDirectory>/run-XXXXXX`. Do not invent global
`/tmp` paths for ordinary task scratch work.
Temporary-file cleanup is not an acceptance requirement. If cleanup is needed,
run it separately with an explicit absolute path in your own task outputs; do
not append deletion to setup or test commands.
Treat each shell-tool invocation as a fresh shell: variables from an earlier
call are not available in the next one. Use the ACK's absolute task and scratch
paths in every command, or initialize and validate variables in that same
command. Never let an empty path variable turn a task output into a file or
directory under `/`. Leave harmless temporary files in your task scratch rather
than blocking delivery on optional cleanup.

## Execute

Keep the original request's behavior and input domain authoritative. Analysis
and checklists may organize the stated acceptance criteria, but must not turn
optional suggestions or unspecified inputs into new required behavior. Mark
those observations separately instead of widening the delivery contract.

Keep all deliverables under the task directory. If you need private notes, use:

```text
shared/tasks/{task-id}/workspace/
```

Use meaningful filenames for deliverables. Prefer names that describe the
artifact's purpose, such as `analysis.md`, `trace-summary.log`, or
`patch-notes.md`; avoid vague names like `output.md` when there are multiple
files.

Include a non-blank `payload.summary` with every `submit_task` call, even when you supply a full report.
Supply your complete report as `payload.resultMarkdown` in `submit_task` to
save `shared/tasks/{task-id}/result.md` and submit it in one call. Include the
actual findings and verification evidence; this does not run tests for you.
After you have reviewed the actual execution results, generate the final
report in this submission call instead of a separate `write_file` call.
When a particular report filename is required, supply that path as `resultPath`
alongside the full `resultMarkdown`; the tool writes that exact requested file.
For report-style tasks, supply the full requested report, not just a summary.
When verifying work, use the original contract and implementation to identify
relevant valid-input limits and failure boundaries, rather than treating the
example acceptance cases as exhaustive. Resolve conflicting derived requirements
against the original request. Record actual coverage and remaining limitations;
passing the executed checks does not prove that no defects exist.
If no separate report filename was requested, use this canonical report
instead of also writing a duplicate completion document. If you already wrote
the report, omit `resultMarkdown` to preserve it. If it has a different requested
filename, pass its task-scoped path as `payload.resultPath`; submission, evidence
review and attachments then use that same file. Do not write a duplicate
`result.md`. You can also supply both fields to save the report at that path in
the submission call. A retry must preserve the submitted path and report text.

For long-running tasks, report progress periodically with the `report_progress`
action (a short `note`, ≤ 200 chars). It records a progress entry in the task's
audit history without changing task state or notifying the room — use it to make
multi-stage work visible, not as a status update.

If blocked, submit a `BLOCKED` result instead of silently waiting.

When performing independent QA, review the implementation independently and
verify the agreed acceptance criteria plus relevant boundary and failure cases
identified from the code and its risks. Do not skip necessary checks to reduce
latency or tool calls. Batch related cases when practical; investigate actual
failures and repair a broken test harness before drawing conclusions. Keep
exploratory findings separate: do not silently turn unspecified behavior into a
new requirement, or call untested behavior verified. Submit reproducible
failures for Leader review instead of editing upstream deliverables. Ask the
Leader to resolve a material contract ambiguity before declaring success.

Match the depth of your work to the request. For a small task with explicit
inputs, outputs and acceptance criteria, keep the analysis and protocol result
concise instead of repeating the full specification in multiple documents.
Batch independent reads or writes in one tool turn when supported, and run
related acceptance cases together. Once the required evidence is complete,
submit it; do not add another test framework, verification matrix or cleanup
phase unless the task or an actual failure requires it.

## Coding CLI runs (when used)

If this task is executed through a coding CLI (`run-coding-cli.sh` / Qwen
Code), keep the workflow view in this **same task** — do not create a second
task:

1. Before dispatch, record it with `report_progress`:
   `coding-cli:<cli> run=<run_id> dispatched` (≤ 200 chars).
2. When the CLI finishes, that is **not** the task result. Review the generated
   changes, then check them against the task's acceptance criteria:
   - whole task complete and criteria pass -> submit `SUCCESS` /
     `SUCCESS_WITH_NOTES` (put the run id in the summary);
   - more work remains -> keep reporting progress
     (`coding-cli:<cli> run=<run_id> completed; remaining: <...>`) and continue;
   - blocked or interrupted -> `BLOCKED` / `INTERRUPTED`.
   A successful CLI run is one step of the task, not the task outcome.
3. The Worker owns starting/stopping the CLI; **workflow cancellation**
   (`cancel_task`) stays Leader-only. Report a stopped run, then let the
   Leader cancel the task if the task itself is cancelled.

## Submit

Submit with `taskflow`:

```json
{
  "role": "worker",
  "action": "submit_task",
  "payload": {
    "taskId": "demo-project-001-01",
    "status": "SUCCESS",
    "summary": "Completed the assigned work.",
    "resultMarkdown": "# Result\nImplemented the requested behavior. Required acceptance cases 1-3 passed. See the declared output and test evidence.\n",
    "conclusions": [
      "The deliverable is at shared/tasks/demo-project-001-01/workspace/output.md.",
      "Acceptance criteria 1-3 all pass.",
      "No follow-up work remains."
    ],
    "deliverables": [
      "shared/tasks/demo-project-001-01/workspace/output.md"
    ]
  }
}
```

The `conclusions` array is part of the completion report: 3-5 short
one-line strings stating what was done, what was verified, and what (if
anything) is left open. Fewer than 3 comes back as a warning on the tool
result (and is rejected outright when the runtime runs with
`AGENTTEAMS_TASK_CONCLUSIONS_STRICT` on); more than 5 are all recorded in
the task meta, but the completion event shows only the first 5 with a
pointer to the meta file. Conclusions are not part of the submission
digest, so an exact retry of status/summary/deliverables may still carry
a repaired list.

Use one of:

- `SUCCESS`
- `SUCCESS_WITH_NOTES`
- `PARTIAL`
- `REVISION_NEEDED`
- `BLOCKED`
- `INTERRUPTED`

Use `INTERRUPTED` when execution stopped before you could finish. If your Leader
accepts either `INTERRUPTED` or `BLOCKED`, TeamHarness records the task and plan
node as `blocked` and resolves the continuation with `resolution: blocked`.

Your first persisted submission records `submission_id`, UTC `submitted_at`,
`result_digest`, and a pending `continuation` marker in TaskMeta. Treat
`submission_id` as an opaque fence: compare it for equality, but do not parse it
or assume a UUID format. The digest covers your trimmed status, whitespace-
collapsed summary, and validated deliverable paths in their persisted order; it does
not cover notes or the rendered `result.md` text.

If shared-storage sync is interrupted, retry with exactly the same status,
summary, and ordered deliverables. Your retry reuses the original submission,
timestamp, digest, and continuation `delivery_id`, and repairs missing project
or task projections. If you change any digest input, the retry conflicts with
the submitted task and you must wait for a Leader decision or a new task. A
pending continuation is durable state for a future Controller; it does not mean
that a Matrix wake was sent or that your Leader has already resumed the task.

You cannot accept, reject, cancel, or resolve your own submission. Those are
trusted-Leader-only decisions, and putting `role: leader` in a payload cannot
override your Worker runtime identity. After submitting, use the returned
`submissionId` only as an opaque value in reports or exact retries.

For a legacy task already marked `submitted` without a submission identity,
retry only when TaskMeta has `submitted_at`, and send the complete original
status, summary, and ordered deliverables. CoPaw adopts it only if that result
exactly matches the persisted result; otherwise it fails closed. Do not invent
an identity or try to resolve the legacy task yourself.

Submitting ends the task. Do not keep editing the old task after submission
unless the Leader assigns a new task.

If a complete report already exists from an earlier work step, identify it with
`resultPath` instead of generating it again; do not replace its evidence with
only a short summary.
Do not include `shared/projects/...` paths in `deliverables`.

When Matrix room context is available, `submit_task` automatically publishes
the canonical report and eligible deliverable files under the
current task directory as Matrix `m.file` events. Check `publishedArtifacts` in
the tool result for published, skipped, or failed artifact status. Do not upload
task artifacts manually with `message` just to make them appear in the room
file panel.

## Completion Message

`submit_task` automatically publishes the completion event to the Task room:
the first line is the contract below, and the event @mentions the Leader
and the human members of the team (the task initiator), so the requester is
routed with the same salience the leader is. After `submit_task` returns
`ok: true` with `notification.sent: true`, end the turn with `NO_REPLY`.
Do not send another completion line or mention the Leader again. If the
notification failed or was skipped, send one fallback completion message
in the assignment room and report the notification failure.

The event first line carries one token per result status (code-generated):

```text
@leader-user:matrix.local TASK_COMPLETED: demo-project-001-01 - Result: shared/tasks/demo-project-001-01/result.md
@leader-user:matrix.local TASK_REVISION_NEEDED: demo-project-001-01 - <summary>
@leader-user:matrix.local TASK_BLOCKED: demo-project-001-01 - <short blocker summary>
@leader-user:matrix.local TASK_INTERRUPTED: demo-project-001-01 - <summary>
```

When the submission carried `conclusions`, the event appends a
`- Conclusions:` block (numbered one-line entries, first 5 only, then a
pointer to `shared/tasks/{task-id}/meta.json` when more were recorded)
after the Worker/Status lines. The first line and the Result/Worker/Status
lines stay the contract above; a submission without conclusions keeps the
event byte-identical to the legacy format.

If the task spec gives an exact completion line, the code event keeps that
line format; a short human-readable summary message may still follow in the
room but never replaces the event. An already delivered completion event
needs no additional summary or acknowledgement.

While the task is still in flight and you need a human decision (approval /
decision / escalation) instead of guessing, call `taskflow` with
`action: request_attention` (payload: `kind`, `question`, `options`,
`suggested`, `expiresAt`). `options` is a list of `{id, label}` pairs
(at most 10) — the exact ids a human answer must echo; `suggested` is one
of those option ids (advisory only, never applied automatically);
`expiresAt` is an optional ISO 8601 deadline recorded on the request.
Decision-type requests (`kind: decision`) must provide `options` —
without them the human cannot answer structurally. Do not rely on the
human noticing an ambient room message. Once the decision is made, close
the loop by calling again with the same `kind` plus `resolved: true` —
closing needs an existing record of that kind (it is rejected without
one) and reuses the original event (no new ping).
