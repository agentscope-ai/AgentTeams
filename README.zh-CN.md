<a name="readme-top"></a>
<h1 align="center">
    <img src="https://img.alicdn.com/imgextra/i2/O1CN01hTYQMO28B3H9qP7RV_!!6000000007893-2-tps-1490-392.png" alt="HiClaw"  width="290" height="72.5">

<p align="center">
  <a href="https://deepwiki.com/higress-group/hiclaw"><img src="https://img.shields.io/badge/DeepWiki-Ask_AI-navy.svg?logo=data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACwAAAAyCAYAAAAnWDnqAAAAAXNSR0IArs4c6QAAA05JREFUaEPtmUtyEzEQhtWTQyQLHNak2AB7ZnyXZMEjXMGeK/AIi+QuHrMnbChYY7MIh8g01fJoopFb0uhhEqqcbWTp06/uv1saEDv4O3n3dV60RfP947Mm9/SQc0ICFQgzfc4CYZoTPAswgSJCCUJUnAAoRHOAUOcATwbmVLWdGoH//PB8mnKqScAhsD0kYP3j/Yt5LPQe2KvcXmGvRHcDnpxfL2zOYJ1mFwrryWTz0advv1Ut4CJgf5uhDuDj5eUcAUoahrdY/56ebRWeraTjMt/00Sh3UDtjgHtQNHwcRGOC98BJEAEymycmYcWwOprTgcB6VZ5JK5TAJ+fXGLBm3FDAmn6oPPjR4rKCAoJCal2eAiQp2x0vxTPB3ALO2CRkwmDy5WohzBDwSEFKRwPbknEggCPB/imwrycgxX2NzoMCHhPkDwqYMr9tRcP5qNrMZHkVnOjRMWwLCcr8ohBVb1OMjxLwGCvjTikrsBOiA6fNyCrm8V1rP93iVPpwaE+gO0SsWmPiXB+jikdf6SizrT5qKasx5j8ABbHpFTx+vFXp9EnYQmLx02h1QTTrl6eDqxLnGjporxl3NL3agEvXdT0WmEost648sQOYAeJS9Q7bfUVoMGnjo4AZdUMQku50McDcMWcBPvr0SzbTAFDfvJqwLzgxwATnCgnp4wDl6Aa+Ax283gghmj+vj7feE2KBBRMW3FzOpLOADl0Isb5587h/U4gGvkt5v60Z1VLG8BhYjbzRwyQZemwAd6cCR5/XFWLYZRIMpX39AR0tjaGGiGzLVyhse5C9RKC6ai42ppWPKiBagOvaYk8lO7DajerabOZP46Lby5wKjw1HCRx7p9sVMOWGzb/vA1hwiWc6jm3MvQDTogQkiqIhJV0nBQBTU+3okKCFDy9WwferkHjtxib7t3xIUQtHxnIwtx4mpg26/HfwVNVDb4oI9RHmx5WGelRVlrtiw43zboCLaxv46AZeB3IlTkwouebTr1y2NjSpHz68WNFjHvupy3q8TFn3Hos2IAk4Ju5dCo8B3wP7VPr/FGaKiG+T+v+TQqIrOqMTL1VdWV1DdmcbO8KXBz6esmYWYKPwDL5b5FA1a0hwapHiom0r/cKaoqr+27/XcrS5UwSMbQAAAABJRU5ErkJggg==" alt="DeepWiki"></a>
  <a href="https://discord.com/invite/NVjNA4BAVw"><img src="https://img.shields.io/badge/Discord-Join_Us-blueviolet.svg?logo=discord" alt="Discord"></a>
  <a href="https://qr.dingtalk.com/action/joingroup?code=v1,k1,MF0nEpuU3YkW2aBsoyJE0mUM3LFDSBqMGvRmTIjUQNk=&_dt_no_comment=1&origin=11?"><img src="https://img.shields.io/badge/DingTalk-Join_Us-orange.svg" alt="DingTalk"></a>
</p>

</h1>

[English](./README.md) | [中文](./README.zh-CN.md) | [日本語](./README.ja-JP.md)

**HiClaw 是一个开源的协作式多智能体操作系统。让多个 Agent 在 Matrix 房间中协作，人类全程可见、随时可介入。 设计了 Manager - Workers 协作架构，人类可通过 Manager Agent 可协调多个 Worker Agents，完成复杂任务，从而加速实现 OPOC（一人一公司）和企业数字员工。**

