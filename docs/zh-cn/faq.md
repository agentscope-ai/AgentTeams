# 常见问题

- [如何查看当前 HiClaw 版本](#如何查看当前-hiclaw-版本)
- [如何对接飞书/钉钉/企业微信/Discord/Telegram](#如何对接飞书钉钉企业微信discordtelegram)
- [Windows 下执行安装脚本闪退](#windows-下执行安装脚本闪退)
- [Manager Agent 启动超时或失败](#manager-agent-启动超时或失败)
- [局域网其他电脑如何访问 Web 端](#局域网其他电脑如何访问-web-端)
- [本地访问 Matrix 服务器不通](#本地访问-matrix-服务器不通)
- [如何主动指挥 Worker](#如何主动指挥-worker)
- [如何切换 Manager 的模型](#如何切换-manager-的模型)
- [如何切换 Worker 的模型](#如何切换-worker-的模型)
- [HiClaw 支持发送和接收文件吗](#hiclaw-支持发送和接收文件吗)
- [为什么 Manager/Worker 一直显示"输入中"](#为什么-managerworker-一直显示输入中)
- [Manager/Worker 不回复消息怎么办](#managerworker-不回复消息怎么办)
- [在房间里和 Manager 聊天没有响应或返回错误状态码](#在房间里和-manager-聊天没有响应或返回错误状态码)
- [HTTP 401: invalid access token or token expired](#http-401-invalid-access-token-or-token-expired)
- [会话管理（通过 IM 指令）](#会话管理通过-im-指令)

---

## 如何查看当前 HiClaw 版本

执行以下命令查看已安装的版本：

```bash
docker exec hiclaw-manager cat /opt/hiclaw/agent/.builtin-version
```

安装时指定版本：

```bash
HICLAW_VERSION=v1.0.5 bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

---

## Windows 下执行安装脚本闪退

如果在 Windows 下执行 PowerShell 安装脚本后窗口立即关闭，请先确认是否已安装 Docker Desktop。如果已安装，请确认 Docker Desktop 是否已启动并完全加载——脚本需要连接 Docker 守护进程，Docker Desktop 未运行时会直接失败退出。

---

## Manager Agent 启动超时或失败

安装完成后如果 Manager Agent 迟迟没有响应，或启动失败
，进容器查看日志：```bash
docker exec -it hiclaw-manager cat /var/log/hiclaw/manager-agent.log
```

**情况一：日志中有进程退出记录**

可能是 Docker VM 分配的内存不足。建议将内存调整到 4GB 以上：Docker Desktop → Settings → Resources → Memory。调整后重新执行安装命令。

**情况二：日志中没有进程退出，但某些组件起不来**

可能是配置脏数据导致的。建议到原安装目录重新执行安装命令，选择**删除重装**：

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

安装脚本检测到已有安装时会询问处理方式，选择删除后重装即可清除脏数据。

**情况三：Mac M 系列芯片 + 低版本 Docker/Podman**

如果你使用的是搭载 Apple M 系列芯片（M1/M2/M3/M4）的 Mac，且 Docker Desktop 版本低于 4.39.0，Manager Agent 可能无法正常启动。

**解决方案：**

- **Docker Desktop**：升级到 4.39.0 或更高版本
- **Podman**：确保 Podman Engine **Server 版本 ≥ 5.7.1**（可通过 `podman version` 查看）

---

## 局域网其他电脑如何访问 Web 端

**访问 Element Web**

在局域网其他电脑的浏览器中输入：

```
http://<局域网IP>:18088
```

浏览器可能会提示"不安全"或"不支持"，忽略提示直接点 Continue 进入即可。

**修改 Matrix Server 地址**

默认配置的 Matrix Server 域名解析到 `localhost`，在其他电脑上无法连通。登录 Element Web 时，需要将 Matrix Server 地址改为：

```
http://<局域网IP>:18080
```

例如局域网 IP 是 `192.168.1.100`，则填写 `http://192.168.1.100:18080`。

---

## 本地访问 Matrix 服务器不通

如果在本机也无法连接 Matrix 服务器，请检查浏览器或系统是否开启了代理。`*-local.hiclaw.io` 域名默认解析到 `127.0.0.1`，开启代理后请求会被转发到代理服务器，无法到达本地服务。

关闭代理，或将 `*-local.hiclaw.io` / `127.0.0.1` 加入代理的绕过列表即可。

---

## 如何主动指挥 Worker

创建 Worker 后，Manager 会自动将你和 Worker 拉入同一个群聊房间。在群聊中，必须 **@ Worker** 才能让它响应，没有 @ 的消息会被忽略。

在 Element 等客户端中，输入 `@` 后再输入 Worker 昵称的首字母，才会出现补全列表，选择对应用户即可。

也可以点击 Worker 的头像，进入**私聊**。私聊中不需要 @，每条消息都会触发 Worker 响应。但注意：私聊对 Manager 不可见，Manager 不会感知到这部分对话内容。

---

## 如何切换 Manager 的模型

HiClaw 支持两种模型切换方式：**切换当前会话模型**（即时生效，不持久化）和**切换主用模型**（持久化，需重启生效）。

### 方式一：切换当前会话模型（即时，不持久化）

通过 IM 斜杠命令 `/model` 可以即时切换当前会话使用的模型，无需重启：

```
/model qwen3.5-plus
```

这种方式仅对当前会话生效，重启后会恢复为主用模型。且仅支持预置的已知模型列表中的模型，完整列表见 [`manager/configs/known-models.json`](../../manager/configs/known-models.json)。

更多 `/model` 命令用法见 [会话管理（通过 IM 指令）](#会话管理通过-im-指令) 中的"模型选择"部分。

### 方式二：切换主用模型（持久化，需重启）

通过 Manager 的**模型切换技能**可以持久化切换主用模型。这种方式支持任意模型名（不限于预置列表），但如果目标模型不在已有配置中，需要重启容器才能生效。

**为什么让 Manager 切换而不是手动改配置？**

OpenClaw 需要在配置中设置模型的上下文窗口大小（`contextWindow`）。HiClaw 默认使用 qwen3.5-plus 的 200K token 窗口。如果切换到窗口不同的模型但没有更新这个设置，当对话接近窗口上限时，OpenClaw 不知道何时压缩上下文，可能导致 session 无法使用。

模型切换技能会根据模型名自动修改 OpenClaw 配置中的 `contextWindow` 和 `maxTokens`。

**切换步骤**

在 Higress 控制台配置好模型供应商后，直接告诉 Manager 模型名即可：
> "切换到 `claude-3-5-sonnet`"

Manager 会使用模型切换技能完成配置更新。

**如果切换没有成功？**

可能是 Manager 没有自动调用模型切换技能。可以主动告诉它：
> "用模型切换技能帮我切换到 `claude-3-5-sonnet`"

---

**Higress 控制台配置**

**单供应商情况**

在 Higress 控制台，将 `default-ai-route` 这个路由配置到你的模型供应商。然后直接告诉 Manager 你想使用的具体模型名（例如 `qwen3.5-plus`）。Manager 会先用该模型名发起一次联通测试，测试通过后自动完成切换。

**多供应商情况**

在 Higress 控制台，创建多条 AI 路由，每条路由配置不同的模型名匹配规则（前缀或正则），分别指向对应的供应商。之后的流程与单供应商完全一致——告诉 Manager 模型名，它会自动完成测试和切换。

参考：[Higress AI 快速开始 — 控制台配置](https://higress.ai/docs/ai/quick-start#%E6%8E%A7%E5%88%B6%E5%8F%B0%E9%85%8D%E7%BD%AE)

---

## 如何切换 Worker 的模型

同样支持两种方式：**切换当前会话模型**和**切换主用模型**。

### 方式一：切换当前会话模型（即时，不持久化）

在 Worker 所在的群聊或私聊中，通过 @Worker 加 `/model` 命令即时切换：

```
@alice /model qwen3.5-plus
```

仅对当前会话生效，重启后恢复为主用模型。仅支持预置的已知模型，完整列表见 [`manager/configs/known-models.json`](../../manager/configs/known-models.json)。

### 方式二：切换主用模型（持久化，需重启）

由 Manager 代为操作，支持任意模型名。

**创建时指定**：在让 Manager 创建 Worker 时直接说明模型，例如"帮我创建一个名为 alice 的 Worker，使用 `qwen3.5-plus`"。

**创建后修改**：随时告诉 Manager 切换某个 Worker 的模型，例如"把 alice 的模型切换为 `claude-3-5-sonnet`"，Manager 会自动更新该 Worker 的配置。

切换前请确保 Higress 已配置好目标模型名到对应供应商的路由，具体配置方式见下文。

---

**Higress 控制台配置**

**单供应商情况**

在 Higress 控制台，将 `default-ai-route` 这个路由配置到你的模型供应商。然后直接告诉 Manager 你想让 Worker 使用的具体模型名（例如 `qwen3.5-plus`）。Manager 会先用该模型名发起一次联通测试，测试通过后自动完成切换。

**多供应商情况**

在 Higress 控制台，创建多条 AI 路由，每条路由配置不同的模型名匹配规则（前缀或正则），分别指向对应的供应商。之后的流程与单供应商完全一致——告诉 Manager 要切换的 Worker 模型名，它会自动完成测试和切换。

参考：[Higress AI 快速开始 — 控制台配置](https://higress.ai/docs/ai/quick-start#%E6%8E%A7%E5%88%B6%E5%8F%B0%E9%85%8D%E7%BD%AE)

---

## HiClaw 支持发送和接收文件吗

**接收你发送的文件**：支持。在 Element Web 中点击附件按钮上传文件，Manager 或 Worker 会收到 Matrix 媒体消息并可以读取其内容。

**向你发送文件**：支持。当你要求 Manager（或 Worker）发送文件时——例如任务产物、生成的报告或它能访问的任意文件——它会将文件上传到 Matrix 媒体服务器，并以可下载附件的形式发送到房间中，你在 Element Web 里点击即可下载。

---

## 为什么 Manager/Worker 一直显示"输入中"

这是正常现象，说明底层的 OpenClaw Agent 引擎正在执行任务。HiClaw 设定的单次任务超时时间为 30 分钟，Agent 最长会持续执行 30 分钟。

如果想查看 Agent 的执行细节，可以进入 Manager 或 Worker 容器，查看 session 日志：

```bash
# Manager
docker exec -it hiclaw-manager ls .openclaw/agents/main/sessions/

# Worker（将 <worker-name> 替换为实际容器名）
docker exec -it <worker-name> ls .openclaw/agents/main/sessions/
```

该目录下的 `.jsonl` 文件由 OpenClaw 实时写入，记录了完整的 Agent 执行过程，包括 LLM 调用、工具使用、推理步骤等。

---

## Manager/Worker 不回复消息怎么办

如果给 Manager 或 Worker 发送消息后没有回复，可以按以下步骤排查：

### 1. 检查是否正在工作

**如果一直不回复，且没有显示"输入中"**，绝大多数原因是 **Agent 正在工作**。

OpenClaw 限制"输入中"状态最多持续 **2 分钟**，工作超过 2 分钟就不会再显示"输入中"了。

**如何确认消息已入队列**：
- 发送消息后，查看消息右边是否有一个 **m 小图标**
- 这个图标表示 Manager 已读
- 出现这个图标就说明消息已入队列，会在当前任务执行完后继续处理该消息

### 2. 检查聊天环境

**私聊 vs 群聊**：
- 如果是**私聊**（只有你和一个 Agent），每条消息都会触发 Agent 响应
- 如果是**2 人以上的房间**（群聊），必须 **@ Agent** 才能让它响应，没有 @ 的消息会被忽略

### 3. 检查 Session 状态

可能是 OpenClaw session 损坏了。进入 Manager 或 Worker 容器，使用 OpenClaw TUI 查看：

```bash
# Manager
docker exec -it hiclaw-manager openclaw tui

# Worker（将 <worker-name> 替换为实际容器名）
docker exec -it <worker-name> openclaw tui
```

进入 TUI 后：
1. 输入 `/sessions` 查看所有 session
2. 切换到对应聊天记录的 session
3. 尝试对话，观察是否有报错

如果 session 确实损坏了，可以尝试在 Element 等 Matrix 客户端中对应的会话里直接输入 `/new` 重置会话，看是否恢复正常。

---

## 在房间里和 Manager 聊天没有响应或返回错误状态码

如果 Manager 没有响应，或者返回了 404、503 等状态码，可以按以下步骤排查：

### 1. 检查 Session 状态

可能是 OpenClaw session 损坏了。进入 Manager 容器，使用 OpenClaw TUI 查看：

```bash
docker exec -it hiclaw-manager openclaw tui
```

进入 TUI 后：
1. 输入 `/sessions` 查看所有 session
2. 切换到对应聊天记录的 session
3. 尝试对话，观察是否有报错

如果 session 确实损坏了，可以尝试在 Element 等 Matrix 客户端中对应的会话里直接输入 `/new` 重置会话，看是否恢复正常。

### 2. 检查 Higress AI 网关日志

如果重置 session 后问题仍然存在，查看 Higress AI 网关日志：

```bash
docker exec -it hiclaw-manager cat /var/log/hiclaw/higress-gateway.log
```

在日志中搜索对应的状态码，常见原因：

- **503**：容器内网络环境问题，导致外网 LLM 服务不可达。
- **404**：模型名称填写有误。

要判断是后端服务出错还是 Higress 自身配置问题，查看日志中的 `upstream_host` 字段：如果该字段有值，说明请求已到达后端，异常状态码是由上游服务返回的；如果为空，说明 Higress 本身无法路由该请求。

### 3. 检查模型配置

可能是模型的上下文窗口大小配置不正确，导致窗口耗尽前没有及时压缩。请参考 [如何切换 Manager 的模型](#如何切换-manager-的模型) 和 [如何切换 Worker 的模型](#如何切换-worker-的模型) 进行正确配置。

---

## HTTP 401: invalid access token or token expired

如果 Manager 或 Worker 调用 LLM 时出现这个错误，检查一下是否在安装时选择了**百炼 Coding Plan**，但还没有去开通。

百炼 Coding Plan 是阿里云提供的免费试用计划，需要先激活才能使用：

1. 访问：https://www.aliyun.com/benefit/scene/codingplan
2. 使用阿里云账号登录
3. 按照指引完成激活

激活后重新执行安装或重启 Manager 容器即可正常使用。

---

## 如何对接飞书/钉钉/企业微信/Discord/Telegram

HiClaw Manager 基于 OpenClaw 构建，原生支持多种消息渠道。要对接其他渠道：

**方法一：直接修改配置**

Manager 的工作目录是宿主机上的 `~/hiclaw-manager`，里面的 `openclaw.json` 可以直接编辑。参照 [OpenClaw 渠道文档](https://docs.openclaw.ai) 中各平台的配置格式进行配置。

修改后重启 Manager 容器使配置生效：

```bash
docker restart hiclaw-manager
```

**方法二：让 Manager 学习你现有的 OpenClaw 配置**

如果你已经在其他地方使用 OpenClaw 接入了其他渠道，可以让 Manager 读取你现有的配置：

- **告诉 Manager 文件位置**：在 Element Web 中告诉 Manager 你的 OpenClaw 配置文件路径（例如 "我的 OpenClaw 配置在 `/home/user/my-openclaw.json`"），Manager 会直接读取。
- **通过附件发送**：在 Element Web 或其他 Matrix 客户端中，把配置文件作为附件上传发送给 Manager，Manager 会接收并读取。

然后让 Manager 帮你在它的配置里添加相同的渠道。

---

## 会话管理（通过 IM 指令）

HiClaw 基于 OpenClaw，通过 Matrix 渠道（Element Web）与 Agent 通信。OpenClaw 支持**斜杠命令**，你可以直接在聊天中以独立消息的形式发送这些指令，由 Gateway 在模型处理前解析执行。

**注意：** 大多数命令必须以**独立消息**发送，且以 `/` 开头。不要在同一则消息中混入其他文字。

**群聊中使用：** 可以在同一条消息中组合 @提及和斜杠命令，例如 `@Worker /compact` 或 `@Worker /new`。@提及确保命令发送给正确的 Agent，斜杠命令仍由 Gateway 正常处理。

以下以 OpenClaw（Manager 和 OpenClaw Worker）为例列出可用命令。CoPaw Worker 使用不同的命令集，详见 [CoPaw 命令参考](https://copaw.agentscope.io/docs/commands)。

### 会话重置与压缩

| 指令 | 说明 |
|------|------|
| `/reset` 或 `/new` | 重置当前会话，开启全新对话。Agent 会回复简短问候以确认。 |
| `/new <model>` | 重置会话并可选择切换模型。支持模型别名、`provider/model` 或提供商名称。 |
| `/compact [instructions]` | 手动压缩对话上下文。在长任务前或切换话题时使用，以释放上下文窗口。 |

### 模型选择

| 指令 | 说明 |
|------|------|
| `/model` 或 `/models` | 显示紧凑的模型选择器（编号列表）。 |
| `/model list` | 与 `/model` 相同。 |
| `/model <数字>` | 按选择器中的编号选择模型。 |
| `/model <provider/model>` | 切换到指定模型，例如 `/model openai/gpt-5.2` 或 `/model anthropic/claude-opus-4-5`。 |
| `/model status` | 显示详细的模型/认证/端点状态。 |

### 其他常用指令

| 指令 | 说明 |
|------|------|
| `/status` | 显示当前状态（包含提供商用量/配额，如已启用）。 |
| `/help` | 显示帮助。 |
| `/commands` | 列出可用命令。 |
| `/stop` | 中止当前 Agent 运行。 |

### 会话指令（可选）

以下指令用于控制会话行为。作为独立消息发送时会持久生效；也可内联在消息中，但不会持久化：

- `/think <off|minimal|low|medium|high|xhigh>` — 控制思考/推理级别。
- `/verbose on|full|off` — 切换详细输出（用于调试）。
- `/reasoning on|off|stream` — 切换是否单独发送推理消息。
- `/elevated on|off|ask|full` — 控制 exec 审批行为。
- `/queue` — 查看或配置队列设置（防抖、上限等）。

**参考：** [OpenClaw 斜杠命令](https://docs.openclaw.ai/tools/slash-commands)
