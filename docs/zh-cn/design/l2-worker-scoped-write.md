# L2 人类 Worker 范围写

状态：已实现
API：`PUT /api/v1/workers/{name}`（既有端点，新增调用方类别）

## 问题

L2 人类（`Human` CR 且 `permissionLevel: 2`，以其 Matrix 令牌认证）可以读取其 `accessibleTeams` 范围内的团队与 Worker，并且（自项目写端点落地以来）可以在该范围内创建并驱动项目。然而，Worker 配置此前仅限 admin/leader：`authorizeHuman` 拒绝除 `get`/`list` 之外的一切 worker 动作。

因此，拥有一支专属团队的团队范围人类，无法调整其所协调 Worker 的能力——启用内置技能、来自源注册表的远程技能，或一个 MCP 服务器——而不升级到 admin。每一次此类变更，都要经人工操作者与一次携 admin 令牌的 `PUT /api/v1/workers/{name}` 调用往返。

## 设计

为 `RoleHuman` 调用方，在既有 `PUT /api/v1/workers/{name}` 端点上扩展代码级边界。项目写端点采用同一模式：中间件无法解析 `worker -> team`，因此授权器中的 `requireSameTeam` 对范围化请求是直通（pass-through），真正的边界由处理器在解析出 worker 所属团队之后强制。

两条规则，由 `ResourceHandler.checkHumanWorkerUpdate` 强制：

1. **团队范围。** Worker 必须是调用方 `accessibleTeams` 中某个团队的成员（经与列表端点相同的团队成员查找解析）。独立 Worker（无团队归属）在 `GET /api/v1/workers` 中对 L2 读者隐藏；更新路径同样将其隐藏并返回 `404`，使端点保持抗探测。跨团队更新基于同一原因返回 `404`——`403` 会令范围化人类枚举其不可见的 worker 并学到其所属团队。只有无团队人类（完全没有 `accessibleTeams`）在中间件层即被以 `403` 拒绝，早于任何 worker 查找。
2. **字段白名单。** 默认 L2 更新只可设置 `skills`（公共目录指派）。`remoteSkills`（注册表源 URI 可能内嵌凭据）与 `mcpServers`（网关 bearer key 被原样注入每个条目——L2 可控的 URL 会将其外泄）在提升能力设计落地前对默认 L2 关闭；请求体中出现的任何其他字段（`model`、`modelProvider`、`runtime`、`image`、`identity`、`soul`、`agents`、`package`、`expose`、`channelPolicy`、`resources`、`containerManaged`、`state`）均以 `400` 拒绝并点名违规字段。所有权、人格、镜像、网络与生命周期仍属团队所有者的域。全请求类型探测测试（`TestL2WorkerUpdateFieldPolicyCoversAllRequestFields`）将策略钉死：`UpdateWorkerRequest` 的每个字段必须被显式裁定，使任何字段不会因遗漏而变为 L2 可写（默认拒绝）。

允许字段的语义不变：merge-patch、非空（或非 nil）者胜、冲突重试循环，与一切更新相同。请求类型新增 `remoteSkills`（此前即便对 admin 也无法经 API 读取，尽管 CRD 与部署器已支持）；admin 可设置，默认 L2 不可（见上节白名单）。

## 契约

| 调用方 | `PUT /api/v1/workers/{name}` |
|--------|------------------------------|
| admin / manager | 全量更新，不变 |
| 团队 leader | 全部 worker、全部字段，不变（当前代码中 leader 路径非团队范围化） |
| L2 人类（默认） | 仅本团队 worker；仅 `skills`（公共目录指派）；`remoteSkills` / `mcpServers` 返回 400（提升能力待设计）；独立与跨团队返回 `404`（抗探测），非白名单字段 `400` |
| worker / 其他 | 由授权器拒绝（不变） |

授权器的变更刻意最小：`RoleHuman` 对 `worker` 的 `ActionUpdate` 现返回 `requireSameTeam`（`ResourceTeam` 为空时直通），而非 `deny`。处理器是单一强制点——与项目端点相同的分层——使范围与白名单无法被任何以 L2 人类身份认证的调用方绕过。

## 范围之外

- worker 的 `DELETE`/`POST`（团队归属变更仍属 admin/leader）。
- L2 人类的唤醒/休眠生命周期（另行决策）。
- 团队化 leader 更新路径（当前代码允许团队 leader 更新任意 worker；属既有行为，不在此范围）。
- **L2** 写经 `accessibleWorkers` 访问独立 Worker（L2 人类保持团队范围；携带 `accessibleWorkers` 的 L2 CR 无效——该字段仅在 `permissionLevel: 3` 生效，且只授予读取；见 [l3-worker-scoped-read.md](l3-worker-scoped-read.md)）。
- 将更新传播到运行中的 worker 容器——既有调和机制已应用 `spec` 变更。

## 测试

- `internal/auth/authorizer_test.go` — `TestAuthorizer_HumanScoped`：范围内与空团队的 `worker` `ActionUpdate` 允许，跨团队拒绝，create/delete/wake/sleep 仍拒绝。
- `internal/server/resource_handler_l2_update_test.go` — 处理器边界：范围内 skills 更新生效（200），携凭据面（`remoteSkills` / `mcpServers`）对默认 L2 返回 400，跨团队 404，独立 404，非白名单字段 400（点名），空体无操作 200，admin 全量更新不变，团队 leader 更新不变，无团队人类被隐藏（404；中间件先行拒绝），全请求类型字段策略探测（逐字段探测，仅 `skills` 可通过）。
