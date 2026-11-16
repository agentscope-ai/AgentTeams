# Worker recovery evidence for #1631

Status: **incomplete; Draft implementation**. This document records contract
tests separately from native Harness and real-model acceptance. No existing
installation was upgraded or deployed for this work.

## Source baseline

Core base: `8dbf1eae8c8ca33c7a01a84fe6132dc6aaba4238`.
The isolated branch already contained `697d4e96` (backend disappearance) and
`bdde5291` (Manager disappearance). Neither is execution-continuation evidence.

| Harness | Core's fixed source SHA | Initial local checkout |
| --- | --- | --- |
| Codex | `c171788b2b7d6cbb8fbbb0cbcbef6cb95a69a217` (publish workflow) | matched |
| Hermes | `66e8237f8ac804be0016e2a2603d063aa1ae509a` | matched |
| QwenPaw | `41e151b271a57b95d5ca467f8759fa5ed299cebc` | matched |
| DeepSeek | `d337004b1e461c70ddb9f87d20d87926b0db79c4` | matched |
| OpenClaw | `5e44ebecabf878eda499b668c9627208dbfa6049` | matched |
| OpenHuman | `99fc3f518295b38061039da95df699f64f7f3ba8` | matched |

Candidate companion pins on this Draft branch: Codex `6962984217dfff5d2cc0782960b3cecf95a8e8f8`
and Hermes `0234f38f8668d943da499d2106230e62c6226087`, plus DeepSeek
`008dd4db04ed32786bb4c2d9eabd32eee3a49893`. All consume the shared
Core contract at `eb52bf70d0a33c848fe6a4af72833ec553cc5ce8`. These are review
candidates, not accepted/released Harness versions.
Core `ecb44a61` main and each companion's latest main were merged into the
isolated work branches, preserving the runtime Skill fixes. The contract
commit includes those shared helpers and the current-member role fix; older
real-model evidence retains its original exact source pins.

## Implemented contract

- `GET /api/v1/tasks/recoverable`: authenticated team Worker/Leader self
  discovery from task records. It does not grant Project list permission.
- `POST /api/v1/tasks/{id}/execution?team=...`: strict task-ETag CAS for claim,
  checkpoint and release. Task identity is Controller namespace/team/task ID;
  Project is optional lookup context. Task status is preserved.
- Execution generation binds the backend container ID/start time and the
  supervisor's process-start identity. It is independent of outbox leases.
  A different process cannot take over a still-live container, even after
  release. An unverifiable backend fails closed.
- Harness identity comes from the actual backend instance runtime label,
  rather than the desired Worker spec. Legacy instances without that label
  require recreation before execution can be claimed. Sleeping Workers retain
  the applied spec hash so a changed Harness is replaced on wake.
- Checkpoints retain version, completed/remaining steps, operation identity
  and verification evidence, artifact digests, saved result and pending reply
  transaction/event. The server verifies persisted artifact bytes first.
  Later checkpoints retain published artifact references and cannot rewrite
  evidence or restart the same verified operation. New output revisions use
  distinct persisted artifact keys.
- A restart with a started/unknown operation enters `needs_verification`.
  A new checkpoint must verify that operation before recording more progress.
  Generic task metadata writes cannot forge/drop the execution record; Worker
  writes require the current process fence. Cancellation/reassignment wins.
- Shared `taskflow.checkpoint_task`, authoritative execution merge, and
  shared Python recovery client are present in both TeamHarness copies.
- The Controller-written current member role takes precedence over a stale
  container creation environment; legacy environments remain the fallback
  when the member role is absent. Tool payloads cannot override that identity.
- Codex companion work admits recovered Tasks before Matrix polling, checks
  session checkpoint digests, builds fresh sessions when stale, preserves the
  unrelated journal backlog/cursor, and separates saved-result delivery.
- Hermes uses the SDK's actual session transition API for fresh recovery
  sessions. Changing only the adapter queue key did not isolate SDK history.
  Its SDK-owned session metadata now binds Task identity, Harness, source
  routing, expiration and shared checkpoint digest. Native reuse requires a
  live persisted SDK row; otherwise recovery creates a new conversation.
  Concurrent recovery admission is serialized. Installed SDK tests cover
  these paths with a mocked Task client; real Hermes restart/reuse passed
  case 11 after configuring the test members' native home rooms.
