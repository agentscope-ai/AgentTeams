# AgentTeams 本地部署指南

[English](../../../usage/deployment/local.md) | 中文

本指南介绍如何在一台 macOS 或 Linux 主机上创建和维护 AgentTeams 实例，包括快速开始、手动配置、非交互式安装、安装选项、验证、升级和卸载。

如果你只想尽快完成第一个任务，请先阅读[快速入门](../../quickstart.md)。Windows 用户请使用独立的 [Windows 部署指南](windows.md)。

## 本地部署会创建什么

当前本地安装默认使用 embedded 架构：

| 名称 | 对象类型 | 创建时机与用途 |
|---|---|---|
| `agentteams-controller` | 容器 | 安装时创建；在一个容器中承载 Higress、Tuwunel、MinIO、Element Web 和 controller。 |
| `agentteams-manager` | 容器 | 安装时由 controller 创建；运行 Manager Agent。 |
| `agentteams-worker-<name>` | 容器 | 创建 Worker 后按需启动；每个 Worker 使用独立容器。 |
| `agentteams-dashboard` | 容器 | 可选的管理界面；Bash 安装器默认启用。 |
| `agentteams-net` | 容器网络 | 安装时创建；连接 controller、Manager、Worker 和 Dashboard。 |
| `agentteams-data` | 数据卷 | 安装时创建；持久化 Matrix、MinIO、Higress 和 controller 数据。 |
| `agentteams-dashboard-data` | 数据卷 | 启用 Dashboard 时创建；保存 Dashboard 数据。 |

`docker ps` 只显示容器，因此不会列出 `agentteams-net` 和 `agentteams-data`。分别使用下面的命令查看容器、网络和数据卷：

```bash
docker ps --filter name=agentteams
docker network ls | grep agentteams-net
docker volume ls | grep agentteams
```

Manager 和 Worker 不把持久状态只保存在容器内。Agent 配置、Matrix 数据和共享文件由 embedded controller 内的服务及挂载的数据卷保存。删除或重建容器不会自动删除数据卷；执行 AgentTeams 卸载流程则会删除这些持久数据。

## 前置条件

### 支持的平台

- macOS：推荐 Docker Desktop，也可以使用兼容的 Podman 环境。
- Linux：推荐 Docker Engine；安装器也会检测 Podman。
- Windows：使用 PowerShell 安装器，参见 [Windows 部署指南](windows.md)。

安装器要求容器运行时已经启动，并且当前用户可以执行 `docker ps` 或 `podman ps`。

### 资源要求

| 资源 | 最低配置 | 建议配置 |
|---|---:|---:|
| CPU | 2 核 | 4 核或更多 |
| 内存 | 4 GB | 8 GB 或更多 |
| 磁盘 | 10 GB 可用空间 | 根据镜像、任务产物和 Worker 数量预留更多空间 |

运行多个 Worker、使用 OpenClaw runtime 或执行构建任务时，需要更多内存和磁盘。

### 网络与凭证

准备以下信息：

- 一个可用的 LLM API Key。
- 对应的 Base URL 和模型 ID；使用安装器内置的阿里云选项时由安装器填写默认地址。
- 可选的 GitHub Personal Access Token，用于 GitHub MCP 场景。
- 拉取 AgentTeams 镜像和访问模型服务所需的网络连接。

### 默认端口

| 端口 | 用途 | 对应变量 |
|---:|---|---|
| `18080` | Higress Gateway；Matrix、模型和文件服务的网关入口 | `AGENTTEAMS_PORT_GATEWAY` |
| `18001` | Higress Console | `AGENTTEAMS_PORT_CONSOLE` |
| `18088` | Element Web 直接访问端口 | `AGENTTEAMS_PORT_ELEMENT_WEB` |
| `18888` | Manager Console；仅在相应 runtime/架构暴露该控制台时使用 | `AGENTTEAMS_PORT_MANAGER_CONSOLE` |
| `13000` | AgentTeams Dashboard | `AGENTTEAMS_PORT_DASHBOARD` |

安装前可以检查端口是否已被占用：

```bash
lsof -nP -iTCP:18080 -sTCP:LISTEN
lsof -nP -iTCP:18001 -sTCP:LISTEN
lsof -nP -iTCP:18088 -sTCP:LISTEN
lsof -nP -iTCP:13000 -sTCP:LISTEN
```

