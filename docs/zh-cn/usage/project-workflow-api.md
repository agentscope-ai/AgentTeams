# 项目 / 工作流查看 API

> 由项目工作流查看 PR 新增（agentteams/AgentTeams#1169）。

Controller 提供两个只读端点，把 TeamHarness 项目状态
（`shared/projects/{id}/meta.json`）暴露为 LangGraph 对齐的工作流视图。
它们是面向人类视图（dashboard、QwenPaw console 插件）的数据源，
也被 `agt get projects` 消费。

## 适用范围与前置条件

这些端点在**任何运行 TeamHarness（projectflow/taskflow）的 AgentTeams 部署**中都能工作——存储布局通过 Controller 配置的对象存储客户端读取，因此 `AGENTTEAMS_STORAGE_PREFIX` 与 `AGENTTEAMS_FS_BUCKET`（包括非默认值）都被自动处理，无需按部署定制代码或配置。

前置条件：

* 编排项目的 Worker 上安装了 TeamHarness MCP（`plugins/teamharness`）。只有 `projectflow`（`create_project` / `create_quick_project`）创建的项目才会产生这些端点读取的 `shared/projects/{id}/meta.json`。没有用 projectflow 而手工管理任务的团队没有项目数据——这是预期行为，不是 bug。
* 项目写入通过 `_sync_project`（随本 API 一同引入）实时推送到共享存储，因此 Controller 读到的是近实时状态，而非启动快照。

部署模式（embedded Docker、incluster K8s）全部支持；在无 K8s 的开发模式下 Controller 与其他端点一样跳过认证，因此 RBAC 仅在配置了认证器时生效。

## 端点

### `GET /api/v1/projects`

列出所有团队（以及全局 `shared/projects/` 前缀）的项目。

查询参数：

| 参数 | 含义 |
|:--|:--|
| `team` | 只返回团队匹配的项目。团队 leader 已被限定到自己的团队（们）；独立项目（空团队）仅在未设置过滤时匹配。 |

响应 `200 OK`：

```json
{
  "projects": [
    {
      "project_id": "demo-project-001",
      "title": "Demo project",
      "status": "active",
      "plan_type": "dag",
      "team_id": "biz-team",
      "mode": "project"
    }
  ],
  "total": 1
}
```

* `status` 是 TeamHarness 写入的原始项目状态：
  `active` | `paused` | `completed`。
* 项目按 `project_id` 排序。跨前缀重复的 id 会去重（meta.json 可能同时镜像
  在 effective 团队名前缀和 CR 名前缀下）。
* meta.json 缺失或损坏的项目被跳过（目录可能存在而文件正在上游写入中）。

### `GET /api/v1/projects/{id}/workflow`

返回一个项目的 LangGraph 对齐工作流。

可选查询参数：

| 参数 | 类型 | 含义 |
|:--|:--|:--|
| `includeTasks` | `bool` | 为 `true` 时同时读取每个任务的 TaskMeta（`shared/tasks/{id}/meta.json`），在响应中附加 `tasks_detail` 数组（spec/result/交付物字段）。默认 `false` 保持响应轻量。 |

响应 `200 OK`：

```json
{
  "project_id": "demo-project-001",
  "title": "Demo project",
  "status": "active",
  "plan_type": "dag",
  "team_id": "biz-team",
  "mode": "project",
  "source": "dingtalk",
  "nodes": [
    {"id": "t1", "name": "Task 1", "status": "completed", "assignee": "@w1:matrix.local"},
    {"id": "t2", "name": "Task 2", "status": "delegated", "assignee": "@w2:matrix.local"}
  ],
  "edges": [
    {"source": "t1", "target": "t2", "conditional": false}
  ],
  "next": ["t2"],
  "interrupts": [
    {"id": "t3", "value": "blocked"},
    {"id": "loop", "value": "waiting for human decision"}
  ],
  "values": {
    "project_id": "demo-project-001",
    "title": "Demo project",
    "status": "active",
    "plan_type": "dag",
    "team_id": "biz-team",
    "mode": "project",
    "task_count": {"completed": 1, "delegated": 1}
  },
  "loop": null,
  "requester": "dingtalk:user:session",
  "source_room_id": "!room:matrix.local",
  "tasks_detail": [
    {
      "task_id": "t1",
      "project_id": "demo-project-001",
      "status": "completed",
      "spec_path": "shared/tasks/t1/spec.md",
      "assigned_to": "@w1:matrix.local",
      "summary": "Alpha report done",
      "result_status": "SUCCESS",
      "deliverables": [{"type": "file", "path": "shared/tasks/t1/output.pdf"}],
      "result_path": "shared/tasks/t1/result.md"
    }
  ]
}
```

`tasks_detail` 仅在 `?includeTasks=true` 时出现。它透传项目级 `nodes[]` 摘要不包含的 TaskMeta 字段：`spec_path`（任务规格文件）、`summary` / `result_status` / `result_path`（提交结果）、`deliverables`（产物清单）与 `cancel_reason`（取消原因）。TaskMeta 按与项目相同的双前缀布局读取（优先 `teams/{team}/shared/tasks/{id}/meta.json`，其次 `shared/tasks/{id}/`），团队作用域的任务优先于任何全局副本。没有 TaskMeta 文件的任务（如尚未委派）会被跳过；单个任务读取错误也会跳过，避免一个坏任务拖垮整个响应。

