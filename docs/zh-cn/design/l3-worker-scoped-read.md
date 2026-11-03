# L3 Worker 范围读访问

状态：已实现
范围：`internal/auth`（身份）、`internal/server`（worker / channels / approval 读路由）

## 问题

`Human` CR 带 `permissionLevel: 3`（worker 范围，`accessibleWorkers`）根本无法对控制器 API 认证：`resolveHuman` 硬拒一切非 2 的等级，且即便 `accessibleWorkers` 字段在身份链上也没有消费者——该字段可变且受校验，但不承载任何权限。L2 权限模型（#1220 §2）的 Q2 已定案预期契约：**L3 人类是其所分配 worker 的只读观察者**（通道配置与工具审批配置的读取），无写入。

## 设计

L2/L3 之分由**数据承载，而非新角色值**：`RoleHuman` 不变，L3 身份就是一个 `AccessibleWorkers` 集合非空的 `RoleHuman` 调用方。所有既有团队范围检查都键控于 `RoleTeamLeader`/`RoleHuman` 加 `TeamMatches`，因此对 admin、manager、leader、L2 人类与 worker SA 均无任何变化。

### 身份（`internal/auth`）

- `CallerIdentity` 新增 `AccessibleWorkers []string`——**仅**对 permissionLevel=3 的人类填充（取自 `spec.accessibleWorkers`）。L2 人类保留 `Teams` + `Capabilities`，且恒携带空 `AccessibleWorkers`；基于 SA 的身份永不自带（与 `Capabilities` 同一不变量）。`permissionLevel` 即判别器——一个同时列出 `accessibleTeams` 或 `capabilities` 的 L3 CR，其身份中两者皆无（等级严格隔离，不静默扩围）。
- `resolveHuman` 现解析两个受支持等级：
  - `2` → `RoleHuman` + `Teams` + `Capabilities`（不变）
  - `3` → `RoleHuman` + `AccessibleWorkers`（新）
  - 其他任何等级 → 拒绝（level 1 使用 admin SA）
- 新读谓词 `WorkerReadable(team, workerName)`：worker 腿（`workerName ∈ AccessibleWorkers`，独立与团队成员同等）并既有团队范围。对每个不携带 `AccessibleWorkers` 的调用方，它精确退化为 `TeamMatches`——无操作。

### 读路由（W8：404，永不 403）

下列四个面上的范围化读检查现在用 `WorkerReadable` 取代 `TeamMatches`：

| 路由 | 范围 |
|---|---|
| `GET /api/v1/workers/{name}` | 已分配 worker（200）；其余 404 |
| `GET /api/v1/workers`（列表） | 过滤为已分配 worker |
| `GET /api/v1/workers/{name}/channels...` | 已分配 worker（200）；其余 404 |
| `GET /api/v1/workers/{name}/approval` | 已分配 worker（200）；其余 404 |

范围外 worker 隐藏为 404（存在性不可被探测），与 L2 团队范围行为一致。

### 写路由（只读是结构性的）

不触碰任何写路由。L3 的只读由两个独立层保证：

1. **授权器/中间件**——`worker` 上的 `ActionUpdate` 走 `requireSameTeam`，而 L3 身份不携带任何团队，因此一切 `PUT /api/v1/workers/{name}` 与 `PUT .../channels/...` 在到达处理器之前即被以 403 拒绝。`create`/`delete`/`wake`/`sleep`/凭据刷新对人类默认拒绝。
2. **处理器兜底**——channels 与 approval 的范围检查（`channelsScope` / `approvalScope`）被 GET 与 PUT 共用，因此并集谓词**仅对 `GET` 应用**；变更保持严格的 `TeamMatches` 谓词，L3 身份（无团队）无法通过。因此即便中间件层将来变化，处理器仍会把 L3 变更隐藏为 404。（`PUT /api/v1/workers/{name}` 处理器如前保持其自有 `TeamMatches` 检查——效果相同。）

## 契约

| 调用方 | `GET` worker | `GET` channels / approval | `PUT` worker / channels / approval |
|---|---|---|---|
| admin / manager（L1） | 任意 worker | 任意 worker | 任意 worker（不变） |
| L2 人类（`permissionLevel: 2`） | 自身 accessibleTeams（不变） | 自身 accessibleTeams（不变） | 既有范围化写策略（不变） |
| **L3 人类（`permissionLevel: 3`）** | **已分配 worker（团队 + 独立）；其余 404——MCP 端点 URL 已清洗（userinfo + 凭据查询值）** | **已分配 worker；其余 404——通道配置读取已脱敏（凭据剥除）** | **拒绝——403（worker/channels，中间件）或 404（approval，处理器）；无上游变更** |
| 团队 leader / worker SA | 不变 | 不变 | 不变 |

L2（level 2）CR 上的 `accessibleWorkers` 是惰性的——worker 腿仅在 level 3 激活。

### 读脱敏（凭据永不达 L3）

维护者决定（#1277 评审）：L3 读者可读取其已分配 worker 的**常规配置/状态**，但**不可见明文凭据**。L3 可读且承载凭据 VALUE 的响应面有两个（逐面审计：approval GET 返回单一 `approval_level` 字符串；checkpoints/workspace-files 对 L3 隐藏为 404；runtime-status 端点对人类经授权器拒绝；通道健康即 `channel/status/detail`）：

