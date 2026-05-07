# HiClaw 快速入门指南

本指南带你完成 HiClaw 的安装、创建第一个 Agent 团队，并完成第一个协作任务。每个步骤都包含验证检查点，确认一切正常运行。

## 前置条件

- 已安装并运行 Docker
- 一个 LLM API Key（如阿里云百炼 Qwen、OpenAI 等）
- （可选）GitHub 个人访问令牌（PAT），用于 GitHub 协作功能

---

## 第一步：安装 Manager 并登录 IM

### 1.1 运行安装脚本

**方式 A：一键安装**

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

按照交互提示配置：
- LLM 提供商和 API Key
- 管理员用户名和密码
- 域名（直接回车使用默认值）
- GitHub PAT（可选）

**方式 B：使用 Make（适合已克隆仓库的开发者）**

```bash
# 最简安装 —— 只需 LLM Key，其余全部使用默认值
HICLAW_LLM_API_KEY="sk-xxx" make install
```

此命令会在本地构建镜像、挂载容器运行时 socket（用于直接创建 Worker），并将配置保存到 `./hiclaw-manager.env`。

两种方式均支持通过环境变量覆盖所有配置项，完整列表见 `install/hiclaw-install.sh` 文件头部注释。

### 1.1a 多容器架构（v1.1.0+ 嵌入式安装）

默认**嵌入式**安装会启动两个主容器（详见 [architecture.md](../architecture.md)）：

| 容器 | 职责 |
|------|------|
| **`hiclaw-controller`** | 内嵌 Higress、Tuwunel、MinIO、Element Web 与 Go controller（REST API 在容器网络内 **8090** 端口）。 |
| **`hiclaw-manager`** | 仅运行 Manager Agent（默认 OpenClaw；若安装时选择 `HICLAW_MANAGER_RUNTIME=copaw` 则为 QwenPaw Manager 镜像）。 |

创建 Worker 后会出现 `hiclaw-worker-*`、`hiclaw-copaw-worker-*`、`hiclaw-hermes-worker-*` 等容器。

**声明式 CLI（无需在 IM 里打字）：** `hiclaw` 在 **`hiclaw-controller`** 与 **`hiclaw-manager`** 内均可用。宿主机上快速示例：

```bash
docker exec hiclaw-controller hiclaw create worker --name alice --model qwen3.5-plus
docker exec hiclaw-controller hiclaw get workers
```

YAML 批量管理请使用 `install/hiclaw-apply.sh`（将文件拷入 `hiclaw-manager` 后执行 `hiclaw apply -f`）。详见 [Declarative Resource Management](../declarative-resource-management.md)。

### 1.2 登录 Element Web

在浏览器中打开 http://127.0.0.1:18088（直接访问端口）。如果已将域名添加到 `/etc/hosts`，也可通过网关访问 http://matrix-client-local.hiclaw.io:18080。

使用管理员凭据登录。

### 验证清单

- [ ] **`hiclaw-controller`** 正在运行：`docker ps | grep hiclaw-controller`
- [ ] **`hiclaw-manager`** 正在运行：`docker ps | grep hiclaw-manager`
- [ ] 浏览器可访问 Element Web：http://127.0.0.1:18088
- [ ] 使用管理员凭据登录成功
- [ ] Higress 控制台：http://localhost:18001（网关默认映射到宿主机 **18080**；Matrix / Element 的 `*-local.hiclaw.io` 经该网关访问）
- [ ] MinIO 在 **controller 容器内**可访问（嵌入式安装默认**不**把 MinIO 控制台端口发布到宿主机）：`docker exec hiclaw-controller curl -sf http://127.0.0.1:9000/minio/health/live`
- [ ] （仅 OpenClaw Manager）OpenClaw 控制 UI：http://127.0.0.1:18888

---

## 第二步：创建 Worker Alice

### 2.1 与 Manager 对话

**方式 A：通过 Element Web（图形界面）**

在 Element Web 中，向 `manager` 用户发起私信（DM）。

发送：
> 请为我创建一个名为 alice 的 Worker，负责前端开发任务。她需要有 GitHub MCP 访问权限。

**方式 B：通过命令行（make replay）**

```bash
make replay TASK="请为我创建一个名为 alice 的 Worker，负责前端开发任务。她需要有 GitHub MCP 访问权限。"
```

此命令通过 Matrix API 发送消息，并在终端等待 Manager 的回复。