- DeepSeek discovers/claims Tasks before Matrix admission, preserves first-sync
  pending input, and uses the same checkpoint/result contract. A missing native
  session returns a dedicated error before admitting input/tools, permitting a
  fresh session; other native failures fence uncertainty and bypass ordinary
  Matrix event retries. This adapter has no real-model Task acceptance yet.
- DSH's native subprocess scrubber removes the Controller credential-file
  environment variable. Its MCP transport now forwards that scoped file path
  explicitly. A real installed MCP transport/checkpoint tool regression first
  reproduced the connection closing, then verified supervisor process headers,
  authorization and conditional checkpoint HTTP after the fix. Its Task HTTP
  server is a fixture; this is not real-model Task continuation.
- Member MinIO policies deny direct put/delete of Task `meta.json`, including
  Manager identities, so a directory sync cannot bypass Controller CAS. Live
  MinIO probes verified denial through the raw binary, preserved reads and
  ordinary artifact writes, and successful Controller conditional writes.
  That probe restored the old fixture's policy; the new Controller has not
  completed a real continuation run. The wildcard also reserves nested files
  named `meta.json` under Task directories; compatibility with such artifact
  names remains open. External storage issuers still need equivalent policy.
- A fresh Hermes SessionStore route now also evicts the old warm SDK Agent.
  Case 12 showed that changing only the route could still execute/write in the
  previous conversation. The installed SDK warm-cache regression failed
  before the fix and passed afterward; real continuation on this newer pin is
  still required.
- Verified incoming Hermes assignments retain their actual Matrix event ID in
  the native input, allowing its native commit to produce a real read receipt.
  Startup-only recovery does not manufacture a Matrix source/read. The wake
  endpoint now delegates retained-instance validation/start to the reconciler.
- Summary-only submissions with an execution record persist a report before
  metadata CAS. A current active native final callback saves its answer before
  invoking submitted-report recovery; pending saved replies stay authoritative.
- Submission retires an already-delivered progress reply before recording the
  final task status. Final delivery prefers the persisted authoritative report;
  an unconfirmed result/transaction cannot be replaced or discarded. Recovery
  transport failures return controlled MCP errors rather than killing the tool
  server. These newer safeguards have unit coverage; cases 06–08 below used the
  earlier Core `516f22a7` contract.

## Local validation