Linux 没有 `lsof` 时，可以使用 `ss -lnt`。

## 选择安装方式

| 方式 | 适用场景 | 行为 |
|---|---|---|
| **快速开始** | 第一次体验 | 询问版本、模型、API Key 和运行时；其余选项使用推荐默认值。 |
| **手动配置** | 需要自定义网络、存储、Dashboard 或安全选项 | 按顺序展示完整交互选项。 |
| **非交互式安装** | 自动化、CI 或重复部署 | 通过环境变量提供配置，不等待终端输入。 |
| **升级已有实例** | 已存在配置文件和容器 | 保留全部配置，或逐项确认新配置。 |

## 快速开始

运行：

```bash
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

选择“快速开始”后，安装器仍会让你完成以下选择：

1. 安装 `latest`、最新稳定版或指定版本。
2. 选择模型服务和模型，并输入 LLM API Key。
3. 选择 Manager runtime：QwenPaw（推荐）、OpenClaw 或旧版 CoPaw。
4. 选择默认 Worker runtime：QwenPaw（推荐）、OpenClaw、支持版本中的 Hermes 或旧版 CoPaw。

其余配置采用以下默认值：

| 配置 | 快速开始默认行为 |
|---|---|
| 管理员 | 用户名 `admin`，密码自动生成 |
| 网络 | 仅绑定本机访问 |
| 端口 | 使用默认端口表中的值 |
| 域名 | 使用 `*-local.agentteams.io` 默认域名 |
| GitHub Token | 不配置 |
| Skills Registry | 使用系统默认值 |
| 数据卷 | `agentteams-data` |
| Manager 工作空间 | `${HOME}/agentteams-manager` |
| Dashboard | 启用，端口 `13000` |
| Matrix E2EE | 关闭 |
| Worker 空闲休眠 | 720 分钟 |
| 宿主机共享目录 | 当前用户主目录 |

安装完成后，终端会打印访问地址、管理员凭证和数据位置。请保存这些信息。

## 手动配置流程

手动配置会按照下面的顺序执行。输入 `b` 可以返回上一个已经执行的步骤。

### 1. 语言和安装模式

安装器根据时区推测中文或英文，你可以在第一步切换。语言会影响交互提示和非交互模式下的部分模型默认值。

### 2. 安装版本

可以选择：

- `latest`：跟随最新镜像标签。
- 最新稳定版本：安装器从 GitHub Release 获取版本；获取失败时使用脚本内置的稳定版本。
- 自定义版本：输入 `vX.Y.Z` 等具体标签。

生产或可重复环境建议固定版本，不要依赖 `latest`。

### 3. 已有实例处理

如果安装器发现 `${HOME}/agentteams-manager.env`，会提供：

- **升级**：保留数据；可以保留全部配置，也可以逐项确认。
- **全新重装**：停止实例并删除数据卷、配置文件、Manager 工作空间、Worker 和网络。
- **取消**：不做修改并退出。

全新重装会要求输入现有工作空间的完整路径作为二次确认。

### 4. 模型服务

中文交互提供阿里云模型服务和 OpenAI 兼容 API 两类入口。根据所选入口，安装器会收集或确定：

- `AGENTTEAMS_LLM_PROVIDER`：选择 AgentTeams 使用的服务适配方式，而不是任意填写的厂商名称。
  - `qwen`：使用阿里云百炼/Qwen 的内置接入方式。
  - `openai-compat`：使用 OpenAI 兼容协议，适用于 OpenAI、百炼 Token Plan、DeepSeek、自建模型网关等服务。
- `AGENTTEAMS_OPENAI_BASE_URL`：OpenAI 兼容 API 的基础地址。填写服务根路径，不要填写完整的 `/chat/completions` 请求地址；通常以 `/v1` 结尾。使用 `qwen` 内置接入方式时一般留空。
- `AGENTTEAMS_LLM_API_KEY`：调用模型服务所需的密钥，是必填敏感信息。安装器用它执行连通性检查，并将其写入本地配置供网关使用。
- `AGENTTEAMS_DEFAULT_MODEL`：Manager 和新建 Worker 默认使用的模型 ID，必须与服务商 API 接受的模型名称完全一致。之后仍可以为具体 Manager 或 Worker 单独覆盖。
- `AGENTTEAMS_EMBEDDING_MODEL`：用于记忆检索等向量化场景的模型 ID。默认尝试 `text-embedding-v4`；设置为空字符串表示关闭 Embedding。该模型必须能使用当前凭证和对应 API 地址访问。

这些变量的关系可以概括为：Provider 决定接入逻辑，Base URL 决定请求发往哪里，API Key 提供访问凭证，Default Model 决定生成任务使用哪个模型，Embedding Model 则独立控制是否启用向量化能力。

阿里云百炼通用接口示例：

```bash
AGENTTEAMS_LLM_PROVIDER=qwen
AGENTTEAMS_OPENAI_BASE_URL=
AGENTTEAMS_LLM_API_KEY=sk-example
AGENTTEAMS_DEFAULT_MODEL=qwen3.6-plus
AGENTTEAMS_EMBEDDING_MODEL=text-embedding-v4
```

DeepSeek 等 OpenAI 兼容服务示例：

```bash
AGENTTEAMS_LLM_PROVIDER=openai-compat
AGENTTEAMS_OPENAI_BASE_URL=https://api.deepseek.com/v1
AGENTTEAMS_LLM_API_KEY=sk-example
AGENTTEAMS_DEFAULT_MODEL=deepseek-chat
AGENTTEAMS_EMBEDDING_MODEL=
```

第二个示例显式关闭了 Embedding。如果兼容服务提供 Embedding API，可以将空值替换为该服务实际支持的 Embedding 模型 ID。

使用 OpenAI 兼容服务时，Base URL 通常需要包含 `/v1`。安装器会测试模型连通性；测试失败时应检查 API Key、Base URL、模型 ID、余额和网络代理。

对于安装器未识别的自定义模型，还会询问以下能力参数，用于生成 runtime 的模型配置：

| 变量 | 意义 | 示例 |
|---|---|---|
| `AGENTTEAMS_MODEL_CONTEXT_WINDOW` | 模型上下文窗口的最大 Token 数 | `150000` |
| `AGENTTEAMS_MODEL_MAX_TOKENS` | 单次响应允许的最大输出 Token 数 | `128000` |
| `AGENTTEAMS_MODEL_REASONING` | 模型是否支持 reasoning 模式 | `true` 或 `false` |
| `AGENTTEAMS_MODEL_VISION` | 模型是否支持图片输入 | `true` 或 `false` |

这些值必须依据服务商的真实模型规格填写。填写过大会导致请求被上游拒绝，错误声明 reasoning 或 vision 能力也可能生成不兼容的请求。

Embedding 模型可以使用默认值、自定义或关闭。连通性检查失败时，安装器会自动禁用 Embedding，避免阻塞主安装流程。

### 5. Manager runtime

| 选项 | 安装器配置值 | 说明 |
|---|---|---|
| QwenPaw | `qwenpaw` | 推荐的 Python Manager runtime，也是本地安装器的默认选项。 |
| OpenClaw | `openclaw` | Node.js Manager runtime。 |
| CoPaw | `copaw` | 旧版 Python runtime，建议升级为 QwenPaw。 |

Manager runtime 决定 Manager 镜像和运行方式。修改已有 Manager runtime 会重建 Manager 容器，应在没有进行中任务时操作。

### 6. 默认 Worker runtime

本地安装器当前提供：

| 选项 | 配置值 | 说明 |
|---|---|---|
| QwenPaw | `qwenpaw` | 推荐默认项，当前 Python runtime。 |
| OpenClaw | `openclaw` | Node.js runtime。 |
| Hermes | `hermes` | 从支持该 runtime 的版本开始显示。 |
| CoPaw | `copaw` | 旧版 Python runtime，建议升级为 QwenPaw。 |

该选项只设置后续创建 Worker 时的默认 runtime。创建 Worker 时仍可以显式选择 controller 支持的其他 runtime。

### 7. 管理员账号

- 用户名默认是 `admin`，安装器会转换为小写。
- 密码至少 8 个字符。
- 留空时自动生成密码并在安装完成后打印。

管理员凭证用于登录 Matrix/Element，并可能用于 Higress Console 和 Dashboard 的共享认证。不要在公开日志或聊天中发送该密码。

### 8. 访问范围

| 选项 | `AGENTTEAMS_LOCAL_ONLY` | 行为 |
|---|---:|---|
| 仅本机访问 | `1` | 服务绑定到本机地址，适合个人体验。 |
| 允许外部访问 | `0` | 相关入口绑定到 `0.0.0.0`，可从局域网或外部网络访问。 |

允许外部访问不会自动完成公网 DNS、TLS、反向代理或防火墙配置。多人或公网使用时必须配置 HTTPS，并限制 Higress Console、controller API 和存储管理接口的访问。

### 9. 端口和域名

手动模式可以修改默认端口，还可以配置：

| 用途 | 默认域名 |
|---|---|
| Matrix Homeserver | `matrix-local.agentteams.io:<gateway-port>` |
| Element Web 网关域名 | `matrix-client-local.agentteams.io` |
| AI Gateway | `aigw-local.agentteams.io` |
| 文件服务 | `fs-local.agentteams.io` |
| OpenClaw Console | `console-local.agentteams.io` |

端口必须在宿主机上唯一。自定义域名需要确保客户端和容器能够正确解析；外部访问还需要配置 DNS、反向代理和证书。

### 10. GitHub 集成

`AGENTTEAMS_GITHUB_TOKEN` 是可选项。配置后可以供 GitHub MCP 场景使用。建议：

- 使用专用 Token，而不是个人长期高权限 Token。
- 只授予任务所需仓库和权限。
- 定期轮换 Token。
- 不要把 Token 提交到仓库或粘贴到 Matrix 房间。

### 11. Skills Registry

`AGENTTEAMS_SKILLS_API_URL` 用于配置 Skills 注册中心。留空时使用系统默认的 Nacos 市场地址。私有注册中心还可以通过环境变量配置 Nacos 用户名、密码或 Token。

### 12. 数据卷和工作空间

| 配置 | 默认值 | 保存内容 |
|---|---|---|
| `AGENTTEAMS_DATA_DIR` | `agentteams-data` | Matrix、MinIO、Higress 和 controller 的持久数据。当前实现将它作为容器卷名称使用。 |
| `AGENTTEAMS_WORKSPACE_DIR` | `${HOME}/agentteams-manager` | Manager 的配置、Skills、记忆和本地工作文件。 |
| `AGENTTEAMS_HOST_SHARE_DIR` | `${HOME}` | 挂载到 Manager 的 `/host-share`，用于访问明确共享的宿主机文件。 |

不要把工作空间设置到临时目录。宿主机共享目录应尽可能缩小范围，避免把不需要的凭证和私人文件暴露给 Agent runtime。

### 13. Dashboard

Bash 安装器默认安装 Dashboard。手动模式可以设置：

- 是否启用 Dashboard。
- Dashboard 独立版本。
- 宿主机端口，默认 `13000`。
- Dashboard 镜像。
- 用于共享认证的 Higress Console 地址。

Dashboard 仅支持当前 embedded controller 架构。PowerShell 安装器当前不安装 Dashboard。

Dashboard 启用后，可以进入**资源中心 → 市场**；如果 Skill 尚未入库，先点击**上传技能**完成上传，再从目标 Skill 所在行点击**分发到 Worker**并选择一个或多个 Worker。也可以从 **Workers → 目标 Worker → 详情 → 上传技能包**直接为单个 Worker 上传 Skill ZIP。两种路径都会把完整包写入 Worker 的持久化 Skill 目录、更新 `Worker.spec.skills` 并尝试重新加载 Worker；详细的包格式、加载影响和验证方式参见 [Worker 指南：通过 Dashboard 分发](../worker-guide.md#方式二通过-dashboard-分发)。

### 14. Matrix E2EE

`AGENTTEAMS_MATRIX_E2EE` 默认是 `0`，即关闭。启用前应确认所选 Manager、Worker runtime 和 Matrix 客户端都支持当前端到端加密工作流。快速开始和非交互模式默认关闭。

### 15. 容器 Socket 与 Docker Proxy

`AGENTTEAMS_MOUNT_SOCKET=1` 默认允许 controller 访问容器运行时，以便直接创建和管理 Worker。

手动模式还可以启用受限 Docker Proxy，并通过 `AGENTTEAMS_PROXY_ALLOWED_REGISTRIES` 增加允许的镜像来源。关闭 Socket 挂载后，controller 无法使用本机容器后端自动创建 Worker。

直接挂载 Docker/Podman Socket 等同于授予很高的宿主机权限。只应在可信主机上使用，并限制谁能够操作 Manager 和 controller。

### 16. Worker 空闲休眠

`AGENTTEAMS_WORKER_IDLE_TIMEOUT` 控制 Worker 在空闲多少分钟后进入休眠，默认 `720`，即 12 小时。缩短时间可以减少资源占用，但会增加唤醒频率。

### 17. Podman 自动启动

在使用 Podman 且系统提供 systemd 时，安装器可以配置 AgentTeams 自动启动。Docker 环境不会显示该选项。

## 非交互式安装

自动化场景可以使用环境变量跳过全部提示：

```bash
AGENTTEAMS_NON_INTERACTIVE=1 \
AGENTTEAMS_LANGUAGE=zh \
AGENTTEAMS_LLM_PROVIDER=openai-compat \
AGENTTEAMS_OPENAI_BASE_URL=https://provider.example.com/v1 \
AGENTTEAMS_LLM_API_KEY=sk-example \
AGENTTEAMS_DEFAULT_MODEL=example-model \
AGENTTEAMS_ADMIN_PASSWORD=replace-with-a-strong-password \
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

