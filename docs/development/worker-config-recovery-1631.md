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