| Check | Status | Evidence boundary |
| --- | --- | --- |
| Controller server/auth/backend suites | passed | Go tests; no native model |
| Existing Controller cutover race tests | passed | disappearance ordering only |
| Recovery/instance identity regressions | passed | Go tests with fake storage/backend HTTP |
| Shared recovery client | 13 passed | Python unit tests plus 3 transport-error subtests |
| Task metadata commit and transition validation + shared recovery | 36 passed | Python tests |
| Task ownership, conclusions and continuation | 100 passed | separate MCP test process; no real model |
| Controller full unit suite | passed | 0.519 seconds after sleeping-spec fix |
| Controller integration envtest with race | passed | 68.402 seconds; source at runtime-identity change |
| Codex existing suite | 29 passed | SDK/transport unit fixtures |
| Codex recovery admission/session/delivery/reassignment | 9 passed; 38 total | mocked client; no native model |
| Hermes startup recovery | partial | 30 host unit tests passed, 1 skipped; SDK cache/admission tests use mocked Task client; real normal restart and valid native-session reuse passed case 11; remaining scenario set open |
| DeepSeek startup recovery | partial | 63 Python and 7 Node tests passed; Task client mocked; native missing-session/no-input, tool/read, resume/error and stop/steer smokes passed with a deterministic model; real-model Task continuation not accepted yet |
| DeepSeek native MCP checkpoint scope | passed, HTTP fixture | actual SDK scrubber and stdio MCP tool preserve credential-file access, process fence and conditional Task HTTP; no model/real Task acceptance |
| Task metadata storage authority | bounded probe and case 13 passed | full OSS unit suite 7.315s; live MinIO raw write/delete denied while reads, ordinary artifacts and Controller CAS remain functional; updated Controller completed one real Codex-to-Hermes continuation |
| Independent receipt oracle | 3 unit tests and case 13 passed | separate per-invocation objects outside the Task directory; stale directory ledger overwrite and uncertain-effect verification covered by unit tests; receipts are not storage-enforced WORM |
| Codex packaged image | built; 33 tests passed | `agentteams/recovery-codex:1631-4759ede`, image ID `8d71656edacc34ad82006eb5ea91f46eba1a83855588d073c71a69fa06b567b0` |
| Codex native stop/steer | passed | actual App Server/tools, deterministic Responses fixture; not Task continuation E2E |
| Two real Harness continuation chain | cases 06/07 passed; full scenario sets incomplete | real Codex to Hermes and reverse replacements, same Task, generation 1 to 2, checkpoint 4 to 10, all steps once, no new input, one final report |
| Result saved / reply unsent against real Matrix | passed, bounded case 01 | zero model calls, unchanged step ledger, one report body; event `$8KK7IrnddlsgCgKUTyJpzAzQZ8EPAxbDooNOOvwc7Hg` |
| Return to retained old Codex session | passed for delivery-only case 02 | zero model calls, unchanged steps; execution continuation with stale session still open |
| Codex normal restart and valid native-session reuse | case 08 passed | same container ID, new process/start identity, same actual native thread, latest-checkpoint digest matched, all steps once and one final reply |
| Canonical final report after execution recovery | case 09 passed | Core `2c90bccf`, Codex `1cbc966`, Hermes `0b0892f`; real replacement continuation, same Task, each step once, zero new Matrix input, unique final reply bytes exactly match the submitted report |

The metadata-commit tests load another assembled server namespace and rebind
global refs. Combining them with the MCP suites in one pytest process failed
(95 failed, 36 passed); the separate CI-style commands above pass. That test
isolation limitation remains open and is not runtime continuation evidence.

### Isolated live fixture (2026-11-16)

`tests/runtime_recovery_e2e.py` is an opt-in setup/seed driver, not an automated
passing acceptance test. It uses only resources prefixed
`recovery-1631-live-20261116`, its own data volume, and a non-overlapping subnet.
The three-step helper is executed only by the real Harness's native model tool;
the driver does not execute steps on the Worker's behalf. Its persisted ledger
counts executions, and immutable step artifacts are uploaded before checkpoints.
Case 06 completed execution plus final delivery without manual repair after
cutover. Read-only verification rechecks artifact bytes, step counts, Docker
stop/destroy/create order, the original Matrix source/read receipt, zero new
external Matrix message inputs and exactly one final report body. The receipt
is read from Matrix history; the new Harness's local receipt database is not
treated as the authority for an old Harness's read.

Versioned evidence: [case 06](evidence/runtime-recovery-1631-case06.json).
This accepts one forced-container-replacement Codex-to-Hermes run, not the
complete per-Harness scenario set, process-only restart or valid-session reuse.

[Case 07](evidence/runtime-recovery-1631-case07.json) independently passed the
reverse Hermes-to-Codex direction using Hermes `9e7f5a8`. The original Hermes
assignment/read receipt and Codex final reply were read back from Matrix;
the same three once-only steps, artifact digests and Docker ordering passed.
Each result applies to its recorded source pins and forced-replacement case.

[Case 08](evidence/runtime-recovery-1631-case08.json) passed a same-Codex normal
restart through the lifecycle API. The container ID stayed the same while its
actual start/process identities and execution generation changed. The local
session was unexpired and matched checkpoint 4, and the real native thread ID
was reused. Checkpoint 10 was delivered with each step once and no new input.
This does not accept every same-Harness restart/session scenario.

