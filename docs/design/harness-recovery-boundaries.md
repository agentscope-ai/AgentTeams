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
| QwenPaw | Existing `qwenpaw_worker` member loader; Core publishes the resolved standard fields | Core Matrix channel `task_execution.py`: self discovery/claim, real SafeJSONSession existence plus native/checkpoint digests, Task-specific queue identities and SDK reload admission sharing | Explicit MCP stdio environment must carry the current process boot identity. Case23 failed without it; case24 source ACK/step-one passed with it. Same-process error retry and initial absent-cursor catch-up remain open. |
| DeepSeek Harness | Existing standard member loader materializes the `.dsh` profile/workspace | `scripts/matrix_bridge.py`: Task admission, checkpoint-bound session marker, native missing-session fallback and saved-result delivery | First-sync pending input is retained. SDK/MCP credential scrubbing must preserve scoped Task access. Case20/21 cover two specific real lanes; case22 uncertain-effect recovery failed before verification. |
| OpenClaw | Candidate `scripts/configure-runtime.py` projects the Worker record, validates native configuration and publishes a private derived cache | Native Task/process/session recovery adapter remains to be implemented | Existing full entrypoint Matrix state reset/relogin must be replaced with preserved crypto/cursor handling before recovery acceptance. Native config validation is not gateway/Task recovery. |
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

Evidence is under [the recovery record](../development/runtime-recovery-1631.md)
and its adjacent `evidence/` directory. Configuration and protocol test counts
must never be presented as the six-Harness execution matrix.

Trace acceptance is also separate. QwenPaw case23 exported explicit Task/team/
generation fields. Hermes case24 repair exported its recovery event ID and
native session. Core `8ba908e3` now decodes Task/team/generation from internal
Hermes recovery event IDs, with31 telemetry regression passes; live readback
of those fields remains open. Turns admitted from a real Matrix event need
separate correlation and are not covered by that decoder. Dashboard trace UI
readback remains unverified for these fixtures.

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
