# Harness recovery boundaries for 1.2.8 (#1631)

Status: candidate implementation, incomplete acceptance. These changes are in
the 1.2.8 scope; this document does not declare that version ready for release.
Use the exact source and artifact evidence for each run. A successful
configuration projection, restart or reply retry proves only that operation.

## Authority and ownership

| Record | Owner and location | Harness responsibility |
| --- | --- | --- |
| Worker settings and asset references | Controller; `agents/<workerIdentity>/runtime/runtime.yaml` under the configured tenant storage root | Read the current version at every start and project it into native inputs. Do not publish private-directory configuration as a newer authority. |
| Credential bindings | Worker identity and scoped credential files/environment | Resolve the declared bindings. Never recover a superseded token from a native session. |
| Task identity, status and execution | Task system; namespace/team/stable task ID, with Project optional | Discover only assigned recoverable Tasks, confirm ownership and claim the actual instance/process generation before inference or tools. |
| Step checkpoint and artifacts | Task execution endpoint and shared object storage | Persist and verify artifact bytes before committing versioned references, completed/remaining steps, operation/evidence and saved delivery state. ACK/progress notes do not complete a step. |
| Native conversation | Each Harness's private session store, beneath the same Worker home | Treat it as a cache. Reuse only an existing, unexpired session consistent with the latest shared checkpoint; otherwise create a fresh native conversation from authoritative context. |
| Matrix consumption and delivery | Durable event IDs, receipts, pending replies and transaction IDs | Preserve backlog and prior cursors. A native read means input committed to a session; it does not prove execution. A cursor in another Harness's format is not a common handoff record. |

The Worker home and task ID survive replacement. Native session formats are
not standardized or migrated between Harnesses. Configuration generation,
execution generation and notification outbox lease are separate identities.
An old session or a delayed callback cannot override newer shared state.

Controller configuration publication covers all six Harnesses. Complete
standardization of every native setting, user configuration write path and
asset is still open. In particular, native UI writes must not remain the only
persisted copy of a Worker setting. Existing compatibility projections remain
while their fields are migrated; clearing them or their private directories is
not a migration strategy.

## Per-Harness implementation boundary

The source paths below belong to the independent Harness repositories unless
identified as Core. “Implemented” describes a code path, not full acceptance.

| Harness | Startup configuration | Native execution/session adapter | Message and acceptance boundary |
| --- | --- | --- | --- |
| Codex | Existing member `runtime.yaml` reader; each standard field still needs effective-native verification | `src/agentteams_codex/bridge.py`: self discovery/claim, checkpoint-bound Task thread cache in `runtime/codex-bridge-state.json`; missing native thread rebuilds | Preserve unrelated journal events/cursor. An interrupted-turn prompt requesting a new message is not automatic recovery. Full current-source no-input execution acceptance remains open. |
| Hermes | Candidate `src/hermes_worker/sync.py` and `bridge.py` consume the Worker transport/model record; compatibility fields remain when not declared | `src/hermes_matrix/overlay_adapter.py`: real SessionStore row/expiry checks, fresh conversation and warm Agent eviction on checkpoint mismatch | Matrix connect and YAML `mcp_servers` are insufficient: the packaged native MCP client must discover Task tools. Case24's local overlay lacked that client, so its completed steps do not prove native submission/final delivery. |
| QwenPaw | Existing `qwenpaw_worker` member loader; Core publishes the resolved standard fields | Core Matrix channel `task_execution.py`: self discovery/claim, real SafeJSONSession existence plus native/checkpoint digests, Task-specific queue identities and SDK reload admission sharing | Explicit MCP stdio environment must carry the current process boot identity. Same-process saved-reply retry now distinguishes an active native stream from historical admission;51 recovery/reload regressions passed, but real delivery-fault E2E and initial absent-cursor catch-up remain open. |
| DeepSeek Harness | Existing standard member loader materializes the `.dsh` profile/workspace | `scripts/matrix_bridge.py`: Task admission, checkpoint-bound session marker, native missing-session fallback and saved-result delivery | First-sync pending input is retained. SDK/MCP credential scrubbing must preserve scoped Task access. Case20/21 cover two specific real lanes; case22 uncertain-effect recovery failed before verification. |
| OpenClaw | Candidate `scripts/configure-runtime.py` projects the Worker record, validates native configuration and publishes a private derived cache | Native Task/process/session recovery adapter remains to be implemented | Harness candidate `e57b75aa` removes unconditional Matrix deletion/relogin. Real SDK SQLite-store tests retain device/cursor/synthetic crypto snapshot through the launch preamble; actual restart, encrypted backlog and replacement snapshots remain unproved. Core has not pinned that candidate. |
| OpenHuman | Candidate entrypoint projects the standard Worker record into native TOML and retains Worker-owned shared storage | Native Task/process/session recovery adapter remains to be implemented | Nonempty legacy native shared data is preserved by refusing overwrite; verified migration and actual DM/group policy behavior remain open. Native CLI/config checks are not Matrix/model/Task acceptance. |