HiClaw 并不和其他 xxClaw 对标，非龙虾本虾，而是一个多 Agent 操作系统。
- 🧑‍💻 **设计了 Manger-Workers 架构**：不用真人去管理每个干活的 Worker Claw，实现由 Agent 管理 Agents。
- 🦞 **每个 Agent 支持自定义**：OpenClaw、Copaw、NanoClaw、ZeroClaw 以及企业自建的 Agent，从养虾到开虾场。
- 📚 **引入 MinIO 共享文件系统**：用于 Agent 之间的信息共享，大幅降低多 Agent 协作带来的 Token 消耗。
- ⛑️ **引入 Higress AI Gateway**：流量入口和各类凭证风险降低了，减少了用户对原生龙虾在安全上的顾虑。
- 🎨 **使用 Element IM 客户端+Tuwunel IM 服务器（均基于 Matrix 实时通信协议）**：节省钉钉、飞书 IM 的接入和企业内的审批成本，方便用户快速体验在 IM 的交互环境中体验模型服务的"爽感"，同时支持以 OpenClaw 原生的方式接入 IM。

![架构](https://img.alicdn.com/imgextra/i4/O1CN01c1VlDE1zYZ46EW3OA_!!6000000006726-49-tps-9895-8231.webp)

## 动态
- **2026-03-14:** HiClaw 1.0.6 发布，企业级 MCP Server 管理——凭证零暴露，工具全接入。Worker 可通过 Higress AI Gateway 安全使用任意 MCP 工具。了解[更多](blog/zh-cn/hiclaw-1.0.6-release.md)。
- **2026-03-10:** HiClaw 1.0.4 发布，支持 CoPaw Worker——内存占用降低 80%，新增本地模式可操作浏览器。了解[更多](blog/zh-cn/hiclaw-1.0.4-release.md)。
- **2026-03-04:** HiClaw 开源，引入 Manager Agent 角色，构建企业级多 Agent 协同平台。了解[更多](blog/zh-cn/hiclaw-announcement.md)。

## 为什么选 HiClaw

- **企业级安全**：Worker 永远不持有真实的 API Key 或 GitHub PAT，只有一个消费者令牌（类似"工牌"）。即使 Worker 被攻击，攻击者也拿不到任何真实凭证。
- **多 Agent 群聊网络**：Manager Agent 智能分解任务，协调多个 Worker Agent 并行执行，大幅提升复杂任务处理能力。
- **Matrix 协议驱动**：基于开放的 Matrix IM 协议，所有 Agent 通信透明可审计，天然支持分布式部署和联邦通信。
- **人工全程监督**：人类可随时进入任意 Matrix 房间观察 Agent 对话，实时干预或修正 Agent 行为，确保安全可控。
- **真正开箱即用的 IM**：内置 Matrix 服务器，不需要申请飞书/钉钉机器人，不需要等待审批。浏览器打开 Element Web 就能对话，或者用手机上的 Matrix 客户端（Element、FluffyChat）随时指挥，iOS、Android、Web 全平台支持。
- **Manager-Worker 架构**：清晰的 Manager-Worker 两层架构，职责分明，易于扩展自定义 Worker Agent 以适应不同场景，支持纳管 Copaw、NanoClaw、ZeroClaw 或是企业自建的 Agent

- **一条命令启动**：一个 `curl | bash` 搞定所有组件 — Higress AI 网关、Matrix 服务器、文件存储、Web 客户端和 Manager Agent 本身。

- **技能生态**：Worker 可以按需从 [skills.sh](https://skills.sh) 获取技能（社区已有 80,000+ 个）。因为 Worker 本身就拿不到真实凭证，所以可以放心使用公开技能库。

## 快速开始
**前置条件**：Docker Desktop（Windows/macOS）或 Docker Engine（Linux）。若在 ECS 或云桌面等虚拟机上部署，请采用 Linux 系统，图形化需求，请使用 Ubuntu，官方镜像包暂不支持虚拟机上的 Window 系统，原因是虚拟机上的 Window 系统不是 Linux Container。

- [Docker Desktop](https://www.docker.com/products/docker-desktop/)（Windows / macOS）
- [Docker Engine](https://docs.docker.com/engine/install/)（Linux）或 [Podman Desktop](https://podman-desktop.io/)（替代方案）

**资源需求**：最低 2C4GB 内存。如果希望部署较多 Worker 体验更强大的 Agent Teams 能力，建议 4C8GB 内存。目前 OpenClaw 内存占用较高。Docker Desktop 用户可在 Settings → Resources 中调整。

![资源](https://img.alicdn.com/imgextra/i4/O1CN01c8qOlx1hPiKMjzGZQ_!!6000000004270-0-tps-2496-690.jpg)

安装步骤：
以下我们以最简单的本地部署、本地访问来演示安装步骤，不到5分钟就能开始玩龙虾了。

第一步：打开终端，Mac 系统输入以下安装命令。

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

**Windows（支持 PowerShell 5+）输入以下安装命令：**

```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; iex $wc.DownloadString('https://higress.ai/hiclaw/install.ps1')
```

这里，输入 Mac 系统的安装命令。

第二步：选择语言，选择中文。

第三步：选择安装模式，快速开始请选择阿里云百炼快速安装。您也可以选择其他模型服务，手动配置。

第四步：选择大模型服务商。选择百炼，您也可以接入其他支持 OpenAPI 协议的模型服务，目前 Anthropic 协议还未支持，排期中。

第五步：选择模型接口。百炼 Coding Plan 和百炼通用接口有所不同，这里我们选择 Coding Plan 接口。[购买Coding Plan](https://bailian.console.aliyun.com/cn-beijing/?source_channel=4qjGAvs1Pl&tab=coding-plan#/efm/index)

第六步：选择模型系列。如果第五步中选择的是百炼 Coding Plan，您可以选择 qwen3.5-plus、GLM等，待 Matrix room 建立起来后，还可通过发送指令，让 Manager 切换其他到模型。

第七步：开始测试 API 联通性，若测试成功，效果如下。
![测试](https://img.alicdn.com/imgextra/i4/O1CN0148wFGG1lYeWKd3Uat_!!6000000004831-2-tps-1752-600.png)

若测试不成功，您需要检查模粘贴的型 API Key是否完整或无空格，若再次尝试仍无法通过，建议像模型服务厂商提交服务工单。

第八步：选择网络访问模式。这里我们选择仅本机使用，若允许外部访问，例如和同事建立 Matrix roon，则选择允许外部访问。选择后，按回车键即可，确定端口号、网关主机端口、Higress 控制台主机端口、Maxtrix 域名、Element Web 直接访问的主机端口、文件系统域名等，均采用默认值，无须手动配置。

第九步：GitHub 集成、Skills 注册中心、数据持久化、Docker 卷、Manager 工作空间，按回车键即可，均采用默认配置，无须手动配置。

第十步：选择 Manager Worker 运行时，目前支持 OpenClaw 和 Copaw，未来还将支持 NanoClaw、ZeroClaw 等。

第十一步：等待安装。安装完成。登录密码是自动生成的。

若希望通过移动端来访问和使用，则需要使用美区账号下载 FluffyChat/Element Mobile。（之所以采用这两个 IM，是因为他们是支持 Matrix 协议的）下载后，连接您的 Matrix 服务器地址，就能随时随地管理您的 Agent 团队。
![测试](https://img.alicdn.com/imgextra/i3/O1CN01Tl4T8q29HIHtPVSJL_!!6000000008042-2-tps-2372-1282.png)

第十二步：浏览器中，输入 http://127.0.0.1:18088/#/login，登录 Element，输入用户名和密码，就可以玩龙虾了，告诉 Manager 创建 Worker 并分配任务。
![登录](https://img.alicdn.com/imgextra/i1/O1CN01C5NvV41P6msPuucrs_!!6000000001792-2-tps-2748-1224.png)

⚠️ **注意：HiClaw 内置了 Higress AI 网关，负责模型 API Key 管理以及入口流量的安全管控。模型 API Key 的切换、新增，以及路由、域名、证书管理，均可在 Higress 控制台管理。**
![网关](https://img.alicdn.com/imgextra/i3/O1CN01dNJz4x1yJcWjHGuVj_!!6000000006558-0-tps-1596-180.jpg)

## 升级

每次更新新版本，您在终端执行以下命令，即可原地升级，默认升级到最新版本：

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```
就地升级，数据和配置会保留；全新重新，会删除所有数据。

若要升级到指定版本，请使用以下命令：

```bash
HICLAW_VERSION=v1.0.5 bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```


## 工作方式

### Manager 是你的 AI 管家

Manager 通过自然语言完成 Worker 的全生命周期管理：

```
你：帮我创建一个名为 alice 的前端 Worker

Manager：好的，Worker alice 已创建。
         房间：Worker: Alice
         可以直接在房间里给 alice 分配任务了。

你：@alice 帮我用 React 实现一个登录页面

Alice：收到，正在处理……[几分钟后]
       完成了！PR 已提交：https://github.com/xxx/pull/1
```

<p align="center">
  <img src="https://img.alicdn.com/imgextra/i3/O1CN01Kvz9CF1l8XwU7izC9_!!6000000004774-0-tps-589-1280.jpg" width="240" />
  &nbsp;&nbsp;&nbsp;&nbsp;
  <img src="https://img.alicdn.com/imgextra/i2/O1CN01lifZMs1h7qscHxCsH_!!6000000004231-0-tps-589-1280.jpg" width="240" />
</p>
<p align="center">
  <sub>① Manager 创建 Worker，分配任务</sub>
  &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;
  <sub>② 人类也可以直接在房间里指挥 Worker</sub>
</p>

Manager 还会定期发送心跳检查--如果某个 Worker 卡住了，它会自动提醒你。

### 安全模型

```
Worker（只持有消费者令牌）
    → Higress AI 网关（持有真实 API Key、GitHub PAT）
        → LLM API / GitHub API / MCP Server
```

Worker 只能看到自己的消费者令牌。网关统一管理所有真实凭证。Manager 知道 Worker 在做什么，但同样接触不到真实的 Key。

### 人工全程监督

每个 Matrix 房间里都有你、Manager 和相关 Worker。你可以随时跳进来：

```
你：@bob 等一下，密码规则改成至少 8 位
Bob：好的，已修改。
Alice：前端校验也更新了。
```

没有黑盒，没有隐藏的 Agent 间调用。

## HiClaw vs OpenClaw 原生

| | OpenClaw 原生 | HiClaw |
|---|---|---|
| 部署方式 | 单进程 | 分布式容器 |
| Agent 创建 | 手动配置 + 重启 | 对话式 |
| 凭证管理 | 每个 Agent 持有真实 Key | Worker 只持有消费者令牌 |
| 人工可见性 | 可选 | 内置（Matrix 房间） |
| 移动端访问 | 取决于渠道配置 | 任意 Matrix 客户端，零配置 |
| 监控 | 无 | Manager 心跳，房间内可见 |

## 架构

```
┌─────────────────────────────────────────────┐
│         hiclaw-manager-agent                │
│  Higress │ Tuwunel │ MinIO │ Element Web    │
│  Manager Agent (OpenClaw)                   │
└──────────────────┬──────────────────────────┘
                   │ Matrix + HTTP Files
┌──────────────────┴──────┐  ┌────────────────┐
│  hiclaw-worker-agent    │  │  hiclaw-worker │
│  Worker Alice (OpenClaw)│  │  Worker Bob    │
└─────────────────────────┘  └────────────────┘
```

| 组件 | 职责 |
|------|------|
| Higress AI 网关 | LLM 代理、MCP Server 托管、凭证集中管理 |
| Tuwunel（Matrix） | 所有 Agent 与人类通信的 IM 服务器 |
| Element Web | 浏览器客户端，零配置 |
| MinIO | 集中式文件存储，Worker 无状态 |
| OpenClaw | 带 Matrix 插件和技能系统的 Agent 运行时 |

## 常见问题

如果 Manager 容器启动失败，执行以下命令查看具体原因：

```bash
docker exec -it hiclaw-manager cat /var/log/hiclaw/manager-agent.log
```

更多常见问题（启动超时、局域网访问等）参见 [docs/zh-cn/faq.md](docs/zh-cn/faq.md)。

### 提交 Bug

提交 Issue 前，建议先导出 Matrix 消息记录，用 AI 工具结合代码库分析问题根因，这能大幅加快修复速度。

```bash
# 导出调试日志（Matrix 消息 + Agent 会话日志，PII 自动脱敏）
python scripts/export-debug-log.py --range 1h
```

然后在 Cursor、Claude Code 等 AI 工具中打开 HiClaw 仓库，让它分析：

> "读取 debug-log/ 下的 JSONL 文件，同时分析 Matrix 消息日志和 Agent 会话日志。结合 HiClaw 代码库，定位 [描述你的 bug] 的根因。重点关注 Agent 交互流程、工具调用失败和错误模式。"

将 AI 的分析结果贴到 [Bug Report](https://github.com/alibaba/hiclaw/issues/new?template=bug_report.yml) 中。

你也可以让 AI 工具直接提交 Issue 或 PR。先安装 [GitHub CLI](https://cli.github.com/)，执行 `gh auth login` 在浏览器中完成登录，然后将 [OpenClaw GitHub skill](https://github.com/openclaw/openclaw/blob/main/skills/github/SKILL.md) 配置到你的 AI 编程工具（Cursor、Claude Code 等）中。之后直接让它根据分析结果提交 Issue 或 PR 即可。

欢迎[提交 Issue](https://github.com/alibaba/hiclaw/issues)，或在 [Discord](https://discord.gg/n6mV8xEYUF) / 钉钉群里随时提问。

## Roadmap

### ✅ 已发布

- ~~**CoPaw** -- 轻量级 Agent 运行时~~ [已在 1.0.4 发布](blog/zh-cn/hiclaw-1.0.4-release.md)：Docker 模式内存占用约 150MB（对比 OpenClaw 的 500MB），还支持本地模式可操作浏览器、访问本地文件。
- ~~**通用 MCP 服务支持** -- MCP 服务集成~~ [已在 1.0.6 发布](blog/zh-cn/hiclaw-1.0.6-release.md)：任意 MCP 服务可安全暴露给 Worker，Worker 仅使用 Higress 签发的 token，真实凭证零泄露。

### 进行中

#### 轻量级 Worker 运行时

- **ZeroClaw** -- 基于 Rust 的超轻量运行时，3.4MB 二进制，冷启动 <10ms，专为边缘和资源受限环境设计。
- **NanoClaw** -- 极简 OpenClaw 替代品，<4000 行代码，基于容器隔离，使用 Anthropic Agents SDK。

目标：将单 Worker 内存占用从 ~500MB 降至 <100MB，在相同硬件上支持更多 Worker。

### 计划中

#### Team 管理中心

开箱即用的可视化控制台，用于观察和管控整个 Agent Team：

- **实时观测**：每个 Agent 的工作过程细节可视化（对话、工具调用、思考过程）
- **主动打断**：发现问题时可随时打断指定 Agent 的工作，接管或调整方向
- **任务时间线**：谁在什么时候做了什么，完整历史记录
- **资源监控**：每个 Worker 的 CPU/内存使用情况

目标：让 Agent Teams 像人类团队一样透明可控--没有黑盒。

---

## 文档

| | |
|---|---|
| [docs/zh-cn/quickstart.md](docs/zh-cn/quickstart.md) | 端到端快速入门，含验证检查点 |
| [docs/zh-cn/architecture.md](docs/zh-cn/architecture.md) | 系统架构详解 |
| [docs/zh-cn/manager-guide.md](docs/zh-cn/manager-guide.md) | Manager 配置与使用 |
| [docs/zh-cn/worker-guide.md](docs/zh-cn/worker-guide.md) | Worker 部署与故障排查 |
| [docs/zh-cn/development.md](docs/zh-cn/development.md) | 贡献指南与本地开发 |
| [docs/zh-cn/faq.md](docs/zh-cn/faq.md) | 常见问题 |

## 构建与测试

```bash
make build               # 构建所有镜像
make test                # 构建 + 运行全部集成测试
make test SKIP_BUILD=1   # 不重新构建，直接运行测试
make test-quick          # 快速冒烟测试（仅 test-01）
```

## 其他命令

```bash
# 通过 CLI 向 Manager 发送任务
make replay TASK="创建一个名为 alice 的前端开发 Worker"

# 卸载所有内容
make uninstall

# 推送多架构镜像
make push VERSION=0.1.0 REGISTRY=ghcr.io REPO=higress-group/hiclaw

make help  # 查看所有可用目标
```

## 社区

- [Discord](https://discord.gg/NVjNA4BAVw)
- [钉钉群](https://qr.dingtalk.com/action/joingroup?code=v1,k1,5K+D/m2s71QW2aBsoyJE0t2oQOMCk2yngAgkih4LyQM=&_dt_no_comment=1&origin=11)
- 微信群--扫码加入：

<p align="center">
  <img src="https://img.alicdn.com/imgextra/i2/O1CN01NSgKhD1Mhw9tr9nhv_!!6000000001467-2-tps-784-744.png" width="200" alt="微信群" />
</p>

## 许可证

Apache License 2.0
