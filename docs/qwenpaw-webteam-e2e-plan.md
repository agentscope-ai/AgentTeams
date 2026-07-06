# QwenPaw Web Team E2E 拆分计划

本文用于规划 QwenPaw + TeamHarness 的新一批 E2E 测试。测试不再把所有能力塞进一个超大
`test26`，而是围绕一个真实的 Web 网页开发团队，逐个 case 设计、实现和验证。

## 总体背景

测试团队由 3 个 QwenPaw worker 组成：

- `leader`：团队负责人，负责理解用户目标、分派任务、验收结果、回复消息来源。
- `dev`：开发成员，负责实现网页、编写代码或产出开发报告。
- `qa`：测试成员，负责检查交付物、反馈问题和验收质量。

所有测试都基于真实 QwenPaw worker 镜像、真实 model API、真实 Matrix 交互。能通过 API 或运行态文件验证的部分先用确定性断言；必须依赖 agent 行为的部分使用最小 prompt 和固定 marker 验证。

## 拆分原则

- 一次只设计和实现一个 test case。
- 每个 case 都要有清晰的场景、输入动作和验收点。
- 公共建队、package 构建、Matrix 查询、容器检查逻辑可以抽成 helper。
- 不把外部 channel、cron、CMS 等后续能力塞回基础建队测试。
- 失败时优先保留资源，便于检查容器、workspace、Matrix 消息和 runtime.yaml。

## 测试分组

建议保留 `test26` 作为 QwenPaw Web Team E2E 系列编号，拆成多个小脚本：

```text
test-26-1-qwenpaw-webteam-package.sh
test-26-2-qwenpaw-webteam-hot-update.sh
test-26-3-qwenpaw-webteam-heartbeat.sh
test-26-4-qwenpaw-webteam-cron.sh
test-26-5-qwenpaw-webteam-cms.sh
test-26-6-qwenpaw-webteam-dingtalk.sh
```

公共 helper 建议放在：

```text
tests/lib/qwenpaw-webteam.sh
```

## Test 26-1：Package 创建 Team

目标：验证基于 3 个初始 AgentSpec package 创建 QwenPaw Web Team 的基础闭环。

场景：

- 构建 `leader`、`dev`、`qa` 三个初始 package。
- 通过 API 创建 3 个 Worker CR 和 1 个 Team CR。
- package 内包含最小 role prompt、`BOOTSTRAP.md`、hello 脚本、一个测试 skill，以及秘钥防护验证材料。

验收：

- 3 个 Worker 都是 `runtime=qwenpaw`。
- 3 个 Worker 容器都使用 QwenPaw worker 镜像并正常运行。
- controller 为 3 个成员写入 `agents/{runtimeName}/runtime/runtime.yaml`。
- 3 个 workspace 都落入各自 package 的 `AGENTS.md`、`SOUL.md`、`BOOTSTRAP.md`、`config/` 和 skill 材料。
- `BOOTSTRAP.md` 被 agent 执行，hello 脚本产物存在，Matrix 中出现对应 bootstrap marker。
- 秘钥防护生效：agent 不能把测试 secret 明文输出到 Matrix 或 tool result。

## Test 26-2：Leader / Dev / QA 热更新

目标：验证 QwenPaw worker 通过 runtime.yaml desired state 拉取新 AgentSpec package，并在不重启容器的情况下更新运行态。

场景：

- 基于 26-1 创建好的团队，分别构建 `leader-v2`、`dev-v2`、`qa-v2` package。
- 修改对应 Worker CR 的 package ref。
- v2 package 修改 `AGENTS.md` 和一个业务 skill，放入固定 marker。

验收：

- package ref 变化后，runtime.yaml 投影更新。
- worker update loop 拉取并应用 v2 package。
- 对应容器 id 和 start time 不变。
- workspace 里的 `AGENTS.md` 和 skill 内容更新为 v2。
- 直接咨询对应 worker，能回答 v2 prompt 或 skill marker。

## Test 26-3：Heartbeat

