# 查看运行指标与日志

你可以直接对 Manager 说：

> 看一下 Controller 和开发 Worker 的 CPU、内存，再查最近 15 分钟的日志，
> 帮我分析 Worker 为什么没有回复。

Manager 的 `task-diagnosis` 技能会使用自己的 Controller 身份调用下面的只读 CLI，
关联运行指标、容器日志与有权限查看的 Tracing，说明观察时间和缺失的证据，
并区分已观察到的事实与可能的原因。诊断不会自动重启容器或重试任务。

## CLI

在已安装 `agt`、已配置授权身份的 Manager 或 Controller 环境中执行：

```bash
agt monitoring
agt monitoring --search dev --page 1 --limit 12 -o json
agt logs component controller --since 15m --tail 300
agt logs manager default --since 1h --tail 300 -o json
agt logs worker dev --since 2026-11-14T08:00:00+08:00 --tail 100 -o json
```

先用 `monitoring` 确认资源的准确类型和名称。通过 `--page`、`--limit`（1–50）
分页查询；JSON 保留 Controller 返回的活动状态、生命周期和数据可用性等全部字段。
CPU 100% 对应一个 CPU 核。未知用量显示为不可用，内存无限额表示没有单独配置容器
内存配额。这些是当前采样值，不能用来证明过去某个时刻的资源用量。

`logs` 支持 `worker`、`manager`、`component` 三种资源，以及 `--tail`（1–1000）
和 `--since`（正时长，或带时区的 RFC3339 时间戳）。文本模式将日志写到标准输出，
将采样和截断提示写到标准错误输出，并移除终端控制字符；JSON 保留 Controller
返回的日志文本和查询范围信息。Controller 会遮盖常见凭据值，分享时仍应只保留
必要内容。容器输出最多读取 1 MiB，不代表完整日志归档，也不包含每个应用日志文件。

两个命令沿用 `AGENTTEAMS_CONTROLLER_URL`、`AGENTTEAMS_AUTH_TOKEN` /
`AGENTTEAMS_AUTH_TOKEN_FILE` 凭据约定，只通过 Controller 发起 GET 请求，
不使用 Docker、容器 exec、宿主机 Socket 或 Dashboard 凭据。管理员和 Manager
可查询容器；受限 Human、Leader 和 Worker 会被拒绝。权限或数据源查询失败时，
命令以非零退出码返回，不会把失败伪装成成功的空结果。

## 可用范围与升级

先升级 Controller 的[监控接口](../../design/runtime-monitoring-api.md)，
再更新包含新版 `agt` 与 Manager 技能资源的共享 Harness 镜像。Controller
镜像打包和两套 QwenPaw 插件包均包含该技能。仅修改源码不会更新已运行 Manager
的 CLI 或已加载技能。

当前指标与日志数据源支持 Docker 嵌入式部署；Kubernetes、外部托管 Worker
可能显示暂不支持。嵌入式部署的 Matrix、网关和存储共用 Controller 容器。
Tracing 的采集与查询后端需要单独配置；后端不可用或某个时间窗口没有日志，
不能证明没有执行或没有错误。
