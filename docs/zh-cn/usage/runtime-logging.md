# Manager 与 Worker 运行时日志

更新后的 Harness 镜像对 Manager、Leader 和 Worker 使用同一启动器，捕获启动阶段及原生 stdout/stderr，保留退出码、转发终止信号，并继续输出脱敏后的 Docker/Kubernetes 控制台日志。仅更新 Controller 不会更新运行中的 Harness。

## 文件与保留策略

| 文件 | 内容 |
| --- | --- |
| `runtime.log` | 启动过程及原生 stdout/stderr |
| `events.jsonl` | 生命周期、消息及已支持的模型/工具事件元数据 |
| `status.json` | 最新状态、boot ID、就绪依据及退出码 |

Manager 默认目录为 `/var/log/agentteams/manager/`，其他成员为 `/var/log/agentteams/workers/<资源名>/`。每次启动都有独立 boot ID，以及 runtime、role 和 name 标记。`AGENTTEAMS_LOG_DIR` 可覆盖目录。删除容器后仍需保留日志时，请在该目录挂载卷；启动器不会上传日志或自动创建持久化卷。

每个轮转日志默认上限为 10 MiB，保留三份备份，可通过 `AGENTTEAMS_LOG_MAX_BYTES` 和 `AGENTTEAMS_LOG_BACKUPS` 调整。日志文件权限为 0600。凭据环境变量值、常见凭据字段、Bearer token 和 URL 密码会被遮蔽。结构化事件不记录消息正文、提示词、答案和工具参数；错误摘要和原生日志仍可能包含应用文本，分享前请检查。

## 诊断覆盖范围

| Harness | 就绪依据 | 消息事件 | 模型/工具事件 |
| --- | --- | --- | --- |
| OpenClaw | 原生 Gateway 健康检查 | 原生控制台 | 原生 verbose 控制台 |
| QwenPaw | 原生 API HTTP 成功 | 原生控制台及追踪适配器 | 本地原生 span 元数据、错误及 HTTP 状态 |
| Hermes | 原生 Matrix 连接成功 | 收到、发出、发送失败 | 本地原生 span 元数据、错误及 HTTP 状态 |
| DeepSeek | Matrix sync 成功 | 收到、发出、通道/执行轮次失败 | 原生请求/工具元数据及执行轮次错误 |
| Codex | Matrix sync 成功 | 收到、发出、轮询/投递失败 | App Server 工具 ID、状态、错误、退出码及执行轮次错误 |
| OpenHuman | 原生 Core 健康检查 | 原生控制台 | 原生控制台 |

OpenClaw 和 OpenHuman 保留原生控制台覆盖，采集器不会推断它们未提供的结构化工具或模型事件。DeepSeek 的原生请求/工具元数据在子进程结束后采集。带 `layer=agent_turn` 的 `inference` 事件可能包含多次模型请求及工具调用。`ready` 只证明注明的检查通过，不代表模型对话成功。

QwenPaw 和 Hermes 复用镜像中的原生插桩，将元数据写入本地日志，无需 Collector。未启用远程 OTel 时不创建 OTLP exporter，正文采集默认关闭；显式 OTel 配置继续控制远程追踪。QwenPaw Worker 默认关闭重复的原生文件日志；显式启用 `QWENPAW_WORKER_LOG_FILE_ENABLED` 时，原生日志保存在 `<日志目录>/native`，除非另行覆盖原生路径。

## 排查方式

优先使用现有 Controller 只读接口：

```sh
agt logs manager default --since 15m --tail 300
agt logs worker alice --since 15m --tail 300
```

结构化事件也输出到容器 stdout，现有 Manager 诊断技能可以直接查询，无需 Docker 权限。CLI 读取容器日志，不会读取每个本地文件。主机运维人员还可查看挂载的日志归档，或执行：

```sh
docker exec agentteams-manager cat /var/log/agentteams/manager/status.json
docker exec agentteams-manager tail -n 100 /var/log/agentteams/manager/events.jsonl
```

先关联 boot ID 和 Matrix room/event ID，再检查具体错误、HTTP 状态或工具退出码。缺少插桩属于证据缺口，不能据此断言调用没有发生。权限、数据源覆盖范围及 Dashboard 跳转见[运行时诊断](runtime-diagnosis.md)。
