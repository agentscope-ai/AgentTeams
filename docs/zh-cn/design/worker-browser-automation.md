# Worker 内置浏览器自动化方案

状态：提案。本文尚未实现或验证浏览器自动化。
功能跟踪：[#1569](https://github.com/agentscope-ai/AgentTeams/issues/1569)。
[English proposal](../../design/worker-browser-automation.md)

## 一、背景与目标

目标是在正常安装 AgentTeams 后，新建 Worker 就能打开网页、读取页面、
模拟点击、输入、滚动、截图、上传和下载，不需要用户逐个进入容器安装
skill 或下载浏览器。

建议在支持的 Worker 镜像内预装固定版本的 agent-browser、Chromium 及其
依赖，通过内置发现型 skill 指导现有 shell 工具调用。浏览器在 Worker
自己的容器中运行，沿用 Worker 的网络。

首批实现目标为 Docker、Helm/Kubernetes 中的 QwenPaw、OpenClaw、Hermes。
这是后续发布的验收目标，不代表当前镜像已支持。DeepSeek Harness、
OpenHuman 必须完成各自镜像与运行时验证后再声明支持；遗留 CoPaw 不作为
新安装验收目标。

接管用户本机 Chrome、可见桌面、实时观看与人工接管、跨容器重建保留登录态，
暂不纳入首版。[#1258](https://github.com/agentscope-ai/AgentTeams/issues/1258)
跟踪的本机 Worker 诉求范围更广。

## 二、当前架构与约束

源码基线：官方 main 的 `7e9a1c73`，检查日期为 2026-11-09。

| 模块 | 已确认行为 | 对集成的影响 |
| --- | --- | --- |
| [Worker 镜像](../../../qwenpaw/Dockerfile) | QwenPaw、OpenClaw base、Hermes 的 Dockerfile 未显式安装完整 Chromium 环境 | 不能根据运行时包或工具名称认定浏览器可启动 |
| [内置模板选择](../../../agentteams-controller/internal/service/builtin_templates.go)、[分发](../../../agentteams-controller/internal/service/deployer.go) | 按角色/运行时选择 builtin 模板并写入 Worker 存储，技能目录也据此判断可用运行时 | 分发、目录展示和镜像支持范围需要一致 |
| [运行配置投影](../../../agentteams-controller/internal/service/runtime_config.go) | 将托管技能名称写入运行配置 | 加入浏览器 builtin 时不能覆盖用户分配 |
| [QwenPaw 技能应用](../../../qwenpaw/src/qwenpaw_worker/update.py) | 同步已分配技能、检查 SKILL.md，再调用 API 刷新和启用 | 仅向 Manager 镜像复制 skill 不足以完成集成 |
| [builtin 升级](../../../manager/scripts/init/upgrade-builtins.sh)与运行时文件同步 | 已有 Worker 可接收 builtin 更新，运行数据另有对象存储同步流程 | 浏览器缓存和活动 profile 不应成为普通 workspace 文件 |

以上为源码检查，未检查实际运行中的容器。传递依赖中可能包含浏览器库，
但不能据此证明可执行文件及系统库完整。

## 三、选型与推荐

| 方案 | 收益 | 代价与限制 |
| --- | --- | --- |
| 启用各运行时原生浏览器工具 | 复用已有工具 | 安装、配置和 skill 各不相同，跨运行时验收难统一 |
| 内置 agent-browser、Chromium 与发现型 skill | 一套 shell 调用流程，不增加服务 | 镜像变大，活动浏览器消耗资源，需要验证架构兼容性 |
| 独立浏览器服务或 sidecar | 生命周期独立，可扩展观看入口 | 增加网络、鉴权、会话分配和部署管理 |

首版推荐第二种方案，不增加浏览器集群、公共 API、CRD 或付费浏览器服务。
保留原生工具兼容性，内置 skill 优先指导统一 CLI，避免一个任务同时启动
两套浏览器。

## 四、推荐设计

### 1. 镜像预装与一键安装

- 先用兼容性验证确定 CLI、浏览器及依赖的确切版本，记录校验和或精确包版本，
  保留第三方许可证；Worker 启动时不使用 latest，不拉取未固定版本的 skill。
- 构建时安装浏览器系统库与中文字体。可执行文件位于 HOME、workspace 挂载之外
  的镜像固定路径，通过 `AGENT_BROWSER_EXECUTABLE_PATH` 指定。
- amd64、arm64 分别验证。当前上游
  [Chrome for Testing 下载器](https://github.com/vercel-labs/agent-browser/blob/main/cli/src/install.rs)
  没有 Linux ARM64 下载目标。可评估 Debian 发行版 Chromium 包或其他固定版本
  产物；不能静默改成模拟执行 amd64。
- 修改 QwenPaw、OpenClaw Worker、Hermes 的构建与发布链。尽量将 OpenClaw
  浏览器依赖放在 Worker 层，避免只为 Worker 任务扩大 Manager 镜像；
  调整共享 base 前先盘点所有下游消费者。
- 现有 Docker 安装器、Helm 默认镜像引用发布的已验证版本。拉取镜像后不需要
  额外浏览器安装命令、npm/pip 下载、供应商订阅或浏览器相关交互。
- 默认无界面执行，不要求 Xvfb、VNC、宿主机显示挂载、特权容器或 host IPC。

### 2. skill 分发与启用

建议将唯一维护的发现型 skill 放在
`manager/agent/worker-skills/browser-automation/SKILL.md`。
该目录当前表示可分配技能，单纯放入文件并不会变成 builtin。

实现时需要为已支持的角色/运行时显式加入默认物化与技能目录展示：
模板型运行时沿用 builtin 模板分发；QwenPaw 加入有效托管技能集合后，
沿用同步、刷新、启用流程。目录展示与实际分发必须采用同一选择规则，
不在每种模板中复制整份上游手册，也不改写用户声明的技能列表。

浏览器 skill 作为运行时 builtin 与包内/用户分配技能分别管理：
初次自动启用，后续无关 reconcile 保留显式本地禁用和其他技能。
保留 builtin 名称，遇到同名用户或包内技能时明确报冲突，不静默覆盖。
启用默认能力前需验证 AgentSpec 包替换、builtin 升级的行为。

发现型说明保持简短、使用第二人称，详细命令从已安装 CLI 读取，
例如所选版本支持时使用 `agent-browser skills get core`。
该命令必须在实际选定发布版本中验证，不能以最新版在线文档代替。

skill 指导 Worker：

1. 为任务建立独立会话并打开指定 URL。
2. 获取交互元素快照，使用当前引用执行点击或输入。
3. 导航或页面变化后重新获取快照，验证实际结果。
4. 必要时使用截图与坐标交互；依靠截图理解页面还需模型和运行时支持图像。
5. 仅将用户需要的产物复制到已有任务共享目录。
6. 完成或失败后关闭会话；遇到阻塞如实报告，不把发出点击当成成功。

### 3. 执行、存储与清理

执行路径为：

```text
Human 经 Matrix 下发任务
  → Worker 运行时读取内置浏览器 skill
  → 现有 shell 工具调用 agent-browser
  → 同一 Worker 容器内的 Chromium 访问目标网站
  → 指定截图/下载产物进入已有任务文件共享
```

以下路径为提议，尚未成为现有契约：

| 数据 | 建议位置与生命周期 |
| --- | --- |
| 浏览器、系统库、字体、CLI 参考说明 | 镜像固定路径，位于同步 HOME 之外 |
| profile、Cookie、缓存、socket、daemon 状态、临时下载 | /tmp/agentteams-browser 下的私有 Worker/任务目录，临时保存 |
| 明确需要交付的截图和下载文件 | 显式复制到已有任务输出/共享目录 |
| 登录态导出 | 默认不开启，不将完整 profile 同步到 MinIO/OSS |

按 Worker 和任务生成唯一会话，同一任务的多次 shell 调用复用该会话。
命名会话属于逻辑隔离，不能防御同一 OS 身份下其他进程的读取；
跨 Worker 隔离仍依赖容器和存储权限。

必须使用选定 CLI 支持的参数设置所有运行目录，包括默认写入 HOME 的状态。
验收时检查真实文件落点，仅移动 Chromium profile 不够；不全局改写运行时
HOME。若所选 CLI 无法将敏感状态放在同步目录之外，应在发布前解决。

任务结束关闭会话，设置有限的 daemon 空闲超时，Worker 退出时清理其拥有
的浏览器进程。活动 profile 数据库及锁文件不做文件同步。容器重建后使用
新浏览器状态，后续再独立设计私有持久化能力。

### 4. 权限、故障与升级

浏览器沿用 Worker 的网络与工具授权，不自动继承用户桌面或浏览器账号。
调试、流式预览监听保持私有，不增加宿主机/Ingress 暴露端口；
已有出口限制继续生效，网页内容不能授权新增工具或改变 Worker 权限。

在真实镜像用户与 Kubernetes security context 下验证 Chromium 沙箱。
优先采用非特权、兼容沙箱的启动方式，不静默增加权限、宿主机挂载或统一
关闭沙箱。具体启动策略在镜像兼容性阶段形成有证据的发布决定。

缺失可执行文件、系统库、启动失败、超时均返回明确浏览器诊断，不运行时
安装其他版本，不导致整个 Worker 退出，也不把浏览器失败标成任务成功。

CLI、浏览器、发现型 skill 作为经过验证的一组版本升级。更新 skill 文本
不会给旧容器安装新浏览器。回滚使用上一组镜像与 skill，保留任务产物，
不因回滚重新启用用户已禁用的能力。

## 五、开发与验收计划

| 阶段 | 交付内容 | 验收证据 |
| --- | --- | --- |
| 兼容性验证 | 精确版本、各架构浏览器来源、启动策略 | 原生 amd64/arm64，在真实 HOME/挂载/安全上下文下启动；记录镜像体积、冷启动、峰值内存 |
| 镜像打包 | 依赖、字体、构建和发布引用 | 拉取镜像后无需额外包/浏览器下载，本地受控网页测试通过 |
| skill 集成 | 单一来源、默认物化与目录映射、QwenPaw 启用 | 新建和升级 Worker 可发现技能，QwenPaw API 确认启用；其他技能和显式禁用在 reconcile 后保留 |
| 网页操作 | 确定性本地测试网页及运行时测试 | 打开、读取、点击、输入、滚动、中文截图、上传、下载，并断言真实结果 |
| 生命周期与隔离 | 独立会话、私有目录与清理 | 并发 Worker/任务不串 Cookie、页面或文件；超时、崩溃、空闲回收和退出释放进程；对象存储没有 profile/缓存 |
| 部署验收 | Docker、Helm 新装和升级 | 三种首批运行时均覆盖两种架构，区分 shell smoke 与真实运行时从 Matrix 接收任务的端到端测试 |

根据代表性并发实测 /dev/shm、CPU、内存后确定资源建议，不提前承诺固定
内存预算。依赖预装，浏览器进程按需启动。

任何运行时/架构未通过浏览器测试，都不能宣称已支持。镜像构建成功、
CLI --version 或 SKILL.md 存在均不足以验收。实现 PR 应补充确定性的
回归与集成测试；本方案 PR 仅修改文档。

兼容性阶段仍需确认三项影响实现的决定：

- CLI/浏览器发布版本组合与原生 ARM64 产物来源。
- 沙箱兼容的执行身份及实测 CPU、内存、共享内存需求。
- 保留显式禁用、并使目录展示与真实镜像能力一致的最小运行时改动。

## 六、参考资料

- [agent-browser 安装与自定义可执行文件](https://agent-browser.dev/installation)
- [与 CLI 版本匹配的技能说明](https://agent-browser.dev/skills)
- [会话模型](https://agent-browser.dev/sessions)
- [配置](https://agent-browser.dev/configuration)
- [成员运行配置契约](../../design/member-runtime-config-contract.md)
- [团队技能目录与物化契约](../../design/team-skills.md)
