# Worker 可选浏览器 skill 方案

状态：提案，附隔离容器 POC。
功能跟踪：[#1569](https://github.com/agentscope-ai/AgentTeams/issues/1569)。
[English proposal](../../design/worker-browser-automation.md)

## 一、结论与目标

在技能库中预置一个官方浏览器 skill，**默认不分配给所有 Worker**。
创建 Worker 时，用户可以选择它、选择其他浏览器 skill，也可以不启用
浏览器能力。skill 更新不要求重新发布整个核心 Worker 镜像。

官方候选包包含简短说明和固定版本 CLI 的安装脚本。依赖安装只发生在选中
的 Worker 执行环境内；Chromium 属于环境依赖，不塞进 skill ZIP。
浏览器沿用该 Worker 容器与网络。

本方案替代最初“所有支持的 Worker 自动启用浏览器”的提议。
“官方技能库预置”与“每个 Worker 强制内置”是两个不同决定。

后续首批验收目标为 Docker、Kubernetes 下的 QwenPaw、OpenClaw、Hermes，
须逐一验证。遗留 CoPaw 不作为新安装目标，DeepSeek Harness/OpenHuman
单独适配。接管本机 Chrome、可见桌面、跨容器重建保留登录态、独立浏览器
集群暂不纳入首版。

## 二、现有流程与缺口

源码基线：官方 main 的 `7e9a1c73`，检查日期为 2026-11-09。

| 当前源码 | 已确认行为 | 方案影响 |
| --- | --- | --- |
| [Worker 技能指南](../usage/worker-guide.md) | 创建/编辑表单可选市场或 Nacos skill，分发后更新 Worker.spec.skills | 复用现有选择入口 |
| [安全导入器](../../../manager/agent/skills/worker-management/scripts/safe-extract-worker-skill.py) | 检查单一 skill 根目录的 ZIP，保留其附带文件 | 说明与安装/执行脚本可以一起打包 |
| [配置投影](../../../agentteams-controller/internal/service/runtime_config.go)、[QwenPaw 技能应用](../../../qwenpaw/src/qwenpaw_worker/update.py) | 同步已分配技能并刷新、启用 | 保留现有激活流程与用户选择 |
| [builtin 选择](../../../agentteams-controller/internal/service/builtin_templates.go) | 必选模板与目录展示共用选择规则 | 不将浏览器加入每个 Worker 的必选模板 |
| [QwenPaw 镜像](../../../qwenpaw/Dockerfile) | 未显式预装 Chromium、agent-browser | 仅分发 skill 文件不能保证浏览器可用 |

**当前上传/分配流程没有系统依赖安装生命周期。** ZIP 上传、技能选择、
API 启用均不能等同于浏览器安装成功。POC 显式调用安装脚本；
“创建时选完自动安装”还需要后续平台集成。

## 三、交付方式取舍

| 方式 | 收益 | 代价 |
| --- | --- | --- |
| 所有 Worker 镜像预装浏览器依赖 | 拉取后首用快，便于离线使用 | 镜像变大，工具版本变更需重建相关镜像 |
| 可选 skill 与环境安装步骤 | 每个 Worker 独立选择，skill/CLI 可独立更新 | 首次下载、安装权限、容器重建恢复需要处理 |
| 可选 skill 配合预装环境镜像或已有浏览器端点 | 适合离线、只读或远端浏览器 | 需要兼容镜像/端点及其独立凭据和生命周期 |

优先验证第二种，无法本地安装时允许第三种。沿用普通 skill 包，不增加
浏览器 provider CRD。其他浏览器 skill 说明各自的环境要求；
用户选择其他方案时，不自动同时启用官方候选包。

## 四、选中 Worker 后的安装与升级

1. 将固定版本官方包发布到现有技能库。建议源码位于
   `manager/agent/worker-skills/`，而不是必选 `worker-agent/skills/`；
   不添加会根据角色静默分配浏览器的自动匹配规则。
2. 用户在现有创建表单明确勾选 skill，写入 Worker.spec.skills，
   不额外合入浏览器默认项。
3. 沿用当前上传、分配顺序物化选中的包。
4. 为经过审核的官方包增加有限的环境安装步骤：核对身份/版本，只在目标
   Worker 内安装依赖并执行启动探测，沿用现有执行权限边界。
   **这一步尚未实现**，不能因任意上传 ZIP 有 scripts 目录就自动执行它。
5. 经运行时适配器刷新/加载技能，分别报告文件分发、依赖就绪、运行时启用
   的结果。安装失败可重试，界面不得显示浏览器已就绪。
6. 容器重建时检查并恢复已选依赖版本；skill ZIP 缓存不等于 Chromium 缓存。

未选择的 Worker 不承担安装成本。更新 skill 文本不会替换已运行的工具：
先安装/探测新版本，在空闲时关闭旧会话，再激活。失败保留上一组可用版本，
并明确报错。移除 skill 后停止后续使用并关闭其会话，不自动删除其他技能
可能共用的系统库。

对于只读文件系统、无系统包安装权限的非 root 镜像或离线环境，使用已经
准备好的兼容镜像/端点，或明确报告环境要求。不自动提权或改装宿主机。
POC 的 root Debian Docker 安装器不代表这些环境的生产支持。

## 五、skill 与浏览器执行约定

候选包包含 SKILL.md 和显式安装/执行脚本。说明使用第二人称，列出支持的
运行时、OS、架构和实际安装成本。

候选工具为固定版本 agent-browser 与本地 Chromium。上游 Chrome for
Testing 下载器没有 Linux ARM64 目标，POC 评估 Debian Chromium；
正式发布仍需确定包版本及镜像/缓存策略。以安装版本的真实 --help 为准，
不把最新版在线文档当成该 npm 版本的契约。

任务步骤为“打开 → 快照 → 点击/输入 → 新快照 → 验证”。
必要时使用截图、坐标；视觉推理需要模型与运行时支持图像。网页内容不能
授权修改工具或权限，不为同一任务隐式启用另一套浏览器 skill。

每个 Worker/任务使用独立会话。profile、缓存、socket、临时下载位于同步
HOME/workspace 之外，保持 Worker 的 HOME 不变；只把需要的产物复制到
已有任务共享目录。默认登录态临时保存，不同步完整 profile 到 MinIO/OSS。
会话名称属于逻辑隔离，跨 Worker 仍靠容器与存储权限隔离。

浏览器按需启动，任务结束/失败关闭，daemon 有空闲超时，Worker 退出释放
其拥有的进程。在真实用户与 security context 下验证沙箱，不为安装默认
增加特权、宿主挂载、host IPC 或公开调试/流式端口。

## 六、POC 与验收

[可复现 POC](../../../examples/worker-browser-skill-poc/README.md)
将候选 skill 打包，在已发布 QwenPaw Worker 镜像启动的临时容器中执行，
不修改基础镜像和已有运行中的 Worker。

| 检查项 | 所需证据 |
| --- | --- |
| 按选择安装 | 原始 Worker 无浏览器依赖，仅临时选中环境安装 |
| 包格式 | ZIP 通过现有安全导入器 |
| 网页交互 | 本地页面打开、快照、点击/输入并验证、滚动、中文截图、上传下载 |
| 会话行为 | 两个逻辑任务不共享 Cookie，关闭后释放浏览器进程 |
| 状态落点 | 实际检查文件，profile/缓存/socket 不进入同步 HOME |
| 安装成本 | 下载包、安装体积、首次启动和操作耗时 |
| 后续产品集成 | 真实创建选择 → 可信安装 → 运行时发现 → Matrix 任务，覆盖失败/重试和重建 |
| 后续兼容性 | Docker/Kubernetes、amd64/arm64、三种首批运行时，验证沙箱与资源限制 |

QwenPaw 镜像衍生容器中的 CLI 成功，不等于 QwenPaw Agent 已加载 skill
并通过 Matrix 完成任务。一个容器中的两个会话也不等于两个独立 Worker。
POC 报告必须保留这些证据边界。

## 七、实施顺序

1. 完成并记录隔离容器 POC，保留失败和成本，确定实际支持的版本组合。
2. 发布可选包，保持现有技能选择和目录语义。
3. 为选中的 Worker 接入经过审核的安装、探测、重试、重建恢复及版本切换。
4. 验证实际运行时激活与任务，补充未选择、安装失败、移除、回滚和隔离测试。
5. 有对应证据后再更新发布默认值与用户指南。

功能 #1569 保持打开，直至实现与验收完成。

## 八、参考资料

- [安装及自定义浏览器](https://agent-browser.dev/installation)
- [CLI skill](https://agent-browser.dev/skills)
- [配置](https://agent-browser.dev/configuration)
- [会话模型](https://agent-browser.dev/sessions)
- [技能目录与物化](../../design/team-skills.md)
- [本机 Worker 诉求 #1258](https://github.com/agentscope-ai/AgentTeams/issues/1258)