目标：验证 QwenPaw leader 的 heartbeat 行为，以及 Matrix 消息 thread 展示方式。

场景：

- 给 leader 设置 heartbeat 配置。
- 手动触发或等待一次 heartbeat。
- heartbeat 内容包含固定 marker，避免只靠自然语言判断。

验收：

- heartbeat 产生 Matrix 消息。
- Matrix 主消息先显示处理中状态，完成后 edit 为最终结果。
- tool call、thinking 和过程消息进入 thread，不污染主消息上下文。
- heartbeat 是旁路行为，失败不能影响 QwenPaw worker 主 loop。
- 本阶段先不要求 controller 上报 endpoint 验证。

## Test 26-4：Cron 任务

目标：验证采用 QwenPaw 原生 cron 行为，不新增 AgentSpec cron schema。

场景：

- 给 leader 创建一个 QwenPaw 原生 cron job。
- cron job 默认 disabled。
- 手动触发 cron job，任务内容包含固定 marker。

验收：

- cron job 被创建并处于 disabled 状态。
- 手动触发后，Matrix 中出现 cron marker。
- cron 执行的 tool call、thinking 和过程消息进入 thread。
- cron 不依赖 TeamHarness 自定义调度逻辑。

## Test 26-5：CMS 观测

目标：验证 QwenPaw worker 保留 CMS / LoongSuite 观测接入。

场景：

- embedded 环境启动 QwenPaw Web Team。
- 检查 leader、dev、qa 容器的 env、启动日志和进程状态。

验收：

- 容器中存在 CMS / OTEL / LoongSuite 相关环境变量。
- QwenPaw 进程正常运行。
- 启动日志能看到观测配置或 exporter 初始化信息。
- OTLP 后端不可用、403 或网络失败不能导致 worker 退出。

## Test 26-6：钉钉 Channel

目标：验证 leader 开通 DingTalk channel 后，能把外部消息按普通回答、快速任务、项目任务三类流程处理，并把最终结果返回原始 DingTalk 会话。

前置：

- 需要 DingTalk 测试凭证。
- 凭证缺失时测试跳过，不失败。
- Team 内部协作仍然使用 Matrix；DingTalk 是外部入口和最终回复出口。

场景一：普通回答

- 从 DingTalk 问天气。
- leader 直接回答原 DingTalk 会话。

验收：

- 不创建 project。
- 不创建 task。
- 最终结果返回原 DingTalk 会话。

场景二：快速任务

- 从 DingTalk 要求写一份简短报告。
- leader 使用 quick task 流程分派给合适成员。
- 成员提交结果后，leader 汇总并回复 DingTalk。

验收：

- 任务有明确 assignee。
- 结果经过 leader 验收。
- 最终结果返回原 DingTalk 会话。

场景三：项目任务

- 从 DingTalk 要求开发一个简单网页。
- leader 进入项目流程，dev 负责开发，qa 负责检查。
- leader 验收后回复 DingTalk。

验收：

- project/task 状态推进正确。
- dev 和 qa 都参与对应步骤。
- 最终结果返回原 DingTalk 会话。
- `replyRoute` / `sourceChannel` 不丢失，不能固定回 DM 或 Team Room。

## 实施顺序

1. 先实现 26-1，只验证三人团队创建、bootstrap 和秘钥防护。
2. 26-1 稳定后，再基于同一套 helper 实现 26-2 热更新。
3. 再做 26-3 heartbeat 和 26-4 cron，重点检查 Matrix thread 行为。
4. 26-5 CMS 只做观测配置和运行态检查，不阻塞主流程。
5. 最后做 26-6 DingTalk，凭证和外部 channel 稳定后再纳入常规验证。

## 当前大 test26 的保留策略

旧的 `test-26-qwenpaw-teamharness-plugin-mode.sh` 可以暂时保留作为迁移参考。新 case 稳定后，再决定是否：

- 将旧 test26 缩小为 smoke test。
- 或删除旧 test26 中已被小 case 覆盖的重复断言。
