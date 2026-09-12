# TeamHarness 任务转换引擎（Task Transition Engine）

任务状态从"各调用点散写 status"升级为"一张表 + 一个入口 + 一条可审计历史"。

## 问题

- 状态转换散落在 `delegate_task` / `ack_task` / `submit_task` /
  `accept_task_result` / `cancel_task` 五个调用点，各自的手写守卫宽严不一
  （`planned→in_progress`、`planned→submitted` 都可达，`accept` 无来源守卫）。
- 转换无历史：谁、什么时间、从什么状态改到什么状态，task meta 里查不到。
- `accept_task_result` 只写 project meta，task meta 与节点状态可永久分叉。
- 进度不可见：worker 执行长任务期间没有任何机器可读的进度记录。

## 设计

### 1. 转换表（单一事实来源）

`plugins/teamharness/contracts/task-transitions.json`：

```json
{
  "states": ["planned","prepared","assigned","in_progress","submitted",
             "completed","revision","blocked","cancelled"],
  "terminal": ["completed","revision","blocked","cancelled"],
  "transitions": {
    "planned":     ["prepared","assigned","in_progress","submitted","cancelled"],
    "prepared":    ["assigned","cancelled"],
    "assigned":    ["in_progress","submitted","cancelled"],
    "in_progress": ["submitted","cancelled"],
    "submitted":   ["completed","revision","blocked","cancelled"]
  }
}
```

- Python 写侧（`server.py` 的 `TRANSITIONS` / `TERMINAL_TASK_STATUSES` 常量）与
  Go 读侧测试加载同一文件并断言一致——跨语言单一事实源，双写漂移在测试期暴露。
- 表是权威集合；其上叠三处**收紧**（行为变更）：

| 动作 | 收紧后 | 原先 |
| --- | --- | --- |
| `ack_task` | from ∈ {`assigned`,`in_progress`} | 仅拒 `submitted` |
| `submit_task` | from ∈ {`assigned`,`in_progress`} | 仅要求非终态 |
| `accept_task_result` | from == `submitted` | 无来源守卫 |

越序转换返回结构化错误（`ok:false`），错误消息引导正确动作
（如 `submit_task: task is 'planned'; ack_task it first`）。

### 2. `_transition_task()` 单一入口

所有任务状态变更必经：转换表校验 → 写 `status` → 追加 `history`（cap 50，丢最旧）
→ task meta 与 project 节点**同批**同步（`_write_task` + `_sync_task` +
`_update_project_task`）。五个 MCP 调用点 + Controller `CancelTask` 全部收口到
该语义（Go 侧在同一 read-modify-write 批次内追加同构条目，actor = authzActor，
不改变 ETag/409 条件写语义）。

`accept_task_result` 收口后**同时更新 task meta**——修复"只写 project meta"的
既有分叉缺口。

### 3. history 条目

task meta（`shared/tasks/{id}/meta.json`）新增 `history: []`（additive）：

```json
{"ts": "2026-09-09T10:00:05Z", "from": "assigned", "to": "in_progress",
 "action": "ack_task", "actor": "worker:default", "note": ""}
```

- `action` ∈ `delegate_task` / `ack_task` / `submit_task` / `accept_task_result`
  / `cancel_task` / `progress`
- `actor` = `role:account`（MCP 侧）或 authzActor（Controller 侧）
- 同状态重入（幂等重试）不记条目；`cancel` 的 reason 记入 `note`

### 4. `report_progress`（新 action）

worker/remote-member 自报进度：`from == to` 的 history 条目（action `progress`），
note 必填（≤200 字符，超长截断并在响应中标记 `truncated`）。**不改状态、不发房间
通知**（v1 防噪音；事件端点与节点面板可见）。状态门 from ∈ {`assigned`,
`in_progress`}。

### 5. 读侧消费者（Controller）

- `GET /api/v1/projects/{id}/workflow?includeTasks=true`：`tasks_detail[].history`
  透传（旧 meta 无字段则省略；畸形条目跳过不报错）。
- `GET /api/v1/projects/{id}/events?limit=&cursor=`：读时聚合项目内全部任务
  history 成升序时间线（`limit` 默认 50 上限 200；`cursor` = 不透明 offset；
  响应 `{project_id, events, next_cursor}`）。零新存储、无写侧钩子。
  权限链同 workflow/history（跨 team 访问隐藏为 404；任务 meta 只取项目属主
  scope，不做跨 scope 回退）。项目级干预事件不在此端点——`/history` 快照端点
  覆盖干预审计，两者互补。

## 边界（v1 不做）

- 项目级干预事件不入本时间线（`/history` 快照端点已覆盖）。
- SSE 推送不做——客户端轮询 events 端点即可。
- `record_loop_iteration` 不改任务状态，属循环记账而非节点转换，不入 history。
- 不跨语言共享运行时表（Python 进程与 Go 二进制无共享部署面）——共享的是
  fixture 与测试断言。
- loop 计划的任务走同一 task meta 机制（task_id 级），语义不变。

## 已知限制

agent 写与 controller 写同一 task meta 的既有竞态（pull-before-write vs ETag
条件写）不恶化：history 追加与现有 task 字段同写事务、同待遇。

## 验证

- Ruby `test-transition-table.rb`：golden fixture 一致性、合法转换全链路
  （history 链完整性/actor 格式/RFC3339 时间戳）、越序拒绝（含错误消息引导）、
  同状态重入幂等、cap 丢最旧、accept 来源守卫 + task meta 同步、cancel 留痕、
  `report_progress` 全部分支（角色门/note 校验/截断/无通知）。
- Go `project_handler_test.go`：fixture 与 `isTerminalTaskStatus` 一致性、
  taskDetail history 透传（有/无/畸形字段）、events 端点（聚合排序/游标续读/
  limit 边界/空项目/404/denied→404/跨 scope 不回退）、`CancelTask` 留痕
  （含重试收敛不重复记）。
- `test-taskflow.rb` 全量回归（越序用例适配为"有 assignee 的完整生命周期"）。
