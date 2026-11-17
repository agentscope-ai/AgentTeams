# QwenPaw Task startup recovery (#1631, incomplete)

The managed Matrix channel now starts a self-scoped Task scan after the SDK
commits its workspace. A missing or delayed Matrix assignment is not required
for admission. The shared Controller protocol claims the actual process start;
notification receipt validation and execution ownership remain separate.

The adapter builds a Task-specific native session from the authoritative spec,
checkpoint and artifact references. It reuses a native session only when its
record exists, has not expired, matches the latest shared checkpoint and still
has the exact native-state digest recorded at that checkpoint. A stale cache
selects a fresh session; old files remain available. No foreign native format
is imported. The per-Worker native cache is a private, atomic 0600 file beneath
the SDK's sessions directory, not an authoritative Task record.

Task queue keys prevent different Tasks in one room from being merged. Actual
execution remains serialized per room, including overlapping SDK reloads.
Admissions are shared with the SDK TaskTracker across reloads. Both the native
stream and TeamHarness PRE_EXECUTE hook check process/generation ownership;
ownership loss cancels the captured native stream. Interruption is recorded
after stream cleanup. A connection failure is not proof of replacement.

A saved result uses its checkpoint's Matrix transaction ID and requires a
server event ID before recording delivery. Final callbacks prefer a saved
Task report over local native text. Startup-only admissions have no Matrix
source event and cannot manufacture read receipts. A verified assignment's
source is attached only to its actual Task room. Normal Matrix cursors and
receipt processing are retained.

## Validation boundaries

`plugins/tests/agentteams-matrix/test_qwenpaw_task_recovery.py` covers discovery,
claim conflicts, cancellation/reassignment, reload admission, queue isolation,
latest-checkpoint/native-state/expiry checks, durable reply selection, and
interruption ordering. Native cases use installed QwenPaw 2.2.2.post1's real
SafeJSONSession and request schema. Authority and transport are synthetic;
the ownership-cancellation test uses a controlled native generator, not real
tool processes. Tests do not execute a model or prove external side effects.
The existing Matrix and TeamHarness suites remain separate regression checks.

Initial broad container tests accidentally used the released image's old
builtin TeamHarness/Worker modules and lacked Ruby/make. The old-source output
permission failure also reproduced with the unmodified Core plugin. Running
native coordination with both current source overlays passed; host packaging
checks run where Ruby/make are available. Do not report the mismatched initial
container suite as passing or modify product permission policy to hide it.

## Still required before recovery acceptance

- Real Controller/storage/Matrix/model three-step Task interruption and restart,
  replacement and cross-Harness continuation, including saved-result-only runs.
- Native tool-subtree stop evidence, same-process failure/retry behavior,
  unknown effects verified externally (prompt context alone is insufficient),
  actual conflicts, cancellation and reassignment while tools are active.
- Full session expiry/loss/reuse and active A→B→A with preserved old sessions
  and cursors; all six same-Harness and 30 directed lanes.
- Shared handoff state machine, room-message handoff and absent-cursor startup
  behavior across Harnesses. This change does not repair the existing initial
  catch-up suppression when QwenPaw has no local Matrix cursor.
- All non-session settings/assets owned by the Worker standard record, and
  Dashboard/CLI switching through one completed Controller capability.

Keep the implementation and companion pins Draft. Configuration readback,
mocked Task authority and native schema/session tests are not real Task E2E.
