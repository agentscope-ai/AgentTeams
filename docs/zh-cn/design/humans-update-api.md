# Human 更新 API

状态：已实现
API：`PUT /api/v1/humans/{name}`

## 问题

Human 生命周期此前有 create / get / list / delete，唯独没有 update。变更一个人类的权限等级、团队范围或 worker 范围，需要删除并重建 Human CR——这会重新置备 Matrix 账户并发放全新的一次性密码，把用户已经登录在用的身份毁掉。团队此消彼长时，也没有办法收窄或扩展现有的授权。

## 设计

`PUT /api/v1/humans/{name}` —— 对 `spec` 可变部分的 merge-patch：

- **指针语义**（与既有 `containerManaged` / `state` 相同模式）：字段缺省 = 不变；字段出现 = 替换；显式空列表 = 清空列表。
- **可变字段：** `displayName`、`email`、`permissionLevel`、`accessibleTeams`、`accessibleWorkers`、`capabilities`、`note`。
- **经此端点不可变：** `name` 与 Matrix 身份（username / matrixUserID）。重新置备账户是有意的销毁重建操作，不是编辑。
- **等级语义（API 授权）：** `1` = admin（Matrix 令牌路径不解析 level-1 人类；他们使用 admin SA）。`2` = 团队范围（`accessibleTeams` + `capabilities`；见 [l2-worker-scoped-write.md](l2-worker-scoped-write.md) 与 [capability-foundation.md](capability-foundation.md)）。`3` = worker 范围——**只读**访问恰好等于 `accessibleWorkers` 的对象（worker 详情、通道配置、审批配置；无写入）；level-3 CR 上的 `accessibleTeams` / `capabilities` 不授予任何东西（见 [l3-worker-scoped-read.md](l3-worker-scoped-read.md)）。等级在下一次认证时生效（缓存身份按认证器的常规 TTL 过期）。
- **校验（在 K8s 写入之前执行）：**
  - `permissionLevel` 必须为 1、2 或 3 → 否则 `400`。
  - `accessibleTeams` 必须引用已存在的 Team CR，`accessibleWorkers` 必须引用已存在的 Worker CR → 否则 `400` 并点名缺失的引用。悬空授权不会静默扩大任何东西，但会让该人类够不到其自认为可达的资源；在写入时拒绝它，是保持权限模型诚实的做法。
  - `capabilities` 必须落在封闭五值集内（`full_access`、`channel_secrets`、`external_sources`、`approval_policy`、`secret_reveal`——见 [capability-foundation.md](capability-foundation.md)）→ 否则 `400` 并点名未知值、列出有效集合。存储时归一化（去重 + 排序）；每次授予/撤销均写入双层审计轨迹。
- **授权：** 该路由为 `human` 类型的 `ActionUpdate`。既有矩阵已仅向 admin/manager 允许该动作；团队 leader、团队范围人类与 worker 账户均落入默认拒绝。无需改动授权器。
- **调和集成：** 写入更新 Human CR；既有 human 调谐器（identity / infra / rooms 各阶段）按其常规周期重新同步 Matrix 邀请、房间成员关系与 `groupAllowFrom`。不新增调和路径。
- **范围之外——房间权力等级：** 本端点不授予或撤销 Matrix 房间管理权。`permissionLevel` 变更对 API 授权立即生效，但本身不改变该人类在既有房间中已持有的权力等级；为人类成员调和房间权力等级是独立关切，本端点及其调和集成均不处理。

## 契约

`PUT /api/v1/humans/{name}` →

| 结果 | 代码 |
|--------|------|
| 更新成功 | `200` + 完整 human 表示 |
| human 不存在 | `404` |
| JSON 非法 / 等级非法 / 悬空引用 / 未知 capability | `400` |
| 引用校验后端失败（K8s 错误） | `500` |
| 重试后 K8s 冲突 | `409` |

请求体（全部字段可选）：

```json
{
  "displayName": "Alice",
  "email": "alice@example.com",
  "permissionLevel": 2,
  "accessibleTeams": ["market-team"],
  "accessibleWorkers": [],
  "capabilities": ["approval_policy"],
  "note": "Marketing lead"
}
```

## 范围之外

- Matrix 身份变更（账户重新置备）。
- 人类本人的自助更新（L2 人类更新自己的授权会是一条提权路径；仅 admin 是安全默认）。
- 批量更新。

## 测试

- `internal/server/resource_handler_human_update_test.go` — level + teams 更新应用且未触碰字段保留；部分 merge 保留其余；显式空列表清空；非法等级（0/4/-1）→ 400；缺失 team → 400 点名；缺失 worker → 400 点名；既有 worker → 200；未知 human → 404。Capabilities：授予生效（去重 + 排序）且其余字段保留；缺省 = 不变；`[]` 清空；未知值 → 400 并列出有效集合（且不被持久化）；授予 + 撤销写出预期的双层审计行。
- `internal/auth/authorizer_test.go` — `TestAuthorizer_HumanUpdateAdminOnly`：admin/manager 允许；团队 leader、L2 人类与 worker 拒绝。
