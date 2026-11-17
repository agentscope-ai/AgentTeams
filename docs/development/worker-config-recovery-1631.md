# Worker-owned configuration recovery (#1631)

Status: incomplete. This follow-up separates configuration authority from
native session persistence and execution-recovery acceptance.

For the 1.2.8 candidate, use the current
[per-Harness boundaries and release gates](../design/harness-recovery-boundaries.md).

Non-session settings belong to the stable Worker identity. A replacement
Harness must reconstruct its native configuration from that Worker record;
copying a previous Harness's private directory is not the configuration
recovery contract. Credentials retain their scoped bindings. Native session
formats stay Harness-specific. Task checkpoints remain team/Task-scoped.

All six Harnesses must use the same Worker directory and YAML field contract:
[Member Runtime Config Contract](../design/member-runtime-config-contract.md).
`agents/<workerIdentity>/runtime/runtime.yaml` is canonical; a Harness switch
must not create a second authority under the target's private directory.

Dashboard and `agt` must submit changes to the same Controller orchestration
and read back the same operation state. They must not implement separate
container replacement or Matrix replay flows.

## Phase A: shared Matrix input persistence

The confirmed [Worker contract and implementation plan](../design/worker-recovery-convergence-1631.md)
now has a first shared storage/API implementation. `WorkerMatrixStore` writes
accepted inputs beneath the authenticated Worker's stable prefix before a
strict conditional commit advances its pending index and native cursor.
The cursor and startup fence share one CAS object. Replacing that start
rejects late old commits; event redelivery retains newer execution/native
facts, and `from_token` rejects a native cursor based on older private state.
Incremental sync retains outstanding timeline gaps. This is not native
session migration or Task execution acceptance.

The authenticated `/api/v1/runtime/matrix` read/start/ingest API derives its
Worker storage identity and actual backend runtime on the server, uses the
uncached reader when supplied, and supports standalone Workers. The existing
Task/Project authorization gates remain in place. Native callers use
`shared/lib/worker_matrix_state.py` or the `matrix.*` JSON/stdin actions in
`task_recovery_rpc.py`. Native callbacks may persist an input without inventing
a batch cursor. MinIO policies reject direct writes to common control objects;
the result/artifact upload paths remain separate.

Local validation for this change:

- Five Go packages passed: service, server, oss, backend and controller.
- Fifteen new Matrix store/API tests passed with the race detector, including
  concurrent batches, replacement during an in-flight commit, failed upload,
  strict-CAS refusal, old-cursor return, preserved gaps, UTF-8 identities and
  the authenticated HTTP routes with Project listing still denied.
- Shared Matrix/Task client/RPC suite:29 passed plus6 subtests.
- Build-workflow contracts:14 passed. CI/release-tool contracts:23 passed
  in an offline Linux container using Bash5.2 and the installed Python venv.
  The first host attempt had2 missing-submodule errors and1 Bash3.2 failure;
  pinned submodules were initialized and the suite rerun in its Linux shell
  environment. Product behavior and assertions were not changed for that.
- Rename contract and whitespace checks passed.

The next Phase A change adds ordinary-message execution at
`GET/POST /api/v1/runtime/matrix/events/{key}/execution`. Claims bind the actual
registered start; conditional checkpoints reuse the Task checkpoint validator
with a Worker-message artifact namespace. Task-linked inputs are rejected by
this endpoint: their execution remains solely in the Task system. Saved result
bytes are verified on claim, unknown operations return verification-only mode,
and cancelled/delivered work cannot enter inference again. Pending saved
results permit delivery acknowledgement or cancellation, not additional work.
Terminal execution remains authoritative even if pending-index cleanup needs
a retry. Late process/generation callbacks and an in-flight write racing with
cancellation are rejected.

The common Python/native facade now supplies message ownership checks, shared
checkpoint digests and fresh-session recovery context. Result upload precedes
checkpoint publication, and a late native answer cannot replace an existing
saved result. Accepted batch order is retained in the pending index; redelivery
does not reorder messages by their opaque event hashes. This ordering
regression failed before removing the hash sort and passed afterward.