非交互模式下，必填项缺失或管理员密码不足 8 个字符会直接失败。示例中的凭证是占位符，不要原样使用。

### 常用环境变量

| 变量 | 默认值或行为 |
|---|---|
| `AGENTTEAMS_NON_INTERACTIVE` | `0`；设为 `1` 跳过提示 |
| `AGENTTEAMS_LANGUAGE` | 根据时区推测 `zh` 或 `en` |
| `AGENTTEAMS_VERSION` | 交互安装默认选择 `latest`；非交互安装默认使用脚本内置的稳定版本 |
| `AGENTTEAMS_REGISTRY` | 根据时区选择镜像仓库 |
| `AGENTTEAMS_LLM_PROVIDER` | 非交互默认值与语言有关 |
| `AGENTTEAMS_OPENAI_BASE_URL` | OpenAI 兼容服务地址 |
| `AGENTTEAMS_LLM_API_KEY` | 必填 |
| `AGENTTEAMS_DEFAULT_MODEL` | 默认模型 ID |
| `AGENTTEAMS_EMBEDDING_MODEL` | 默认 Embedding 模型；空字符串表示关闭 |
| `AGENTTEAMS_ADMIN_USER` | `admin` |
| `AGENTTEAMS_ADMIN_PASSWORD` | 未设置时自动生成；显式设置时至少 8 个字符 |
| `AGENTTEAMS_LOCAL_ONLY` | `1` |
| `AGENTTEAMS_MANAGER_RUNTIME` | `qwenpaw` |
| `AGENTTEAMS_DEFAULT_WORKER_RUNTIME` | `qwenpaw` |
| `AGENTTEAMS_MATRIX_E2EE` | `0` |
| `AGENTTEAMS_MOUNT_SOCKET` | `1` |
| `AGENTTEAMS_DOCKER_PROXY` | `1`；启用受限的容器运行时代理 |
| `AGENTTEAMS_DATA_DIR` | `agentteams-data` |
| `AGENTTEAMS_WORKSPACE_DIR` | `${HOME}/agentteams-manager` |
| `AGENTTEAMS_HOST_SHARE_DIR` | `${HOME}` |
| `AGENTTEAMS_WORKER_IDLE_TIMEOUT` | `720` 分钟 |
| `AGENTTEAMS_DASHBOARD` | `1` |
| `AGENTTEAMS_PORT_DASHBOARD` | `13000` |