### 2.2 等待 Manager 响应

Manager Agent 将会：
1. 注册 `alice` 的 Matrix 账号
2. 在 Higress 中创建 `worker-alice` Consumer（含 key-auth 凭据）
3. 在 MinIO 中生成 Alice 的配置文件
4. 创建 Matrix 房间（你、Manager 和 Alice 三方）
5. 启动 Worker（直接创建或输出安装命令，取决于是否挂载了容器运行时 socket）

### 2.3 启动 Worker Alice

有两种方式启动 Worker：

**方式 A：直接创建（本地部署，推荐）**

如果你要求 Manager "直接创建"，Manager 会通过挂载的容器运行时 socket 自动在宿主机上创建并启动 Worker 容器，无需手动操作。

> 需要使用 `make install`（会自动挂载 socket），或在启动 Manager 容器时手动挂载 Docker/Podman socket。

**方式 B：Docker Run 命令（远程或手动部署）**

如果 Manager 没有容器运行时 socket 访问权限，它会回复一条 `docker run` 命令。将其复制到目标主机上运行：

```bash
docker run -d --name hiclaw-worker-alice \
  -e HICLAW_WORKER_NAME=alice \
  -e HICLAW_FS_ENDPOINT=http://<MANAGER_HOST>:9000 \
  -e HICLAW_FS_ACCESS_KEY=<ACCESS_KEY> \
  -e HICLAW_FS_SECRET_KEY=<SECRET_KEY> \
  hiclaw/worker-agent:latest
```

Manager 的回复中会提供所有具体参数值。

### 验证清单

- [ ] Alice 的房间出现在 Element Web 中（3 名成员：你、manager、alice）
- [ ] Higress 控制台显示 `worker-alice` Consumer（http://localhost:18001）
- [ ] MinIO 中存在 `agents/alice/SOUL.md` 文件（可通过 MinIO 控制台或 `mc ls` 查看）
- [ ] Worker 容器正在运行：`docker ps | grep hiclaw-worker-alice`

---

## 第三步：给 Alice 分配任务

### 3.1 在 Alice 的房间发送任务

在 Element Web 中打开 Alice 的房间，发送：

> Alice，请为一个 hello-world 项目创建一个简单的 README.md，包含项目名称、描述和使用说明。将结果保存到共享任务文件夹。

### 3.2 观察任务执行过程

在房间中观察：
1. Manager 接收并转发任务
2. 任务元数据和规格出现在 MinIO（`shared/tasks/{task-id}/meta.json` 和 `spec.md`）
3. Alice 开始处理任务
4. Alice 写入结果（`shared/tasks/{task-id}/result.md`）
5. Alice 在房间中通知完成
6. Manager 将 `meta.json` 状态更新为 `completed`

### 验证清单

- [ ] Manager 在 MinIO 中创建了任务 `meta.json` 和 `spec.md`
- [ ] Alice 确认并开始工作
- [ ] Alice 在房间中发布进度更新
- [ ] 结果文件出现在 MinIO 共享任务目录
- [ ] Alice 在房间中通知完成
- [ ] 任务 `meta.json` 状态更新为 `completed`

---

## 第四步：人工介入进行中的任务

### 4.1 分配新任务

在 Alice 的房间中发送：

> Alice，写一个打印 'Hello, World!' 的 Python 脚本，保存为 hello.py。

### 4.2 发送补充指令

在 Alice 工作期间，发送额外指令：

> 补充需求：脚本还需要接受一个命令行参数作为名字，打印 'Hello, <name>!' 而不是固定的 World。

### 4.3 观察整合结果

Alice 和 Manager 应当将原始需求和补充需求都纳入最终结果。

### 验证清单

- [ ] Manager 转发了原始指令和补充指令
- [ ] Alice 确认了补充需求
- [ ] 最终结果同时包含原始功能和补充功能

---

## 第五步：观察心跳机制

### 5.1 分配一个耗时较长的任务

发送一个需要一定时间完成的任务。

### 5.2 等待心跳周期

Manager Agent 会定期执行心跳检查（由 OpenClaw 内置心跳机制触发）。心跳期间：
- Manager 检查每个 Worker 房间的最近活动
- 对于有分配任务的 Worker，Manager 询问进度
- 询问消息在房间中对所有人可见

### 验证清单

- [ ] Manager 在 Alice 的房间中发送了状态询问消息
- [ ] Alice 回复了当前进度
- [ ] 人工管理员可以在房间中看到完整的交流过程

