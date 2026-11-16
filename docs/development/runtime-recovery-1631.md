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

Candidate companion pins on this Draft branch: Codex `49e88704a920e4a6f03bab610e244c865dacd7f9`
and Hermes `702c54fcbbe6ba100bb3f305e2b14eb4c4a692f9`. Both consume the shared
Core contract at `516f22a7fa758d128141812f3ebff076d1004725`. These are review
candidates, not accepted/released Harness versions.

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
- A restart with a started/unknown operation enters `needs_verification`.
  A new checkpoint must verify that operation before recording more progress.
  Generic task metadata writes cannot forge/drop the execution record; Worker
  writes require the current process fence. Cancellation/reassignment wins.
- Shared `taskflow.checkpoint_task`, authoritative execution merge, and
  shared Python recovery client are present in both TeamHarness copies.
- Codex companion work admits recovered Tasks before Matrix polling, checks
  session checkpoint digests, builds fresh sessions when stale, preserves the
  unrelated journal backlog/cursor, and separates saved-result delivery.
- Hermes uses the SDK's actual session transition API for fresh recovery
  sessions. Changing only the adapter queue key did not isolate SDK history.
  Valid-session reuse remains incomplete.
- Summary-only submissions with an execution record persist a report before
  metadata CAS. A current active native final callback saves its answer before
  invoking submitted-report recovery; pending saved replies stay authoritative.

## Local validation

| Check | Status | Evidence boundary |
| --- | --- | --- |
| Controller server/auth/backend suites | passed | Go tests; no native model |
| Existing Controller cutover race tests | passed | disappearance ordering only |
| Recovery/instance identity regressions | passed | Go tests with fake storage/backend HTTP |
| Shared recovery client | 11 passed | Python unit tests |
| Task metadata commit and transition validation + shared recovery | 34 passed | Python tests |
| Task ownership, conclusions and continuation | 97 passed | separate MCP test process; no real model |
| Controller full unit suite | passed | 0.519 seconds after sleeping-spec fix |
| Controller integration envtest with race | passed | 68.402 seconds; source at runtime-identity change |
| Codex existing suite | 29 passed | SDK/transport unit fixtures |
| Codex recovery admission/session/delivery/reassignment | 9 passed; 38 total | mocked client; no native model |
| Hermes startup recovery | partial | 30 host unit tests passed, 1 skipped; installed SDK fresh session IDs/admission smoke passed with mocked Task client; latest hosted checks tracked on PR #9 |
| Codex packaged image | built; 33 tests passed | `agentteams/recovery-codex:1631-4759ede`, image ID `8d71656edacc34ad82006eb5ea91f46eba1a83855588d073c71a69fa06b567b0` |
| Codex native stop/steer | passed | actual App Server/tools, deterministic Responses fixture; not Task continuation E2E |
| Two real Harness continuation chain | incomplete; no clean passing run | cases 01-03 ran all steps once across Codex/Hermes, but required repairs or failed final delivery; case 04 stopped for verification; case 05 failed Harness preflight |
| Result saved / reply unsent against real Matrix | passed, bounded case 01 | zero model calls, unchanged step ledger, one report body; event `$8KK7IrnddlsgCgKUTyJpzAzQZ8EPAxbDooNOOvwc7Hg` |
| Return to retained old Codex session | passed for delivery-only case 02 | zero model calls, unchanged steps; execution continuation with stale session still open |

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
Real cross-Harness replacement and step execution have been observed; the full
execution-plus-final-delivery acceptance has no clean passing record yet.

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
- Cases 03-05 were cancelled after evidence capture. They are failed runs,
  not accepted continuation evidence. Case 06 is a fresh run of the latest pins.

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
- Finish/validate Hermes native persistence/reuse and interruption handling;
  implement/validate QwenPaw, DeepSeek, OpenClaw and OpenHuman startup resume
  and delivery adapters. Audit same-process retry and revision handling.
- Resolve Manager ordinary-message handoff/receipts from the original #1631
  reproduction. Worker Task fences do not resolve Manager cursor replay alone.
- Verify all six same-Harness and 30 directed cross-Harness combinations,
  including switching back with stale native sessions/cursors retained.

No matrix cell has completed its full required scenario set. Partial real
execution is labelled separately from acceptance:

| From / to | Codex | Hermes | QwenPaw | DeepSeek | OpenClaw | OpenHuman |
| --- | --- | --- | --- | --- | --- | --- |
| Codex | untested | partial; failed full acceptance | untested | untested | untested | untested |
| Hermes | delivery-only return; continuation untested | untested | untested | untested | untested | untested |
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