All six must implement the same admission and checkpoint semantics. Differences
above explain native integration work, not permission to weaken the common
contract. A tool server that answers `tools/list` outside the Agent does not
prove its tools are registered in the Agent's native runtime.

## Outcome-specific recovery

- Saved result with pending delivery: verify the stored bytes and execution
  ownership, resend with the same durable transaction, acknowledge the Matrix
  event and do not enter inference or re-execute the completed operation.
- In-progress Task at a verified boundary: retain `in_progress`, restore the
  latest checkpoint and artifacts, and infer/execute only remaining steps.
- Started/unknown operation: first obtain external verification evidence or
  remain pending verification. Do not clear its ID, fabricate an artifact or
  replay the effect to make the Task appear recoverable.
- Cancelled, reassigned or replaced execution: reject stale native tools and
  replies. Connection failure alone is not proof that ownership was lost.
- Return to an older Harness: keep its old session for the regression, but
  reject reuse when its checkpoint is behind. Do not import the intermediate
  Harness's native conversation.

## Evidence and remaining release gates

| Evidence | What passed | What remains unproved |
| --- | --- | --- |
| Core configuration projection tests | Six same-Harness and30 directed configuration projections with in-memory storage | Native startup, real scoped storage permissions, Matrix or Task execution |
| Real case20 and21, exact sources in their JSON records | Hermes→DSH and DSH→Hermes, same Task, no new external Matrix input, effects once, artifact/report and final-event checks | Other directions, the new configuration candidate, complete fault/session/cancellation coverage |
| Real case23 | Actual startup inference and diagnosis of missing QwenPaw MCP boot identity | ACK/step execution/cutover failed; effects0/0/0 |
| Real case24 | Fixed QwenPaw native ACK, source native read, checkpoint4; Hermes fresh session; effects once across generations1/2/2; old stop before create | Native submission/final reply failed because the local overlay omitted the MCP SDK; not a third accepted direction |
| Case24 generation3 repair | With the missing MCP dependency installed and native discovery preflight passed, the same Task automatically submitted through native MCP and delivered one authoritative final reply, without new external Matrix input or additional effects; old QwenPaw sessions retained | Does not retroactively pass the two-generation case, saved-result-only retry, active return to QwenPaw or complete fault matrix |
| Real case25 | Hermes native ACK/read and checkpoint4; QwenPaw fresh-session continuation, steps once at generations1/2/2 and native report submission | Final delivery failed: QwenPaw ended its native stream without a final message after `submit_task`; the adapter misclassified the persisted report as an interrupted turn. Checkpoint9 remains pending verification. The callback fix has42 native-SDK regression passes, but has not passed a new real lane. |
| Real case26 | After an initial QwenPaw model-directory startup timeout and automatic restart, unchanged execution/delivery checks passed: same Task, no external Matrix input, effects once at generations1/2/2, artifact/report hashes and one final report | Added native-cache checks failed: the SDK saved its session again after the completion callback, invalidating the cached native digest. Execution/delivery success does not establish valid-session reuse; the combined lane remains unaccepted pending the cache-order fix and a new real run. |
| Real case27 | QwenPaw5272ce2/Core4c680c6c source session/cache/checkpoint4 consistency passed after the ordering fix. Hermes965b612/Shared05967bf6 continued steps once at generations1/2/2 and delivered the authoritative report; Leader accepted the Task | Strict no-new-room-message check failed: nine automatic Leader tool-status events arrived before the Worker's final report. No event was discarded and the check remains in place. This is partial evidence, not a third accepted direction. |
| Real case28 | Hermes→QwenPaw on the pinned candidates passed the complete three-step checks: absent target Task cache, fresh native session, generation1→2, checkpoint4→10, effects once, artifact/report hashes, one final reply and no external Matrix input after stop. Final QwenPaw session and cache digests match | This adds the third accepted directed lane, not a complete Harness scenario set. Local source overlays, plaintext Matrix and the configured quiet test Leader are explicit fixture boundaries. |
| Real case29 | QwenPaw normal restart passed: same container with a new actual process start/generation, same valid native session reused, checkpoint4→10, effects once, matching artifacts/report, one final reply and no external Matrix input | Does not prove abnormal exit, same-Harness container replacement, missing/expired sessions, saved-result-only delivery or uncertain-effect verification. |

