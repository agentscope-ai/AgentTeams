# 个人文件区、回收站与工作区配额

状态：已实现（#1414 P1）
API：`GET/PUT/DELETE /api/v1/humans/{name}/files/{path...}`、`GET /api/v1/humans/{name}/trash`、`POST /api/v1/humans/{name}/trash/{id}/restore`、`DELETE /api/v1/humans/{name}/trash/{id}`、`GET/PUT /api/v1/humans/{name}/quota`

## 范围

P1 为每个 Human 提供私有的、由控制器代理的文件区，含回收站与可选工作区配额：

- **P1 范围内：** 按 Human 划分的存储命名空间、代理文件 API（读 / 列表 / 写 / 软删除）、回收站（列表 / 恢复 / 清空）、路径校验、单文件大小上限，以及可选的工作区配额（`workspaceQuotaBytes`）。
- **后续阶段（不在本期）：** 团队默认授权与 `workspaceAccess`（P2）、文件同步 / 载体 / `file_sync`（P3）、Agent 归属与 UI（P4）。P1 中文件区仅本人、admin 与 manager 可访问。

## 存储布局

文件区位于共享桶的顶层 `humans/` 命名空间下，与 `agents/`、`teams/` 平行：

```
humans/                              （由初始化器创建）
└── <username>/
    ├── files/                       个人工作文件（本 API）
    │   └── ...
    └── .trash/                      回收站（本 API）
        ├── <id>/meta.json           原路径、deletedAt、caller、role、size
        └── <id>/content             被删除文件的内容
```

- `<username>` 为 Human 的有效用户名（`spec.username`，缺省取 metadata 名称）。
- `Deployer.EnsureHumanStorage` 幂等地创建 `files/.keep` 与 `.trash/.keep`；Human reconciler 在 infra 阶段之后调用它。存储失败是**非致命的**——仅记录日志并提前重新入队（与团队共享存储相同的契约），因此存储抖动不会阻塞 Matrix 用户 / 房间同步。
- P1 刻意不创建 `memory/` 与 `digest/`。
- 回收站条目计入工作区配额。

回收站 ID 格式：`<UTC yyyymmddThhmmssZ>-<8 位十六进制>`（如
`20261024T123045Z-abcd1234`）。时间戳前缀保证列表可按删除时间排序；随机后缀区分同一秒内的多次删除。

## 授权矩阵

所有路由均经过认证；授权在 Handler 内、Human CR 解析之后完成，以便两种拒绝形态共存：

| 调用方 | 读 / 列表 / 写 | 回收站恢复 | 回收站清空 | 配额 GET | 配额 PUT |
|---|---|---|---|---|---|
| 本人（用户名与文件区匹配） | ✔（仅自己） | ✔（仅自己） | ✖ 403 | ✔（仅自己） | ✖ 403 |
| manager | ✔（任意） | ✔ | ✖ 403 | ✔ | ✖ 403 |
| admin | ✔（任意） | ✔（`?overwrite=1`） | ✔ | ✔ | ✔ |
| 其他 Human / 团队 leader / worker | ✖ 404 | ✖ 404 | ✖ 404 | ✖ 404 | ✖ 404 |

完全无访问权的调用方收到**与 Human 不存在时完全相同的 404 响应**——API 不会向外人证实某 Human 的存在或其文件布局。

## 文件 API

### `GET /api/v1/humans/{name}/files/{path...}`

- **文件**（普通路径）：以 `Content-Type` 与 `ETag` 头流式返回字节内容。
- **目录**（路径以 `/` 结尾、`?list=1`、或路径为空）：

```json
{"path":"files/docs/","entries":[
  {"name":"b.txt","type":"file","size":6,"updatedAt":"2026-10-24T12:00:00Z"},
  {"name":"nested","type":"directory","updatedAt":"2026-10-24T12:00:00Z"}
]}
```

  目录条目由嵌套键合成；`.keep` 标记对象不显示；条目按名称排序。

### `PUT /api/v1/humans/{name}/files/{path...}`