节点状态归一化为前端友好枚举：

| API 值 | 原始 TeamHarness 状态 |
|:--|:--|
| `pending` | `planned` |
| `delegated` | `assigned` |
| `in-progress` | `in_progress`、`submitted` |
| `completed` | `completed` |
| `revision` | `revision` |
| `blocked` | `blocked`、`cancelled` |

语义（镜像上游 `_ready_nodes` / `_ready_loop_nodes`）：

* `next` —— 就绪节点：原始状态为 `planned`/`assigned` 且依赖全部
  `completed` 的任务。项目非 active 或 loop 处于 `waiting_user` /
  `blocked` / `completed` 时为空。
* `interrupts` —— 等待人工决策点：blocked 任务，或 `waiting_user` /
  `blocked` 状态的 loop。
* `values.task_count` —— 按归一化状态统计的节点数。

错误响应：

| 状态码 | 含义 |
|:--|:--|
| `400` | 缺少项目 id。 |
| `403` | 已认证但该角色完全不能读取项目（如 Worker）。 |
| `404` | 项目不存在（所有扫描前缀下都无 meta.json）——**或**调用者是限定读者（团队 leader / L2 人类）且不拥有该项目（隐藏存在性以防 id 枚举）。 |
| `500` | K8s 或对象存储故障。 |

### `GET /api/v1/projects/{id}/tasks/{taskId}/artifact`

下载一个任务的一个产物，为 dashboard 和 console 插件补全「交付物 → 下载 → 审 → 接受」闭环。

可选查询参数：

| 参数 | 含义 |
|:--|:--|
| `path` | 要下载的产物路径。必须是任务**已声明**的产物之一——`result_path`、`spec_path` 或 `deliverables` 的某一项（均从 TaskMeta 读取）。省略时默认提供 `result_path`（已发布结果）。 |

不带 `?path=` 时下载任务的 `result_path`（已发布结果）。带 `?path=` 时，请求路径必须是任务已声明的产物之一——`result_path`、`spec_path`（任务规格书）或 `deliverables` 的某一项。随后路径通过严格白名单校验：必须位于 `shared/tasks/{taskId}/` 或 `shared/projects/{projectId}/` 之下，且不得包含 `..` 或以 `/` 开头。由于**白名单 + 已声明产物**双重校验，被攻破的 Worker 无法构造读取任意 MinIO 对象的路径，客户端也无法下载恰好位于任务目录但未声明的文件。

文件以 `Content-Disposition: attachment`（文件名为 basename，非 ASCII 名用 RFC 5987 `filename*=utf-8''...` 编码——中文文件名可正确下载）返回，`Content-Type` 由扩展名推断。

错误响应：

| 状态码 | 含义 |
|:--|:--|
| `400` | 缺少项目 id 或任务 id。 |
| `403` | 已认证但该角色完全不能读取项目（如 Worker）。 |
| `404` | 项目不存在 / 调用者不拥有它（隐藏存在性）/ 任务不在项目图中 / 任务没有已发布产物 / 请求路径不是已声明产物 / 产物文件缺失 / 产物路径被拒绝。 |
| `500` | K8s 或对象存储故障。 |

## 人类干预与生命周期端点（W-PR-2）

上面的只读端点之外，还有让人类干预 agent 编排工作流的写端点。所有写入都经过
**代码级授权**：中间件拒绝跨团队写入（authorizer `requireSameTeam`），handler
解析出归属团队后还会显式调用 `checkProjectAccess`（因为中间件无法把 project
路径映射到团队）。每次写入都打上审计字段（`updated_by` / `updated_at`，给了
原因时还有 `pause_reason`），并应用 mtime 乐观锁——如果读取与写入之间 worker
推送了更新的 `meta.json`，写入以 `409` 失败而不是覆盖它。

### `POST /api/v1/projects`

创建项目（结构化，对齐 TeamHarness `create_project`）。admin/manager 可以
不带团队创建独立项目；team-leader 或 L2 人类必须传一个自己可访问的
`team_id`。

请求体：

```json
{
  "title": "新项目",
  "source": "matrix",
  "requester": "@luo:server",
  "team_id": "biz-team",
  "project_id": "可选自定义 id",
  "source_room_id": "!room:server"
}
```

省略 `project_id` 时自动生成；必须为纯 token（`[A-Za-z0-9._-]`）。响应
`201 Created`：

```json
{
  "project_id": "proj-2026-08-12T00:00:00Z",
  "title": "新项目",
  "status": "active",
  "team_id": "biz-team",
  "plan_type": "dag"
}
```

错误：`400` 缺 title/非法 id/受限调用方缺 team；`409` 项目已存在；
`403`/`404` 跨团队（拒绝 / 隐藏存在性）。

### `POST /api/v1/projects/{id}/pause`

