# Member Runtime Config Contract

This document defines the Worker-owned standard configuration directory and
YAML format shared by Codex, Hermes, QwenPaw, DeepSeek Harness, OpenClaw and
OpenHuman. The selected Harness does not change the directory or field names.
Controller publishes the record; Harness adapters consume it and generate their
native configuration. Dashboard and `agt` use the same Controller write path.

The record carries desired state, Worker identity and team facts. Credentials
normally use environment/file bindings. Existing remote-managed compatibility
fields may carry explicitly projected credentials; consumers must not copy a
native device token into the standard transport policy.

## Storage Path

Canonical object path (relative to the tenant/installation storage root):

```text
agents/{workerIdentity}/runtime/runtime.yaml
```

`workerIdentity` is the stable Controller-resolved Worker storage identity
(`member.runtimeName`), not the Harness name or Project ID. The old proposed
`shared/runtime/members/...` path is not the implemented contract.

Worker-level files remain under the same root:

```text
agents/{workerIdentity}/
  runtime/runtime.yaml       # shared configuration record
  SOUL.md                   # persisted personality
  AGENTS.md                 # instructions and projected role context
  skills/                   # assigned skill assets
  config/                   # projected tool configuration and Worker settings
```

Native session/cache directories may retain Harness-specific formats. Derived
native configuration is rebuilt from the Worker record and assets; it must not
be the only persistent copy of a user setting. Task execution checkpoints and
delivery records remain in the Task system, not in this configuration record.
`metadata.generation` tracks configuration, not process execution ownership.

`shared/lib/worker_runtime_config.py` supplies a version/identity/storage-scope
validator and derives `runtime/compat-config.json` for legacy-shaped native
bridges. With a managed model, materialization does not read old compatibility
input. This JSON is a local credential-bearing cache (mode 0600), not a second
authority or a portable session. Workspace sync excludes both files from
uploads. MinIO member policy denies direct writes/deletes of these paths,
while retaining reads and existing delivery-state persistence. Equivalent
external storage-policy enforcement still needs verification.

## Authority and compatibility

- All six Harnesses must follow this directory and YAML contract. Switching
  changes `member.runtime` and configuration metadata, while retaining Worker
  identity, settings, credential bindings and asset locations.
- Controller is the publisher of resolved desired state and authorization.
  Adapters must not overwrite a newer record with their local native config.
- At startup, resolve credentials through the declared bindings and apply the
  latest record before admitting work. Missing bindings must not select stale
  native credentials. Restart and replacement use the same loading contract.
- Consumers must preserve compatible fields they do not own and must not treat
  missing optional fields as an instruction to erase existing user settings.
- Compatibility readers support older Controllers during migration. They are
  not a substitute for completing standard-record support in every Harness.

Implementation status: Controller projection and all-Harness configuration
tests do not prove every native consumer conforms. See
[Worker configuration recovery acceptance](../development/worker-config-recovery-1631.md)
for current gaps and evidence. Six-Harness startup, switching and Task recovery
acceptance remains incomplete.

## Common startup and consumption boundary (#1631)

The confirmed [implementation baseline and staged plan](worker-recovery-convergence-1631.md)
defines the target storage paths, versioned records, migration and recovery
order. New records in that plan are implementation work, not completed acceptance.

The recovery path is source Harness → Worker contract → target Harness. There
must be no production A-to-B session or configuration converter. Six native
adapters implement one contract; the 30 directed pairs are regression cases.

Keep desired configuration and mutable consumption state separate, under the
same stable Worker identity. Controller reconciliation must not overwrite
consumption state when it republishes `runtime.yaml`.

