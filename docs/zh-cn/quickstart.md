# AgentTeams 快速入门

[English](../quickstart.md) | 中文

本指南使用推荐的本地部署路径，带你安装 AgentTeams、创建第一个 Worker，并完成一个可以人工介入的任务。详细配置和 Kubernetes 部署将在专题文档中说明；完成本指南后，可以继续尝试 [AgentTeams 使用案例](usage/use-cases.md)。

## 完成后你将获得什么

- 一套运行在本机的 AgentTeams 实例。
- 一个可以通过 Dashboard 对话的 Manager。
- 一个由 Manager 创建和管理的 Worker。
- 一次 Human、Manager 与 Worker 共同参与的任务执行记录。

## 前置条件

- macOS 或 Linux。Windows 用户参见 [Windows 部署指南](usage/deployment/windows.md)。
- OrbStack、Docker Desktop、Docker Engine，或兼容的 Podman 环境。未安装或未启动时，安装器会引导你安装并启动，随后继续。
- 至少 2 核 CPU 和 4 GB 可用内存；运行多个 Worker 时建议 4 核 CPU 和 8 GB 内存。
- LLM API Key 可以在进入 Dashboard 后配置。安装器支持阿里云百炼/Qwen，也支持 OpenAI 兼容服务。
- 本机端口 `18080`、`18001`、`18088` 未被占用。默认启用 Dashboard 时还需要端口 `13000`。

> 默认直接启动本地服务和 Dashboard。需要逐项配置安装参数时，在命令末尾添加 `--manual`。

## 第一步：安装 AgentTeams

在终端中运行：

```bash
bash <(curl -sSL https://agentteams.cloud/install.sh)
```

等待 Dashboard 自动打开。安装器会将登录密码复制到剪贴板，登录页预填用户名 `admin`；粘贴密码后即可登录。浏览器或剪贴板不可用时，可以使用终端打印的地址和账号密码。

未配置模型时，在 Manager 对话页点击“配置模型”，前往模型列表填写 API Key、选择模型并验证，然后返回对话。Worker 运行时和模型可在创建 Worker 时选择，embedding 可在需要时配置。安装前已设置 `AGENTTEAMS_LLM_API_KEY` 的，会沿用该值；自定义服务还可以设置 `AGENTTEAMS_LLM_PROVIDER=openai-compat`、`AGENTTEAMS_OPENAI_BASE_URL` 和 `AGENTTEAMS_DEFAULT_MODEL`。

如果使用 OpenAI 兼容服务，Base URL 通常需要包含 `/v1`，具体以服务商文档为准。

安装成功后，终端会打印：

- Dashboard 登录地址和可选的 Element Web 地址。
- 管理员用户名和密码。
- Higress Console 地址。
- 配置文件、数据卷和 Manager 工作空间位置。

请先保存安装器输出的登录信息。

以后升级时，重新运行相同命令，在唯一的升级确认处按回车即可。账号密码、模型配置、聊天记录、工作空间和数据挂载会保留；安装器先准备镜像，再备份原配置并替换服务，核心服务会短暂重启。普通嵌入式部署升级不再统一重建所有 Worker。发现已有数据但无法可靠恢复配置时，会停止并保留原数据。Dashboard 的版本保留规则及独立的数据库备份、回滚流程，参见[升级说明](../../install/README.md#upgrade-an-existing-local-installation)。

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
http://127.0.0.1:13000/
```

使用安装器打印的管理员用户名和密码登录。登录后，你应该可以看到 Manager 的会话或房间。

如果页面无法打开或 Manager 未就绪，先查看：

```bash
docker logs --tail 200 agentteams-controller
docker logs --tail 200 agentteams-manager
```

更多排查方法参见 [FAQ](usage/troubleshooting/faq.md)。

## 第三步：创建第一个 Worker

在 Dashboard 中打开 Manager 对话，发送：

> 请创建一个名为 alice 的 Worker，负责 Python 开发和代码测试。

Manager 会根据当前配置询问或确认 Worker 的角色、模型、运行时和 Skills。第一次体验可以接受推荐选项。所选运行时的镜像在首次使用时下载，因此第一次启动可能需要更长时间。

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

等待 `alice` 进入 `Running` 状态，并确认对应会话已经出现在 Dashboard 中。

## 第四步：完成第一个任务

在 Dashboard 中打开 Alice 的会话，发送：

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
- [ ] 可以登录 Dashboard 并与 Manager 对话。
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
bash <(curl -fsSL https://agentteams.cloud/install.sh) uninstall
```
