# 主动助手：统一 Teams 触发（Draft，暂不合并）

Refs https://github.com/agentscope-ai/AgentTeams/issues/1683

本变更依赖任务协作协议重构。Controller 保存角色／用户上下文、调度检查点和发送回执，复用现有持久化协作队列。到期后发送 `assistant_digest` Teams 控制事件和 `m.mentions`，六种 Harness 沿已有 Matrix 输入链路验证回执，执行同一份规范输入。公共机制不调用私有 Console／HTTP 唤醒接口、cron、heartbeat 或原生会话文件。

原 QwenPaw POC 的直连 Console runner 已移除。原 POC 的截图只证明原演示链路，不代表本次 Teams 触发已经完成六种 Harness 的真实运行验收。

## 公共流程

1. Controller 每分钟检查已就绪的 Leader／Manager，默认开启每日摘要。角色或会话未就绪时等待。
2. 根据角色、用户、当前执行者和上一条发送检查点生成稳定事件身份；同周期调度重试复用同一 outbox 记录。
3. Controller 在固定助手房间发送标准 Teams 消息／@ 唤醒。可见正文仅为提示，执行输入来自鉴权回执。
4. 公共校验检查当前 owner、房间和启用状态。正文伪造、旧角色／实例、其他用户或已关闭的调度不能启动推理。回执暂不可用时保留重试，不能执行未经验证的正文。
5. Agent 通过 `agt assistant context` 获取公共摘要与最近 12 条 Matrix 会话消息，按权限采集并核实信息，通过 `agt assistant send` 提交提醒与摘要。
6. Controller 投递到可信用户路由，返回真实 Matrix messageId 并更新摘要／检查点。共享队列根据发送回执完成；唤醒成功或模型声称完成都不代表提醒已送达。

```sh
agt assistant context --team <Team-CR-name>
agt assistant receipt <request-id> --team <Team-CR-name>
agt assistant remember --team <Team-CR-name> --file context.json
agt assistant send --team <Team-CR-name> --file message.json
```

Manager 省略 `--team`。这些是公共数据／操作接口，触发载体始终为 Teams 消息。

```json
{"owner":"<current-owner>","enabled":true,"everySeconds":86400,"summary":"用户关注方向和上次摘要"}
```

```json
{"owner":"<current-owner>","requestId":"<scheduled-request-id>","text":"提醒正文与已核实来源","summary":"给角色接任者的用户上下文"}
```

相同 requestId 的重试须保持正文与摘要不变。已保存的发送回执不会重发，并可补齐上下文检查点。`sent` 表示 Matrix 已接受；Dashboard 通知已读不等于 Matrix 已读回执。

## Draft 合并门禁与未覆盖范围

- 等任务协议重构完成，再基于最终协议 rebase 并做联合回归；不提前合并，不启用自动合并。
- 六种 Harness 分别认证标准触发、去重、伪造／过期／跨用户拒绝、暂不可用重试与旧执行者撤权。
- 镜像须同时包含新的 `agt assistant` CLI 与本分支公共脚本／插件。现有发布镜像不会自动获得该能力；正式合并前统一核对 Harness 源码 pin、Controller 镜像与发布依赖。
- 原子角色交接、多副本上下文写入、跨发送身份的崩溃窗口和未提交结果的原生执行恢复仍需联合验收。单进程锁不能替代协议级保证。
- Manager 当前仅映射内置管理员会话，多 L1 Admin 路由待完成。
- Team Admin 更换时的团队业务资料与个人兴趣分层迁移待完成；同一 Admin 下的 Leader 接管不代表该场景通过。
- 原生 SDK 的 transcript／进程恢复只承担执行状态，不拥有公共用户上下文、调度或发送事实。
- 角色资料细粒度读取隔离按讨论暂缓；旧角色写入与发送撤权仍为强制验收项。

本目录 Dockerfile 仅用于隔离测试覆盖构建，基础镜像须显式指定，使用独立时间标签、网络、卷和端口。六种 Harness 正式使用各仓库的构建入口和公共源码 pin。
