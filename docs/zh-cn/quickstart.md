# AgentTeams 快速入门

[English](../quickstart.md) | 中文

本指南使用推荐的本地部署路径，带你安装 AgentTeams、创建第一个 Worker，并完成一个可以人工介入的任务。详细配置和 Kubernetes 部署将在专题文档中说明；完成本指南后，可以继续尝试 [AgentTeams 使用案例](usage/use-cases.md)。

## 完成后你将获得什么

- 一套运行在本机的 AgentTeams 实例。
- 一个可以通过 Element Web 对话的 Manager。
- 一个由 Manager 创建和管理的 Worker。
- 一次 Human、Manager 与 Worker 共同参与的任务执行记录。

## 前置条件

- macOS 或 Linux。Windows 用户参见 [Windows 部署指南](usage/deployment/windows.md)。
- Docker Desktop、Docker Engine，或兼容的 Podman 环境已经启动。
- 至少 2 核 CPU 和 4 GB 可用内存；运行多个 Worker 时建议 4 核 CPU 和 8 GB 内存。
- 一个可用的 LLM API Key。安装器支持阿里云百炼/Qwen，也支持 OpenAI 兼容服务。
- 本机端口 `18080`、`18001`、`18088` 未被占用。默认启用 Dashboard 时还需要端口 `13000`。

> 第一次体验建议使用安装器的“快速开始”模式。如果需要自定义模型地址、外部访问、端口、持久化、运行时或镜像，请选择“手动配置”。

## 第一步：安装 AgentTeams

在终端中运行：

```bash
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

按照提示完成配置：

1. 选择中文。
2. 选择“快速开始”或“手动配置”。
3. 填写 LLM API Key；手动配置其他 OpenAI 兼容服务时，还需要填写 Base URL 和模型 ID。
4. 等待模型连通性检查和容器启动完成。

如果使用 OpenAI 兼容服务，Base URL 通常需要包含 `/v1`，具体以服务商文档为准。

安装成功后，终端会打印：

- Element Web 登录地址。
- 管理员用户名和密码。
- Higress Console 地址。
- Dashboard 地址（启用时）。
- 配置文件、数据卷和 Manager 工作空间位置。

请先保存安装器输出的登录信息。

## 第二步：验证并登录

检查主要容器：

```bash
docker ps --filter name=agentteams-controller
docker ps --filter name=agentteams-manager
```

也可以通过 `agt` 检查 Manager 状态：

```bash
docker exec agentteams-controller agt get managers
```

默认情况下，在浏览器中打开：

```text
http://127.0.0.1:18088/#/login
```

使用安装器打印的管理员用户名和密码登录。登录后，你应该可以看到 Manager 的会话或房间。

如果页面无法打开或 Manager 未就绪，先查看：

```bash
docker logs --tail 200 agentteams-controller
docker logs --tail 200 agentteams-manager
```

更多排查方法参见 [FAQ](usage/troubleshooting/faq.md)。

## 第三步：创建第一个 Worker

在 Element Web 中打开与 Manager 的私聊，发送：

> 请创建一个名为 alice 的 Worker，负责 Python 开发和代码测试。

Manager 会根据当前配置询问或确认 Worker 的角色、模型、运行时和 Skills。第一次体验可以接受推荐选项；如果选择运行时，请优先使用安装时已经准备好默认镜像的运行时。

创建过程通常需要几十秒。Manager 会调用 controller 完成以下工作：

1. 创建 Worker 资源和 Matrix 身份。
2. 准备网关权限与共享存储配置。
3. 启动独立的 Worker 容器。
4. 创建包含 Human、Manager 和 Worker 的 Matrix 房间。

可以在终端检查状态：

```bash
docker exec agentteams-controller agt get workers
docker ps --filter name=agentteams-worker
```

等待 `alice` 进入 `Running` 状态，并确认对应房间已经出现在 Element Web 中。

## 第四步：完成第一个任务

打开 Alice 所在的 Matrix 房间，发送：

> 请创建一个 Python 命令行程序：接收一个名字参数并输出问候语。请同时编写 README 和基础测试，并在完成后说明生成了哪些文件以及如何运行测试。

执行期间，你可以在房间中看到任务分配、进度和结果。Alice 完成后，检查回复是否包含：

- 实现文件。
- README 或使用说明。
- 测试文件与测试结果。
- 产物位置或获取方式。

## 第五步：体验人工介入

在 Worker 仍在执行时追加要求：

> 补充要求：没有传入名字时默认使用 `World`，并为这个分支补充一个测试。

确认 Worker 已理解补充要求，并在最终结果中同时覆盖原始任务和新增要求。这就是 AgentTeams 的 Human-in-the-loop 工作方式：Human 可以看到协作过程，并在任务完成前持续纠偏。

## 完成检查

- [ ] `agentteams-controller` 和 `agentteams-manager` 正在运行。
- [ ] 可以登录 Element Web 并与 Manager 对话。
- [ ] Alice 的状态为 `Running`。
- [ ] Alice 的 Matrix 房间对 Human 可见。
- [ ] Worker 返回了实现、说明和测试结果。
- [ ] Worker 处理了执行期间追加的要求。

完成以上检查后，你已经跑通了 AgentTeams 的最小闭环。

## 下一步

- 先阅读 [AgentTeams 概览](overview.md)，了解角色、组件和部署方式。
- 参考 [AgentTeams 使用案例](usage/use-cases.md)，尝试软件交付、研究分析、内容本地化、故障分析、长期项目协作，以及自定义 Skill 的添加与实际使用。
- 使用 [本地部署指南](usage/deployment/local.md) 查看模型、端口、域名、存储、运行时和自动化安装选项。
- 阅读 [Manager 指南](usage/manager-guide.md) 和 [Worker 指南](usage/worker-guide.md)。
- 使用 [声明式资源管理](usage/resource-management.md) 学习 `agt` CLI 和 YAML。
- 阅读 [架构说明](design/architecture.md)，了解 Matrix、Higress、对象存储和 controller 的关系。
- 使用 [Kubernetes 部署指南](usage/deployment/kubernetes.md) 在集群中创建团队共享实例。

## 卸载

以下操作会移除 AgentTeams 容器、网络、数据卷、配置文件和本地工作空间。确认不再需要其中的数据后再执行：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh) uninstall
```