把项目状态置为 `paused`。暂停会停止新任务派发（`ready_nodes` 返回空）但
**不会中断进行中任务**；它们的完成报告仍会到达（文档化行为——进行中任务不被
取消）。可选请求体 `{"reason": "..."}` 记录到 `pause_reason`。响应 `200`
返回更新后的工作流（`buildWorkflow`）。错误：`409` 已暂停/已完成；`404`
不存在或无权访问。

### `POST /api/v1/projects/{id}/resume`

把暂停的项目恢复为 `active`。响应 `200` 返回更新后的工作流。错误：`409`
未暂停；`404` 不存在或无权访问。

### `POST /api/v1/projects/{id}/replan`

替换项目的 DAG 计划。请求体携带新任务（可选 `tasks` 数组）：

```json
{
  "tasks": [
    {"taskId": "t1", "title": "步骤 1", "assignedTo": "@dev:server", "dependsOn": []},
    {"taskId": "t2", "title": "步骤 2", "dependsOn": ["t1"]}
  ]
}
```

字段按 TeamHarness `_normalize_task` 归一化（`taskId`/`task_id`、
`assignedTo`/`assigned_to`、`dependsOn`/`depends_on`，status 默认
`planned`，`pending` 映射为 `planned`）；已存在的 task id 在原始条目省略
字段时保留之前的 title/assignee/status。校验对齐 `_validate_task_graph`：
重复 id、未知依赖、依赖环都以 `400` 拒绝。前置条件（`409`）：`plan_type`
必须是 `dag`（loop 的重规划走 `record_loop_iteration`）、状态必须是
`active`、不能有 `in_progress`/`submitted` 任务。响应 `200` 返回更新后的
工作流。

### `POST /api/v1/projects/{id}/tasks/{taskId}/cancel`

取消单个任务。请求体要求 `reason`（可选 `replacementTaskId`）。任务必须
可变——终态任务（completed/revision/blocked/cancelled）以 `409` 拒绝。
任务的 `TaskMeta` 打上 `status=cancelled` + `cancel_reason`，项目节点状态
同步更新。响应 `200` 返回更新后的工作流。错误：`400` 缺 reason；`404`
任务不在项目里/任务 meta 缺失；`409` 终态任务。

### `POST /api/v1/projects/{id}/complete`

把项目标记为已完成（终态）。所有任务必须处于终态
（completed/revision/blocked/cancelled——不能有 in_progress/submitted/
planned），否则 `409`。响应 `200` 返回更新后的工作流。

### 通知

写入成功后，Controller 用 `SendMessageAsAdmin` 向项目的 `source_room_id`
（回退到 `reply_route.target_session`）发送管理员消息，让房间里的 agent
无需轮询就能知道干预发生。尽力而为：房间未知或未配置 Matrix 时不发通知。

## 认证与授权

接受两种 bearer 令牌路径（复合认证器）：

1. **Kubernetes service account 令牌**（TokenReview）：admin / manager /
   worker。团队 leader（`team_leader` 角色的 worker）只能看自己团队的项目。
2. **Matrix 访问令牌**（L2 人类）：令牌用
   `GET /_matrix/client/v3/account/whoami` 验证；归属的 Matrix localpart
   匹配 `permissionLevel: 2`（Team）的 `Human` CR。人类的 `accessibleTeams`
   作为多团队范围——他们控制的所有团队聚合到单个列表/读取视图。非 L2 人类
   （permissionLevel 1 或 3）被拒绝。

授权矩阵：

| 调用方 | List | 获取工作流 | 写入（create/pause/resume/replan/cancel/complete） |
|:--|:--|:--|:--|
| admin / manager | 所有团队 | 任意项目 | 任意项目 |
| team-leader（SA） | 仅自己团队 | 仅自己团队 | 仅自己团队 |
| L2 人类（Matrix） | 所有 `accessibleTeams` | 任意可控团队 | 任意可控团队 |
| worker | 拒绝 | 拒绝 | 拒绝 |

## `agt` CLI

`agt get projects [name]` 包装两个端点：

```bash
agt get projects                      # 列出全部
agt get projects --team biz-team      # 按团队过滤
agt get projects demo-project-001     # 工作流详情
agt get projects demo-project-001 -o json
agt get projects demo-project-001 --mermaid   # 渲染 DAG 为 mermaid
```

CLI 原样转发配置的 bearer 令牌（`AGENTTEAMS_AUTH_TOKEN` 或
`AGENTTEAMS_AUTH_TOKEN_FILE`），所以 L2 人类也可以用——把任一变量指向自己的
Matrix 访问令牌即可，无需单独的 CLI 认证模式。

### `agt project`（W-PR-2 写命令）

`agt project` 包装写端点，人类无需 raw curl 即可干预：

```bash
agt project create --title "新项目" --team biz-team --source matrix
agt project pause demo-project-001 --reason "客户评审"
agt project resume demo-project-001
agt project replan demo-project-001 --tasks tasks.json   # JSON 数组文件
agt project cancel demo-project-001 demo-project-001-01 --reason "不再需要"
agt project complete demo-project-001
```

同样的 bearer 令牌转发适用（L2 人类用 Matrix 令牌）。
