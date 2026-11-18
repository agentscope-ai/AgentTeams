# Worker recovery evidence for #1631

Status: **incomplete; Draft implementation**. This document records contract
tests separately from native Harness and real-model acceptance. No existing
installation was upgraded or deployed for this work.

## Live acceptance by source and target

The diagonal records normal restart with a compatible native session.
Each off-diagonal pass requires the original Task to continue after the old
instance stops, with a new process generation, verified independent effects
and one final Matrix reply without a new incoming message. Each report keeps
the image and shared-contract pins actually tested.

| Source → Target | Codex | Hermes | QwenPaw | DeepSeek | OpenClaw | OpenHuman |
| --- | --- | --- | --- | --- | --- | --- |
| Codex | [Pass 82](evidence/runtime-recovery-1631-case82-verified.json) | [Pass 86](evidence/runtime-recovery-1631-case86-verified.json) | [Pass 99](evidence/runtime-recovery-1631-case99-verified.json) | [Pass 94](evidence/runtime-recovery-1631-case94-verified.json) | [Pass 121](evidence/runtime-recovery-1631-case121-verified.json) | — |
| Hermes | [Pass 80](evidence/runtime-recovery-1631-case80-verified.json) | [Pass 68](evidence/runtime-recovery-1631-case68-verified.json) | [Pass 69](evidence/runtime-recovery-1631-case69-dashboard.json) | [Pass 81](evidence/runtime-recovery-1631-case81-verified.json) | [Pass 111](evidence/runtime-recovery-1631-case111-verified.json) | [Partial 128](evidence/runtime-recovery-1631-case128-partial.json) |
| QwenPaw | [Pass 103](evidence/runtime-recovery-1631-case103-verified.json) | [Pass 70](evidence/runtime-recovery-1631-case70-verified.json) | [Pass 77](evidence/runtime-recovery-1631-case77-verified.json) | [Pass 105](evidence/runtime-recovery-1631-case105-verified.json) | [Pass 114](evidence/runtime-recovery-1631-case114-verified.json) | — |
| DeepSeek | [Pass 97](evidence/runtime-recovery-1631-case97-verified.json) | [Pass 91](evidence/runtime-recovery-1631-case91-verified.json) | [Pass 107](evidence/runtime-recovery-1631-case107-verified.json) | [Pass 85](evidence/runtime-recovery-1631-case85-verified.json) | [Pass 123](evidence/runtime-recovery-1631-case123-verified.json) | — |
| OpenClaw | [Pass 122](evidence/runtime-recovery-1631-case122-verified.json) | [Pass 115](evidence/runtime-recovery-1631-case115-verified.json) | [Pass 138](evidence/runtime-recovery-1631-case138-verified.json) | [Pass 124](evidence/runtime-recovery-1631-case124-verified.json) | [Pass 110](evidence/runtime-recovery-1631-case110-verified.json) | — |
| OpenHuman | — | — | — | — | — | — |

**5/6 normal restart lanes and 20/30 directed handoffs have passed.** A dash
indicates a lane awaiting acceptance on the common contract. The independent
[QwenPaw → Hermes → QwenPaw round trip](evidence/runtime-recovery-1631-case96-round-trip-verified.json)
verifies retained cursors and a fresh returning session. Fault scenarios and
the original Manager replay regression are tracked separately. Historical
runs retain their source-pinned scope in the sections below. The
[Harness comparison](../design/harness-recovery-boundaries.md) describes
the implementation boundaries.

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
  Matrix event retries. Its bounded real-model Hermes→DeepSeek replacement passed case 20; the remaining scenario set is open.
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
| DeepSeek startup recovery | partial | 63 Python and 7 Node tests passed; Task client mocked; native missing-session/no-input, tool/read, resume/error and stop/steer smokes passed with a deterministic model; real-model Hermes→DeepSeek replacement passed case 20; remaining scenarios open |
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

### Case 20: independent-ledger execution continuation passed

[Case 20 proof](evidence/runtime-recovery-1631-case20-verified.json) accepted one
real Hermes→DeepSeek in-progress replacement using Core/shared
`eb52bf70d0a33c848fe6a4af72833ec553cc5ce8`, Hermes
`0234f38f8668d943da499d2106230e62c6226087` and DeepSeek
`008dd4db04ed32786bb4c2d9eabd32eee3a49893`. The Controller binary SHA256 is
`b7ad1c098348085e37b4b58fc5a5d77b207cde61d0daaa30d0dc68612ec4be77`.
The images are local overlays, not published builds: Controller
`local-20261117053228`, Workers `local-20261117053547`.

The same Task remained in progress at source checkpoint 5 after step one and
a real native read receipt. Old die/destroy preceded target creation, and
Docker inspect/top confirmed the old instance was absent. With no external
Matrix input after stop, DeepSeek discovered/claimed generation 2 and created
fresh native session `session-task-c22b4fe8f9154a55af77e9573b8fac26`.
Its unchanged helper executed steps two/three. The private ledger recorded
exactly three effects, generations 1/2/2 and start versions 1/6/8; all six
intent/effect requests were accepted. Native audit found no manual
`checkpoint_task` or raw HTTP bypass. Artifact bytes/digests were verified,
and checkpoint 11 retained all three artifacts and the saved canonical report.
One Matrix reply exactly matched that 762-byte report; no external message
arrived between stop and final delivery. Repeated idempotent submission calls
did not duplicate the final report.

Preparation exposed a local overlay COPY removing the `mc` wrapper executable
bit. Before any execution claim/model call, the Task was still assigned.
The overlay restored the public Dockerfile's chmod, preserved the same Task
and queued assignment, and sent no new Matrix message. Future driver seeding
now requires `runtime.ready` after the current container start, rather than
trusting only Docker Running. The proof records this preparation boundary and
the actual driver file hash/source changes.

Tracing was configured before model execution, capture off. Two source Hermes
traces were read through the Controller API (`ddaae5bdbf7ce352d6c58c19ab63a887`,
`b1b65720630bcd7dd64d4fcf5e4cc858`). A DeepSeek trace and canonical Task
correlation were not found; browser automation timed out. Full tracing/UI
acceptance remains open. This positive execution case does not cover abnormal
restart, lost/expired sessions, unknown effects, cancellation/reassignment,
saved-result redelivery or the other six-Harness/30-direction cells.

