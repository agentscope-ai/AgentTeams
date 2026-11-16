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

Candidate companion pins on this Draft branch: Codex `852fe57fe9e7daa5805c9cf14d6bafd66dcb9995`
and Hermes `7bbde8b3972ff66b20cf6e7cb25d582527ade60a`. Both consume the shared
Core contract at `3fe81174f391f7741f5f5471b110616742d1de91`. These are review
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

## Local validation

| Check | Status | Evidence boundary |
| --- | --- | --- |
| Controller server/auth/backend suites | passed | Go tests; no native model |
| Existing Controller cutover race tests | passed | disappearance ordering only |
| Recovery/instance identity regressions | passed | Go tests with fake storage/backend HTTP |
| Shared recovery client | 9 passed | Python unit tests |
| Task metadata commit and transition validation + shared recovery | 32 passed | Python tests |
| Codex existing suite | 29 passed | SDK/transport unit fixtures |
| Codex recovery admission/session/delivery/reassignment | 9 passed; 38 total | mocked client; no native model |
| Hermes startup recovery | initial adapter implemented | 28 host unit tests passed, 1 skipped; installed SDK recovery/admission smoke passed with mocked Task client; `3477332` hosted image/test checks passed; latest head pending |
| Codex packaged image | built; 33 tests passed | `agentteams/recovery-codex:1631-4759ede`, image ID `8d71656edacc34ad82006eb5ea91f46eba1a83855588d073c71a69fa06b567b0` |
| Codex native stop/steer | passed | actual App Server/tools, deterministic Responses fixture; not Task continuation E2E |
| Two real Harness continuation chain | blocked during first admission; no passing record | isolated Matrix/Controller/two Worker boot passed; first Task remained assigned, ledger empty; stale standalone role bypassed recovery and is now fixed in source |
| Result saved / reply unsent against real Matrix | not tested | distinct from execution continuation |

### Isolated live fixture (2026-11-16)

`tests/runtime_recovery_e2e.py` is an opt-in setup/seed driver, not an automated
passing acceptance test. It uses only resources prefixed
`recovery-1631-live-20261116`, its own data volume, and a non-overlapping subnet.
The three-step helper is executed only by the real Harness's native model tool;
the driver does not execute steps on the Worker's behalf. Its persisted ledger
counts executions, and immutable step artifacts are uploaded before checkpoints.
No cross-Harness replacement or step execution has passed yet.

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

Every cell below is **not tested with real Harness execution**:

| From / to | Codex | Hermes | QwenPaw | DeepSeek | OpenClaw | OpenHuman |
| --- | --- | --- | --- | --- | --- | --- |
| Codex | untested | untested | untested | untested | untested | untested |
| Hermes | untested | untested | untested | untested | untested | untested |
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