---

## 第六步：创建 Worker Bob 并协作

### 6.1 创建 Worker Bob

在与 Manager 的私信中发送：

> 创建一个名为 bob 的 Worker，负责后端开发。他需要有 GitHub MCP 访问权限。

### 6.2 启动 Bob

按照与 Alice 相同的流程操作（参考第二步）。

### 6.3 分配协作任务

在与 Manager 的私信中发送：

> 我需要 Alice 和 Bob 协作：Alice 负责创建前端 HTML 页面，Bob 负责创建后端 API。他们通过 MinIO 中的共享文件进行协调。

### 验证清单

- [ ] Bob 的房间出现在 Element Web 中（3 名成员）
- [ ] Higress 控制台显示 `worker-bob` Consumer
- [ ] Manager 将任务拆分给 Alice 和 Bob
- [ ] 两个 Worker 分别在各自房间中汇报进度
- [ ] MinIO 中出现共享协调文件

---

## 第七步：通过 MCP 进行 GitHub 操作

> **注意**：此步骤需要在 Manager 安装时配置 GitHub PAT。

### 7.1 分配 GitHub 任务

在 Alice 的房间中发送：

> Alice，请执行以下 GitHub 操作：1）读取测试仓库的 README.md，2）创建名为 'feature/alice-update' 的分支，3）创建新文件 docs/quickstart-update.md，4）创建 Pull Request。

### 7.2 观察 MCP 工具调用

Alice 使用 `mcporter` 调用 Higress 托管的 GitHub MCP Server。MCP Server 集中保存 GitHub PAT —— Alice 永远看不到它。

### 验证清单

- [ ] Alice 报告已读取仓库内容
- [ ] Alice 报告已创建分支
- [ ] Alice 报告已创建文件
- [ ] Alice 报告已创建 PR
- [ ] 在 GitHub 上验证 PR 存在

---

## 第八步：多 Worker GitHub 协作

### 8.1 分配协作 GitHub 任务

在与 Manager 的私信中发送：

> Alice 和 Bob 在测试仓库上协作：Alice 创建 'feature/alice-docs' 分支并添加 docs/alice.md，Bob 创建 'feature/bob-api' 分支并添加 src/bob.py。两人分别创建独立的 PR。

### 验证清单

- [ ] Alice 创建了她的分支和文件
- [ ] Bob 创建了他的分支和文件
- [ ] GitHub 上存在两个独立的 PR
- [ ] 两个 Worker 分别在各自房间中报告完成

---

## 第九步：动态 MCP 权限控制

### 9.1 撤销 Alice 的 GitHub 访问权限

在与 Manager 的私信中发送：

> 撤销 Alice 对 GitHub MCP Server 的访问权限。

### 9.2 验证撤销效果

让 Alice 执行一个 GitHub 操作，她应该收到 403 错误。

### 9.3 恢复访问权限

在与 Manager 的私信中发送：

> 恢复 Alice 对 GitHub MCP Server 的访问权限。

### 9.4 验证恢复效果

再次让 Alice 执行 GitHub 操作，应该成功。

### 验证清单

- [ ] Manager 确认已撤销权限
- [ ] Alice 尝试 GitHub 操作时收到 403 错误
- [ ] Manager 确认已恢复权限
- [ ] Alice 可以再次执行 GitHub 操作

---

## 恭喜！

你已成功完成 HiClaw 的全部验证步骤。你的 Agent 团队已完全就绪，具备以下能力：

- 基于 IM 的通信（Matrix 协议）
- 人工监督（Human-in-the-Loop）
- 多 Worker 协作
- 集中式凭据管理
- 基于 MCP 的外部工具集成
- 动态权限控制

---

## 卸载

彻底移除 HiClaw 及其数据：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/higress-group/hiclaw/main/install/hiclaw-install.sh) uninstall
```

与 `install/hiclaw-install.sh uninstall` 行为一致：停止并删除 **`hiclaw-manager`**、所有 **`hiclaw-worker-*`**（及其他 Worker）容器、**`hiclaw-controller`**（内嵌 Higress / Tuwunel / MinIO / Element Web）、可选 **`hiclaw-docker-proxy`**、**`hiclaw-data`** 数据卷、**`hiclaw-manager.env`**、工作空间目录、**`hiclaw-net`** 网络及安装日志。