Exact-head hosted checks passed for Codex `69629842`
([43960937273](https://github.com/agentteams-group/agentteams-harness-codex/actions/runs/43960937273)),
Hermes `0234f38f`
([43960940113](https://github.com/agentteams-group/agentteams-harness-hermes/actions/runs/43960940113))
and DeepSeek `008dd4db`
([43960954223](https://github.com/agentteams-group/agentteams-harness-deepseek/actions/runs/43960954223)).
Publication was not requested. Core `c7978e24` Controller checks passed; its
full integration run remains pending at that older head. Core `9e4fe59a`
Controller [43963110164](https://github.com/agentscope-ai/AgentTeams/actions/runs/43963110164)
and full integration [43963110039](https://github.com/agentscope-ai/AgentTeams/actions/runs/43963110039)
subsequently passed. The latter Hermes job verified both the remote Task README
and its current nonce. This does not establish the cause of the older intermittent
README failure or pre-approve later commits.

### Case 21: reverse execution continuation passed

[Case 21 proof](evidence/runtime-recovery-1631-case21-verified.json) accepted one
real DeepSeek→Hermes in-progress replacement at the same Core/shared and Harness
SHAs as case 20. Task `live-deepseek-harness-hermes-21` retained its identity,
generation advanced 1→2 and checkpoint 4→10. The private ledger recorded steps
once each, generations 1/2/2 and start versions 1/5/7. The source sent a native
read receipt; old die/destroy preceded target creation. Without new external
Matrix input, Hermes reconstructed a fresh native session, continued steps two
and three, retained verified artifacts and delivered exactly one canonical reply.
The driver checks and native source audit passed. Returning to Hermes also
retained completed Task 20 checkpoint 11 unchanged; that delivery-only cache
check is not active same-Task A→B→A execution acceptance.

### Case 22: unresolved effect captured; recovery acceptance failed

[Case 22 evidence](evidence/runtime-recovery-1631-case22-failed.json) records a
real Hermes helper process (PID 6186, start tick 22034597) interrupted after the
independent external effect, before its verified checkpoint/artifact. The Task
remained in progress at checkpoint 1 with a started operation and no completed
steps. DeepSeek claimed generation 2 but exited before verifying that outcome.
Counts stayed 1/0/0. Its first Docker state contained a network endpoint error;
later Docker storage was confirmed full (183G/183G), and even minimal container
creation, image deletion and cache cleanup initially failed with ENOSPC. The
original native exit cause is not established.

The fixture now exposes this bounded after-effect barrier and verifies that a
replacement reads the previous outcome before continuing. Thirteen local
fixture/oracle/boundary tests pass, including interruption, outcome readback,
no duplicate effect, and Controller serialization of an empty artifact slice.
The common verifier also compares artifact content against the independent
ledger. Unit results do not count as a real unknown-effect recovery pass.

After explicit user authorization, 92 local AT test containers and 45 selected
AT test/1.2.7 image references were removed, with all volumes retained. Private
container metadata and bounded v7 logs were backed up first. Minimal container
startup passed after cleanup. The failed Task and independent ledger remain in
their volumes; additional repaired starts must be recorded separately and must
not reset the Task/effect count or be called a clean two-generation acceptance.

| From / to | Codex | Hermes | QwenPaw | DeepSeek | OpenClaw | OpenHuman |
| --- | --- | --- | --- | --- | --- | --- |
| Codex | normal restart + valid session case 08 passed; other scenarios incomplete | replacement cases 06/09/13 passed; other scenarios incomplete | untested | untested | untested | untested |
| Hermes | replacement case 07 passed; other scenarios incomplete | normal restart + valid session case 11 passed; other scenarios incomplete | strict replacement case28 passed; case25 failed,26 partial retained | replacement case 20 passed; other scenarios incomplete | case33 execution/delivery/cache passed; tracing/full smoke open | untested |
| QwenPaw | untested | strict replacement case30 passed; case24 failed and case27 partial retained | normal restart + valid session case29 passed; other scenarios incomplete | untested | untested | untested |
| DeepSeek | untested | replacement case 21 passed; other scenarios incomplete | untested | untested | untested | untested |
| OpenClaw | untested | untested | untested | untested | untested | untested |
| OpenHuman | untested | untested | untested | untested | untested | untested |

Each real run must record exact Core/Harness SHAs, task ID, old/new process
identities, stop/absence/start ordering, execution generation, checkpoint
version, step counters, artifact hashes and Matrix event/transaction IDs.
Run the three-step in-progress continuation with **no new Matrix input after
stop** separately from saved-result redelivery. Also cover normal/abnormal
restart, replacement, valid/expired/missing session, uncertain side effects,
conflicting claim, cancellation and reassignment. Keep failures and missing
evidence open in #1631. The user authorizes merge only after actual validation;
incomplete changes remain Draft. No release or existing-install deployment.

### Case25: native submission succeeded; final delivery failed

[Case25 evidence](evidence/runtime-recovery-1631-case25-failed.json) records
Hermes `4cb4e6a` with Core/shared `8ba908e3` and QwenPaw `9d7346ab`
with Core/shared `f96f9dda`, using the unchanged `774d6285` Controller binary.
These were local source overlays; the evidence includes image identities and
does not represent a canonical full image build. Native Hermes MCP discovery
passed before inference. Source Hermes acknowledged/read Task
`live-hermes-qwenpaw-25`, persisted step one and checkpoint4, and stopped before
the QwenPaw replacement was created. No new external Matrix message was sent.

QwenPaw continued steps two/three and submitted the persisted report through
its native MCP tool. The independent ledger recorded each step once, with
generations1/2/2. Its SDK then ended inference without a final message. The
completion callback rejected that valid submitted-report path and recorded an
unknown native-turn operation at checkpoint9 instead of delivering the report.
This is a failed lane. The Task, report, unknown operation and ledger remain
unchanged for later verified recovery.

The fix delegates an empty native final to authoritative report verification.
It requires a submitted Task with a stored report, retains execution fencing
and unknown-operation checks, and uses the existing durable reply transaction.
The native-SDK regression initially failed four cases; after the fix42 tests
pass, including empty/in-progress results, replaced execution and unknown
operation rejection, and both legacy/bundled native completion callbacks.
This regression result does not retroactively pass case25 or prove delivery
retry after arbitrary callback failures.

Additional targeted native-SDK delivery/gate/reload suites passed144 tests
with7 existing mock coroutine warnings; host contract golden checks passed13
and skipped3 optional native SDK probes. An attempted all-bundle run inside
the local QwenPaw image was not green (433 passed,26 failed,4 errors,13 skipped):
that invocation lacked a system-Python YAML dependency, the E2EE test backend
and the uninitialized OpenClaw submodule. Its log is retained; it is not
reported as a full bundle pass. The repository CI installs those dependencies
and initializes submodules separately.

### Case26: execution/delivery passed; native cache consistency failed

[Case26 evidence](evidence/runtime-recovery-1631-case26-partial.json) uses
Core/shared `05967bf6`, Hermes `965b612a`, QwenPaw `4ce3bf30` and the unchanged
`774d6285` Controller binary. Source native ACK/read and checkpoint4 passed.
QwenPaw initially exited on a model-directory API timeout, then automatically
restarted in the same container. No manual Worker restart or new external
Matrix message was sent. The execution claim binds the later actual start and
new process, generation2. Steps remained once each at generations1/2/2;
checkpoint10 is submitted/delivered, with matching artifacts/authoritative
report and one Matrix report event. The original execution/delivery verifier
passed without weakening its checks.

A stronger follow-up native-cache check failed: `remember` ran inside the
completion callback, before the SDK committed turn usage and saved the native
session again. Its cached native digest no longer matched the actual session
file. This safely causes rebuilding instead of reusing that session, but does
not satisfy valid-session reuse. Preserve the partial evidence and do not count
the combined lane as a third strict acceptance. The fix moves cache binding
after the native stream and cleanup finish; both legacy/bundle regressions
failed before it. Recovery/reload tests now pass47 and fixture/oracle tests14;
real validation of the new ordering remains open.

QwenPaw companion `07230780` separately disables automatic remote catalog
discovery when saving an explicitly declared managed model. The real SDK
probe accessed the slow `/v1/models` endpoint before the fix; afterward native
configuration/readback passed with zero catalog or model calls. All32 API
regressions passed. The probe is part of independent image smoke CI.

Case26's explicit Task/team/generation2 trace was read through Controller and
Langfuse. Dashboardv1.4.1 setup/token login reached the workspace, but Tracing
navigation returned to a rejected-login page; UI acceptance remains open.

### Case27: native cache fixed; strict message isolation failed

[Case27 evidence](evidence/runtime-recovery-1631-case27-partial.json) records
QwenPaw `5272ce21` / shared Core `4c680c6c` → Hermes `965b612a` / shared Core
`05967bf6`, with the unchanged `774d6285` Controller binary. After native source
ACK/read and step one, the real session file digest matched its cache and the
latest checkpoint4; the cache-order fix therefore passed this source-side real
check. No fixture helper process was running before cutover. Old stop/delete
preceded target creation. Steps stayed once at generations1/2/2, artifacts and
the authoritative report matched, and the final report had one Matrix copy.
Leader subsequently accepted the native Worker submission, leaving `completed`.

The strict no-new-room-message assertion failed on nine automatic Leader
tool-status bubbles between source stop and Worker final reply. The driver
sent zero new inputs, but this fixture did not remain quiet. Preserve the
partial evidence; do not exclude those events or count the lane as accepted.
Future isolated runs set the test Leader's supported
`HERMES_TOOL_PROGRESS_MODE=off` through its Worker environment before assignment;
native review stays enabled and all backlog/events and the strict assertion
remain intact. This changes fixture display only, not production defaults.

The verifier now checks exactly one native Worker submission and, for an
already `completed` Task, a subsequent genuine Leader acceptance history.
`completed` alone, missing/repeated submissions, a wrong reviewer or reversed
sequence fail. This handles normal acceptance without rewriting Task status;
fixture/oracle checks pass16 tests and6 negative subtests. Actual process start
binding and QwenPaw source/final cache consistency checks were also added.

Hermes case27 internal recovery traces `aedc74aa0212ab8418d2800bf5802207` and
`23ed6a3a8a9b258b7ca10fe680db1df2` were read directly from Langfuse with explicit
Task/team/generation2 and native session correlation. Specific Controller and
Dashboard UI readback remain open. The v11 Controller and both Workers were
stopped after recording evidence, retaining data, sessions and message backlog.

Independent QwenPaw `5272ce21` test/image/native configuration smoke CI passed;
Hermes `965b612a` test/image CI passed. Core candidate pins now consume these
exact revisions. Neither independent CI result proves full six-Harness or
30-direction execution acceptance.

### Case28: pinned Hermes→QwenPaw continuation passed

[Case28 proof](evidence/runtime-recovery-1631-case28-verified.json) uses Hermes
`965b612a` / shared05967bf6 → QwenPaw `5272ce21` / shared4c680c6c,
Controller binary774d6285 and drivere8717a7c. These are local source overlays;
independent canonical test/image CI passed separately. The fresh v12 fixture
enabled tracing before creating Workers. Before assignment, the test Leader's
native effective tool-progress setting was verified off through its Worker
environment; native review stayed enabled. Matrix backlog and strict
no-external-message assertions were preserved.

Task `live-hermes-qwenpaw-28` had native ACK/read and a durable step-one
checkpoint4, with no QwenPaw Task cache. Source helper processes were absent at
the boundary; old die/destroy preceded target creation. Without new Matrix
input, QwenPaw claimed generation2, rebuilt a native Task session and executed
steps two/three. The independent ledger proves steps once at generations1/2/2.
Checkpoint10 is submitted/delivered, all artifact/report bytes match and the
canonical final report has exactly one Matrix event. The final native session
digest and latest shared checkpoint both match the cache. This accepts one
new direction; cases25/26/27 remain failed or
partial at their original revisions.

### Case29: QwenPaw normal restart and valid session reuse passed

[Case29 proof](evidence/runtime-recovery-1631-case29-verified.json) uses the same
QwenPaw5272ce21/shared4c680c6c candidate and Controller774d6285. After native
ACK/read, step one and checkpoint4, the existing normal Worker restart API
stopped and restarted the same container. The execution identity binds its new
actual start/process; generation advanced1→2. Its existing, unexpired native
Task session was reused with the same session ID and valid source/final
digests. Without new Matrix input, steps two/three completed, checkpoint
advanced4→10, artifacts and the canonical report matched, and one final report
event was delivered. The independent ledger remains1/2/2 with no repeated step.

This is one same-Harness normal-restart/valid-session scenario, not abnormal
restart, replacement, lost/expired-session, saved-result-only or unknown-effect
acceptance. Both live cases use plaintext Matrix (`AGENTTEAMS_MATRIX_E2EE=0`);
encrypted backlog/crypto continuity and Dashboard trace UI remain unaccepted.

### Saved-reply retry after native stream closure

QwenPaw's process-local admission guard also blocked delivery recovery after a
native completion had saved a result but failed to send it or acknowledge its
delivery. The two Matrix adapters now share an additional live-native set on
the SDK task tracker. Polls do not reclaim an executing native turn. After its
stream, tools and final native persistence close, a saved pending reply may be
claimed and retried with the original transaction; historical admission still
prevents another inference for that generation. Ownership changes and unknown
operations retain the authoritative claim checks.

Fault regressions on the installed QwenPaw SDK failed on the previous code and
passed after the fix for both adapter copies and both send/ack failure points.
Recovery/reload suites passed51 tests. These use the real channel/session SDK
with injected native-stream, authority and Matrix boundaries; they do not
establish real delivery-fault E2E, another directed lane or step continuation.

OpenClaw Harness candidate
[`e57b75aa`](https://github.com/agentteams-group/agentteams-harness-openclaw/commit/e57b75aa2c631a215d1c2b1dd6d4d7e065b5fa95)
removes launch-time Matrix deletion/password relogin. Its12 host tests and2
actual pinned-SDK SQLite-store tests passed. The latter reopen existing stores
around the real launch preamble with both clean-shutdown marker values, retain
device/cursor/synthetic crypto snapshot, and isolate a different device. This
does not prove a real SIGKILL/WAL crash, encrypted traffic, replacement snapshot
or native Task recovery. The candidate remains Draft and unpinned by Core.

### Case30: QwenPaw→Hermes strict continuation passed

[Case30 proof](evidence/runtime-recovery-1631-case30-verified.json) uses
QwenPaw`e0bc7bb5`/shared`67dba059` → Hermes`965b612a`/shared`05967bf6`,
Controller binary`774d6285` and the recorded driver hash. QwenPaw's new
independent test/image/native-smoke CI passed; Core's candidate pin now selects
that revision. The local source overlay initially lacked the canonical
`chmod +x mc-wrapper.sh`, and failed before any Task was assigned. A new image
restored the executable mode and passed actual `mc`/SDK/entrypoint preflight;
the failed overlay and startup evidence remain recorded separately.

Task`live-qwenpaw-hermes-30` had a native read/ACK and checkpoint4 with step one
complete, a matching QwenPaw cache/transcript and no running helper. The target
had no Hermes Task session. Docker die/destroy preceded target creation.
Without external Matrix input, Hermes claimed generation2, created its native
conversation and continued steps two/three. The independent ledger contains
exactly three effects at generations1/2/2. Checkpoint12 is submitted/delivered;
artifacts and the canonical report match, and final event
`$k2Qv6pxJaqQa1wDtpm_G6tNQ_dcc9MZVHxuY_hrxL_4` appears exactly once.
The strict query found zero external room messages between stop and that reply.
The Worker and main Task subsequently reached completed. Native Leader review
remained enabled; its effective tool-progress setting was verified off before
cutover. No Matrix backlog or old native session was cleared.

Controller and direct Langfuse readback both verified traces
`74acad0de8d6cef9a6ecbd2ff1e9461d` and
`64e484a76834eca50ef1032442769af1`, including Task/team/generation2,
room/event and native session`20261117_095017_66399845`. Dashboard UI was not
tested on this fixture. Including the earlier Codex↔Hermes cases, six directed
lanes have bounded real continuation evidence. These results apply to their
recorded source SHAs, not a fresh six-lane regression on one latest candidate.
The full Harness scenario set remains open. Saved-result testing is separate.

### Case31: real saved-result retry passed; native-cache acceptance failed

[Case31 proof](evidence/runtime-recovery-1631-case31-delivery.json) records a
test-only QwenPaw first-send fault after submission. An independent read-only
observer saw checkpoint9 with the verified saved report, three completed steps
and zero canonical Matrix replies. Without a restart or new external message,
the same actual process/generation2 retried the original transaction, reached
checkpoint10/delivered and sent exactly one canonical report. No new Task model
turn occurred after the fault; the independent step/effect ledger stayed once
per step. Controller and Langfuse readback verified trace
`6821fb22519c9ce966257686aa9a1d75` with Task/team/generation/session correlation.
Dashboard UI was not tested. Exact source pins, image digests and the injected
patch are archived in the proof. The fault is before sending, not response loss.

The normal continuation verifier still failed its native Task cache assertion:
the interrupted stream left no accepted QwenPaw cache. The assertion was not
relaxed, and this saved-delivery result adds neither a direction nor a complete
restart/continuation acceptance. The original fixture and failures are retained.

### Native recovery JSON bridge

`shared/lib/task_recovery_rpc.py` exposes the existing shared recovery client to
native non-Python adapters over JSON stdin/stdout. Discovery and conditional
claims keep the existing scoped Worker API; reply mutations recheck Task/team,
process, instance and generation and delegate to the common implementation.
It does not widen Project listing or create a second execution lease. Empty
native finals require an already submitted durable result. Six host boundary
tests pass; they mock authority and are not Task continuation E2E.

### Case32: OpenClaw real execution/delivery observations; full lane unaccepted

[Case32 partial proof](evidence/runtime-recovery-1631-case32-partial.json)
records Hermes965b612a/shared05967bf6 → OpenClaw34c4fb8/shared1a364763,
using a canonical local OpenClaw Dockerfile build. Without new external Matrix
input, OpenClaw claimed generation2 and invoked native model/exec/MCP tools.
The three effects remained once at generations1/2/2; checkpoint10 was delivered,
all artifacts and the authoritative report matched, one final Matrix report
was read back, and the Task subsequently completed through native Leader review.

This is not full lane acceptance. The driver requested cutover before capturing
its required source native/cache/process boundary snapshot. The later snapshot
is explicitly retained as post-cutover. The OpenClaw native cache initially
matched58 transcript events, then a native skill-workshop notice appended a59th
event; the old cache correctly fails comparison with that newer transcript.
No cache or verifier assertion was rewritten to pass. Task trace/UI acceptance
also remains open. The driver now captures and exclusively persists its source
boundary before lifecycle mutation, with a regression test. SDK-based native
cache/transcript readback is required for future OpenClaw acceptance.

### Case33: Hermes→OpenClaw execution/delivery/cache passed; tracing open

[Case33 proof](evidence/runtime-recovery-1631-case33-verified.json) records
Hermes965b612a/shared05967bf6 → OpenClaw7828fc2/shared1a364763, with the
unchanged Controller Go binary774d6285 and the revised driver at its recorded
hash. OpenClaw used its canonical local Dockerfile build, with an explicit
local test Controller image; the image was not published. Its independent
test/image CI passed at7828fc2; the prior34c4fb8 image CI also passed.

Before lifecycle mutation, the driver persisted checkpoint4/step one with a
matching Hermes cache and29 actual native messages, no helper processes and no
target Task cache. Old die/destroy preceded target creation. Without new
external Matrix input, OpenClaw claimed generation2 and its real native
model/exec/MCP tools continued steps two/three. The independent oracle contains
exactly three accepted effects at generations1/2/2. Checkpoint10 is delivered;
all artifacts and the canonical report match, with exactly one Matrix reply
`$FRlAffwpT0s1VgqtcXs7joEHZsaqIpZDLIeoIono2EU`. Actual SDK SQLite session
`498957cf-ec03-4434-ac91-aefd27e009f5`, transcript digest and latest checkpoint
matched the native cache at verification. Source read/receipt and zero external
Matrix messages after stop were read back. The Task later completed through
native Leader review. This adds a seventh direction with bounded real Task
execution/delivery evidence; normal OpenClaw restart and other fault scenarios
are still untested.

Full smoke acceptance remains open: tracing was configured before Workers,
but Controller readback returned no OpenClaw traces. The Controller currently
projects observability WorkerEnv only for QwenPaw/Hermes, and this OpenClaw
native config has no diagnostics exporter. Trace collection/correlation and
Dashboard UI must be implemented and verified, not inferred from saved settings.

### Case34: OpenClaw normal restart completed steps; valid-session case failed

[Case34 partial proof](evidence/runtime-recovery-1631-case34-partial.json)
uses Core0f36d4c3 and OpenClawf0874a0. The same container restarted with a new
actual process identity, claimed generation2 and reached checkpoint10/delivered;
the native Worker submitted and the Leader accepted the result. The strict
valid-session-reuse verifier failed because the source native transcript no
longer matched its saved cache before restart. The target used a different
native session. Do not relabel this as a dedicated stale-session acceptance or
count a fourth normal-restart/valid-session Harness. The original assertion and
failure are preserved. Tracing and Dashboard login also remain unaccepted.
Controller and Workers were stopped after evidence capture, with volumes kept.

### Cases36–37: common-contract Hermes restart and valid-session reuse

[Case36 partial evidence](evidence/runtime-recovery-1631-case36-partial.json)
uses Core19eb30c6, Hermes59b756d and native SDK2bd378ed. Without new Matrix
input after normal restart, the same Task continued from checkpoint4 to10,
generation1 to2; all three effects occurred once and the canonical final reply
was delivered. Valid-session reuse failed: the restart materialized the default
Worker state and advanced the CR generation, which incorrectly advanced the
effective configuration version5 to6. The target correctly rejected that
descriptor and rebuilt. The partial case and original failure remain retained.

[Case37 verified evidence](evidence/runtime-recovery-1631-case37-verified.json)
uses the fixed Core262ecb13 server, the same Hermes59b756d/native SDK2bd378ed
and shared client19eb30c6. A fresh v19 fixture exercises the first implicit
Worker-state to explicit Running restart, rather than repeating a restart
after that default has already been saved. Effective configuration remains
version3. Before stopping, the shared descriptor matches actual persisted SDK
rows and checkpoint4. After the old process dies, the new actual start claims
generation2 and reuses native session `20261117_182827_58677490`. With zero new
external Matrix input, steps two/three finish at checkpoint10. The independent
oracle records each step once at generations1/2/2; artifacts and the submitted
report match their hashes, with one final Matrix report and native source read
receipt. The strict verifier passes without changing its reuse assertion.

[Trace readback](evidence/runtime-recovery-1631-case37-traces.json) records
Controller and direct Langfuse retrieval. The owned Langfuse/ClickHouse services
had been OOM-stopped; they were restarted with their existing data before this
fixture's Workers were created. Dashboard UI trace acceptance remains open.
The SDK also ran its background skill-library review after the recovered Task
turn, inheriting the Task trace context. That separate model/tool activity is
identified in the evidence and is not another accepted Task execution. Generic
background/tool ownership fencing and accurate origin labeling remain open.
Case37 establishes one normal Hermes restart with valid-session reuse under
the new common contract; it does not add a fourth Harness to the historical
3/6 count or prove the remaining restart faults or 30-direction matrix.

### Case38: common-contract Hermes→QwenPaw reached all steps; submission cancelled

[Case38 partial evidence](evidence/runtime-recovery-1631-case38-partial.json)
uses Core262ecb13, Hermes59b756d/shared19eb30c6 and QwenPawe88ebe8/sharedf6afb507
with native SDK2.2.2.post1. The source checkpoint4 matches its common descriptor
and actual Hermes content. Old die/destroy precede target creation/start. With
no new driver Matrix message, QwenPaw discovers/claims generation2 and executes
steps two/three. The independent oracle records exactly one effect per step at
generations1/2/2; all three persisted artifacts match. Native SDK readback and
the recovered Task's Trace route identify the real target session/tool history.

This case fails the strict final-submission assertion. The ownership monitor
used the same state restriction as a new inference/final-cache admission.
The Controller deliberately marks a durable `started` checkpoint as needing
verification while its existing native tool is still running. A monitor poll
of that same owner cancelled the stream before submission and then recorded
an unknown native turn at checkpoint9. Task38 remains in_progress and
needs_verification; its original state is retained, not cleared or completed.

The fix separates ongoing ownership monitoring from admission/final-cache
permissions. Only monitoring the same process/instance/generation may retain
its still-started operation. Unknown effects, cancelled Tasks, changed runtime,
reassignment and replaced execution still fail; a new inference or cache
commit cannot use that exception. Both Matrix compatibility copies are fixed.
The actual native-stream regression failed for both copies before the fix.
The focused native-SDK suite now passes119 tests, including delayed tool
completion plus unknown/cancelled/replaced-owner rejection. These tests mock
Controller/model authority and do not retroactively accept case38. A new
real Task/image must verify the fix before counting a passing direction.


### Case39: common-contract Hermes→QwenPaw continuation passed

[Strict verified evidence](evidence/runtime-recovery-1631-case39-verified.json)
uses the actual Core262ecb13 server, Hermes59b756d/shared19eb30c6/SDK2bd378ed,
and QwenPaw0589ed8/shared62c1d369/SDK2.2.2.post1. The new v20 Task retains its
identity through replacement: source checkpoint4 and its common Hermes cache
match persisted native content; old die/destroy precede target creation.
Without a new external Matrix input, QwenPaw claims generation2, resumes steps
two/three, submits through its native Task tool and reaches checkpoint10.
The independent oracle records all three effects once; persisted artifacts
and canonical result hashes match, and the final report appears once.
The target Trace route selects the actual SDK session; no private journal
fallback authorizes this proof. The submitted Task intentionally has no new
session-cache write because the server rejects submitted-Task cache commits.

[Trace evidence](evidence/runtime-recovery-1631-case39-traces.json) includes
Controller and direct Langfuse retrieval plus actual Dashboard v1.4.1 readback
through its normal Controller-admin login: exact Task/room/generation/native
session,24 model observations,25 tool observations and1,017,481 tokens.
Three HTTP-error nodes recovered during the turn; this is not an injected
unknown-side-effect acceptance. This verifies the bounded H→Q Task lane under
the new standard, not the complete Matrix/ordinary-message integration,
Dashboard runtime-switch UI, all fault scenarios or a new eighth direction.
Case38 remains failed/partial with its original Task state retained.

Core now pins QwenPaw0589ed8 after its matching test/image CI44155767972 passed
(publication skipped). Core62c1d369 Controller CI44156255299 passed. The older
Core262ecb13 integration44146912227 failed only its OpenClaw/OpenClaw task03
README assertion; the assignment did reach the Worker room despite earlier
m.direct errors, so those errors alone do not explain the failure. Later
Core86e7e134 integration44153988880 has a QwenPaw/Hermes failure under diagnosis.
These integration failures remain open; no timeout/assertion is relaxed.


### Case40: common-contract QwenPaw→Hermes continuation passed

[Strict evidence](evidence/runtime-recovery-1631-case40-verified.json) uses
Core262ecb13, source QwenPaw0589ed8/shared62c1d369/SDK2.2.2.post1 and target
Hermes59b756d/shared19eb30c6/SDK2bd378ed. The source Task remains in_progress at
checkpoint4 with a shared QwenPaw descriptor matching the actual persisted SDK
state. The tightened driver requires this cache before initiating replacement.
Old die/destroy precede target creation. With no external Matrix input, Hermes
claims generation2, creates a fresh native Task session and completes remaining
steps at checkpoint10. The independent effect ledger records each step once;
artifacts/report hashes and the unique final Matrix reply pass unchanged strict
assertions. An initial verification read preceded final reply persistence;
the same verifier passed once native delivery completed, without repair/reseed.

[Trace readback](evidence/runtime-recovery-1631-case40-traces.json) verifies the
actual recovered turn through Controller, direct Langfuse and Dashboardv1.4.1:
Task40/generation2/native session20261117_194337_6e1646f1,17 model and24 tool
observations,387,672 tokens and102.09seconds. Error nodes remain visible.
The SDK background skill review inherits Task context and is recorded separately;
it is not another Task execution. Subsequent room chatter remains a policy/
consumption investigation, not proof of the original Manager replay bug.
Both owned v20 fixture Workers were slept through the normal lifecycle API
after evidence capture; Tasks, native files and infrastructure data are retained.

Cases39/40 now establish the new standard's two reference directions. They use
different Tasks and do not establish same-Task return-to-an-old-session safety.
Historical totals remain3/6 normal restart/reuse and7/30 directed lanes, and
full Matrix hooks, ordinary messages, handoff and fault suites remain incomplete.

### Common sync batch preparation (native hooks still pending)

`WorkerMatrix.consumer` and `WorkerMatrix.sync`, also exposed through the existing
JSON/stdin facade, centralize native cursor selection and ordered batch commits.
Account/device/format/Harness must match; a shared token from another native
consumer is never reused. Decoded, policy-classified inputs retain server order.
A new consumer whose timeline does not reconnect the old shared room anchor
must retain a backfill gap even when the SDK calls its initial timeline unlimited.
Existing gaps are retained; missing pagination/retained anchors reject the batch
without cursor advancement. The Controller's input-first persistence and CAS
remain authoritative. Native adapters must call this before dispatch or local
cursor advancement; this helper alone is not a completed native integration.
The common/evidence regression suite passes80 tests plus52 subtests, including
all six cursor identities, cross-device/format isolation, stale cursor rejection,
limited/initial gaps, exact ordering, old-start fencing and the shared facade.


### QwenPaw accepted-input and native persistence hooks

The packaged channel and the compatibility adapter now commit accepted text,
media, control and buffered group-context inputs through the Worker-self Matrix
API before enqueue/history admission. Verified Task assignments are linked to
an unambiguous owned team/Task/room identity; their execution authority remains
the existing Task. Redelivery retains the original input/work classification;
a read failure cannot be treated as an absent record. These are per-callback
commits without a consumer/token, not a fabricated native batch checkpoint.
The compatibility adapter retains its distinct SyncEngine implementation rather
than being replaced with the packaged channel. Its awaited Task callback no
longer calls the packaged channel's nonexistent acceptance-future method.

At REPLY_START, a required accepted input is saved by the native SDK, then read
back through SafeJSONSession. The stored pending source identities and actual
state digest precede a current-start/input-ETag-bound native persistence fact;
only then may read acknowledgement and reasoning proceed. Failure stops before
reasoning/read. These facts do not complete work, advance Task checkpoints or
publish a result. Legacy context without a common input receives no new-standard
persistence claim. Native session readback is not a claim that every native
file has already been uploaded by workspace sync.

Focused installed-SDK tests pass105 cases, including both adapter paths, real
SafeJSONSession serialization/readback, input-before-queue/history, no callback
acceptance/token save on persistence failure, fact-before-read/model, Task scope,
redelivery and unavailable-read rejection. Controller/model authority is mocked;
this is not a new real-model E2E pass. Common/evidence Python tests pass83 plus52
subtests. The earlier native callback regression failed6 cases before the fix.
Full-batch consumer cursor/backfill integration, initial/legacy import and ordinary
message execution/result recovery remain open. Existing Task recovery still
runs independently of incoming Matrix events. A new packaged image and real
Task must validate these hooks before updating real Harness acceptance.


The required-input guard is independent of the read-receipt delivery service.
During channel reload, an absent receipt service must not skip the native save
or common persistence fact. With the persistence adapter present, save/fact
still precede inference; without the adapter, a required input fails closed.
A four-case native regression failed both compatibility paths before this fix.
Verified coordination handoffs remain control inputs rather than creating an
ordinary-message execution alongside their coordination authority. The focused
installed-SDK suite now passes111 tests. This follow-up is later than the case41
image: its native regression is not a new real-model/fault-matrix acceptance.


### Case41: QwenPaw restart/reuse with common accepted-input persistence

[Strict case41 evidence](evidence/runtime-recovery-1631-case41-verified.json)
uses actual Core server262ecb13,QwenPaw189664f/sharedfb8bfd0c/SDK2.2.2.post1
and local image09f8bc45. Before restart, the original Matrix assignment is a
shared native_committed input linked to recovery-team/the same Task. Its fact
binds the actual source process/instance, SDK session reference and saved-state
digest; it is not a Task checkpoint. The independent shared descriptor and SDK
state match checkpoint4. Normal restart keeps the container ID but changes its
actual start/process. With no new external Matrix input, generation2 reuses the
same valid native session, finishes steps two/three and reaches checkpoint10.
The oracle records each step once; all artifacts, canonical submission/report,
unique final reply and original native read source pass strict verification.

[Trace proof](evidence/runtime-recovery-1631-case41-traces.json) retrieves both
turns via Controller and direct Langfuse. The source native-fact POST ends at
20:32:53.155Z, before first model start20:32:53.198Z; the retained shared record
identifies that same source session. Actual Dashboard readback of the resumed
turn verifies Task/room/generation2 and the reused SDK session,4 model calls,
5 tools,162,148 tokens,39.10seconds and71 nodes, with no error/HTTP-failure nodes.
Owned v21 Workers were slept after proof capture; data remains preserved.

This revalidates QwenPaw under new input hooks, not a fourth restart Harness or
an eighth direction. Full-batch/legacy/ordinary-message execution integration
and the full fault/return matrix are still open. Case41's image predates the
later absent-receipt-service/control-classification fix; the111-test installed
SDK follow-up does not retroactively change this case's source SHA. Matching
Corefb8bfd0c Controller CI44168348788 and Q189664f test/image CI44168366402 passed
(publication skipped); Core9dd2ca4b pins that candidate after its matching CI.

### Shared per-event acceptance reused by Hermes

`WorkerMatrix.accept` now owns the input-key, immutable redelivery classification,
self-scoped Task/team/room resolution and conditional ingest used by both QwenPaw
copies and the Hermes candidate. `matrix.accept` exposes the same policy through
the JSON bridge for non-Python adapters. Existing input identity survives Task
reassignment; a new input still requires exactly one owned Task/team/room. The
server continues to compare actual immutable input bytes. A per-event callback
does not invent a sync token or claim timeline coverage.

Common recovery/session/Matrix tests pass79 plus6 subtests; the new acceptance
regression failed5 cases before implementation. QwenPaw's installed-SDK focused
suite passes111 with candidate product source mounted into the existing pinned
SDK image. That source-mounted check is not a new packaged-image or model E2E
acceptance. Hermes native accepted-message/context/control/verified-assignment
hooks are being connected in its companion; its shared native-fact guard, batch
cursor/backfill, legacy import and ordinary-message execution remain incomplete.
Historical real coverage remains3/6 and7/30. Core24af3dd3 Controller44176802757
passed; f852ff57 integration44175315246 was still running at this check.

[Packaged adapter evidence](evidence/runtime-recovery-1631-shared-input-adapters.json)
records Hermes e803db0 and QwenPaw66e1e53, both using shared6281c493.
Hermes imagef167d66a repeats15 native callback/session/direct-control tests,
the decoded-input smoke,2 native approval queue tests and native Task recovery/
tool discovery. QwenPaw imageff108c0c repeats111 focused SDK tests. Only tests
and pure Python test dependencies were mounted, not product source. These
checks mock authority/transport and make no new real-model or direction claim.
Hermes normal callback tests failed3 cases before hooking common persistence;
its common native-fact-before-inference guard remains open. Both companion
matching CI runs and Core6281c493 Controller CI were running when recorded;
Core submodule pins remain on the previously green companions until those
new-head jobs pass.

The pinned Hermes SDK's `pre_llm_call` plugin dispatch is bounded and fail-open;
it cannot be the required persistence gate by itself. Native
`agent/turn_context.py` builds the staged user row before that hook and persists
the turn after turn-start compaction. Its persistence helper logs failures.
The remaining native guard must therefore verify actual saved SDK rows and
propagate failure before any inference, including compaction, rather than
equating a hook callback, receipt scan or session routing ID with that fact.

### Hermes native input guard and independent read delivery

The companion now wraps the pinned SDK prologue call site directly, outside
the fail-open plugin dispatcher. It uses the actual SDK writer, then reads the
current user row back by its SDK row ID and platform event ID before submitting
the common native fact. Missing shared input, disabled/failed persistence,
unreadable rows or failed common CAS stop the turn before plugin hooks,
turn-start compaction or inference. Startup without a real Matrix source does
not manufacture input facts. Repeated native flushes retain one user row.

Five actual-SDK tests pass, including a full native turn against a deterministic
local streaming model endpoint and a CAS-failure turn making no inference or
compaction call. The constructor's model-name-only `/api/show` capability query
is recorded separately from inference. These are fixture-authority/model tests,
not a new real-model Task continuation pass. Initial verification mistakes
(non-streaming fixture and classifying `/api/show` as inference) were corrected
without weakening the native ordering assertions.

The independent managed Hermes read scanner also waits for a current common
native fact matching the actual SDK session row. An SDK row alone cannot race
ahead of a pending/failed common commit. Legacy or unconfigured installations
retain their previous read boundary. Common/receipt recovery tests pass90,
with1 optional native-OpenClaw skip and15 subtests.

Hermes e803 CI44179805992 failed its upgrade fixture because a random seed's
numeric suffix plus `:` and nonce matched the SDK Telegram-token redactor.
Actual native inference and Matrix delivery retained history but masked the
nonce. c628197 changes the synthetic proof separator to `|`, retains exact
seed+nonce assertions and leaves egress redaction enabled. Matching
CI44181973721 passed including plain/encrypted cold restore, second restart
and old-key decryption; publication skipped. QwenPaw66e image CI44180199934
failed a localhost custom-provider API timeout in its managed-model smoke.
Local exact-image reproduction passed with zero catalog/model calls; cause
remains unconfirmed. d5ee25f adds failure diagnostics without changing timeout,
assertions or product behavior; matching CI44183066683 is running.

### Case42: Hermes guarded input and real restart continuation

[Strict Task proof](evidence/runtime-recovery-1631-case42-verified.json) uses
the actual Core262ecb13 server and Hermes16963d5/shared69538c9a/SDK2bd378ed,
local image213d6482. The original Matrix input has a current common native
fact referencing the actual SDK row/session before cutover. Source checkpoint4
retains completed step one and pending two/three, with a matching shared/native
session descriptor. Normal restart preserves container ID, changes actual
process/start and generation1→2, reuses session20261117_220131_a8ff56ae and
reaches checkpoint10 without any new external Matrix input. The unchanged
strict verifier checks each irreversible effect once, all artifact/report
hashes, native submission and one canonical report/reply. Task ID is unchanged.

[Tracing proof and limits](evidence/runtime-recovery-1631-case42-traces.json)
records Controller/direct-Langfuse readback plus actual Dashboard inspection
of target54d884897397e288992f5335c11aaaf9: Task/room/generation2 and reused
SDK session match, with11 generation observations,14 tools,382649 tokens.
Its57 observations include3 expected405 `/api/show` capability probes, not
failed inference. SDK background skill review bbe1987ef347e4ee84293f328b7d40e2
is recorded separately, not another Task execution. Later child activity
extends trace duration; it is not the Task's execution wall clock.

Source8564b835f993fad7f0da9badaf14e9ef exists but has no observations or
Task/generation metadata. ClickHouse was OOM-killed before the run and restored
after diagnosis; source telemetry completeness and exact source native-POST/
first-model ordering remain unverified. Independent source native-input fact
and Task execution proof are retained; this gap is not hidden by target tracing
or fixture SDK tests. Shared consumer count remains0; full-batch integration
is still open. Both v22 Workers were slept after proof and actually exited.

Docker storage also reached100% before Task assignment, preventing oracle
volume creation.18 unreferenced recovery-image tags and eligible old build
cache were removed;19 older owned fixture containers were stopped after their
logs were saved privately. No containers, Task data, sessions or volumes were
deleted. The interrupted fresh oracle setup resumed using its original private
configuration before seeding Task42. Disk pressure and ClickHouse OOM are
separate observed resource failures, not a sole explanation for project scope.

[Packaged guard proof](evidence/runtime-recovery-1631-hermes-input-guard.json)
passes16 native/receipt tests plus7 subtests,5 input-guard tests and native
approval/recovery/tool smokes without product source mounts. Hermes169 image
CI44183993767 failed because the successful test turn's asynchronous title
request arrived during the following failed-turn assertion. a815ed0 gives the
failed turn its own endpoint and still forbids every inference/auxiliary call;
CI44185945088 passed, publication skipped. Product paths are unchanged from
the real169 image. QwenPawd5ee25f CI44183066683 passed without a product change;
the original localhost timeout remains an unreproduced failure, not a claimed
root-cause fix. QwenPaw7dba11e also passed CI44186624120 with shared69538c9a.
Core695 Controller44184054560 and earlier f852 integration44175315246 passed.
Core now pins matching green Hermesa815ed0 and QwenPaw7dba11e/shared69538c9a;
the new Core head still needs matching checks. Full coverage remains3/6 normal
restart/reuse and7/30 historical directions, not complete fault suites.

### Case68: packaged Hermes normal restart with valid native session reuse

[Strict case68 evidence](evidence/runtime-recovery-1631-case68-verified.json)
records Core `52a3659b`, Hermes `282c457` and shared contract `52a3659b`.
After the initial native Matrix read and Task ACK, step one was committed
while the Task remained `in_progress`. The first observation occurred before
the native session descriptor was saved. The cutover ran on the same Task
after reading back its matching native digest and checkpoint version 5.
The real restart preserved the container and changed its process identity.
Generation 2 reused the original native session, ran steps two and three,
and submitted the same Task at checkpoint 12. The independent ledger
records generations 1/2/2, all artifact and report hashes match, and Matrix
has one canonical final reply with no external message after the stop.
This lane used plaintext Matrix and did not export to Langfuse.

### Cases69–70: Dashboard and CLI switch the same running Task

Both lanes used Core `52a3659b`, Hermes `282c457`, QwenPaw `89d9f3e`
and shared contract `52a3659b` in the isolated Docker environment. Each
source sent a native read receipt and acknowledged its Task, committed step
one at checkpoint 5, and stopped while the Task was still `in_progress`.
Each target took generation 2, created a new native session, ran steps two
and three and submitted the same Task at checkpoint 12. Each independent
ledger has one effect per step (generations 1/2/2), all artifact and report
hashes verified, and Matrix contains one canonical report and zero external
room messages after cutover.

- [Case69 Dashboard proof](evidence/runtime-recovery-1631-case69-dashboard.json)
  used Dashboard `588c45b` to send the Hermes → QwenPaw request and display
  the completed operation with the same request ID. The interactive test
  observer timed out before the click. Controller's persisted source-stop
  observation preceded the actual target Docker creation by 3.86 seconds.
  A separate Docker query confirmed the old ID was gone. The room audit
  checked from the pre-submit browser snapshot through the final report.
  QwenPaw's native Task session was correlated through Controller trace
  readback.
- [Case70 CLI proof](evidence/runtime-recovery-1631-case70-verified.json)
  used `agt worker switch-runtime` for QwenPaw → Hermes. The live Docker event
  stream records old die → destroy → new create → start. The verifier checks
  both sides of the persisted runtime handoff and the new Hermes native
  session. Start the Docker event subscription before requesting a
  replacement, since retention can roll over before an after-the-fact read.

These are two current-candidate directed lanes. The remaining directed
combinations, the retained-session switchback on these builds, encrypted
Matrix continuity and the full fault matrix remain separate gates.

[Trace evidence for cases69–70](evidence/runtime-recovery-1631-cases69-70-traces.json)
verifies generation-2 model and tool observations and the native session
through the Controller and Langfuse. The Dashboard Tracing view independently
displayed case69's trace ID and recovered Task, room, generation and native
session metadata.

### Case71: OpenHuman startup recovery and test tool policy

[Partial case71 evidence](evidence/runtime-recovery-1631-case71-partial.json)
records Hermes → OpenHuman. The live Docker event stream confirmed the old
container died and was removed before the new instance was created. OpenHuman
`05f35ab` claimed the same Task in generation 2 and entered inference without
any new Matrix input from the driver. Its native shell denied the fixture's
open-ended Python invocation. No second or third effect was observed. This
lane does not count as completed Task continuation.

The disposable fixture now allowlists one scoped `agentteams-recovery-step`
command that invokes the same independent oracle and verifies the fixture
Controller, team and Task. It preserves native autonomy and approval
settings. The production Harness and command policy are untouched. A fresh
live lane is needed to exercise the new fixture command.

### Cases72–75: native startup follow-up

[Partial cases72 and 74](evidence/runtime-recovery-1631-cases72-74-partial.json)
both reached generation 2 under the original Task without a follow-up
assignment. Each independent ledger still contains only step one.

- OpenHuman case72 received an upstream HTTP 200 in 6.25 seconds. Its native
  turn timed out later and the Task remained `needs_verification`. The image
  also failed to import OTel at startup. OpenHuman `9ee359c` installs the
  common pinned telemetry requirements and exercises the import in image
  smoke. This packaging change does not establish a fix for the native
  timeout or prove OpenHuman model/tool tracing.
- DeepSeek case74 recovered ownership but the native launcher failed the
  initial TeamHarness MCP connection or tool synchronization. A standalone
  probe in the same container initialized the server and listed seven
  tools. DeepSeek `99890d8` emits native readiness after the successful
  Matrix and Task scan, before running inference. This fixes the misleading
  startup timeout event independently of the native MCP failure.
- QwenPaw case73 failed to reach the source step boundary with the original
  60-second local API startup window. QwenPaw `763877d` gives a slow SDK
  initialization 180 seconds, while retaining the native version and
  default-agent readiness checks. The original assignment was retained in
  the same fixture, and the patched process registered a current start and
  advanced its checkpoint. A separate case75 is reserved for the controlled
  normal restart with a valid native session.

### Cases76–79: QwenPaw restart and the next directed lanes

[Case77 QwenPaw restart](evidence/runtime-recovery-1631-case77-verified.json)
verified the same Task and native SDK session after Docker restarted the Worker.
Generation advanced from 1 to 2 and checkpoint from 5 to 12. The independent
ledger recorded each step once, and the persisted report matched exactly one
Matrix reply, with no new external input after shutdown. The source native
read receipt preceded shutdown but followed step one. This case does not prove
strict read-before-step ordering. The images used Core/shared contract
`52a3659b` and QwenPaw `763877d`.

[Partial cases76, 78 and 79](evidence/runtime-recovery-1631-cases76-79-partial.json)
do not increase the directed switch count:

- Hermes → Codex ran all three steps across two processes with no new Matrix
  input, but the native TeamHarness connection dropped and submission never
  happened. Codex [#13](https://github.com/agentteams-group/agentteams-harness-codex/pull/13)
  commit `77e1215` forwards the packaged Python search path to that MCP
  subprocess by name. Its image passed the same MCP checkpoint validation
  request followed by another request on the same connection. Case80 retests
  the actual handoff and final submission.
- Hermes → DeepSeek reached generation 2, and native MCP submission completed,
  but the native shell sandbox could not run the fixture. The independent
  ledger contains only step one. Case81 uses the isolated fixture with an
  explicitly configured shell mode.
- Hermes → OpenClaw stopped the old instance before creating the target.
  Docker marked the target OOM-killed before it claimed the Task.

[Case80 Hermes → Codex CLI handoff](evidence/runtime-recovery-1631-case80-verified.json)
passed with Codex `77e1215`. The read-only verifier checks the same Task through
generation 1 → 2, steps one through three exactly once, actual Codex rollout and
tool output, matching durable artifacts and one final Matrix reply with no
new incoming message after shutdown. The local source and target images
identify different Core contract snapshots in the evidence. This result adds
one directed lane and does not claim the aligned full matrix.

[Case81 Hermes → DeepSeek CLI handoff](evidence/runtime-recovery-1631-case81-verified.json)
passed on Hermes `282c457`, DeepSeek `6f5aadb` and a common Core contract
`52a3659b`. The independent ledger confirms one execution of each step, and
Matrix readback confirms one final reply with no new incoming message after
shutdown. The read-only probe also decoded the actual DeepSeek session file
and found the generation-2 recovery input and native tool results. This local
fixture explicitly selected a shell mode compatible with its Docker VM.
That setting is not a change to the production default.

[Case82 Codex restart](evidence/runtime-recovery-1631-case82-verified.json)
used Codex `77e1215` and verified the original App Server thread with a new
turn, continuing steps two and three after a native restart. The Task ID was
unchanged, the execution generation increased, and each step and the final
Matrix reply appeared once with no new incoming message.

[Case85 DeepSeek restart](evidence/runtime-recovery-1631-case85-verified.json)
passed on DeepSeek `6f5aadb`. The first observation saw a verified step-one
checkpoint before the native cache descriptor was visible. The harness
persisted the descriptor later. The same fixture continued at that boundary:
the restart reused the same native DSH session for a later turn, ran steps
two and three once, submitted the original Task, and sent one matching Matrix
reply without new incoming messages.

[Case86 Codex → Hermes CLI handoff](evidence/runtime-recovery-1631-case86-verified.json)
passed with Codex `77e1215` and Hermes `282c457`. It verifies the original
Task, independent effect counts, old stop before target creation, new
execution generation, and one final Matrix reply without a new Matrix input.
The proof retains the different source/target contract pins.

Case87 DeepSeek → Hermes completed all three steps and delivered the original
Task result. The fixture had timed out after creating the Task and never
queued the source assignment, so Matrix history contained neither the
assignment nor its read receipt. This direction remains unaccepted. The
live driver now checks the actual source Matrix read before requesting a
handoff and records it in the cutover proof.

[Case88 unknown operation](evidence/runtime-recovery-1631-case88-held.json)
interrupted a live Hermes tool after its external effect but before its
checkpoint. Codex claimed the original Task at generation 2 with the operation
still started and entered `needs_verification`. The independent ledger shows
only the original POST and no target verification request. The Controller
records stop before target registration. This is evidence of safe hold only:
automatic verification, continuation, and a final reply did not occur.
The driver's bounded Docker event capture did not observe the late stop and
replacement and is not used as lifecycle evidence for this case.

[Case91 DeepSeek → Hermes CLI handoff](evidence/runtime-recovery-1631-case91-verified.json)
passed with DeepSeek `6f5aadb`, Hermes `282c457`, and Core contract `52a3659b`
on both sides. The original Matrix assignment had a source read receipt
before step one. Docker recorded the source dying and being removed before
the target was created. Hermes built a new native session from shared
checkpoint version 5 and continued the original Task at generation 2. The
final checkpoint reached version 13. The independent ledger records one
effect per step. The verifier read back all three artifacts, the stored
result, and one matching Matrix reply with zero new messages after shutdown.

[Case92 Hermes → OpenClaw startup failure](evidence/runtime-recovery-1631-case92-oom.json)
used the rebuilt OpenClaw image from `957876c` with common contract
`52a3659b`. The source died and was removed before the target was created.
Docker marked the target OOM-killed with exit code 137. The handoff remained
in `creating` and the original Task remained in progress at generation 1,
checkpoint 6, with step one complete. The target never claimed the Task.

[Case93 QwenPaw → Hermes → QwenPaw partial proof](evidence/runtime-recovery-1631-case93-return-partial.json)
continued the original Task in generations 1, 2 and 3, at checkpoints 5,
11 and 16. Three independent effects and one final Matrix report were
observed. The returning QwenPaw used its own earlier Matrix cursor at initial
sync and created a different native session. The old session digest remained
identical. The return Docker events verified source termination and removal
before target creation. The first Docker capture timed out before replacement,
so the complete round trip still needs a replay with both event streams.

[Case94 Codex → DeepSeek CLI handoff](evidence/runtime-recovery-1631-case94-verified.json)
passed on Codex `77e1215`, DeepSeek `6f5aadb` and Controller `52a3659b`.
The Codex and DeepSeek images identify different common contract pins in
the proof. The live cutover followed the source Matrix read and durable
checkpoint 5. DeepSeek continued in a new native session at generation 2 and
completed the original Task at checkpoint 12. The verifier recorded one
effect per step, matching artifacts, one final Matrix reply and no external
Matrix input after source shutdown.

[Case95 DeepSeek source failure](evidence/runtime-recovery-1631-case95-source-failed.json)
received the task assignment, saved step one and its output, and then failed
with `Task execution lost ownership`. The persisted generation-1 checkpoint
contained a started `fixture:…:boundary` operation that the independent
effect oracle did not register. No handoff was started. The fixture now asks
the model to end its turn after the boundary step and scopes final submission
to a turn with all three completed steps. The native tool and verifier still
enforce the independent effect and checkpoint requirements.

[Case96 QwenPaw → Hermes → QwenPaw round trip](evidence/runtime-recovery-1631-case96-round-trip-verified.json)
passed the complete same-Task verifier on common contract `52a3659b` with
QwenPaw `763877d` and Hermes `282c457`. Both source containers died and were
removed before their replacements were created. The Task advanced through
three distinct processes, one independent effect per generation and step.
The final QwenPaw turn used a fresh native session, with the original
session and its digest retained. The first returning Matrix sync matched
the existing QwenPaw device cursor, and only one final report was delivered
without new input.

[Case97 DeepSeek → Codex CLI handoff](evidence/runtime-recovery-1631-case97-verified.json)
passed with DeepSeek `6f5aadb` and Codex `77e1215` after simplifying the
source boundary instruction. The same Task continued in a new Codex App
Server session at generation 2, from checkpoint 5 to 12, with three
independent effects and one final Matrix reply. This run retained different
shared-contract pins. The source boundary was captured about 224 seconds
before Docker recorded the old container exiting. Core `696146ff` now
signals the Worker reconcile after durably writing the operation. Its
test verifies the ordering and idempotent request behavior. The live
latency with the updated Controller still needs measurement.

[Case98 OpenClaw restart partial evidence](evidence/runtime-recovery-1631-case98-openclaw-partial.json)
used OpenClaw `957876c` under amd64 emulation. The requested restart reached
the new process, which exited at native config validation. Docker did not
report OOM for that failure. A separate reproduction with the same record
and fixture environment passed the native validator. The Controller
automatically started the same container again. It recovered the original
Task at generation 2 and completed the remaining two steps, with one final
report and zero incoming messages after shutdown. The original native
session was not reused, so this run does not count as a successful normal
restart/reuse lane. OpenClaw #10 `a5f6b21` uses the common session descriptor
and reports the native validator process outcome. The new candidate still
needs a live test.

[Case99 Codex → QwenPaw CLI handoff](evidence/runtime-recovery-1631-case99-verified.json)
passed on Controller `6421981f`, Codex `ef12936` and QwenPaw `763877d`.
Codex saved step one at checkpoint 5. QwenPaw recovered the same Task at
generation 2 and checkpoint 12, with the real SDK session, one effect per
step and one Matrix report. No new messages arrived after source shutdown.
The new Controller recorded about 3.6 seconds from the source boundary
snapshot to Docker exit and another 4.7 seconds from exit to target
creation. This is one handoff measurement, not a distribution across
Harnesses or a measure of the time to finish inference.

[Case100 OpenClaw ARM64 restart diagnosis](evidence/runtime-recovery-1631-case100-openclaw-partial.json)
reproduced an initial exit from the native config validator. The validator
reported that its shared-state read and cleanup failed. On a copy of the
same Worker workspace, the same runtime config and native CLI failed with the
live state path and succeeded with a separate validation state directory.
OpenClaw [#10](https://github.com/agentteams-group/agentteams-harness-openclaw/pull/10)
`c1ffa90` validates through a temporary state directory and leaves the native
session directory alone.

The Controller automatically retried the same container and the original
Task finished at generation 2 with one final Matrix report. The boundary
snapshot preceded the persisted native session descriptor, and the target
used a different session. The driver now requires the matching OpenClaw
session descriptor and actual native transcript before requesting a normal
restart and checks the final execution used the captured startup.

[Case103 QwenPaw → Codex continuation](evidence/runtime-recovery-1631-case103-verified.json)
passed with Controller `6421981f`, QwenPaw `763877d` and Codex `ef12936`.
QwenPaw completed step one at checkpoint 5. Docker recorded the old instance
exiting and being removed before creating Codex. Without further Matrix input,
Codex took generation 2 and completed the original Task at checkpoint 12. The
independent ledger recorded each of the three step effects once, and Matrix
recorded one final report. The verified report retains the exact shared-library
pins used by each image.

[Case101 OpenClaw restart failure](evidence/runtime-recovery-1631-case101-openclaw-partial.json)
used the ARM64 OpenClaw `c1ffa90` image. The shared descriptor at checkpoint
4 matched the native transcript before shutdown. Native config validation
passed on the first restart, then gateway state discovery refused admission
while the old PID namespace's heartbeat was fresh. A later Controller
retry started the container, and the Worker claimed generation 2 with the
same checkpoint. OpenClaw rejected the attempted inference as a duplicate
of the original Matrix assignment in the reused native session. The Task
remained in progress with only step one complete. The gateway lease wait
and the generation-specific native turn are separate fixes.

[Case105 QwenPaw → DeepSeek](evidence/runtime-recovery-1631-case105-verified.json)
passed on Controller `6421981f`, QwenPaw `763877d` and DeepSeek `6f5aadb`
with shared Core `52a3659b`. The original task advanced from checkpoint 5
to 12 and generation 1 to 2. Docker stopped and removed QwenPaw before
creating DeepSeek. The verifier checked one effect for each step, four
artifact/report hashes, and one Matrix report with no new incoming message
after the old instance stopped.

[Case104 OpenClaw restart partial evidence](evidence/runtime-recovery-1631-case104-openclaw-oom.json)
used OpenClaw `058ec67` on ARM64. The Worker waited 81 seconds for the
previous gateway lease and started the gateway during the captured Docker
restart. Generation 2 inferred without a new Matrix input and the oracle
recorded all three steps once. Docker reported OOM exit 137 while the final
reply was still unconfirmed. The run therefore adds no restart pass.
OpenClaw `c6dcf56` separately prevents a replayed assignment from reusing
the original native dedup key on a later generation. The SDK regression
failed on the old adapter and passed on the rebuilt ARM64 adapter.

[Case107 DeepSeek → QwenPaw](evidence/runtime-recovery-1631-case107-verified.json)
passed with the same Controller and Harness revisions as case 105. The old
DeepSeek instance exited and was removed before creation of QwenPaw. The
target claimed generation 2 and finished the original task with a single
final Matrix report and no intervening external message. The Task oracle
and storage independently verified three unique step effects and four
artifact/report hashes.

[Case106 OpenClaw restart partial evidence](evidence/runtime-recovery-1631-case106-openclaw-oom.json)
used the updated ARM64 adapter `c6dcf56`. The gateway waited 73 seconds for
the old native lease before starting. The acquired generation 2 turn used its
own native input ID, and the oracle recorded one effect per step. The Docker
container had no fixed memory limit and again reported OOM exit 137 before
the final Matrix report was committed. The next run samples Worker peak and
host-available memory with completed test stacks stopped.

[Case109 original Manager replay reproduction](evidence/runtime-recovery-1631-case109-manager-replay.json)
kept the same Manager resource, Matrix identity and room across Codex
`ef12936` → Hermes `282c457` → Codex `ef12936` on Controller `6421981f`.
Hermes replied to the ready-state probe. After returning, Codex inferred
again on exactly that Matrix source event and produced its second reply.
In the same returned boot, Codex also processed the earlier unanswered
event that arrived before Hermes completed its initial sync. The reproduced
failure needs a terminal source-event fact shared across Matrix consumers.
A generic reply relation is insufficient: Codex emitted an intermediate
related event as well as a final reply. The proof includes Matrix relations
and native inference/send trace events, separate from the Worker matrix.

[Case110 OpenClaw normal restart](evidence/runtime-recovery-1631-case110-verified.json)
passed on Controller `6421981f` and ARM64 OpenClaw `c6dcf56` with shared Core
`cdc2800c`. The captured Docker restart reused the same native session ID
and entered generation 2, checkpoint 10, from generation 1, checkpoint 4.
The independent ledger verified exactly one effect per step. The Task
verifier confirmed one final reply and zero new Matrix inputs after stop.
This run stopped the completed disposable tracing stack before testing and
sampled memory; the observed Worker cgroup peak was about 4.35 GiB during
restart. The failure records for 104 and 106 remain available.

[Case111 Hermes → OpenClaw](evidence/runtime-recovery-1631-case111-verified.json)
passed on Controller `6421981f`, Hermes `282c457` and ARM64 OpenClaw
`c6dcf56`. Docker removed the source before creating the target. The
new native OpenClaw session continued the same Task from generation 1,
checkpoint 5 to generation 2, checkpoint 11. The verifier read three
distinct step effects and one final Matrix reply, with no new Matrix input
after cutover. The earlier OpenClaw OOM attempt remains historical evidence.

[Case112 Manager Codex → Hermes → Codex](evidence/runtime-recovery-1631-case112-manager-verified.json)
passed the original round-trip regression on Controller `6421981f`, shared
Core `4fca52d0`, Codex `28dcf22` and Hermes `f5d74a3`. Hermes committed an
explicit terminal Matrix receipt for the reply it delivered. The returning
Codex consumed the older cursor and performed one turn for the earlier
pending input, with zero turns or extra replies for the completed source.
No new input was sent to the returning process. The same Manager identity
and room were retained, with distinct container IDs. The preceding
containers were absent on inspection, while Docker's historical lifecycle
events were unavailable for this probe. Other Manager Harnesses and
completion paths still need independent validation.

[Case112 same-Harness Manager backlog](evidence/runtime-recovery-1631-case112-manager-restart.json)
then stopped the returned Codex container through the controller, sent one
Matrix message while the instance was stopped, and started the same
container and image. The new process handled that pending event once
without further input.

[Case113 OpenClaw → Hermes source failure](evidence/runtime-recovery-1631-case113-openclaw-source-timeout.json)
reached the native OpenClaw prompt but never completed step one before
the 600-second fixture deadline. The task remained assigned and the
independent ledger had no effects, so no cutover was requested. This
records an inconclusive switch attempt with an observed source failure.

[Case114 QwenPaw → OpenClaw](evidence/runtime-recovery-1631-case114-verified.json)
passed with Controller `6421981f`, source QwenPaw `763877d` and ARM64
target OpenClaw `c6dcf56`. The verifier matched the source's actual SDK
session to its shared descriptor. It observed removal of the old instance
before target creation, three unique step effects on the original Task,
one final Matrix event, and no new input after cutover.

[Case120 Manager Codex → QwenPaw → Codex](evidence/runtime-recovery-1631-case120-manager-qwen-verified.json)
passed on Controller `6421981f`, Codex `28dcf22`, and QwenPaw `a76f5c0` with
its Core receipt adapter `b3f89683`. QwenPaw posted and edited the same
thread reply, then durably emitted one terminal receipt for the original
Matrix input. The returned Codex native consumer stayed live, performed no
inference on that source, and sent no extra reply. No new input triggered
the returned Manager. The previous two container identities were absent on
inspection. The evidence retains the source, reply/edit, and receipt
relations.

[Case115 OpenClaw → Hermes](evidence/runtime-recovery-1631-case115-verified.json)
passed on Controller `6421981f`, OpenClaw `c6dcf56` and Hermes `f5d74a3`.
OpenClaw finished step one before the old instance exited. Hermes claimed
the original Task in generation 2 after the old container was removed.
The oracle recorded one effect per step, and Matrix recorded one final
reply with zero new inputs after cutover. The first attempt's source
timeout remains in case 113.

[Case121 Codex → OpenClaw](evidence/runtime-recovery-1631-case121-verified.json)
passed on Controller `6421981f`, Codex `28dcf22` and OpenClaw `c6dcf56`.
After the original Matrix assignment was read and step one was committed,
Docker observed the Codex instance die and be removed before OpenClaw was
created. OpenClaw created a native recovery session for the same Task and
completed steps two and three in generation 2. Checkpoint 5 advanced to 11.
The independent oracle recorded one effect per step. Stored artifact hashes
matched, and Matrix contained one final reply and zero new inputs after cutover.

[Case122 OpenClaw → Codex](evidence/runtime-recovery-1631-case122-verified.json)
passed on Controller `6421981f`, OpenClaw `c6dcf56` and Codex `28dcf22`.
The source read the original Matrix assignment and committed step one at
checkpoint 4. Docker observed it die and be removed before the target was
created. Codex continued the same Task at generation 2 and submitted at
checkpoint 11. Each effect and the final Matrix reply occurred once.

[Case123 DeepSeek → OpenClaw](evidence/runtime-recovery-1631-case123-verified.json)
passed on Controller `6421981f`, DeepSeek `6f5aadb` and OpenClaw `c6dcf56`.
After the old container was removed at checkpoint 5, OpenClaw recovered the
original Task in a new native session and reached checkpoint 11 in generation
2. The verifier matched the three independently counted effects to the
persisted artifacts. Both cases had zero new Matrix inputs after cutover.

[Case118 interrupted effect](evidence/runtime-recovery-1631-case118-held.json)
used Controller `6421981f`, Hermes `f5d74a3` and Codex `28dcf22`. Docker
removed Hermes while the first tool process was still waiting to persist its
artifact and checkpoint. The replacement claimed generation 2 and held
checkpoint 1 in `needs_verification`. The independent ledger still had one
POST when the automatic continuation run timed out. An operator then invoked
only the fixture outcome verification in the same replacement container.
That invocation read the original effect from the independent ledger and
committed its artifact and verification as checkpoint 2. Codex discovered the
verified Task and ran steps two and three without new Matrix input. It
submitted at checkpoint 9 with one effect per step and one final reply.
The automatic verification scenario remains open. [Explicit-verification evidence](evidence/runtime-recovery-1631-case118-operator-verified.json).

The OpenHuman test image now installs its bounded command and policy wrapper
from `tests/fixtures/openhuman-recovery.Dockerfile`. The wrapper selects the
fixture Controller hostname at runtime, so the same local image tag works
across cases. The command accepts the driver's two- or three-digit Task suffix.
The 27 fixture, policy and evidence tests passed locally.

[Case124 OpenClaw → DeepSeek](evidence/runtime-recovery-1631-case124-verified.json)
passed with Controller `6421981f`, OpenClaw `c6dcf56` and DeepSeek `6f5aadb`.
The original Task stayed in progress through replacement. DeepSeek built a
native recovery session and completed steps two and three, moving checkpoint
4 to 11 in generation 2. The independent ledger and object digests matched
all three steps. Matrix recorded one final reply and zero new inputs.

OpenHuman cases 117 and 126 were stopped and excluded from acceptance. The
local test image had retained an older wrapper and executable path. Its
active native executable hash was `06f732658bfe8ce85098e7cbf7d3fa3288bef5b33462b3013cd1521eb988e7fd`.
The [CI artifact](https://github.com/agentteams-group/agentteams-harness-openhuman/actions/runs/44301182954)
for Harness `7788c27` and Core `cdc2800c` has hash
`22cbbf4687231dc0e35acc14110c728e7454ced89c94827f91ab791443100a26`.
The local layer now routes to that executable and checks the hash before
seeding new cases 127 (restart) and 128 (Hermes → OpenHuman). The fixture
Dockerfile prioritizes the executable at the canonical image path over a
retained wrapper target. OpenHuman runtime acceptance remains in progress.

[Case125 OpenClaw → QwenPaw](evidence/runtime-recovery-1631-case125-partial.json)
reached a native submission and final delivery on the original Task with
one accepted effect for each step. The oracle also rejected repeated
registration, verification and POST attempts for step two. The strict
verifier failed on the additional POSTs. This case remains partial and is
excluded from the directed pass count.

[Case128 Hermes → OpenHuman](evidence/runtime-recovery-1631-case128-partial.json)
proved the old container was removed, the exact CI executable started, and
the replacement claimed the original Task at generation 2. It reached no
new step. Native execution timed out after five minutes and the independent
ledger retained only the source effect. In case127 the same timeout occurred
before the first step of the OpenHuman restart fixture. The fixture's short
model requests returned HTTP 200 for both streaming and non-streaming.
The private synthetic request capture in case129 showed a successful real
model response for the Task within five seconds. Its request had zero
structured tool schemas, and the response carried a textual TeamHarness ACK
request. The native Task turn timed out again without an ACK. The precise
point of failure in the tool path still needs diagnosis.

The live driver now supports `RECOVERY_E2E_EXPIRE_HERMES_SESSION=1` for a
normal Hermes restart with an expired shared descriptor. It first proves the
old native session and checkpoint agree, ages only the test descriptor and
restarts through the Controller. The verifier requires a fresh native session
ID and the same Task, effects and result assertions as an ordinary restart.
[Case130 passed](evidence/runtime-recovery-1631-case130-expired-session-verified.json)
on Controller `6421981f` and Hermes `f5d74a3`. The same container stopped and
started with a new process ID. It replaced the expired descriptor's session
with a fresh native ID and advanced the original Task from checkpoint 5 to
12 in generation 2. The oracle recorded each step once, with no new Matrix
input and one final reply. This adds the expired-session fault scenario.
The 27 fixture, policy and evidence tests passed with the driver change.

OpenHuman case131 confirmed the native registry contained 52 tools, with
seven visible to the Task, including the MCP call tool. The model's response
was parsed into a textual tool request. This run timed out before the
corresponding tool execution log and ACK. The installed ARM64 image for
OpenHuman `7788c27` and Core contract `cdc2800c` has now passed startup
configuration smoke. Case133 runs the same recovery scenario on that
native image to check whether the failure is independent of emulation.

[Case132](evidence/runtime-recovery-1631-case132-source-timeout.json) stopped
at the OpenClaw source boundary before switching to QwenPaw. The original
Task remained assigned, and the independent ledger recorded zero effects.
Case125 remains the available partial execution and delivery evidence for
that direction.

OpenHuman case133 reproduced the tool-path timeout on the native ARM64
image. In that same Worker identity, three direct reads through the packaged
Task RPC succeeded. The native registry exposed seven tools, but the
recorded model request contained no tool schemas. The routing provider read
its remote provider's general capabilities, while the reliable and model
route wrappers expose tool support through the dedicated method. OpenHuman
PR #3 commit `8165de5` now forwards that method for remote-only routing and
adds a native regression that constructs the actual gateway provider. This
change preserves the existing local-model routing behavior. Native CI and
full recovery on the fixed image remain pending.

The live driver accepts `RECOVERY_E2E_ABRUPT_RESTART=1` for the same-Harness
restart scenario. It sends SIGKILL at the committed boundary of a labelled
fixture with Docker auto-restart disabled. The verifier requires exit 137,
the original container's later start, the new process identity, and the
same Task, effect and Matrix assertions.
[Case135 passed](evidence/runtime-recovery-1631-case135-abrupt-restart-verified.json)
on Controller `6421981f` and Codex `28dcf22`. The original notification and
step-one checkpoint were durable despite the initial test-driver seed
observation timing out. The driver continued from the existing boundary.
After exit 137, the same container started a new process. Codex reused the
matching native session and submitted the original Task at checkpoint 12
(generation 2). Matrix recorded one final reply and no new input after the
stop. The independent ledger recorded each step once.

[Case137 DeepSeek restart after SIGKILL](evidence/runtime-recovery-1631-case137-abrupt-restart-verified.json)
passed on Controller `6421981f`, DeepSeek `6f5aadb`, and the shared contract
`52a3659b`. Docker recorded exit 137 and a later start of the same container.
The replacement process resumed the original Task in generation 2 using its
matching native session. The checkpoint advanced from 5 to 12, all effects
were recorded once and the single final Matrix reply followed zero new inputs.

[Case138 OpenClaw → QwenPaw](evidence/runtime-recovery-1631-case138-verified.json)
passed on Controller `6421981f`, OpenClaw `c6dcf56` and QwenPaw `a76f5c0`.
The source completed step one and the old container was removed before the
QwenPaw instance was created. QwenPaw finished the original Task in
its native session at generation 2 and checkpoint 11. Its trace records the
original Task, room, new generation, and native session. The verifier observed
one effect per step, zero rejected or repeated effect requests, no new Matrix
input after cutover, and one final reply. Case125's partial result and
case132's source timeout remain in the evidence log.

[Case136 supervised OpenHuman](evidence/runtime-recovery-1631-case136-approval-held.json)
used the checksum-verified executable from OpenHuman `8165de5`. The fixed
provider parsed one native structured tool call. The real Matrix room then
contained a pending `mcp_call_tool` approval for that same turn. The approval
expired, the Task stayed assigned, and the independent ledger had no
execution effects. This run verifies the approval hold and restored tool
request shape. Task continuation remains a separate assertion.

`RECOVERY_E2E_OPENHUMAN_PREAPPROVED=1` selects the existing `AGENTTEAMS_YOLO`
policy for Agent A in the disposable fixture before assignment. It permits
testing headless execution without sending human approval events after
cutover. The local fixture retains its bounded shell command allowlist.
Production approval configuration and the default supervised test path are
unchanged. Cases 140–143 record the explicit fixture setting in their scope.
The driver now captures its own hash at import and records immutable source,
target and Controller image IDs in new reports. All 27 focused fixture and
evidence tests passed with these changes.

[Case139 supervised Hermes → OpenHuman](evidence/runtime-recovery-1631-case139-approval-held.json)
retained the original Task in progress at checkpoint 5 and generation 2
while awaiting tool approval. The independent ledger contained only the
source effect. This confirms the approval gate remained active after the
actual handoff.

[Case140 preapproved OpenHuman restart](evidence/runtime-recovery-1631-case140-steps-only.json)
advanced the original Task from checkpoint 5 to 9 after restarting the same
container. It completed steps two and three without a new Matrix input,
and each of the three artifact digests and independently recorded effects
matched. The installed native configuration confirmed full autonomy and the
bounded fixture command on the restarted instance. The model then spent
its remaining turns probing shared paths through a native file tool that
rejects paths outside the native workspace. It reached the ten-iteration
limit without submitting the Task. The fixture now directs OpenHuman to
submit the already persisted paths through TeamHarness. The full
restart and final delivery remain pending in case143.