Local validation for the message change: the same five Go packages passed;
targeted race tests include the Task checkpoint regressions and23 Matrix
store/API cases; common Matrix/Task client/RPC41 passed plus6 subtests;
build-workflow14, rename and whitespace checks passed. Controller CI for the
previous642bba4f head passed in run44115181152; its integration run44115191717
was still running at the last refresh. Those runs do not validate later code.

The next shared increment adds `GET/PUT /api/v1/runtime/sessions`. The query
selects an assigned Task by explicit team/task identity or an ordinary message
by event key. The Controller derives the Worker/runtime storage namespace and
stores versioned `WorkerSessionCache` descriptors under
`agents/W/runtime/sessions/<harness>/<work-key>.json`. Descriptors bind native
reference/digest/expiry to the authoritative checkpoint version/digest and the
Controller's current `runtime.yaml` generation. Conditional writes require the
registered actual start and current execution owner. Task/message authority is
only read, never copied into or rewritten by the cache.

`shared/lib/worker_sessions.py` and `session.*` native RPC actions require the
adapter to verify actual native content before reuse. A compatible descriptor
alone is insufficient. Same-Harness restart can retain an unchanged native
cache across execution generations. Checkpoint/config changes, expiry and
cross-Harness lookup require rebuilding. Returning to an old Harness preserves
its old descriptor but rejects it when behind shared authority. Unknown effects,
pending result delivery, cancellation and reassignment cannot enter normal
session continuation through this endpoint. Native persistence callbacks keep
their original checkpoint snapshot; they cannot refresh and relabel stale bytes
with a newer checkpoint. UTF-8 identity keys and the common Python-compatible
checkpoint digest remain separate conventions.

Local validation for this increment: five Go packages passed; six session
tests cover the descriptor/authority boundary, stale writers, assignment and
cancellation, restart/old-Harness return, missing config/invalid records and
Python-compatible digest values. The common Python suite passed49 tests plus6
subtests; build-workflow contracts passed14. These checks do not prove native
serialization or real continuation.

The following increment adds `GET/POST events/{key}/native` and
`matrix.native.*` facade/RPC actions. A final native-input reference/digest is
conditionally recorded with the registered actual startup identity. A source
receipt cannot prove target persistence, including when replacement races with
the write. The receipt does not change execution/checkpoints, retire Task work,
or imply tool execution/completion. Adapters must verify the actual persisted
input row content; an earlier read ACK or a session ID alone is insufficient.

Explicit `matrix.backfill` persists backwards `/messages` pages before a CAS
advances a limited timeline's gap. The Controller records the old coverage
event, current pagination token and pending insertion anchor. Pages are
reversed from server backwards order into their queue position, without sorting
by timestamps/hashes or replaying history older than the old coverage anchor.
Empty pages with continuation tokens remain incomplete. A missing end token
before a known old anchor retains a boundary requiring verification, rather
than erasing the hole. Legacy gaps without recorded bounds are preserved;
initial/legacy import is still required, not guessed from read receipts.
An overlapping timeline can itself prove connection to its old anchor.
Ordinary new inference is blocked while room gaps remain, but an already saved
result can still be delivered. Native hooks and actual Matrix pagination remain
to be exercised against the new common interface.

No new real Harness lane is accepted. H/Q adapter hooks, initial/legacy
consumption import, common startup scheduling and durable lifecycle
handoff remain implementation work. Native snapshot evidence
must not be inferred from these object-store/HTTP/client tests. Historical
normal-restart and directed-lane counts remain3/6 and7/30 at their recorded SHAs.

## Verified boundary

`TestWorkerConfigurationSurvivesHarnessChanges` exercises all six Harnesses
against the real Controller configuration projector with in-memory storage.
Its 36 subtests cover six same-Harness recreations and 30 directed changes.
Each uses a fresh Deployer and checks the stable Worker/Matrix identity,
model, inline prompts, skills, MCP servers, channel policy, credential
bindings and storage scope. The target Harness/generation changes while
the configuration remains at `agents/worker-a/runtime/runtime.yaml`.

