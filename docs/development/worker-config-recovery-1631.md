# Worker-owned configuration recovery (#1631)

Status: incomplete. This follow-up separates configuration authority from
native session persistence and execution-recovery acceptance.

Non-session settings belong to the stable Worker identity. A replacement
Harness must reconstruct its native configuration from that Worker record;
copying a previous Harness's private directory is not the configuration
recovery contract. Credentials retain their scoped bindings. Native session
formats stay Harness-specific. Task checkpoints remain team/Task-scoped.

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