| Data | Authority and required content |
| --- | --- |
| Configuration and assets | The existing Worker YAML, `config/`, `skills/` and prompt assets: model, MCP, Skills, channel policy and credential bindings. Each startup regenerates native inputs from these sources. A native configuration edit must update its owned Worker setting rather than become a private-only authority. |
| Matrix consumption and handoff | Worker-scoped durable state, independent of a Harness private directory: room/event identity, retained input or retrievable history reference, input-commit evidence, pending handling/reply reference and associated team/Task ID where present. Publishing a sync cursor requires durable retention of its accepted inputs. This common writer/reader and its persistence migration remain to be implemented; merely relocating a native cursor does not implement the contract. |
| Task execution | Existing `teams/<team>/shared/tasks/<taskID>/meta.json.execution` plus Task description and artifacts. Reuse its actual instance/process generation, versioned completed/remaining steps, operation identity/evidence, artifact digests, saved result and reply transaction/event. Do not duplicate authoritative checkpoints in a Worker-local index. Worker message state may reference a Task and observed checkpoint version, but startup must reread Task authority. |
| Native session | Optional Harness-owned cache. Reuse only when it exists, is unexpired and matches the latest Task checkpoint and scope; otherwise reconstruct from the Task and retained room context. Preserve the old cache for return-switch checks. |

A Matrix `next_batch` token describes one consumer's sync position; it is not
proof of task execution. Native SDK/device cursors are transport details and
must not be replaced with another consumer's token to skip pending inputs.
Likewise, `received`, native `read`, Task ACK and a progress note do not mean a
step or reply was completed. Non-Task room inputs also need durable pending
handling/reply state; a native read alone cannot retire them.

Every normal restart, crash replacement and Harness switch uses this order:

1. Load the current Worker YAML and assets and resolve credential bindings.
2. Reconcile durable room inputs/handling references and discover assigned
   recoverable Tasks, even if no new Matrix event arrives.
3. Reread Task ownership and claim execution for the actual new instance and
   process. Configuration generation and notification outbox leases grant no
   execution authority.
4. Verify the latest checkpoint and persisted artifact/result bytes before
   considering a compatible local session. The local session cannot advance
   or replace authoritative state.
5. Deliver a saved result with its existing transaction without inference;
   continue only remaining work at a verified boundary; verify an uncertain
   operation or remain pending verification. Cancellation/reassignment must
   fence stale model/tool/reply callbacks.

The shared layer owns these decisions and durable commits. An adapter only
projects native configuration, supplies native session existence/expiry/history
and input-commit evidence, invokes/cancels native execution, and sends through
the native transport with the shared reply identity. No additional Task
database or cross-Harness native transcript format is required.

Source audit at Core `e0bc358b` found the remaining separation explicitly:
`shared/lib/agentteams_receipts.py` stores a Worker-local
`.agentteams/message-receipts.sqlite` with received/read evidence and its own
sync token; it is not an execution inbox. The QwenPaw Matrix channel still
stores `matrix_sync_state.json`, `matrix_sync_token` and
`matrix_auth_state.json` beneath its `WORKING_DIR`. Some Harness sync paths
persist these files, but there is no verified common cross-Harness consumption
record yet. Passing one pair demonstrates that pair's path, not completion of
this shared mutable-state boundary.

## Existing runtime application behavior

- Controller writes this file whenever the member's non-secret desired state or
  team facts change.
- QwenPaw worker polls this file and applies changed model, AgentSpec package,
  MCP, channel and team context configuration inside the runtime.
- Controller does not write runtime-facing `AGENTS.md`, `SOUL.md`, skills,
  `openclaw.json`, or `mcporter-servers.json` for `runtime=qwenpaw`.
- AgentSpec package version changes update this file and should be applied by
  QwenPaw without restarting the pod.

`desired.agentPackage` is an AgentTeams AgentSpec package. It is not the
TeamHarness plugin package. TeamHarness plugin is runtime infrastructure; an
AgentSpec package is the user-deployed agent template and business capability
bundle.

## Runtime Provider ID Warning

`spec.modelProvider` is the **AI-gateway provider name** (Higress / APIG). It is used
by the controller to resolve the AI route, gateway base URL and consumer
authorization. It is **not** a free-form runtime provider id.

Do not pass a QwenPaw built-in provider id into the runtime projection. QwenPaw uses
the runtime provider id to select its local provider entry. For a built-in provider,
the entry is restored from QwenPaw's built-in catalog and `freeze_url=True` prevents the
gateway base URL from being applied; worker startup then fails its base-URL readback.
The gateway provider name itself is valid and should not be renamed for this reason.

AgentTeams-managed runtimes should use the non-built-in runtime provider id
`agentteams-gateway`. A gateway provider named `deepseek` may still be selected through
`spec.modelProvider`; the runtime provider id must remain separate.

### QwenPaw built-in provider ids (v2.2.2.post1)

