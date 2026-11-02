# Capability 地基

状态：已实现
API：`PUT /api/v1/humans/{name}`（新字段） · CRD：`Human.spec.capabilities`

## 问题

L2 权限模型（#1220）需要一种方式来授予超出角色基线的具名敏感面权限：写通道凭据、管理 remoteSkills 注册表、将 worker 的 `approval_level` 设为 OFF。角色矩阵（admin / manager / team-leader / worker / human）回答的是*你是谁*；capabilities 回答的是*你可触及哪些敏感面*。没有地基（字段 + 校验 + helper + 审计），每个消费方 PR 都会自造一套门禁与一套审计轨迹。

本设计只建地基。它**不**门控任何既有操作——消费方是 #1220 §12 中的后续 PR（#1216 工具审批端点上的 `approval_policy` OFF 门禁、携 `external_sources` 恢复 mcpServers/remoteSkills、密钥契约）。

## 值集合

五个值，封闭集合，单一事实来源（`agentteams-controller/internal/auth/capability.go`）：

| 值 | 授予 | 消费方 |
|-------|--------|----------|
| `full_access` | 元值：蕴含全部 capability | （permissionLevel 1 在角色基线已蕴含全部） |
| `channel_secrets` | 写通道凭据 | 通道凭据端点 |
| `external_sources` | remoteSkills 注册表 + 凭据管理 | mcpServers/remoteSkills 恢复 |
| `approval_policy` | 将 worker 的 `approval_level` 设为 OFF | #1216 工具审批端点 |
| `secret_reveal` | （预留，无 v1 消费方——#1220 §6.4） | — |

本表与代码常量间的漂移由 `TestValidCapabilitiesMatchesDocumentedValueSet` 钉死。

## 设计

### CRD

`Human.spec.capabilities: []string`（omitempty）。列表形态，使未来新值可增补。团队 leader 与其他基于 SA 的身份永不持有 capabilities（#1220 §5）——唯一写入者是 human-update API，仅 admin/manager 可用。

### human-update API

`PUT /api/v1/humans/{name}` 新增 `capabilities`，语义与 `accessibleTeams` 相同的 merge-patch（缺省 = 不变 / 显式列表 = 替换 / `[]` = 清空）。未知值 → `400` 并列出封闭集合。值以规范化形式存储（去重 + 排序）。仅 admin/manager 可更新 human（既有授权器默认拒绝，由 `TestAuthorizer_HumanUpdateAdminOnly` 钉死）——自我授予在结构上不可能。

### HasCapability

`auth.HasCapability(caller *CallerIdentity, cap Capability) bool`：

- admin / manager → 恒 true（角色基线）；
- worker → 恒 false；
- human / team-leader → 集合成员判定，其中 `full_access` 蕴含所有值。

capability **绝不蕴含团队范围**：消费方按 #1220 §3 的完整检查顺序组合——角色基线 AND `TeamMatches` AND `HasCapability`——依序执行。`CallerIdentity.Capabilities` 由 Matrix 认证器填充（Human CR 在该处已被取回，零新增 I/O）；SA 身份永不带该字段。

### 双层审计（#1220 §8）

`internal/audit`：`Client.Record(ctx, Event)` 写入

1. 一条即时结构化日志行（恒写，即使无存储）；
2. 一个只增的 `audit/<YYYY-MM-DD>.jsonl` 对象（UTC 日期），read-modify-write 配 `PutObjectIfMatch` ETag 乐观并发与 3 次重试（100/200/400 ms + 抖动）。进程内并发由客户端互斥锁串行化；持久层假定单控制器副本（当前 embedded 与 k8s 部署皆是）。

密钥卫生：`Event` 是封闭 schema——`Before`/`After` 只携 capability 名，`Detail` 是受控摘要，不存在凭据可藏身的自由文本字段（由 `TestEventJSONHasClosedSchema` 钉死）。

v1 只接线 capability 授予/撤销事件（每个变更值一个事件）。后续消费方（approval_level 变更、通道凭据写入、外部源新增/更新）调用同一 `Record`。

## 契约

| 面 | 契约 |
|---------|----------|
| CRD | `spec.capabilities` 省略 = 无；未知值在准入时拒绝（400）；团队 leader 永不持有 capabilities |
| API | merge-patch：缺省 = 不变 / 列表 = 替换 / `[]` = 清空 / 未知值 = 400（错误列出合法集合） |
| Helper | `HasCapability`：admin/manager true、worker false、human/leader 集合成员判定且 `full_access` 元蕴含；无团队范围语义 |
| 审计 | 每次授予/撤销 → 日志行 + `audit/<date>.jsonl` 追加；永不携密钥值 |

## 已知限制

- 持久审计层为单控制器副本；可查询读侧（`GET /api/v1/audit`）是独立的 stacked PR——见 `docs/design/audit-events-api.md`。
- `secret_reveal` 无 v1 消费方（预留值）。
- 身份缓存：capability 变更于受影响人类的 Matrix 令牌身份缓存过期时对其可见——与 `accessibleTeams` 既有语义相同。

## 测试

- `internal/auth/capability_test.go` — 角色基线矩阵（admin/manager 恒 true、worker 恒 false、leader 结构性 false、nil / 未知角色 false）、逐值集合成员判定、`full_access` 元蕴含、值集漂移钉死、规范化语义。
- `internal/auth/matrix_authenticator_test.go` — L2 人类携已授予的 capabilities；无 capability 的人类携空集且什么都不持有。
- `internal/server/resource_handler_human_update_test.go` — 授予生效（去重 + 排序）且其他字段保留；缺省 = 不变；`[]` 清空；未知值 → 400 点名该值并列出集合（且不落库）；授予 + 撤销写入预期审计行。
- `internal/audit/audit_test.go` — 对象创建携可解析行、追加保留既有行、ETag 冲突重试、重试耗尽后放弃（全有或全无，不 panic）、20 路并发无丢行、封闭 schema 密钥卫生钉死、nil 存储降级。