- 原始请求体即文件内容。
- 单文件大小上限：默认 **64 MiB**，可通过环境变量 `HUMAN_FILE_MAX_BYTES` 覆盖；超限 → `413`，`{"code":"size_exceeded"}`。
- 可选 `If-Match: <etag>` 提供乐观并发控制；etag 过期或目标尚不存在 → `412`，`{"code":"etag_mismatch"}`。
- 写入前执行工作区配额检查：`usedBytes + size` 超过 `workspaceQuotaBytes` → `413`，`{"code":"quota_exceeded"}`。
- **跨 Controller 原子预留：** 配额不只用进程内检查；每个 Human 使用
  `humans/<user>/.control/quota.json` 账本。首次用 `PutObjectIfAbsent`
  （`If-None-Match:*`）创建；后续按 `newSize-oldSize` 增量用
  `PutObjectIfMatch`（`If-Match`）预留，冲突重试；文件写失败则释放预留。
  账本不在 `files/`/`.trash/` 下，不计入 live usage；`.control/` 为 API
  保留路径。purge 与覆盖式 restore 在账本存在时同步调整；关闭配额期间
  已有账本仍随写入推进，重新启用配额时按 live usage 安全重建；重建只升不降，
  保留尚未落盘的在途预留，宁可保守多算也不放过超配额写入。
- **跨副本回归：** 两个独立 Handler 共享同一 OSS，在 10 字节配额下必须
  一个 200、一个 413，最终 usage ≤10。
- 成功 → `200` `{"path":"files/<rel>","size":N,"etag":"..."}`。

### `DELETE /api/v1/humans/{name}/files/{path...}`

软删除：文件先复制到 `.trash/<id>/{meta.json,content}`，**之后**才删除原文件。成功 → `200` `{"trashId":"<id>"}`。不支持目录删除。

## 回收站

| 端点 | 访问权限 | 行为 |
|---|---|---|
| `GET .../trash` | 本人 / manager / admin | `{"entries":[{id,path,size,deletedAt,caller,role}]}`，最新在前 |
| `POST .../trash/{id}/restore` | 本人 / manager / admin | 将字节写回原路径并删除该条目；目标已存在 → `409` `{"code":"path_conflict"}`；`?overwrite=1` 仅 admin 可用 |
| `DELETE .../trash/{id}` | **仅 admin**（其他调用方 `403`） | 永久删除 + `human_trash_purge` 审计事件 |

## 配额

`Human.spec.workspaceQuotaBytes`（可空 `int64`）是文件区的总字节预算——`files/**` 加 `.trash/**`——缺省为不限制。

- **`GET /api/v1/humans/{name}/quota`**（本人 / manager / admin）→
  `{"usedBytes":N,"quotaBytes":M|null}`。`usedBytes` 通过实时列举两个前缀计算（不做任何持久化）；未设置配额时 `quotaBytes` 为 `null`。
- **`PUT /api/v1/humans/{name}/quota`**（仅 admin）——请求体
  `{"quotaBytes":N}` 设置配额、`{"quotaBytes":null}` 清除；负值 → `400`。值会被 patch 到 Human CR 上，变更写入审计（`human_quota_set`）。
- **强制：** 写入在字节落盘前检查——`usedBytes + size > quota` → `413`
  `{"code":"quota_exceeded"}`。回收站恢复不做配额拦截：这些字节已计入
  `usedBytes`，搬回 `files/` 不会增长用量（带 `?overwrite=1` 的恢复还会替换目标并减少用量）。已存在的数据即使超出被调低后的配额也不会被拒绝，只拒绝进一步的增长。

## 路径规则

路径相对于 `files/`，经 NFC 归一化后、在任何存储访问之前完成校验。以下情况以 `400` 拒绝：绝对路径、`..` / `.` 段、空段、反斜杠、控制字符、非 UTF-8 输入、总长超过 1024 字节、任一段超过 255 字节、保留顶层名 `.trash`、`.sync` 与 `.control`，以及 Windows 保留设备名（`CON`、`NUL`、`COM1-9`、`LPT1-9`，含或不含扩展名）。

## 审计

每个变更操作都会追加到双层审计日志：

| 事件 | Detail 字段 |
|---|---|
| `human_file_write` | path、size、etag |
| `human_file_delete` | path、size、trashId |
| `human_trash_restore` | path、size、trashId、overwrite |
| `human_trash_purge` | path、size、trashId |

事件携带 `who` / `role` / `target`（Human CR 名称），与其他控制器审计事件一样写入 `audit/<date>.jsonl`。

## 错误

| 条件 | 状态码 | 响应体 |
|---|---|---|
| 无访问权（或 Human 不存在） | `404` | `{"message":"human not found"}` |
| 非 admin 执行 admin 专属操作 | `403` | `{"message":"admin role required"}` |
| 非法路径 / 非法 id / 写入目录 | `400` | 消息指明问题 |
| 超过单文件大小上限 | `413` | `{"code":"size_exceeded",...}` |
| 超过工作区配额 | `413` | `{"code":"quota_exceeded",...}` |
| `If-Match` 过期 | `412` | `{"code":"etag_mismatch",...}` |
| 恢复目标已存在 | `409` | `{"code":"path_conflict",...}` |