Source: `qwenpaw/providers/provider_catalog.py` at QwenPaw `v2.2.2.post1`.

`freeze_url=True` — these must never be used as the runtime provider id:

```text
modelscope
aliyun-codingplan
aliyun-codingplan-intl
aliyun-tokenplan
aliyun-tokenplan-intl
zhipu-cn
zhipu-cn-codingplan
zhipu-intl
zhipu-intl-codingplan
openai
openai-response
kilo
minimax
minimax-cn
kimi-cn
kimi-intl
kimi-codingplan
deepseek
gemini
openrouter
siliconflow-cn
siliconflow-intl
volcengine-cn
volcengine-cn-codingplan
volcengine-cn-agentplan
mimo-tokenplan
mimo
```

`freeze_url=False` — not the hard-freeze set, but still built-in provider ids with
provider-specific behavior; do not use them as the AgentTeams runtime provider id:

```text
dashscope
agentscope-platform
qwenpaw-local
opencode
azure-openai
anthropic
ollama
lmstudio
```

The safe rule for AgentTeams-managed workers is therefore: **any gateway provider name
is allowed, but the runtime provider id is always `agentteams-gateway` (or an explicit
non-built-in runtime id), never a QwenPaw built-in provider id.**

## Field Contract

```yaml
apiVersion: agentteams.io/v1beta1
kind: MemberRuntimeConfig

# Configuration metadata; this generation does not grant Task execution ownership.
metadata:
  generation: 12
  updatedAt: "2026-06-03T12:00:00Z"

# Team facts. This replaces runtime/plugin calls that would otherwise query
# team information through the agt CLI.
team:
  name: demo-team
  storageId: demo-team
  teamRoomId: "!team:matrix.local"
  leaderName: leader # controller derives this from the Team workerMembers entry whose role is team_leader
  leaderRuntimeName: leader
  leaderDmRoomId: "!dm:matrix.local"
  admin:
    name: admin
    matrixUserId: "@admin:matrix.local"

# Current member facts. This tells the runtime who it is and which role/runtime
# adapter should be applied.
member:
  name: worker-a
  runtimeName: worker-a
  role: worker
  runtime: qwenpaw
  matrixUserId: "@worker-a:matrix.local"
  personalRoomId: "!worker-dm:matrix.local"

# Optional during compatibility migration. Controller-resolved transport and
# authorization; device/session state and tokens are excluded from transport.
matrix:
  transport:
    homeserver: https://matrix.local
    encryption: true
    groupPolicy: allowlist
    groupAllowFrom: ["@leader:matrix.local"]
    dm:
      policy: allowlist
      allowFrom: ["@leader:matrix.local"]

# Desired state projected from CRD. Controller writes what should be true;
# QwenPaw runtime applies it to local QwenPaw/TeamHarness configuration.
desired:
  model:
    providerId: agentteams-gateway
    model: qwen-plus
    gatewayUrl: http://aigw-local.agentteams.io:8080

  agentPackage:
    ref: nacos://market.agentteams.io:80/public/dev-worker?version=1.2.0

  mcpServers:
    - name: github
      url: https://aigw.example.com/mcp-servers/github/mcp
      transport: http

  channelPolicy:
    groupAllowExtra: []
    groupDenyExtra: []
    dmAllowExtra: []
    dmDenyExtra: []

  state: Running

  # Resolved Worker override/Team Leader cadence; adapters must not replace
  # an explicit policy with their own implicit LLM wake-up default.
  heartbeat:
    enabled: false
    every: 0m

# Object storage location facts. This section has remote storage coordinates
# only. It does not contain access keys or local runtime paths.
storage:
  provider: oss
  bucket: agentteams-storage
  endpoint: http://minio:9000
  teamPrefix: teams/demo-team
  sharedPrefix: teams/demo-team/shared
  globalSharedPrefix: shared
  memberPrefix: agents/worker-a

# Secret locations. The YAML stores where to read secrets, never the secret
# values themselves.
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN
  gatewayKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY
  storageAccessKeyEnv: AGENTTEAMS_FS_ACCESS_KEY
  storageSecretKeyEnv: AGENTTEAMS_FS_SECRET_KEY
  serviceAccountTokenPath: /var/run/secrets/agentteams/token
```
