# Worker Hot Update Design

Worker 热更新主要覆盖三类运行中变更：

| 热更新对象 | 来源 | CR 字段 | Runtime config 投影 | Worker 行为 |
|---|---|---|---|---|
| Worker 模板 package | AI Registry 中的 AgentSpec package | `spec.package` | `desired.agentPackage` | 拉取并应用新的 md prompt、skills |
| Model | 控制台模型配置 | `spec.model` | `desired.model` | 刷新 runtime model 配置 |
| MCP | 控制台 MCP 配置 | `spec.mcpServers` | `desired.mcpServers` | 刷新 runtime MCP 配置 |

## 目标

Worker 热更新以 Worker CR 的期望状态为唯一控制入口。控制台不直接操作
worker runtime，而是通过更新 Worker CR 触发变更；controller 再将 CR 的期望
状态投影给 worker；worker 在不重启 pod 的情况下完成内部热更新。

本设计中的 package 指 HiClaw AgentSpec package，也就是 Worker 模板包。它不是
TeamHarness plugin package。

## 核心边界

AgentSpec package 只表示 Worker 模板内容，主要包含：

```text
md prompt
skills
其他模板静态资产
```

AgentSpec package 不包含：

```text
model
mcpServers
worker name
runtime lifecycle
TeamHarness plugin package
```

model 和 MCP 是 Worker CR 的独立字段。

## 控制台流程

用户可以在控制台上传或编辑符合 AgentSpec 标准的 Worker 模板，例如
`SOUL.md`、`AGENTS.md`、skills。

编辑完成后，控制台将模板发布到 AI Registry，形成新的 AgentSpec package
版本。

用户点击发布到指定 worker 时，控制台只做覆盖逻辑：用户选择一个
AgentSpec package 版本，控制台更新目标 Worker CR 的 `spec.package`。

## Controller 控制逻辑

对于支持热更新的 runtime，controller 负责将 Worker CR 的期望状态写入
worker 可读的 runtime config，例如：

```text
agents/{runtimeName}/runtime/runtime.yaml
```

runtime config 的字段契约见附录二。

控制逻辑：

```text
spec.package 变化
  -> 写 runtime config desired.agentPackage
  -> 不重启 pod
  -> 不直接写 AGENTS.md / SOUL.md / skills 到 worker OSS 目录

spec.model 变化
  -> 写 runtime config desired.model
  -> 不重启 pod

spec.mcpServers 变化
  -> 写 runtime config desired.mcpServers
  -> 不重启 pod
```

对于旧 runtime，OpenClaw、Hermes、CoPaw 的现有逻辑保持不变。

## Worker 热更新逻辑

worker 监控 runtime config。当 `desired.agentPackage` 的 `ref`、`version` 或
`digest` 变化时：

1. 标记状态为 package updating。
2. 从 AI Registry 拉取新的 AgentSpec package。
3. 校验 digest。
4. 解包到 staging 目录。
5. 应用 md prompt、skills 等模板内容。
6. 刷新 runtime 内部状态。
7. 记录 last applied package。
8. 标记状态为 ready。

如果 package 拉取、校验或应用失败，worker 保留上一个成功版本，不重启 pod，
并暴露 package update failed 诊断。

当 `desired.model` 变化时，worker 只刷新 runtime model 配置。

当 `desired.mcpServers` 变化时，worker 只刷新 runtime MCP 配置。

## 运行中 Worker 更新规则

| 变更类型 | 控制台操作 | CR 变化 | Worker 行为 |
|---|---|---|---|
| 更新模板 md/skill | 发布 AgentSpec 新版本并发布到 worker | 更新 `spec.package` | 拉取并应用新 package |
| 更新模型 | 修改 worker 模型配置 | 更新 `spec.model` | 热刷新 model |
| 更新 MCP | 修改 worker MCP 配置 | 更新 `spec.mcpServers` | 热刷新 MCP |
| 改名 | 暂不支持 | 不允许 | 不处理 |

## 附录一：AgentSpec 标准

现有 HiClaw 对 AgentSpec 的消费逻辑在：

- `hiclaw-controller/internal/executor/nacos_ai_service.go`
- `hiclaw-controller/internal/executor/package.go`

### Package 引用

Worker CR 中当前的 package 字段是字符串 URI：

```yaml
spec:
  package: nacos://registry/public/dev-worker/1.2.0
```

Nacos AgentSpec URI 格式：

```text
nacos://[user:pass@]host:port/{namespace}/{agentspec-name}[/{version}]
nacos://[user:pass@]host:port/{namespace}/{agentspec-name}/label:{labelName}
```

### Nacos AgentSpec 对象

AI Registry 返回的 AgentSpec 核心结构：

```json
{
  "namespaceId": "public",
  "name": "dev-worker",
  "description": "worker template",
  "bizTags": "...",
  "content": "{...}",
  "resource": {
    "resource-id": {
      "name": "AGENTS.md",
      "type": "config",
      "content": "...",
      "metadata": {
        "encoding": "base64"
      }
    }
  }
}
```