[Case 09](evidence/runtime-recovery-1631-case09.json) passed the updated
Codex-to-Hermes forced replacement in the new November 17 fixture, using Core
`2c90bccf`, Codex `1cbc966` and Hermes `0b0892f`. It additionally compares the
Matrix final body and checkpoint result bytes with the authoritative submitted
report. Task ID stayed stable, generation advanced 1 to 2, checkpoint 4 to 10,
each step ran once, and the original read/final events and zero new external
inputs were verified. This does not validate startup of the retained old
Matrix database.

[Case 10](evidence/runtime-recovery-1631-case10-failed.json) is retained as a
failed full acceptance: Hermes reused the same actual native session and
executed every step once, but four Leader home-channel setup notices arrived
between stop and the final reply. The strict no-new-input assertion failed;
it was not relaxed or replaced by a delivery-only result.

Before the next Task, the native `MATRIX_HOME_ROOM` setting was configured on
these disposable members through the Worker API (`prepare` command). No
production default, Matrix history or database was changed.
[Case 11](evidence/runtime-recovery-1631-case11.json) then passed normal Hermes
restart plus actual valid native-session reuse, including an unexpired cache
whose digest matched checkpoint 4, stable native session
`20261116_174411_c9c3228e`, generation 1 to 2, three once-only steps, unique
canonical report bytes, original read receipt, and zero new external inputs.
The [native SQLite audit](evidence/runtime-recovery-1631-case11-session-audit.json)
also found 42 messages in the selected native session before its recorded
stop, confirming the reuse was not just a SessionStore ID comparison.

[Case 12](evidence/runtime-recovery-1631-case12-failed.json) failed before
Hermes-to-DeepSeek cutover. The native helper executed step one, then a direct
Task-directory mirror uploaded stale `meta.json` and `attempts.json`, losing
the shared checkpoint and step ledger. Native messages also landed in the
previous warm Agent's session despite a newly selected SDK route. The driver
refused cutover; DeepSeek never started. Evidence was preserved and only that
fixture Task was cancelled. These failures are not accepted continuation.

New fixtures use separate per-invocation effect-receipt objects outside the Task
directory instead of trusting a mutable `attempts.json` file. Receipt-backed
uncertain effects are verified without another effect; absent evidence waits
for verification. The driver additionally checks that actual messages exist
in the selected Hermes native session, and records Controller source
separately from the shared adapter contract. These updated paths still need
the complete real-model scenario set after the storage and warm-cache fixes.

[Case 13](evidence/runtime-recovery-1631-case13.json) passed with Controller
`e3788c38`, shared contract `2c90bccf`, Codex `1cbc9665` and Hermes `6bbd17d6`.
It used the protected member storage policy and receipt oracle version 2 on
the fresh, separately labelled `recovery-1631-live-20261117-v3` fixture.
The Task stayed `in_progress` at checkpoint 4, then recovered without another
Matrix input, generation 1 to 2 and checkpoint 4 to 10. Outside-directory
receipts prove steps one/two/three ran in generations 1/2/2 exactly once;
artifact bytes and the canonical 597-byte report were read back and checked.
The source/read/final events and old die/destroy before new create are retained.
The selected fresh Hermes native session contained 35 messages. This accepts
one updated Codex-to-Hermes replacement, not every required scenario.

The new v2 fixture did not start a Task: its random storage secret began with
`-`, which the existing `mc alias set` invocation parsed as a CLI flag.
It was stopped with data retained. The fixture generator now prefixes new
storage secrets with `recovery-`; the production CLI parsing limitation remains
open. The v3 setup used subnet `10.231.166.0/24` and separate storage. No old
Matrix backlog or existing installation was reset.

[Case 14](evidence/runtime-recovery-1631-case14-failed.json) is retained as a
failed Hermes-to-DeepSeek run. Hermes selected a fresh native session with 33
persisted messages and durably finished step one. The target overlay then
exited with `ModuleNotFoundError: task_recovery` before claiming the Task or
running another step: it retained the published image's old entrypoint rather
than copying the checkout's shared-library path export. The driver did not
count startup as continuation. That Task was cancelled with its checkpoint and
receipt retained. A corrected overlay copies all checkout runtime scripts;
the canonical source entrypoint already exported the path. A separate Task
will exercise that package, keeping this failed run distinct.

