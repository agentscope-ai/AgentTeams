# 成员运行时配置契约

本文档定义 AgentTeams 控制器为受管运行时成员写入对象存储的 YAML 配置快照。受管运行时 worker 与 TeamHarness 插件适配器读取此文件，而不是查询 `agt` CLI 以获取团队与成员事实。

该配置仅供运行时消费。它承载非密期望状态与团队事实。密钥保留在环境变量、挂载文件或服务账户令牌中。

## 存储路径

推荐对象路径：

```text
shared/runtime/members/{memberName}/runtime.yaml
```

## 范围

- 控制器在成员的非密期望状态或团队事实变更时写入此文件。
- QwenPaw worker 轮询此文件，并在运行时内应用变更后的 model、AgentSpec 包、MCP、通道与团队上下文配置。
- 对 `runtime=qwenpaw`，控制器不写面向运行时的 `AGENTS.md`、`SOUL.md`、技能、`openclaw.json` 或 `mcporter-servers.json`。
- AgentSpec 包版本变更会更新此文件，且应由 QwenPaw 在不重启 pod 的前提下应用。

`desired.agentPackage` 是 AgentTeams AgentSpec 包，不是 TeamHarness 插件包。TeamHarness 插件是运行时基础设施；AgentSpec 包是用户部署的 agent 模板与业务能力包。

## 字段契约

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
  leaderName: leader # controller derives this from the Team workerMembers entry whose role is team_leader
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
    providerId: agentteams-gateway # master current: implicit in generated openclaw.json
    model: qwen-plus # master current: comes from spec.model and is written to openclaw.json, not directly injected to worker
    gatewayUrl: http://aigw-local.agentteams.io:8080 # master current: injected through AGENTTEAMS_AI_GATEWAY_URL

  agentPackage:
    ref: nacos://market.agentteams.io:80/public/dev-worker?version=1.2.0 # master current: comes from spec.package; controller resolves and deploys it directly to OSS
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