1. 通道配置读对（`GET .../channels`、`GET .../channels/{name}`）——由 `channelCredentialKeys` 拒绝名单（下文）剥除。
2. `WorkerResponse` 的 `mcpServers` URL（worker 详情 `GET /workers/{name}` + worker 列表 `GET /workers`）——结构体是 name/url/transport，但 URL VALUE 可能内嵌凭据：查询中的 API key（`?api_key=...`、`?apiKey=...`、`?key=...`——任意厂商命名）或 userinfo 组件中的 user:password 对（`https://user:pass@host`）。`sanitizeMCPURLForL3` **归约**每个 URL 为 `scheme://host[:port]/path`：userinfo 组件、**整串查询串**与任何 fragment 一并整体丢弃。MCP 端点是任意外部 URL，其查询命名空间是未分类输入——对已知凭据字段名的拒绝名单（如通道配置拒绝名单）无法构成该命名空间的完整凭据契约（评审 0918 第 4 轮：`apiKey`/`key` 存活于逐键过滤），因此任何查询值——无论是否已分类——均不暴露给 L3。这与 MCP 目录面（`redactMCPURL`：仅 host/path）是同一密钥契约；worker 响应额外保留 scheme 作为传输安全信号。不含上述任何组件的 URL 逐字节原样返回；无法解析、非绝对或无 host 的 URL 失败关闭为空串（不可证明的 URL 不予服务）。已知残留：编码*在路径中*的凭据（按惯例罕见）会存活——归约到 `scheme://host[:port]` 是一行即可的收紧。该清洗仅对 `IsWorkerScoped()` 调用方生效；L1/L2/SA 响应携带 URL 逐字原样（团队控制器合法地管理它们）。

因此两条通道配置读路由对 worker 范围调用方均在**服务端**剥除承载凭据的字段：`channelCredentialKeys` 拒绝名单（qwenpaw 2.2.x 通道模型的密钥字段，大小写不敏感的叶名，任意嵌套深度）在响应写出前被移除。字段是**省略**，不是以哨兵值替换（存在性可见，值永不可见）；常规字段保留；`types`/`schemas` 原样透传（其文档以 schema 属性键的形式携带凭据字段名——按键名剥除会破坏表单渲染）。剥除在服务端是有意设计：仅前端遮罩不构成边界，因为原始响应即契约。对 L3 读者，200 响应体若非合法 JSON 则失败关闭为 `{}`（不可解析的上游响应无法证明无凭据）。L1/L2 响应从不经过剥除（往返读契约，#1220 §13 Q5）。

## 范围之外

- 任何形式的 L3 写（Q2：只读；未来的写授权是新设计，不是翻个开关）。
- L3 对其他读面的访问（运行时配置、工作区文件、checkpoints、技能、项目、团队、MCP 目录、审计事件）——它们保持团队范围，故 L3 人类在它们上面什么也看不到（其团队集合为空）。扩展其中任何一个是后续工作。
- 房间权力等级 / Matrix 名册（房间管理面，按 humans-update 契约属独立关切）。

## 测试

- `internal/auth/matrix_authenticator_test.go` — L3 解析（`ResolvesL3Human`）、双向等级严格隔离（`L3StrictPerLevel`、`L2IgnoresAccessibleWorkers`）、不支持等级与未知用户的拒绝。
- `internal/auth/authenticator_test.go` — `WorkerReadable` 的 L3 腿与非 L3 调用方的无操作契约。
- `internal/auth/authorizer_test.go` — `HumanL3WriteDenied`：L3 读通过授权器（由处理器过滤），一切 L3 worker 写被拒（update 经统一无团队拒绝）。
- `internal/server/resource_handler_test.go` — `GetWorker_L3Scoped`、`ListWorkers_L3Scoped`、`UpdateWorker_L3Denied`（处理器级探测：更新一个*已分配* worker 仍 404）、`GetWorker_L3MCPCredentialsSanitized`（原始 L3 详情响应中无查询 api_key 与 userinfo 哨兵；host/path/非凭据查询值保留）、`ListWorkers_L3MCPCredentialsSanitized`（同一契约于列表面）、`GetWorker_L2MCPCredentialsVerbatim`（清洗不过度应用：L2 逐字读取 URL）。
- `internal/server/worker_channels_test.go` — `ChannelsL3AssignedReadAllowed`（团队 + 独立）、`ChannelsL3UnassignedHidden`（404，无外呼）、`ChannelsL3MutationDenied`（处理器级探测：PUT 一个*已分配* worker 仍 404，无外呼）、`ChannelsL3ReadsSanitizeCredentials`（两条读路由，qq/matrix/feishu/voice/dingtalk 上非空哨兵凭据：原始 L3 响应均不含，常规字段保留，凭据键缺失；L2 + admin 逐字往返；schemas 不动）、`ChannelsL3UnparseableUpstreamFailsClosed`（非 JSON 的 200 体 → 对 L3 读者 `{}`）。
- `internal/server/worker_approval_test.go` — `ApprovalGet_L3AssignedAllowed`、`ApprovalGet_L3UnassignedHidden`、`ApprovalPut_L3Denied`（无上游 PUT）。
