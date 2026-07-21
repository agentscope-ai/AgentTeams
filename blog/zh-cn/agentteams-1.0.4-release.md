# HiClaw 1.0.4：轻量级 CoPaw Worker 来了，内存大幅降低，还能直接操作你的本地环境

> 发布日期：2026 年 3 月 10 日

---

## 当我们在谈论 "轻量级 Worker" 时，我们在谈论什么？

如果你用过 HiClaw，可能已经熟悉了 Manager + Worker 的多 Agent 协作模式。一个 Manager 作为"AI 管家"，管理着多个专业化的 Worker —— 前端开发、后端开发、数据分析……

但在实际使用中，我们也收到了不少反馈：

**"每个 Worker 都要跑一个完整容器，内存压力有点大"** —— 默认的 OpenClaw Worker 容器大概占用 500MB 内存，如果你需要同时跑 4-5 个 Worker，一台 8GB 内存的服务器就有点吃紧了。

**"Worker 跑在容器里，没法访问我的本地环境"** —— 有些任务需要操作浏览器、访问本地文件系统、运行桌面应用……这些在容器隔离环境下都做不到。

在 1.0.4 版本，我们给出了答案：**CoPaw Worker**。

---

## CoPaw 是什么？

[CoPaw](https://github.com/agentscope-ai/CoPaw) 是一个基于 Python 的轻量级 AI Agent 运行时，核心特点是：

- **轻量级**：基于 Python，不需要 Node.js 全家桶，内存占用只有 OpenClaw Worker 的 1/5
- **控制台友好**：内置 Web 控制台，可以实时查看工具调用、Thinking 输出、执行过程
- **执行速度快**：Python 原生启动，冷启动时间短
- **扩展方便**：基于 OpenAI SDK 的工具定义，上手成本低

HiClaw 1.0.4 通过实现 Matrix Channel 和配置桥接层，将 CoPaw 接入了 HiClaw 的多 Agent 协作体系。代码量不大，但解锁了很多新的可能性。

---

## Manager-Worker 架构：大幅降低接入新 Agent 的复杂度

CoPaw Worker 的成功接入，充分体现了 HiClaw 的 Manager-Worker 架构在**降低新 Agent 接入成本**方面的优势。

### 传统方式的痛点

如果你想让一个新的 Agent 运行时（比如 CoPaw）接入用户，传统方式需要：

1. **支持完整的 Channel 生态**：OpenClaw 支持十几种消息渠道 —— Discord、Telegram、Slack、飞书、钉钉、微信、iMessage……每个渠道都有不同的 API、认证方式、消息格式
2. **实现各种 Channel 适配器**：需要逐个开发、测试、维护
3. **用户需要逐个配置**：每个渠道的 webhook、token、证书……
4. **客户端生态碎片化**：不同渠道有不同的客户端，用户体验不一致

这是一个巨大的工程量，很多优秀的 Agent 运行时就是因为这个门槛太高，无法触达用户。

### HiClaw 的解法：Matrix 作为统一通信层

HiClaw 的 Manager-Worker 架构，把通信层统一到了 Matrix 协议上：

```
┌─────────────────────────────────────────────────────────────────┐
│                        HiClaw Manager                            │
│                                                                 │
│   ┌─────────────────────────────────────────────────────────┐  │
│   │              Tuwunel Matrix Server                       │  │
│   │                  (内置，开箱即用)                         │  │
│   └─────────────────────────────────────────────────────────┘  │
│                              │                                  │
│              ┌───────────────┼───────────────┐                 │
│              ↓               ↓               ↓                 │
│         Discord          Telegram         Slack               │
│         (通过桥接)        (通过桥接)       (通过桥接)            │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
                               ↑ Matrix 协议
                               │
┌──────────────────────────────┴─────────────────────────────────┐
│                        Worker                                   │
│                                                                 │
│   只需要实现 Matrix Channel —— 一个协议，搞定所有渠道           │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**对于新的 Agent 运行时来说，接入 HiClaw 只需要做一件事：实现 Matrix Channel。**

### CoPaw 接入 HiClaw 的实际工作量

HiClaw 1.0.4 接入 CoPaw，核心代码只有两个文件：

1. **`matrix_channel.py`** (~450 行)：实现 Matrix 协议通信
2. **`bridge.py`** (~230 行)：桥接 openclaw.json 到 CoPaw 配置

就是这样！CoPaw 不需要关心 Discord、Telegram、Slack……它只需要和 Matrix 通信，就能：

- ✅ 复用 Manager 支持的所有 Channel 生态
- ✅ 复用开箱即用的 Matrix 客户端（Element Web 自带，移动端有 Element、FluffyChat 等）
- ✅ 和其他 Worker（无论是什么运行时）无缝协作
- ✅ 被 Manager 统一管理、监控、调度

**对于用户来说，接入新的 Agent 运行时是零学习成本的** —— 因为交互方式完全一样，还是通过 Matrix 客户端对话，Manager 会自动处理底层差异。

### 这意味着什么？

如果你正在开发一个新的 Agent 运行时，或者想把现有的 Agent 接入 HiClaw 生态：

- **不需要**：逐个适配 Discord、Telegram、Slack……
- **只需要**：实现 Matrix 协议（一个成熟的开放标准）
- **就能获得**：十几种消息渠道 + 开箱即用的客户端 + 多 Agent 协作能力

这就是 Manager-Worker 架构的核心价值：**一次接入，处处可用**。

---

## 两种部署模式，解决两个痛点

### 模式一：Docker 容器模式 —— 更省内存的 Worker

如果你只是需要更多的 Worker 并行干活，不需要访问本地环境，**Docker 模式的 CoPaw Worker 是最佳选择**：

| 对比项 | OpenClaw Worker | CoPaw Worker (Docker) |
|--------|-----------------|----------------------|
| 基础镜像 | Node.js 全家桶 | Python 3.11-slim |
| 内存占用 | ~500MB | ~150MB |
| 启动速度 | 较慢 | 较快 |
| 安全性 | 容器隔离 | 容器隔离 |

安全性完全一致，但内存占用大幅降低。

只需要在 Element 里告诉 Manager 创建一个 CoPaw Worker：

![创建 CoPaw Worker](https://img.alicdn.com/imgextra/i1/O1CN01bmZG111gDWAIjn3lI_!!6000000004108-2-tps-596-348.png)

实际资源占用只有约 150MB：

![CoPaw Worker 资源占用](https://img.alicdn.com/imgextra/i4/O1CN015VW8OW22ksYtf7eLH_!!6000000007159-2-tps-775-121.png)

这意味着同样的硬件配置下，你可以跑更多的 Worker。以前 8GB 内存只能跑 8-10 个 OpenClaw Worker，现在可以跑 40+ 个 CoPaw Worker。

**按需启用控制台**

为了节省内存，CoPaw Worker 默认关闭 Web 控制台。需要调试时，只需要在 Element 里让 Manager 打开：

![让 Manager 打开控制台](https://img.alicdn.com/imgextra/i1/O1CN012K4lMc1XcP91vwFhT_!!6000000002944-2-tps-582-356.png)

Manager 会自动重启 CoPaw Worker 容器并启用控制台，无需手动操作。调试完成后，也可以让 Manager 关闭控制台以节省资源。

打开控制台后，你可以直接在 CoPaw 控制台中管理 Worker，例如查看和管理 CoPaw 内置的技能：

![CoPaw 控制台技能管理](https://img.alicdn.com/imgextra/i2/O1CN01WPXRkd1MQ4i13ibDg_!!6000000001428-2-tps-2559-889.png)

### 模式二：本地 Host 模式 —— 直接操作你的电脑

有些任务天然需要访问本地环境：

- **操作浏览器**：自动化测试、网页截图、数据采集
- **访问本地文件**：读取桌面上的文件、操作本地 IDE
- **运行桌面应用**：自动化 Figma、Sketch、本地数据库客户端

这些任务在容器里做不到，因为容器是隔离环境。

**CoPaw Worker 的本地模式，就是为这类任务设计的。** 只需要让 Manager 创建一个远程模式的 CoPaw Worker，Manager 会给你一条 pip 命令，在本机执行即可：

![远程模式创建 CoPaw Worker](https://img.alicdn.com/imgextra/i2/O1CN01p5yHQ81fl7bpQXRgs_!!6000000004046-2-tps-882-480.png)

Worker 直接运行在你的本机上，拥有完整的本地访问权限。同时，它仍然通过 Matrix 与 Manager 和其他 Worker 通信，完美融入 HiClaw 的多 Agent 协作体系。

例如，让它打开浏览器到阿里云官网搜索 AI 网关，它会自己打开浏览器并按步骤操作：

![CoPaw Worker 操作浏览器](https://img.alicdn.com/imgextra/i2/O1CN01WKoWQt1P2fJRfymaG_!!6000000001783-2-tps-2526-1319.png)

**架构示意：**

```
┌─────────────────────────────────────────────────────────────┐
│                    HiClaw Manager                            │
│                    (容器环境)                                │
│                                                             │
│    Worker A (Docker)    Worker B (Docker)                   │
│    前端开发              后端开发                            │
└─────────────────────────────────────────────────────────────┘
              ↑ Matrix 通信
              │
┌─────────────┴───────────────────────────────────────────────┐
│                    你的本地电脑                              │
│                                                             │
│    Worker C (CoPaw 本地模式)                                │
│    浏览器操作 / 本地文件访问                                │
└─────────────────────────────────────────────────────────────┘
```

本地模式默认启用控制台（`--console-port 8088`），你可以打开 `http://localhost:8088` 实时查看 Worker 的执行过程。

---

## CoPaw 控制台：可视化的调试体验

不管是 Docker 模式还是本地模式，CoPaw Worker 都可以启用 Web 控制台。

控制台可以实时查看：

- **Thinking 输出**：Worker 在思考什么
- **工具调用**：调用了哪些工具，参数是什么
- **执行结果**：工具返回了什么
- **错误信息**：哪里出错了

对于调试和优化 Agent 行为非常有帮助。特别是当你发现 Worker 没按预期工作时，打开控制台看一眼 Thinking 输出，往往能快速定位问题。

![CoPaw 控制台可视化调试](https://img.alicdn.com/imgextra/i3/O1CN01TFxPwq1NYbheUTr2X_!!6000000001582-2-tps-2519-1281.png)

---

## 围绕社区痛点的优化

除了 CoPaw Worker 这一重大特性，1.0.4 还针对社区反馈的一系列痛点做了优化。

### 模型切换更可控

之前有用户反馈：切换模型时，Manager 可能会"自作主张"修改其他配置，导致意外的行为。

1.0.4 将 Worker 模型切换拆分为独立的 `worker-model-switch` skill，职责更单一，行为更可预测。同时修复了模型 `input` 字段的硬编码问题，现在会根据模型是否支持视觉能力动态设置。

### Worker 不再"交头接耳"

在项目群聊中，Worker 之间有时会进行不必要的对话，浪费 token。

1.0.4 优化了 Worker 的唤醒逻辑，确保只有被 @mention 时才会触发 LLM 调用。同时修复了 CoPaw MatrixChannel 回复未携带发送者信息的问题，避免 Manager 忽略 Worker 回复导致的重复调用。

### AI 身份认知

在 SOUL.md 中增加了 AI 身份声明，确保 Agent 明确知道自己是 AI，而不是人类。这可以避免一些奇怪的身份混淆问题，比如 Agent 假装自己是真人用户。

```markdown
## My Role

You are an AI assistant powered by HiClaw. You help users complete tasks
through natural language interaction, but you are not a human.
```

### Token 消耗基线 CI

1.0.4 新增了 Token 消耗基线的 CI 流程，可以定量分析每个版本的 Token 优化效果。

在关键流程中（创建 Worker、分派任务、多 Worker 协作等），CI 会记录 Token 消耗情况，并与上一个版本对比。这样可以：

- 量化优化效果
- 发现意外的 Token 回归
- 为后续优化提供数据支撑

---

## 快速开始

### 安装与升级

安装和升级使用相同的命令，脚本会交互式引导你选择：

**macOS / Linux：**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

**Windows（PowerShell 7+）：**

```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; iex $wc.DownloadString('https://higress.ai/hiclaw/install.ps1')
```

安装过程中会询问你默认使用哪种 Worker 运行时：

```
Select default worker runtime:
  1) openclaw (~500MB)
  2) copaw (~150MB, lightweight)

Enter your choice [1-2]:
```

升级时，脚本会自动检测现有安装，选择"就地升级"即可。升级过程中也会询问默认 Worker 运行时，选择后：
- **已有的 Worker**：不受影响，继续使用原有运行时
- **新创建的 Worker**：会使用你选择的默认运行时（CoPaw 或 OpenClaw）

---

## 致谢

感谢 [CoPaw 团队](https://github.com/agentscope-ai/CoPaw) 的工作！CoPaw 是一个设计精良的轻量级 Agent 运行时，控制台体验尤其出色。HiClaw 通过实现 Matrix Channel 和配置桥接层接入 CoPaw，整个过程很顺畅，代码量也不大。

如果你对 CoPaw 本身感兴趣，可以查看 [CoPaw GitHub 仓库](https://github.com/agentscope-ai/CoPaw)。

---

## 写在最后

HiClaw 1.0.4 的核心目标是让 Worker 更轻、更灵活：

- **更轻**：CoPaw Worker 内存占用大幅降低
- **更灵活**：本地模式解锁了浏览器操作等新场景
- **更易接入**：Manager-Worker 架构让新 Agent 运行时只需实现 Matrix 协议

如果你有以下的场景，特别推荐尝试 CoPaw Worker：

- 需要同时运行大量 Worker，但内存有限
- 需要 Worker 操作浏览器、访问本地文件
- 想要更轻量级的 Worker 调试体验

**现在就开始：**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

---

*HiClaw 是开源项目，基于 Apache 2.0 协议。如果你觉得有用，欢迎 Star ⭐ 和贡献代码！*

**相关链接：**
- [GitHub 仓库](https://github.com/alibaba/hiclaw)
- [Changelog v1.0.4](https://github.com/alibaba/hiclaw/blob/main/changelog/v1.0.4.md)
- [CoPaw GitHub 仓库](https://github.com/agentscope-ai/CoPaw)
