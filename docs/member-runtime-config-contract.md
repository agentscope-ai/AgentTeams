# Member Runtime Config Contract

This document defines the YAML config snapshot that the AgentTeams controller writes
to object storage for a managed runtime member. Managed runtime workers and
TeamHarness plugin adapters read this file instead of querying the `agt` CLI for
team and member facts.

The config is for runtime consumption only. It carries non-secret desired state
and team facts. Secrets stay in environment variables, mounted files, or service
account tokens.

## Storage Path

Recommended object path:

```text
shared/runtime/members/{memberName}/runtime.yaml
```

## Scope

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

## Field Contract

```yaml
apiVersion: agentteams.io/v1beta1 # master current: no runtime yaml protocol yet
kind: MemberRuntimeConfig # master current: no runtime yaml protocol yet

# Config snapshot metadata. QwenPaw uses this section to detect whether the
# config changed since the last poll.
metadata:
  generation: 12 # master current: controller/status has CR generation, but it is not written to worker
  updatedAt: "2026-06-03T12:00:00Z" # master current: no corresponding worker injection

# Team facts. This replaces runtime/plugin calls that would otherwise query
# team information through the agt CLI.
team:
  name: demo-team # master current: controller derives this from Team.spec.teamName or Team.metadata.name, but does not inject it to worker
  storageId: demo-team # master current: no independent field
  teamRoomId: "!team:matrix.local" # master current: stored in Team.status.teamRoomID, but not injected to worker
  leaderName: leader # master current: controller derives this from Team.spec.leader.name, but does not inject it to ordinary workers
  leaderRuntimeName: leader # master current: controller can derive this from leader.workerName or leader.name, but does not inject it to worker
  leaderDmRoomId: "!dm:matrix.local" # master current: stored in Team.status.leaderDMRoomID, but not injected to worker
  admin:
    name: admin # master current: controller derives this from Team.spec.admin.name or default admin, but does not inject it to worker
    matrixUserId: "@admin:matrix.local" # master current: controller resolves this for rooms and policy, but does not inject it to worker

# Current member facts. This tells the runtime who it is and which role/runtime
# adapter should be applied.
member:
  name: worker-a # master current: controller management name, not injected as a field
  runtimeName: worker-a # master current: injected through AGENTTEAMS_WORKER_NAME
  role: worker # master current: controller knows the member role internally, but does not inject it to worker
  runtime: qwenpaw # master current: comes from spec.runtime and is passed to backend runtime/image selection, not injected as env
  matrixUserId: "@worker-a:matrix.local" # master current: stored in Worker or Team member status; worker receives token but not user id
  personalRoomId: "!worker-dm:matrix.local" # master current: stored in Worker.status.roomID or Team.status.members[].roomID, but not injected to worker

# Desired state projected from CRD. Controller writes what should be true;
# QwenPaw runtime applies it to local QwenPaw/TeamHarness configuration.
desired:
  model:
    providerId: hiclaw-gateway # master current: implicit in generated openclaw.json
    model: qwen-plus # master current: comes from spec.model and is written to openclaw.json, not directly injected to worker
    gatewayUrl: http://aigw-local.agentteams.io:8080 # master current: injected through AGENTTEAMS_AI_GATEWAY_URL

  agentPackage:
    ref: nacos://market.hiclaw.io:80/public/dev-worker?version=1.2.0 # master current: comes from spec.package; controller resolves and deploys it directly to OSS
    name: dev-worker # master current: not written to worker
    version: 1.2.0 # master current: not written to worker
    digest: "sha256:..." # master current: not written to worker

  mcpServers:
    - name: github # master current: comes from spec.mcpServers and is written to mcporter-servers.json
      url: https://aigw.example.com/mcp-servers/github/mcp # master current: comes from spec.mcpServers and is written to mcporter-servers.json
      transport: http # master current: comes from spec.mcpServers and is written to mcporter-servers.json

  channelPolicy:
    groupAllowExtra: [] # master current: controller merges policies and writes the result to openclaw.json
    groupDenyExtra: [] # master current: controller merges policies and writes the result to openclaw.json
    dmAllowExtra: [] # master current: controller merges policies and writes the result to openclaw.json
    dmDenyExtra: [] # master current: controller merges policies and writes the result to openclaw.json

  state: Running # master current: controller consumes spec.state to create, stop, or sleep the container; it is not injected to worker

# Object storage location facts. This section has remote storage coordinates
# only. It does not contain access keys or local runtime paths.
storage:
  provider: oss # master current: worker infers provider from env and available CLI, no independent field
  bucket: agentteams-storage # master current: injected through AGENTTEAMS_FS_BUCKET
  endpoint: http://minio:9000 # master current: injected through AGENTTEAMS_FS_ENDPOINT
  teamPrefix: teams/demo-team # master current: no standard field
  sharedPrefix: teams/demo-team/shared # master current: no standard field
  globalSharedPrefix: shared # master current: no standard field
  memberPrefix: agents/worker-a # master current: controller implicitly uses agents/{runtimeName}; it is not injected as a field

# Secret locations. The YAML stores where to read secrets, never the secret
# values themselves.
credentials:
  matrixTokenEnv: AGENTTEAMS_WORKER_MATRIX_TOKEN # master current: real token is injected into this env
  gatewayKeyEnv: AGENTTEAMS_WORKER_GATEWAY_KEY # master current: real gateway key is injected into this env
  storageAccessKeyEnv: AGENTTEAMS_FS_ACCESS_KEY # master current: real storage access key is injected into this env
  storageSecretKeyEnv: AGENTTEAMS_FS_SECRET_KEY # master current: real storage secret key is injected into this env
  serviceAccountTokenPath: /var/run/secrets/kubernetes.io/serviceaccount/token # master current: provided by Kubernetes service account mount, not env injection
```
