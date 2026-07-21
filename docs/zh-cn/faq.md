# 常见问题

> **使用 AgentTeams v1.0.9 或更早版本？** v1.1.0 架构发生了重大变化。旧版单容器架构的常见问题请参阅 [常见问题（旧架构）](faq-legacy.md)。

- [如何查看当前 AgentTeams 版本](#如何查看当前-agentteams-版本)
- [新架构说明（v1.1.0+）](#新架构说明v110)
- [如何使用 agt CLI 管理资源](#如何使用-agt-cli-管理资源)
- [如何为 Worker 配置 GitHub 凭据](#如何为-worker-配置-github-凭据)
- [如何对接飞书/钉钉/企业微信/Discord/Telegram](#如何对接飞书钉钉企业微信discordtelegram)
- [Windows 下执行安装脚本闪退](#windows-下执行安装脚本闪退)
- [安装失败：embedded 镜像 "manifest unknown"](#安装失败embedded-镜像-manifest-unknown)
- [Manager Agent 启动超时或失败](#manager-agent-启动超时或失败)
- [局域网其他电脑如何访问 Web 端](#局域网其他电脑如何访问-web-端)
- [Element 提示 homeserver 不是有效的 Matrix 服务器](#element-提示-homeserver-不是有效的-matrix-服务器)
- [本地访问 Matrix 服务器不通](#本地访问-matrix-服务器不通)
- [如何主动指挥 Worker](#如何主动指挥-worker)
- [如何接入第三方、本地或多供应商模型](#如何接入第三方本地或多供应商模型)
- [为什么自定义 Higress AI 路由总是匹配不到](#为什么自定义-higress-ai-路由总是匹配不到)
- [如何切换 Manager 的模型](#如何切换-manager-的模型)
- [如何切换 Worker 的模型](#如何切换-worker-的模型)
- [如何配置 OpenRouter 或模型名带斜杠的供应商](#如何配置-openrouter-或模型名带斜杠的供应商)
- [如何切换 Worker 的运行时](#如何切换-worker-的运行时)
- [为什么 QwenPaw 仍然使用 `copaw` 作为 runtime 值或镜像名](#为什么-qwenpaw-仍然使用-copaw-作为-runtime-值或镜像名)
- [如何接入自己实现的 agent 作为 Worker](#如何接入自己实现的-agent-作为-worker)
- [AgentTeams 可以连接已有的 Higress 实例吗](#agentteams-可以连接已有的-higress-实例吗)
- [如何使用 Worker 模板市场](#如何使用-worker-模板市场)
- [AgentTeams 支持发送和接收文件吗](#agentteams-支持发送和接收文件吗)
- [为什么 Manager/Worker 一直显示"输入中"](#为什么-managerworker-一直显示输入中)
- [Manager/Worker 不回复消息怎么办](#managerworker-不回复消息怎么办)
- [在房间里和 Manager 聊天没有响应或返回错误状态码](#在房间里和-manager-聊天没有响应或返回错误状态码)
- [HTTP 401: invalid access token or token expired](#http-401-invalid-access-token-or-token-expired)
- [如何查看 Manager Agent 日志](#如何查看-manager-agent-日志)
- [会话管理（通过 IM 指令）](#会话管理通过-im-指令)

---

## 如何查看当前 AgentTeams 版本

执行以下命令查看已安装的版本：

```bash
docker exec agentteams-manager cat /opt/agentteams/agent/.builtin-version
```

v1.1.0+ 架构下，也可以通过 controller 里的 CLI 查询：

```bash
docker exec agentteams-controller agt version
```

早期 `latest` 镜像如果是在版本元数据规范化之前重新构建的，可能会显示 commit hash
而不是语义化版本号。遇到这种情况，可以用 hash 对照 release 或 commit 历史，或者使用
明确的 `AGENTTEAMS_VERSION` 升级到指定版本。

安装时指定版本：

```bash
AGENTTEAMS_VERSION=v1.1.0 bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

---

## 新架构说明（v1.1.0+）

从 v1.1.0 开始，AgentTeams 从**单一全包容器**切换为由 `agentteams-controller` 管理的**多容器架构**：

| 组件 | 旧架构（≤v1.0.9） | 新架构（v1.1.0+） |
|------|-------------------|-------------------|
| 基础设施（Higress、Tuwunel、MinIO、Element Web） | 打包在 `agentteams-manager` 内 | 运行在 `agentteams-controller` 容器中（使用 `agentteams-embedded` 镜像） |
| Manager Agent | 在 `agentteams-manager` 内 | 独立的 `agentteams-manager` 容器（轻量级，仅 Agent） |
| Worker 管理 | Shell 脚本（`create-worker.sh`）+ `workers-registry.json` | 声明式 CRD，通过 `agt` CLI（`agt create worker`、`agt apply`） |
| Worker 运行时 | 仅 OpenClaw | OpenClaw、**QwenPaw**（Python；旧称 **CoPaw**）或 Hermes |

**主要优势：**
- Manager 镜像体积缩小约 1.7 GB（不再打包 Higress 二进制文件）
- Worker 声明式管理 —— 定义 YAML，apply 即可
- 三种 Worker 运行时可选：OpenClaw（Node.js）、QwenPaw（Python；旧称 **CoPaw**）、Hermes
- 团队支持：Team Leader DAG 编排
- Worker 模板市场：一键创建预配置的 Worker

**安装后你会看到：**

```bash
docker ps
# agentteams-controller    -- Controller + 所有基础设施服务
# agentteams-manager       -- Manager Agent（轻量级）
# agentteams-worker-alice  -- Worker 容器（按需创建）
```

---

## 如何使用 agt CLI 管理资源

`agt` CLI 同时存在于 **`agentteams-controller`**、**`agentteams-manager`** 与 Worker 镜像中（同一二进制，通过 controller 的 REST API 操作资源）。**`install/agentteams-apply.sh`** 在 **`agentteams-manager`** 内执行 `agt apply`（因 YAML 会拷贝进该容器）。临时运维命令常用：`docker exec agentteams-controller agt …`。

**进入 Controller 容器（示例）：**

```bash
docker exec -it agentteams-controller sh
```

### 查询资源

```bash
# 集群概览
agt status

# 列出所有 Worker（表格格式）
agt get workers

# 以 JSON 格式列出 Worker（便于脚本处理）
agt get workers -o json

# 查看特定 Worker 的详细信息
agt get workers alice
agt get workers alice -o json

# 列出某个团队中的 Worker
agt get workers --team dev-team

# 列出所有团队
agt get teams

# 列出所有 Human
agt get humans

# 列出所有 Manager
agt get managers

# 查看 Controller 版本
agt version
```

### 创建资源

```bash
# 使用默认模型和运行时创建 Worker
agt create worker --name alice

# 指定模型和运行时创建 Worker
agt create worker --name bob --model claude-sonnet-4-6 --runtime hermes

# 创建带技能的 Worker
agt create worker --name charlie --skills github-operations

# 使用自定义 SOUL.md 创建 Worker
agt create worker --name diana --soul-file /path/to/SOUL.md

# 创建 Worker 但不等待就绪
agt create worker --name eve --no-wait

# 创建团队
agt create team --name dev-team --goal "全栈 Web 开发"

# 创建 Human
agt create human --name john --level 1

# 创建 Manager
agt create manager --name default --model qwen3.5-plus
```

### 修改资源

```bash
# 切换 Worker 模型
agt update worker --name alice --model claude-sonnet-4-6

# 切换 Worker 运行时（会触发容器重建）
agt update worker --name alice --runtime hermes

# 更新 Worker 技能
agt update worker --name alice --skills github-operations,code-review
```

### 应用 YAML 定义

```bash
# 应用单个 YAML 资源
agt apply -f worker-alice.yaml
```

对于直接 CLI 参数未覆盖的字段（例如 `spec.mcpServers`），请使用 YAML：

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  workerName: alice
  skills:
    - github-operations
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
      transport: http
```

```bash
# 从 zip 包导入 Worker
agt apply worker --name alice --zip worker-package.zip
```

### Worker 生命周期管理

```bash
# 停止（休眠）Worker
agt worker sleep --name alice

# 唤醒休眠中的 Worker
agt worker wake --name alice

# 查看 Worker 状态
agt worker status --name alice
```

### 删除资源

```bash
# 删除 Worker（停止容器、清理 Matrix 账号和网关 Consumer）
agt delete worker alice

# 删除团队
agt delete team dev-team

# 删除 Human
agt delete human john
```

> **提示：** Manager Agent 的大部分操作（创建 Worker、切换模型、分配任务）底层都调用了同一套 `agt` CLI。直接使用 CLI 适合调试、批量操作或自动化脚本场景。

声明式 YAML 资源定义的完整文档请参阅 [声明式资源管理](declarative-resource-management.md)。

---

## 如何为 Worker 配置 GitHub 凭据

GitHub 凭据应作为 MCP Server 凭据配置，不要复制到 Worker 容器里。Worker 通过
`mcporter` 和 AI Gateway 调用 GitHub，真实 GitHub PAT 保存在网关侧 MCP 配置中。

安装时，在安装脚本询问可选 GitHub Personal Access Token 时输入，或提前设置
`AGENTTEAMS_GITHUB_TOKEN`：

```bash
AGENTTEAMS_GITHUB_TOKEN=ghp_xxx bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

该变量存在时，AgentTeams 会自动配置 GitHub MCP Server，并生成 Manager 侧
`mcporter` 配置。之后在 Worker YAML manifest 中声明 GitHub MCP 能力：

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  workerName: alice
  skills:
    - github-operations
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
      transport: http
```

通过当前支持的 YAML 路径应用：

```bash
agt apply -f worker-alice.yaml
```

如果已有安装当时跳过了 token，请在原工作目录重新执行安装并提供
`AGENTTEAMS_GITHUB_TOKEN`，或在网关中手动配置 GitHub MCP Server 并授权目标
Manager/Worker consumer。不要把 PAT 粘贴进 Worker 提示词或容器本地配置。

---

## Windows 下执行安装脚本闪退

如果在 Windows 下执行 PowerShell 安装脚本后窗口立即关闭，请先确认是否已安装 Docker Desktop。如果已安装，请确认 Docker Desktop 是否已启动并完全加载——脚本需要连接 Docker 守护进程，Docker Desktop 未运行时会直接失败退出。

---

## 安装失败：embedded 镜像 "manifest unknown"

如果安装脚本报错类似：

```
ERROR: Failed to pull agentteams-embedded image.
Attempted: higress/agentteams-embedded:v1.1.0 and higress/agentteams-embedded:latest
```

说明注册中心中没有该版本的 embedded 镜像。有三个解决方案：

1. **指定有 embedded 镜像的版本**：查看 [发布页面](https://github.com/agentscope-ai/AgentTeams/releases) 确认可用版本。
2. **从源码本地构建**：克隆仓库后执行 `make install-embedded`。
3. **覆盖镜像**：设置 `AGENTTEAMS_INSTALL_EMBEDDED_IMAGE` 为自定义镜像。

> 如果你有意使用旧版单容器架构（v1.0.9 或更早），设置 `AGENTTEAMS_FORCE_LEGACY=1`。注意此选项仅适用于打包了基础设施服务的镜像。

---

## Manager Agent 启动超时或失败

安装完成后如果 Manager Agent 迟迟没有响应，需要检查日志。

**新架构（v1.1.0+）下**，Manager 作为独立容器运行，需要在两个地方查看日志：

```bash
# Controller（基础设施）日志
docker logs agentteams-controller

# Manager Agent 日志
docker logs agentteams-manager
```

**情况一：Controller 正常但 Manager 容器未启动**

Controller 会自动启动 Manager 容器。如果 `docker ps` 中看不到 Manager 容器，检查 Controller 日志排查创建失败的原因。

**情况二：Docker VM 分配的内存不足**

建议将内存调整到 4GB 以上：Docker Desktop → Settings → Resources → Memory。调整后重新执行安装命令。

**情况三：配置脏数据**

建议到原安装目录重新执行安装命令，选择**删除重装**：

```bash
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

安装脚本检测到已有安装时会询问处理方式，选择删除后重装即可清除脏数据。

**情况四：Mac M 系列芯片 + 低版本 Docker/Podman**

如果你使用的是搭载 Apple M 系列芯片（M1/M2/M3/M4）的 Mac，且 Docker Desktop 版本低于 4.39.0，Manager Agent 可能无法正常启动。

**解决方案：**

- **Docker Desktop**：升级到 4.39.0 或更高版本
- **Podman**：确保 Podman Engine **Server 版本 ≥ 5.7.1**（可通过 `podman version` 查看）

**情况五：Linux 主机 SELinux 拦截挂载目录**

如果详细日志，尤其是 `mc-mirror.log`，在工作目录或共享目录下出现
`permission denied`，且主机启用了 SELinux，可能是容器没有访问 bind mount 的
SELinux 标签。请在 Docker/Podman 允许挂载的位置重新执行安装；如果是手动等价挂载，
需要给工作目录和共享目录的 `-v` 参数加上 `:z`，让容器可以访问挂载路径。

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

如果登录页仍然提示 homeserver 错误：

1. 确认安装时选择了允许外部访问。本机模式只绑定到 `127.0.0.1`，局域网其他设备无法访问。
2. 确认 Manager 所在机器的防火墙放行 `18080`（Matrix/Higress Gateway）和 `18088`（Element Web）。
3. 不要在其他设备上使用默认的 `matrix-local.agentteams.io`；该域名会解析到当前设备自己的 loopback 地址。

如果通过 Tailscale 使用 FluffyChat 或 Element Mobile，规则相同：homeserver 填
`http://<tailscale-ip>:18080`，并确认手机和 AgentTeams 所在机器在 Tailscale 网络内互通。

---

## Element 提示 homeserver 不是有效的 Matrix 服务器

Element 要求填写自定义 homeserver 时，不要填写 Element Web UI 的地址或端口。这两个地址对应不同组件：

- Element Web UI：`http://<host>:18088`
- Matrix/Higress gateway homeserver：`http://<host>:18080`

如果看到“homeserver 不是有效的 Matrix 服务器”一类提示，请将 `:18088` 改成 `:18080` 后重新登录。局域网或 Tailscale 访问时，使用客户端能访问到的主机 IP，例如 `http://192.168.1.100:18080`。

---

## 本地访问 Matrix 服务器不通

如果在本机也无法连接 Matrix 服务器，请检查浏览器或系统是否开启了代理。`*-local.agentteams.io` 域名默认解析到 `127.0.0.1`，开启代理后请求会被转发到代理服务器，无法到达本地服务。

关闭代理，或将 `*-local.agentteams.io` / `127.0.0.1` 加入代理的绕过列表即可。

---

## 如何主动指挥 Worker

创建 Worker 后，Manager 会自动将你和 Worker 拉入同一个群聊房间。在群聊中，必须 **@ Worker** 才能让它响应，没有 @ 的消息会被忽略。

在 Element 等客户端中，输入 `@` 后再输入 Worker 昵称的首字母，才会出现补全列表，选择对应用户即可。

也可以点击 Worker 的头像，进入**私聊**。私聊中不需要 @，每条消息都会触发 Worker 响应。但注意：私聊对 Manager 不可见，Manager 不会感知到这部分对话内容。

---

## 如何接入第三方、本地或多供应商模型

AgentTeams 不会直接读取你的 `~/.openclaw/openclaw.json` 供应商定义。模型流量会经过
AgentTeams AI Gateway。OpenClaw/QwenPaw 通常只看到一个名为 `hiclaw-gateway` 的
provider；Higress 再根据请求里的模型名路由到真实上游供应商。

### 第三方 OpenAI 兼容 API

对于 OpenAI 兼容服务，在 Higress AI 路由中配置：

- 供应商 base URL；供应商要求 `/v1` 时需要包含 `/v1`
- 供应商 API Key
- 能匹配你准备让 Manager 或 Worker 使用的 model id 的模型匹配规则

然后让 Manager 切换到同一个 model id，或创建/更新 Worker 时指定这个模型。不要只把
`/model list` 当成 Higress 可用供应商列表；它展示的是 Agent 侧已知模型列表，不是
Higress 里定义的全部路由。

### Ollama、LM Studio 等本地模型

本地模型需要暴露 OpenAI 兼容 API，并且 AgentTeams 容器必须能访问该地址。在 Docker 容器内，
`localhost` 指的是容器自身，不是你的 Mac 或宿主机。请使用容器可达的宿主机地址，例如
Docker Desktop 下的 `http://host.docker.internal:<port>/v1`，或在 Linux/Podman 下使用
宿主机局域网 IP。

### 多供应商和按任务使用不同模型

在 Higress 中配置多条 AI 路由，每条路由使用不同的前缀或正则匹配规则，例如一条匹配
`qwen*`，另一条匹配 `claude*`。然后明确给 Manager 或 Worker 指定目标模型。AgentTeams
可以让不同 Worker 使用不同模型，但不会内置按任务类型自动选择模型的策略；这类策略需要
通过 Worker 角色设计或显式切换模型表达。

---

## 为什么自定义 Higress AI 路由总是匹配不到

AgentTeams 安装时会创建 `default-ai-route`。如果这条路由没有配置
`modelPredicates`，它可能匹配所有模型请求，因此后添加的自定义路由看起来像是优先级更低。

存在多条 AI route 时，需要让模型匹配规则不重叠：

- 给每条自定义 route 配置 `modelPredicates`，例如用前缀匹配 `deepseek`，或用正则匹配 `^openrouter/.*$`。
- 同时也要限制 `default-ai-route` 只匹配它负责的模型，例如 `qwen*`，不要让它保持空 `modelPredicates`。
- 切换 Manager 或 Worker 模型时使用同一个 model id；路由是根据请求里的模型名选择的，不是根据供应商展示名选择的。

---

## 如何切换 Manager 的模型

AgentTeams 支持两种模型切换方式：**切换当前会话模型**（即时生效，不持久化）和**切换主用模型**（持久化，需重启生效）。

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

OpenClaw 需要在配置中设置模型的上下文窗口大小（`contextWindow`）。AgentTeams 默认使用 qwen3.5-plus 的 200K token 窗口。如果切换到窗口不同的模型但没有更新这个设置，当对话接近窗口上限时，OpenClaw 不知道何时压缩上下文，可能导致 session 无法使用。

模型切换技能会根据模型名自动修改 OpenClaw 配置中的 `contextWindow` 和 `maxTokens`。

如果看到 `model_context_window_exceeded`，先用 `/new` 开启新会话，或切换到上下文窗口更大的模型。随后确认目标模型配置里的 `contextWindow` 与供应商实际窗口一致，再继续长上下文对话。

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

## 如何配置 OpenRouter 或模型名带斜杠的供应商

在 Higress AI 路由配置中，**服务名称**是内部名称，不是模型名，不能包含 `/`。
`openrouter/`、`stepfun/` 这类模型前缀应放在模型匹配规则里。

OpenRouter 示例：

| 字段 | 值 |
|------|----|
| 服务名称 | `openrouter` |
| 模型匹配规则 | regex，例如 `^openrouter/.*$` |
| 协议 | `openai` |
| 自定义 URL | `https://openrouter.ai/api/v1` |

配置完成后，告诉 Manager 使用完整模型名，例如
`openrouter/stepfun-eur-1-70b`。Higress 会根据模型名前缀匹配到对应供应商路由。

---

## 如何切换 Worker 的运行时

AgentTeams v1.1.0+ 支持三种 Worker 运行时：

| 运行时 | 语言 | 适用场景 |
|--------|------|----------|
| OpenClaw | Node.js | 通用场景，成熟生态 |
| QwenPaw | Python | Python 原生工作流、数据科学（旧称 **CoPaw**） |
| Hermes | Python | 自主编程，开发任务 |

### 创建时指定

创建 Worker 时指定运行时：

```
agt create worker --name alice --runtime hermes
```

或通过 YAML：

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  runtime: hermes
  model: qwen3.5-plus
```

如果未指定运行时，使用安装时设定的默认值（`AGENTTEAMS_DEFAULT_WORKER_RUNTIME`），最终回退为 `openclaw`。

### 创建后切换

告诉 Manager 切换 Worker 的运行时：
> "把 alice 的运行时切换为 hermes"

Manager 会通过 worker-management 技能触发容器重建。Worker 的 Matrix 账号、房间、网关 Consumer、MinIO 数据和持久化凭据都会保留。容器本地临时状态（缓存、进行中的任务）会丢失。

---

## 为什么 QwenPaw 仍然使用 `copaw` 作为 runtime 值或镜像名

`QwenPaw` 是原 `CoPaw` 运行时的对外展示名称。为了兼容已有安装，部分内部名称会继续保留
`copaw`，包括 Worker CRD 的 runtime 值、`agentteams-copaw-worker` 这类镜像名，以及
`AGENTTEAMS_MANAGER_RUNTIME=copaw` 这类环境变量值。

除非 chart、controller 和镜像已经明确支持新的值，否则不要把这些内部值改成
`qwenpaw`。保留 `copaw` 是为了避免破坏已有配置、Helm values、镜像拉取和升级路径。

---

## 如何接入自己实现的 agent 作为 Worker

不能直接通过新增任意 `spec.runtime` 值来接入。当前 Worker CRD 只接受
`openclaw`、`copaw` 或 `hermes` 三种运行时。

大多数自定义 Worker 场景应通过 Worker package 或自定义镜像完成：把角色提示词、
skills、依赖和可选 Dockerfile 打包，或在保留受支持 runtime 的前提下设置自定义
image。具体见 [导入已有 Worker](import-worker.md)，以及
[声明式资源管理](declarative-resource-management.md#worker-资源) 中的
`spec.package` / `spec.image` 字段。

如果要新增一种完全不同的 runtime，需要修改 controller、runtime 默认镜像以及对应
agent 模板接线，不是纯配置项。

---

## AgentTeams 可以连接已有的 Higress 实例吗

当前不能通过 `gateway.provider=higress` 连接已有 Higress。Helm chart 会校验
`gateway.provider=higress` 必须搭配 `gateway.mode=managed`，也就是由 AgentTeams 部署并管理 Higress。

不建议把已有 Higress 配置目录直接复制到 AgentTeams 托管的 Higress 中。AgentTeams 会调和它需要的
AI routes、consumers 和网关资源，复制进去的资源可能和 AgentTeams 管理的状态冲突，或在安装/调和流程中被覆盖。

当前支持的路径是：

- 使用 AgentTeams 托管的 Higress 承载 AgentTeams 流量
- 在适用场景下使用外部 `ai-gateway` provider

如果要支持已有的自管 Higress，需要单独设计 external Higress 模式，包括 Gateway/Console URL、
访问凭据、资源命名隔离，以及避免影响已有 routes 和 consumers 的保护策略。

---

## 如何使用 Worker 模板市场

AgentTeams v1.1.0+ 包含基于 Nacos 的 Worker 模板市场。无需从零配置 Worker，可以直接导入预构建模板：

**通过 Manager 对话：**

告诉 Manager 你需要什么样的 Worker：
> "我需要一个擅长 React 前端开发的 Worker"

Manager 会搜索市场、推荐匹配的模板，经你确认后一键导入。

**通过 CLI：**

```bash
agt apply -f my-worker.yaml
```

在 YAML 中通过 `package` 引用市场模板。

---

## AgentTeams 支持发送和接收文件吗

**接收你发送的文件**：支持。在 Element Web 中点击附件按钮上传文件，Manager 或 Worker 会收到 Matrix 媒体消息并可以读取其内容。

**向你发送文件**：支持。当你要求 Manager（或 Worker）发送文件时——例如任务产物、生成的报告或它能访问的任意文件——它会将文件上传到 Matrix 媒体服务器，并以可下载附件的形式发送到房间中，你在 Element Web 里点击即可下载。

Manager 或 Worker 输出的路径通常是容器内部路径。如果你在宿主机上无法直接访问该路径，
请要求 Agent 把文件作为附件发送，或提供可下载链接，而不是依赖原始容器路径。

---

## 为什么 Manager/Worker 一直显示"输入中"

这是正常现象，说明底层的 Agent 引擎正在执行任务。AgentTeams 设定的单次任务超时时间为 30 分钟，Agent 最长会持续执行 30 分钟。

如果想查看 Agent 的执行细节，可以进入 Manager 或 Worker 容器，查看 session 日志：

```bash
# Manager
docker exec -it agentteams-manager ls .openclaw/agents/main/sessions/

# Worker（将 <worker-name> 替换为实际容器名）
docker exec -it <worker-name> ls .openclaw/agents/main/sessions/
```

该目录下的 `.jsonl` 文件实时写入，记录了完整的 Agent 执行过程，包括 LLM 调用、工具使用、推理步骤等。

> **注意**：Hermes 运行时的 Worker，session 数据存储在 `~/.hermes/state.db` 中。

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

可能是 session 损坏了。进入 Manager 或 Worker 容器，使用 OpenClaw TUI 查看：

```bash
# Manager
docker exec -it agentteams-manager openclaw tui

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

### 1. 检查容器状态

在新架构下，首先确认 Controller 和 Manager 容器都在运行：

```bash
docker ps | grep -E "agentteams-controller|agentteams-manager"
```

如果 `agentteams-manager` 未运行，查看 Controller 日志：

```bash
docker logs agentteams-controller
```

### 2. 检查 Session 状态

可能是 session 损坏了。进入 Manager 容器，使用 OpenClaw TUI 查看：

```bash
docker exec -it agentteams-manager openclaw tui
```

进入 TUI 后：
1. 输入 `/sessions` 查看所有 session
2. 切换到对应聊天记录的 session
3. 尝试对话，观察是否有报错

如果 session 确实损坏了，可以尝试在 Element 等 Matrix 客户端中对应的会话里直接输入 `/new` 重置会话，看是否恢复正常。

### 3. 检查 Higress AI 网关日志

如果重置 session 后问题仍然存在，查看 Higress AI 网关日志。在新架构下，Higress 运行在 Controller 容器内：

```bash
docker exec -it agentteams-controller cat /var/log/agentteams/higress-gateway.log
```

在日志中搜索对应的状态码，常见原因：

- **503**：容器内网络环境问题，导致外网 LLM 服务不可达。
- **404**：模型名称填写有误。

要判断是后端服务出错还是 Higress 自身配置问题，查看日志中的 `upstream_host` 字段：如果该字段是真实 host，说明请求已到达后端，异常状态码是由上游服务返回的；如果是 `-` 或为空，说明 Higress 没有选中上游集群；日志里出现 `response_code_details: cluster_not_found` 时，通常表示模型路由或服务来源配置不正确。

自部署 OpenAI 兼容服务时，检查 Higress 的供应商配置是否指向真实 URL，而不是不存在的服务名。同时在容器内用相同 base URL 和 API key 验证上游是否可达。

### 4. 检查模型配置

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

## 如何查看 Manager Agent 日志

在新架构（v1.1.0+）下，Manager 作为独立容器运行：

```bash
# Manager Agent 日志（stdout/stderr）
docker logs agentteams-manager

# Manager Agent session 日志（详细执行过程）
docker exec -it agentteams-manager ls .openclaw/agents/main/sessions/

# Controller / 基础设施日志
docker logs agentteams-controller

# Higress 网关日志（在 Controller 容器内）
docker exec -it agentteams-controller cat /var/log/agentteams/higress-gateway.log

# Higress Console / 控制台后端日志（v1.1.0+ 嵌入式 — 同样在 Controller）
docker exec -it agentteams-controller cat /var/log/agentteams/higress-console.log
```

OpenClaw Control UI（可视化 session 检查），打开：

```
http://localhost:18888
```

---

## 如何对接飞书/钉钉/企业微信/Discord/Telegram

AgentTeams Manager 基于 OpenClaw 构建，原生支持多种消息渠道。要对接其他渠道：

**方法一：直接修改配置**

Manager 的工作目录是宿主机上的 `~/agentteams-manager`，里面的 `openclaw.json` 可以直接编辑。参照 [OpenClaw 渠道文档](https://docs.openclaw.ai) 中各平台的配置格式进行配置。

修改后重启 Manager 容器使配置生效：

```bash
docker restart agentteams-manager
```

**方法二：让 Manager 学习你现有的 OpenClaw 配置**

如果你已经在其他地方使用 OpenClaw 接入了其他渠道，可以让 Manager 读取你现有的配置：

- **告诉 Manager 文件位置**：在 Element Web 中告诉 Manager 你的 OpenClaw 配置文件路径（例如 "我的 OpenClaw 配置在 `/home/user/my-openclaw.json`"），Manager 会直接读取。
- **通过附件发送**：在 Element Web 或其他 Matrix 客户端中，把配置文件作为附件上传发送给 Manager，Manager 会接收并读取。

然后让 Manager 帮你在它的配置里添加相同的渠道。

---

## 会话管理（通过 IM 指令）

AgentTeams 基于 OpenClaw，通过 Matrix 渠道（Element Web）与 Agent 通信。OpenClaw 支持**斜杠命令**，你可以直接在聊天中以独立消息的形式发送这些指令，由 Gateway 在模型处理前解析执行。

**注意：** 大多数命令必须以**独立消息**发送，且以 `/` 开头。不要在同一则消息中混入其他文字。

**群聊中使用：** 可以在同一条消息中组合 @提及和斜杠命令，例如 `@Worker /compact` 或 `@Worker /new`。@提及确保命令发送给正确的 Agent，斜杠命令仍由 Gateway 正常处理。

以下以 OpenClaw（Manager 和 OpenClaw Worker）为例列出可用命令。**QwenPaw** Worker 使用不同的命令集，详见 [QwenPaw 命令参考](https://copaw.agentscope.io/docs/commands)。

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