### 落盘结构

controller 从 AI Registry 拉取 AgentSpec 后，会将它转成 package 目录：

```text
{outputDir}/{agentspec-name}/
├── manifest.json
├── config/
│   ├── SOUL.md
│   ├── AGENTS.md
│   ├── MEMORY.md
│   └── memory/
├── skills/
│   └── <skill-name>/
│       └── SKILL.md
└── crons/
    └── jobs.json
```

落盘规则：

```text
content -> manifest.json
resource.type + resource.name -> package 文件路径
```

示例：

```json
{
  "type": "config",
  "name": "AGENTS.md"
}
```

落成：

```text
config/AGENTS.md
```

```json
{
  "type": "skills",
  "name": "code-review/SKILL.md"
}
```

落成：

```text
skills/code-review/SKILL.md
```

如果 `resource.name` 已经包含 `type/` 前缀，则不会重复添加前缀。如果
`metadata.encoding` 是 `base64`，controller 会先解码再写文件。

### 与热更新的关系

AgentSpec 标准只定义 Worker 模板 package 的内容和落盘方式。model 和 MCP 不在
AgentSpec 中，必须继续通过 Worker CR 的 `spec.model` 和 `spec.mcpServers`
独立更新。

## 附录二：Member Runtime Config 契约

完整契约见 `docs/member-runtime-config-contract.md`。

Member runtime config 是 controller 写入对象存储、供 managed runtime worker 和
TeamHarness plugin adapter 读取的 YAML 快照。worker 和 adapter 读取该文件获取
team/member facts 与 CR 期望状态，不再通过 `hiclaw` CLI 查询这些信息。

该文件只包含非 secret 的运行态事实和期望状态。secret 值仍来自环境变量、挂载
文件或 service account token。

推荐对象路径：

```text
agents/{runtimeName}/runtime/runtime.yaml
```

核心范围：

- controller 在 member 的非 secret 期望状态或 team facts 变化时写入该文件。
- QwenPaw worker 轮询该文件，并在 runtime 内部应用 model、AgentSpec package、
  MCP、channel 和 team context 配置。
- 对 `runtime=qwenpaw`，controller 不再写 runtime-facing `AGENTS.md`、
  `SOUL.md`、skills、`openclaw.json` 或 `mcporter-servers.json`。
- AgentSpec package 版本变化更新该文件，并由 worker 在不重启 pod 的情况下应用。

字段形态：

```yaml
apiVersion: hiclaw.io/v1beta1
kind: MemberRuntimeConfig

metadata:
  generation: 12
  updatedAt: "2026-06-03T12:00:00Z"

team:
  name: demo-team
  storageId: demo-team
  teamRoomId: "!team:matrix.local"
  leaderName: leader
  leaderRuntimeName: leader
  leaderDmRoomId: "!dm:matrix.local"
  admin:
    name: admin
    matrixUserId: "@admin:matrix.local"

member:
  name: worker-a
  runtimeName: worker-a
  role: worker
  runtime: qwenpaw
  matrixUserId: "@worker-a:matrix.local"
  personalRoomId: "!worker-dm:matrix.local"

desired:
  model:
    providerId: hiclaw-gateway
    model: qwen-plus
    gatewayUrl: http://aigw-local.hiclaw.io:8080

  agentPackage:
    ref: nacos://market.hiclaw.io:80/public/dev-worker?version=1.2.0
    name: dev-worker
    version: 1.2.0
    digest: "sha256:..."

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

storage:
  provider: oss
  bucket: hiclaw-storage
  endpoint: http://minio:9000
  teamPrefix: teams/demo-team
  sharedPrefix: teams/demo-team/shared
  globalSharedPrefix: shared
  memberPrefix: agents/worker-a

credentials:
  matrixTokenEnv: HICLAW_WORKER_MATRIX_TOKEN
  gatewayKeyEnv: HICLAW_WORKER_GATEWAY_KEY
  storageAccessKeyEnv: HICLAW_FS_ACCESS_KEY
  storageSecretKeyEnv: HICLAW_FS_SECRET_KEY
  serviceAccountTokenPath: /var/run/secrets/agentteams/token
```

与热更新相关的字段：

| 字段 | 用途 |
|---|---|
| `metadata.generation` | worker 判断 runtime config 是否变化 |
| `desired.agentPackage` | Worker 模板 package 热更新输入 |
| `desired.model` | runtime model 热更新输入 |
| `desired.mcpServers` | runtime MCP 热更新输入 |
| `team` / `member` | TeamHarness adapter 和 worker 识别当前 team/member facts |
| `storage` | worker 拉取和同步对象存储内容所需的非 secret 坐标 |
| `credentials` | secret 读取位置，只存 env/path，不存 secret 值 |