完整变量仍以 [`install/agentteams-install.sh`](../../../../install/agentteams-install.sh) 文件头部和各安装步骤为准。

### 镜像覆盖

本地构建、私有 Registry 或预发布验证可以使用：

- `AGENTTEAMS_INSTALL_EMBEDDED_IMAGE`
- `AGENTTEAMS_INSTALL_MANAGER_IMAGE`
- `AGENTTEAMS_INSTALL_MANAGER_QWENPAW_IMAGE`
- `AGENTTEAMS_INSTALL_MANAGER_COPAW_IMAGE`
- `AGENTTEAMS_INSTALL_WORKER_IMAGE`
- `AGENTTEAMS_INSTALL_COPAW_WORKER_IMAGE`
- `AGENTTEAMS_INSTALL_QWENPAW_WORKER_IMAGE`
- `AGENTTEAMS_INSTALL_HERMES_WORKER_IMAGE`
- `AGENTTEAMS_DASHBOARD_IMAGE`

镜像版本需要与 controller 和资源协议兼容。不要任意混用不同版本的 controller、Manager 和 Worker 镜像。

## 安装后验证

### 1. 检查容器

```bash
docker ps --filter name=agentteams-controller
docker ps --filter name=agentteams-manager
docker ps --filter name=agentteams-dashboard
```