Evidence is under [the recovery record](../development/runtime-recovery-1631.md)
and its adjacent `evidence/` directory. Configuration and protocol test counts
must never be presented as the six-Harness execution matrix.

Accepted directed lanes are currently3/30: case20 Hermes→DSH, case21
DSH→Hermes and case28 Hermes→QwenPaw, each at its recorded sources. Case29 is
a same-Harness normal-restart/valid-session scenario and adds no cross-Harness
direction. No Harness has completed its full required scenario set. Recorded
live fixtures use `AGENTTEAMS_MATRIX_E2EE=0`; they do not validate encrypted
backlog or crypto continuity.

Trace acceptance is also separate. QwenPaw case23 exported explicit Task/team/
generation fields. Hermes case24 repair exported its recovery event ID and
native session. Core `8ba908e3` now decodes Task/team/generation from internal
Hermes recovery event IDs, with31 telemetry regression passes; live readback
of those fields passed directly from Langfuse in case27, with internal recovery
event ID and native session `20261117_072926_4326694b`. Specific Controller
readback of those case27 traces remains open. Turns admitted from a real Matrix
event need separate correlation and are not covered by that decoder. Dashboard
trace UI readback remains unverified for these fixtures.

Case26 QwenPaw trace `4c1b76a68080ec1f5122185f28a1992d` was read through
Controller and directly from Langfuse with Task/team/generation2, room/event,
native session and eight model observations. Dashboardv1.4.1 on the isolated
fixture reached the workspace with token login, but navigating to Tracing
returned to a rejected-login page. That UI path remains failed/unaccepted;
the underlying authentication cause is not established.

Before full acceptance, complete six same-Harness and30 directed lanes, normal
and abnormal restart/replacement, valid reuse, lost/expired/stale sessions,
saved-result-only delivery, external uncertain-effect verification, execution
conflicts and cancellation/reassignment. Include active A→B→A with the old
session and cursor retained. Record exact component SHAs/image identities,
stop/start order, execution generations, checkpoint versions, independent
effect counts, artifact digests and Matrix event evidence.

Controller still needs the persistent preflight→stop-new-execution→step
checkpoint→stop-old→confirm-stopped→create-target→recover state machine.
Dashboard and `agt` must submit to and observe that same operation. Existing
runtime selection/update or an idle-only restriction is not this capability.
Keep incomplete PRs Draft and #1631 open; inclusion in the 1.2.8 scope does not
authorize publishing or deploying this candidate.
