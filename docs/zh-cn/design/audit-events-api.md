# 设计：GET /api/v1/audit —— 持久审计存储的查询端点

## 背景

capability 地基（PR #1220 issue，分支 `feat/capability-foundation`，PR #1237）引入了双层审计存储的写侧：`internal/audit` 客户端向 MinIO 中的 `audit/<YYYY-MM-DD>.jsonl` 对象追加 JSONL 事件——持久、只增、位于 K8s/SQLite 数据面之外。在此之前该存储只写不读：事件无法读回，工作台因此无法展示"谁在何时改了什么"。

本设计补上读侧，一个新的控制器端点：

```
GET /api/v1/audit
    ?team=<team-name>     team scope; L2 users may only query teams they
                          can access (same scope rules as the worker/team
                          read APIs). Absent: L1 (admin/manager) only.
    ?from=<RFC3339>       inclusive lower bound on event time
    ?to=<RFC3339>         inclusive upper bound on event time
    ?kind=<category>      capability | approval_level | channel | source
                          (derived from the action; unknown future actions
                          pass through as their own kind)
    ?cursor=<opaque>      keyset pagination cursor from a previous response
    ?limit=<n>            page size, default 50, max 200
```

输入校验（全部 400，先于任何存储扫描）：

- `from`/`to` 必须可解析为 RFC3339；两者同时在场时比较**原始时刻**——`from` 严格晚于 `to` 被拒绝，包括同一天内（用于枚举按日对象的按日截断不得掩盖日内顺序）。
- cursor 必须解码为 base64url JSON，其 `ts` 可解析为 RFC3339、`date` 为 `YYYY-MM-DD`；畸形字段在解码时即被拒绝（400），绝不留到扫描期间才以服务端错误形式暴露。

响应：

```json
{
  "events": [
    {
      "ts": "2026-09-14T03:22:10.123456789Z",
      "kind": "capability",
      "actor": "admin",
      "target": "h1",
      "targetTeam": "alpha-team",
      "action": "capability_grant",
      "capability": "approval_policy",
      "before": ["approval_policy"],
      "after": ["approval_policy", "channel_secrets"],
      "detail": ""
    }
  ],
  "cursor": "<opaque>"
}
```

- 事件按新到旧返回，按 (timestamp, seq) 做 keyset 分页，seq 是事件在其按日对象内的行号（稳定：对象只增）。
- `before`/`after` 仅在定义处出现（capability 增/删、approval_level 变更）。它们永不含密钥值——写入方按构造保证（仅字段名）。
- `cursor` 仅当页满时在场（可能还有后续事件）。
- `targetTeam`（相对 issue 事件形态的增补）使 L1 视图能将事件归属到团队；团队范围查询已按它过滤。

## 范围规则（与既有读 API 一致）

- **admin / manager**：全范围。不带 `?team=` 时看到全部事件，包括无目标团队的事件（如 human 上的 capability 变更，属全局而非团队范围）。带 `?team=T` 时仅 `targetTeam` 为 T 的事件。
- **team leader / L2 人类**：`?team=` 必填（否则 400 并附自解释消息）；跨团队读被隐藏为 404（W8 抗探测——403 会暴露该团队的存在）。
- **worker**：拒绝（该路由是资源类型 `audit` 的 `ActionGet`；适用授权器默认拒绝）。

授权器在中间件层放行 human 与团队 leader 的读——处理器才是真正的范围边界，与 worker/team 读路径完全相同（"处理器按 accessibleTeams 过滤"）。

## 降级

- **按日对象缺失**（当日无事件记录）：正常——当日零事件，不是错误。
- **存储读失败**（对象 get 或前缀 list）：502，携标准错误信封。
- **对象行畸形**：整个请求 502——绝不返回部分、静默截断的列表。

无界范围枚举 `audit/` 前缀（非按日对象忽略）；显式范围直接计算日集合，且每请求上限 366 天（超出 400），同时约束扫描与内存。

## 为何用 keyset 分页

存储只增且随时间无界；offset 分页每页都要重扫重排，且在并发追加下会漂移。按 (timestamp, seq) 的 keyset 在追加下稳定：seq 只在当日尾部增长，故事件在返回它的那页之后的位置永不变。

## 文件

| 文件 | 变更 |
|------|--------|
| `agentteams-controller/internal/audit/query.go` | 新增：查询引擎（按日枚举、过滤、keyset 分页、类型化错误） |
| `agentteams-controller/internal/audit/query_test.go` | 新增：引擎测试（分页、过滤、畸形、存储失败、tiebreak） |
| `agentteams-controller/internal/server/audit_handler.go` | 新增：HTTP 处理器（范围、校验、信封） |
| `agentteams-controller/internal/server/audit_handler_test.go` | 新增：处理器测试（角色矩阵、400/404/502 映射） |
| `agentteams-controller/internal/server/http.go` | +3：路由注册 |
| `agentteams-controller/internal/auth/authorizer.go` | +18：human / team-leader 的 `audit` 读 case（处理器是范围边界） |
| `agentteams-controller/internal/auth/authorizer_test.go` | +22：角色矩阵测试 |

## 兼容性

- 只读：无 schema 变更、无写入方变更、无数据迁移。
- 路由为增补；不调用它的工作台旧客户端不受影响。
- stacked 于 `feat/capability-foundation`（PR #1237）之上，因写入方与本读取方共享 `internal/audit` 包与 MinIO 布局。#1237 合并后，本 PR rebase 到 main 即自成一体。

## 开放问题

- v1 无实时尾随（仅轮询）；若工作台想要实时，SseStream 是后续项。
- `secret_reveal`（#1220 §8 预留）在写侧接线后将以自有 kind 出现；读侧无需变更。