Dashboard 是可选组件，未启用时不会出现。

### 2. 检查资源

```bash
docker exec agentteams-controller agt get managers
docker exec agentteams-controller agt get workers
```

安装完成时 Worker 列表可以为空。创建 Worker 后应看到 `Running` 或正在准备的状态。

### 3. 检查服务

```bash
curl -fsS http://127.0.0.1:18088/ >/dev/null
curl -fsS http://127.0.0.1:18001/ >/dev/null
docker exec agentteams-controller curl -fsS http://127.0.0.1:9000/minio/health/live
```

如果修改了端口，请替换命令中的默认值。

### 4. 登录

默认入口：

| 服务 | 地址 |
|---|---|
| Element Web | `http://127.0.0.1:18088/#/login` |
| Higress Console | `http://127.0.0.1:18001` |
| Dashboard | `http://127.0.0.1:13000` |

使用安装完成时打印的管理员凭证登录 Element Web，然后按照[快速入门](../../quickstart.md)创建第一个 Worker。

## 配置和数据位置

| 内容 | 默认位置 |
|---|---|
| 安装配置 | `${HOME}/agentteams-manager.env` |
| 安装日志 | `${HOME}/agentteams-install.log` |
| Manager 工作空间 | `${HOME}/agentteams-manager` |
| 持久数据 | Docker/Podman 卷 `agentteams-data` |
| Dashboard 数据 | 卷 `agentteams-dashboard-data` |