This is configuration-projection unit coverage. It does not start a native
Harness, prove a native configuration was loaded, stop a tool process, consume
a Matrix event or resume a Task. It must not be reported as a passing
six-Harness/30-direction execution matrix.

## Worker transport snapshot

The Controller now publishes `matrix.transport` in the Worker record from its
already-resolved Worker-level Matrix projection. This retains the endpoint,
DM/group allowlists, encryption and message-history settings without
reimplementing role/Human policy in each Harness. Only the explicit transport
fields are copied; access tokens, user/device IDs and other native state are
not copied into this snapshot. Identity and credential bindings remain in the
existing standard fields.

[Hermes #11](https://github.com/agentteams-group/agentteams-harness-hermes/pull/11)
can load this standard snapshot without reading `openclaw.json` when a managed
model is declared, materialize
its Matrix identity/token from Worker fields/bindings, and feed the existing
native bridge. Missing bindings fail before falling back to an old token.
Controllers without the optional snapshot retain the compatibility path.
Models not declared in the standard record retain their compatibility model;
this does not yet standardize every native-owned configuration setting.
Controller reconciliation now also publishes the standard record after
generating compatibility assets for standalone Hermes, OpenClaw and OpenHuman;
QwenPaw, Codex and DSH keep their existing standard-record branch. Team records
are published after channel-policy injection, including policy reset on detach.
Actual Deployer + in-memory storage regressions cover standalone create/update,
Team create/update and detach; the Hermes cases failed before these call-path
fixes. These tests do not execute native Harnesses.

Hermes #11 additionally has a passing real-MinIO/mc loader-to-native-SDK check
in two independent containers, with no compatibility object. The second
container has no native directory and restores previously persisted personality
and shared artifacts. Records are seeded by the test, with synthetic root S3
credentials; no running Controller, production Worker IAM, Matrix/model
connection or Task execution is covered. Full startup and continuation remain
unverified for the new path.

## Remaining implementation and acceptance

- Audit standard fields and configuration write paths across all six Harnesses.
  Hermes still preserves native-only settings outside the bridge-owned keys;
  OpenClaw and OpenHuman retain the legacy `openclaw.json` projection/bridge.
  Existing runtime-specific configuration must not be the only saved authority.
- Implement missing Worker-level settings and target projections. Persist
  successful user configuration changes in the Worker record and prevent an
  old native configuration from overwriting newer authoritative settings.
- Rebuild each target with an empty native configuration directory. Verify
  effective settings and scoped credentials, including a subsequent restart.
- Complete the shared durable handoff, process ownership and automatic startup
  admission contract. Verify old process/tool termination before replacement.
- Verify message cursors, receipts, pending replies and Task checkpoints
  separately from native session caches. With no new Matrix input, continue
  pending work without repeated effects or duplicate replies.
- Exercise Dashboard and CLI against the same backend flow, including failures
  and recovery. The existing CLI `update/apply --runtime` flag proves only
  configuration submission; Dashboard switching remains disabled.

Track full recovery acceptance and exact-source evidence in
[the existing recovery record](runtime-recovery-1631.md), Core issue #1631 and
[Dashboard #260](https://github.com/agentteams-group/agentteams-dashboard/issues/260).

## Common observability settings

The Controller publishes collection-only `desired.observability` in the same
Worker record for all six Harnesses: revision, enabled, captureContent and
collectorEndpoint. `credentials.traceHeadersEnv` binds ingestion authentication
to `OTEL_EXPORTER_OTLP_HEADERS`; no ingestion or backend query secret is written
into that desired-state section. Disabled records clear collection endpoint
and content capture. Worker, Team and detach projections read the same persisted
configuration through the API reader, rather than dropping it on roster updates.

OpenClaw now receives collection environment bindings alongside QwenPaw/Hermes.
Its adapter enables the installed SDK's diagnostics-otel exporter from this
record. Other Harnesses accepting the record is not proof they export traces.
Core package tests cover common projection and enable/disable; installed native
schema and real OTLP-exporter boundary tests are separate from real model,
Controller/Langfuse readback and Dashboard UI acceptance, which remain open.
