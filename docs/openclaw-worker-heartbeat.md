# OpenClaw Worker autonomous checks

OpenClaw can periodically wake its model to check for pending work. These checks
consume model tokens even when no visible reply is sent. Ordinary Workers now
default to these checks off.

In Dashboard, open the Worker's editor and change **自主巡检**. The switch is
available for OpenClaw Workers when the connected Controller supports it. Only
the Manager or a platform administrator can change this setting.

The same setting is available through the CLI:

```bash
agt update worker --name alice --llm-heartbeat-enabled=false
agt update worker --name alice --llm-heartbeat-enabled=true
```

The Worker API and manifests use `llmHeartbeatEnabled`. Omission in an update
preserves the current value; explicit `false` disables the checks. No separate
environment variable is required. The Controller projects off as
`agents.defaults.heartbeat.every: "0m"`; omission would re-enable OpenClaw's
implicit default. An enabled standalone Worker uses a 30-minute cadence.
Native runtime-config writes and snapshot rollback cannot change this cadence;
use the Worker setting so a second configuration path cannot override it.

For a Team Leader with no Worker override, the existing Team `heartbeatEvery`
policy still applies. An explicit Worker switch overrides it. A heartbeat-only
edit changes runtime configuration without recreating the container; allow the
normal configuration-sync cycle to apply it. An already running turn can finish.

This switch does not disable health reports, Matrix connectivity, task handling
or independent schedules such as Memory Dreaming.

## Creating Workers with fewer model turns

The bundled worker-management skill uses one `agt create worker` call with a
bounded readiness wait. The CLI polls in code; the Manager does not need to
reason about every status read. For an existing Worker, use
`agt worker wait-ready --name alice --timeout 45s -o json`. A timeout does not
delete or recreate the Worker. Model conversation probes are a separate,
explicit verification step.