安装配置中包含敏感信息。应限制文件权限，不要提交到 Git，也不要直接附在 Issue 中。

## 升级

重新运行安装器会检测现有配置，并进入升级流程：

```bash
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

升级模式提供：

- **保留全部配置**：沿用现有环境文件中的设置。
- **逐项确认**：在每一步显示当前值并允许修改。

固定目标版本：

```bash
AGENTTEAMS_VERSION=vX.Y.Z \
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

升级会重启 Manager，并可能重建 Worker 容器。持久数据会保留，但应先结束进行中的任务并备份重要数据。

## 卸载

> 警告：卸载会删除 AgentTeams 容器、Worker、网络、数据卷、Dashboard 数据、环境文件、安装日志和 Manager 工作空间。

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh) uninstall
```

如果只想停止服务而保留数据，请使用容器运行时停止相关容器，不要执行卸载命令。

## 故障排查

### 容器未启动

```bash
docker ps -a --filter name=agentteams
docker logs --tail 200 agentteams-controller
docker logs --tail 200 agentteams-manager
```

### 模型检查失败

确认：

- API Key 没有多余空格且仍然有效。
- Base URL 满足服务商要求，通常包含 `/v1`。
- 模型 ID 存在且账号有访问权限。
- 宿主机和容器能够访问模型服务。
- 账号余额、配额和并发限制正常。

### 端口冲突

重新运行安装器并选择手动配置，修改对应的 `AGENTTEAMS_PORT_*` 变量。升级已有实例时选择“逐项确认”。

### Manager 或 Worker 一直处于准备状态

```bash
docker exec agentteams-controller agt get managers -o json
docker exec agentteams-controller agt get workers -o json
docker logs --tail 300 agentteams-controller
```

更多问题参见 [FAQ](../troubleshooting/faq.md)。