[Case 15](evidence/runtime-recovery-1631-case15-failed.json) then started the
corrected package and claimed generation 2, but ran no target steps. The first
limited Matrix timeline had no saved local cursor; gap filling raised before
Task execution, which was still deferred until after Matrix sync. The Task
was cancelled with only the original step-one receipt. DeepSeek `9eee22a`
now resumes from the Task system before Matrix polling, and initial limited
timelines page backwards from `prev_batch` without clearing pending events.
Two focused regressions first failed, then passed; the final Python suite has
63 tests after removing the unused claim-only mode, with 7 Node tests passing.
The [Matrix pagination contract](https://spec.matrix.org/v1.19/client-server-api/#get_matrixclientv3roomsroomidmessages)
defines `prev_batch`/`from` and backward direction. Empty intermediate pages
with advancing tokens remain covered; repeated tokens still fail closed.

[Case 16](evidence/runtime-recovery-1631-case16-partial.json) progressed through
real Hermes-to-DeepSeek execution: the stable Task reached generation 2 and
all three steps were verified once, with receipts in generations 1/2/2.
Final submission nevertheless failed repeatedly because MCP's container
environment still said `standalone` while current Controller `runtime.yaml`
said `worker`. The Task was retained `in_progress` at checkpoint 9, with the
Worker stopped and all artifacts preserved; there is no accepted final reply.
Core `9bdbbe92` fixes that precedence in both compatibility copies. The stale
standalone/leader role regressions failed before the change; MCP 303 tests and
protocol 80 tests passed afterward. This partial execution is not full
DeepSeek continuation acceptance or a saved-result/no-model redelivery test.

After the main merges, local validation passed: Codex 41, Hermes 32 with one
skip, DeepSeek 63 Python plus 8 Node tests. Core server/controller/backend/OSS/
modelhealth/service/app passed (29.484/0.489/2.353/8.202/1.123/10.048/2.126s).
The shared role fix and refreshed dependency pins still need real-model and
exact-head hosted reruns. No existing installation was updated.

[Finalization evidence](evidence/runtime-recovery-1631-case16-finalization.json):
the retained Task was subsequently finalized after replacing only its stopped
test Worker with the role-fixed package: generation 3 submitted and delivered
a report, without another step receipt. This is not accepted as a clean
Hermes-to-DeepSeek run: the model also wrote a checkpoint that dropped artifact
references and changed the verified operation evidence to `test`. The fixture
Controller was still `e3788c38`, which admitted those regressions. New
Controller regressions first failed, then the updated server suite passed
(28.434s after the final guard). The server now rejects discarded/replaced artifact references and
dropping, restarting or rewriting the same verified operation. That protection has not
yet been exercised by a new real-model run. The standard verifier also rejects
an additional unrecorded replacement instead of attributing the final process
to the original target image.

The Core integration run on `3eadbca5` completed with 24 successful jobs and
one failed Hermes LLM-interaction shard: `test-03-assign-task` found a README
without the current Task marker. That run used Hermes `0b0892f`; the newer
warm-Agent fix is not yet verified by that hosted scenario. Do not relax its
marker assertion. Codex hosted validation passed on `f1d28b5`. Hermes hosted
validation passed on `6bbd17d` (run `43935165345`, publish job skipped).
The added installed-SDK test at `e6f9af9` uses actual persisted messages for
valid-cache cases and verifies a fresh empty native session is rebuilt;
it passed locally with a mocked Task client, and its hosted test/image jobs
passed at `e6f9af9` (run `43937762092`, publish skipped). Core `e3788c38` integration
run `43935173141` was still running at this check.

Live policy evidence: [bounded MinIO probe](evidence/runtime-recovery-1631-storage-policy.json).

Recorded live failures and partial evidence:

- Case 01: Codex step one, followed by Hermes steps two/three after fixes.
  A later startup delivered the persisted report without model execution.
  Report SHA256: `4c34228421d63d37a126a6ac7fa747f4e1baf1fe470a2ac418f01979dc5ed301`.
- Case 02: Codex generation 1 step one; Hermes generation 2 steps two/three.
  A retained old Codex session later performed saved-result delivery only,
  event `$dP4RwwDwjELSwQ9dxRyVApoLm7M2hJOVccEOIAoyqwg`.
- Case 03: all three steps once and native Hermes MCP submission, but final
  delivery failed because no report file existed. This exposed the active
  final-callback and summary-only report gaps now covered by regressions.
- Case 04: actual fresh Hermes session `20261116_142936_8fded5cf`, but fixture
  instructions caused duplicate operation registration. Step two remained
  `needs_verification`; no unproved side effect was rerun. The fixture now
  explicitly owns its own operation/checkpoint writes.
- Case 05: actual old Hermes image on wake while desired runtime was Codex.
  No step ran. The sleeping applied-hash/runtime-identity regression is fixed.
- Cases 03-05 were cancelled after evidence capture. They remain failed runs.
- Case 06 passed on Core `516f22a7`, Codex `49e8870`, Hermes `702c54f`.
  The task remained `in_progress` at checkpoint 4 before replacement, then
  reached `submitted` with delivered checkpoint 10. Matrix source/read/final
  events and three verified artifact digests are recorded in the evidence file.

Live setup exposed and source regressions now cover these issues:

- A Worker created before joining a Team kept `standalone` in its container
  environment. Native startup must still use authenticated self-discovery.
- Real Hermes `openclaw.json` has no `team_id`. It cannot gate Task recovery.
- A delivered progress turn does not complete an `in_progress` Task. Submitted
  saved replies remain delivery-only, and a delivered native turn is not replayed.
- Codex recovery must retain original Matrix source IDs, exclude synthetic
  recovery IDs from read receipts, and preserve other rooms' same-ID admissions.

Local Docker disk cleanup removed the previous build blocker. Overlay image
startup required executable modes on the entrypoint and copied `mc-wrapper.sh`;
the official Harness Dockerfiles already set these modes. Real Qwen
`qwen3.6-plus` Responses gateway probe returned HTTP 200. This is provider
connectivity evidence, not native Task execution. Codex tracing bootstrap lacks
`opentelemetry` in this image; trace ingestion/readback remains open. Only the
new fixture's Collector/Controller joined the existing tracing network; existing
service configuration and volumes were not changed.

The November 17 rerun built Core `2c90bccf` (Controller binary SHA256
`e1dc3478fe26d520efd05a3080d65814677fa3696e2a03866ce46bce5fe3e4e7`)
and updated Codex/Hermes overlay images. Only the labelled disposable
Controller was replaced. Its HTTP health and CLI authentication became ready,
but Tuwunel stayed CPU-busy without listening on port 6167. An isolated copy
of its 81 MB database and a process backtrace were retained for diagnosis;
restarting that fixture's Matrix process did not immediately restore service.
That failed database startup added no real execution acceptance. Case 09 later
ran against the separate fresh fixture described below. No Matrix backlog or
database was cleared.

A second fixture, `recovery-1631-live-20261117`, has its own new data volume
and non-overlapping network. Its Controller and Matrix health checks returned
200; this is setup evidence, not recovery of the retained old database. Run
the driver with `RECOVERY_E2E_PREFIX=recovery-1631-live-20261117`; fresh setup
also requires an inspected `RECOVERY_E2E_SUBNET` (this run used
`10.231.164.0/24`). Private state/bootstrap files are scoped to that prefix.
The first updated overlay lacked the copied mc wrapper's executable mode;
the corrected Harness overlays use `local-20261117013200`. This was a local
overlay packaging failure; the canonical Dockerfiles already set that mode.

Reproduce the focused checks from Core:

```sh
cd agentteams-controller
go test ./internal/server ./internal/auth ./internal/backend
go test -race ./internal/controller -run 'Cutover|Replacement|BackendSwitch' -count=1
go test -race ./internal/server ./internal/backend -run 'TestTaskRecovery|TestRecoverableTasks|TestDockerRecovery|TestPodRecovery' -count=1
cd ..
python3 -m unittest discover -s tests -p test_task_recovery.py -v
PYTHONPATH="$PWD/shared/lib" python3 -m pytest plugins/tests/teamharness/test_task_meta_commit.py plugins/tests/teamharness/test_transition_validation.py tests/test_task_recovery.py -q
AT_SOURCE="$PWD" PYTHONPATH="$PWD/shared/lib:$PWD/codex-harness/src:$PWD/deepseek-harness/scripts" python3 -m unittest discover -s codex-harness/tests -p 'test_*.py'
```

The last command requires the independent Codex repository checked out at
`codex-harness/`; that directory is a local companion checkout, not Core source.

## Required remaining work

- Persist the complete cutover state machine: preflight, stop admission,
  checkpoint at a step boundary, stop old instance/tools, observe absence,
  create target, then recover. The existing disappearance checks cover only
  part of this sequence. Do not substitute idle-only switching.
- Finish and validate the common startup/admission contract in all six
  Harnesses, including native tool interruption uncertainty. Normal subprocess
  restarts within one live container still fail closed on the process conflict;
  a verified process-tree handoff is required to make that path automatic.
- Validate real Hermes native persistence/reuse and interruption handling,
  and DeepSeek Task continuation; implement/validate QwenPaw, OpenClaw and
  OpenHuman startup resume and delivery adapters. Audit same-process retry
  and revision handling, including uncertain Matrix HTTP acceptance across
  device/transport changes; stable transactions alone do not prove that case.
- Expand the case 13 protected-policy/warm-Agent/receipt-oracle real-model
  acceptance across the remaining scenarios, and resolve the nested
  `meta.json` artifact compatibility and external-storage policy gaps.
- Resolve Manager ordinary-message handoff/receipts from the original #1631
  reproduction. Worker Task fences do not resolve Manager cursor replay alone.
- Verify all six same-Harness and 30 directed cross-Harness combinations,
  including switching back with stale native sessions/cursors retained.

No matrix cell has completed its full required scenario set. Partial real
execution is labelled separately from acceptance:

### Case 17: rejected apparent success

[Case 17](evidence/runtime-recovery-1631-case17-failed.json) ran in a fresh
`recovery-1631-live-20261117-v4` installation (subnet `10.231.167.0/24`). The
Controller really contained `1629bdf5`; its Linux binary SHA256 was
`a0100a2183a32a952f1331144cdf19c7f2a3a62b91ac8cfc4ae387e7e7dbeab7`.
Hermes `9c82bf1f` and DeepSeek `035da183` used shared Core `9bdbbe92`.
No existing installation or old Matrix database was changed.

The native DeepSeek shell scrubbed `AGENTTEAMS_AUTH_TOKEN_FILE`, although its
MCP transport explicitly forwarded the path. The unchanged acceptance helper
failed before execution with that missing environment key. The model then
manually uploaded step outputs/receipts and submitted checkpoints without the
prescribed start/verified boundary. The Task reached submitted/delivered, but
receipt start versions were `1/5/6`, not the helper's `1/5/7` sequence. This is
not a passing execution-continuation sample. The updated verifier rejects it.

The shared recovery client now falls back to the same scoped Worker token
mount used by `agt` when only the environment path was scrubbed; it does not
use Matrix credentials or an admin token. The regression first raised the
same KeyError, then all 14 recovery-client tests passed. An actual read-only
probe inside that Worker, with the environment path removed, authenticated
self-discovery through the mounted token. Two receipt-boundary tests cover
the valid sequence and reject this real counterexample. The Controller also
requires a persisted operation start before accepting verification of a new
operation identity; the new regression first failed.

This exposed a remaining oracle trust gap: Worker-written receipt objects can
be synthesized. Before accepting another DeepSeek execution case, move effect
counts to a separately owned fixture service/ledger, with Worker-readable
results and no Worker write access to the authoritative counts. Record both
effect execution and verification calls. This is test infrastructure, not the
deferred product Task database. Native Python telemetry also reported missing
`opentelemetry`. DeepSeek now installs the shared telemetry dependencies and
OpenAI SDK in its Python venv; packaged native smoke passed. Real DeepSeek
trace ingestion and trace/UI validation remain open.
The two fixture Workers were stopped with all data retained.


### Independent effect oracle v3 (not real-model acceptance)

The next fixture uses a separate HTTP service/container with its own durable
ledger volume and no Docker socket or Worker storage mount. It forwards each
Worker credential to real self-scoped Controller discovery and validates the
current instance/process, Task status, registered operation and completed-step
prefix. Only this service executes/counts the fixture side effect. Worker
outputs are artifacts, not the authority for execution counts. The driver has a
separate private proof credential that is never projected into Workers.

Duplicate POST execution is rejected and recorded. A replacement with an
unknown result can GET/verify an existing effect without executing it again;
an absent result remains unproved. The ledger fsyncs before returning a result.
Twelve local fixture/oracle/boundary tests pass, including HTTP credential
forwarding/proof isolation, cancellation/reassignment/ownership rejection,
restart readback and forged Worker receipt rejection. Task HTTP in those unit
tests is mocked; fresh real-model acceptance is still required. Historical
oracle-v2 evidence is retained with its trust limitation.

[Case 18](evidence/runtime-recovery-1631-case18-failed.json) failed before
its first effect: the fixture wrongly assumed execution remains `active`
after an operation-start checkpoint. The Controller intentionally sets
`needs_verification` for unresolved operations, including a newly registered
start. The updated mock first reproduced the failure. The helper now registers
a private-ledger intent before the Task start checkpoint; execution requires
that exact process, generation and expected checkpoint version. Replacement
processes can verify existing effects but cannot execute the previous
process's unproved start. Twelve tests pass with the actual Controller state
transition represented. No production state rule was relaxed. Both Workers
were stopped and the failed Task and empty effect ledger were preserved.

[Case 19](evidence/runtime-recovery-1631-case19-failed.json) completed source
Hermes step one with independent ledger version 1, checkpoint version 4 and
a durable output. The old container exited/destroyed before DeepSeek creation;
the new native session discovered the same Task and claimed generation 2 with
zero new external Matrix inputs. However, the target manually pre-registered
the helper-owned step-two operation before invoking the helper. No matching
ledger intent existed, so the target effect remained blocked. Counts were
1/0/0, and the sample failed rather than rewinding the checkpoint or fabricating
a result. Both Workers were stopped and their data retained.

The shared resume instructions and both Task-execution Skill copies now
explain checkpoint-owning executors: invoke them unchanged and do not register
their operation IDs or duplicate their checkpoints outside their protocol.
This clarifies who records the start; it retains the requirement to persist a
start before any side effect and to verify uncertain outcomes. Recovery-client
14 tests and the TeamHarness contract/parity check pass. A fresh real-model
run is required to validate the instruction change.

| From / to | Codex | Hermes | QwenPaw | DeepSeek | OpenClaw | OpenHuman |
| --- | --- | --- | --- | --- | --- | --- |
| Codex | normal restart + valid session case 08 passed; other scenarios incomplete | replacement cases 06/09/13 passed; other scenarios incomplete | untested | untested | untested | untested |
| Hermes | replacement case 07 passed; other scenarios incomplete | normal restart + valid session case 11 passed; other scenarios incomplete | untested | untested | untested | untested |
| QwenPaw | untested | untested | untested | untested | untested | untested |
| DeepSeek | untested | untested | untested | untested | untested | untested |
| OpenClaw | untested | untested | untested | untested | untested | untested |
| OpenHuman | untested | untested | untested | untested | untested | untested |

Each real run must record exact Core/Harness SHAs, task ID, old/new process
identities, stop/absence/start ordering, execution generation, checkpoint
version, step counters, artifact hashes and Matrix event/transaction IDs.
Run the three-step in-progress continuation with **no new Matrix input after
stop** separately from saved-result redelivery. Also cover normal/abnormal
restart, replacement, valid/expired/missing session, uncertain side effects,
conflicting claim, cancellation and reassignment. Keep failures and missing
evidence open in #1631. No automatic merge, release or existing-install deploy.
