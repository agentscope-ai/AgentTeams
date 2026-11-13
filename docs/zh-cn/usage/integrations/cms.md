# 接入阿里云云监控 CMS 2.0

本指南介绍如何将 AgentTeams 接入阿里云云监控服务 CMS 2.0,以获得 AI Agent 应用的全方位可观测能力。

Core v1.2.7 + Dashboard v1.4.1 的 Controller 可观测配置、Collector 和 Langfuse/AgentLoop 查询入口见 [Dashboard Tracing](../dashboard.md#可选的-tracing)。下文的 CMS 环境变量属于独立的 CMS 导出集成，不会配置 Dashboard 查询后端。

## 版本要求

**⚠️ 重要：CMS 2.0 可观测性支持从 AgentTeams v1.0.9 版本开始提供。**

## 概述

AgentTeams 通过 OpenTelemetry 协议支持接入 CMS 2.0,使您能够:

- 监控 Manager 和 Worker Agent 的完整请求链路
- 追踪大模型 API 调用延迟和 Token 消耗
- 可视化 Agent 协作模式
- 分析任务执行路径和瓶颈

## 前置条件

- AgentTeams v1.0.9 或更高版本
- 拥有阿里云账号并已开通 CMS 2.0 服务
- 已创建 CMS 2.0 工作空间
- AgentTeams 部署环境到 CMS 2.0 端点的网络连通性

## 步骤一:获取 CMS 2.0 接入配置

在配置 AgentTeams 之前,需要从 CMS 2.0 控制台获取接入参数。

### 1.1 登录 CMS 2.0 控制台

1. 访问 [CMS 2.0 控制台](https://cmsnext.console.aliyun.com/next/home)
2. 选择目标工作空间

### 1.2 进入接入中心

1. 在左侧导航栏单击**接入中心**
2. 在**AI 应用可观测**区域找到 **OpenClaw** 卡片
3. 单击 **OpenClaw** 卡片打开接入配置页面

### 1.3 生成接入命令

1. 在参数配置区域输入**应用名**（将作为在 CMS 2.0 中显示的服务名称）
2. 根据需求选择**连接方式**:
   - **公网接入点**:适用于部署在公有云或需要通过互联网访问的 AgentTeams
   - **VPC 接入点**:适用于部署在阿里云 VPC 内的 AgentTeams（推荐,安全性更高且延迟更低）
3. 单击 LicenseKey 右侧的**点击获取**以生成认证凭据
4. 页面将根据您的配置生成完整的接入命令

### 1.4 记录接入参数

记录生成的配置中的以下参数:

| **参数**              | **说明**                     | **示例**                                                                     |
|---------------------|----------------------------|---------------------------------------------------------------------------|
| `endpoint`          | OTLP Trace/Metric 数据上报地址   | `https://proj-xtrace-xxx.cn-hangzhou.log.aliyuncs.com/apm/trace/opentelemetry` |
| `x-arms-license-key`| 数据写入鉴权的 License Key        | `d95vgxi0cn@xxxxx`                                                        |
| `x-arms-project`    | 日志服务项目名称                   | `proj-xtrace-xxx-cn-hangzhou`                                             |
| `x-cms-workspace`   | 云监控 2.0 工作空间标识             | `default-cms-xxx-cn-hangzhou`                                             |
| `serviceName`       | 应用名称（在 CMS 2.0 中显示）        | `agentteams-manager`                                                          |

**重要提示:**
- endpoint URL 会根据路径后缀自动路由到链路追踪或指标采集服务
- 使用 VPC 连接时,请确保 AgentTeams 部署环境具有到 VPC 端点的网络访问权限
- License Key 是敏感信息,请妥善保管

## 步骤二:配置 CMS 导出默认值

在 Controller 部署中配置以下环境变量，由它向托管 Agent 容器投影 CMS 默认配置：

```bash
# 启用链路追踪采集
AGENTTEAMS_CMS_TRACES_ENABLED=true

# CMS 2.0 工作空间标识（对应 x-cms-workspace）
AGENTTEAMS_CMS_WORKSPACE=default-cms-xxx-cn-hangzhou

# Manager 服务名称（对应 serviceName）
AGENTTEAMS_CMS_SERVICE_NAME=agentteams-manager

# 日志服务项目名称（对应 x-arms-project）
AGENTTEAMS_CMS_PROJECT=proj-xtrace-xxx-cn-hangzhou

# 认证 License Key（对应 x-arms-license-key）
AGENTTEAMS_CMS_LICENSE_KEY=d95vgxi0cn@xxxxx

# OTLP 端点 URL（对应 endpoint）
AGENTTEAMS_CMS_ENDPOINT=https://proj-xtrace-xxx.cn-hangzhou.log.aliyuncs.com/apm/trace/opentelemetry
```

通过正常部署流程应用配置并重建容器。仅执行 `docker restart` 不会重新读取宿主机文件中修改的环境变量。

### v1.1.0+ 多容器 / Helm 环境下的变量落点

| 场景 | 配置位置 |
|------|----------|
| **嵌入式 Docker** | 将 `AGENTTEAMS_CMS_*` 持久化到安装器环境配置并通过安装器应用。安装器将变量传入 `agentteams-controller`，再由 Controller 向托管 Agent 提供默认值。 |
| **Helm / Kubernetes** | 设置 Chart 的 `cms.enabled`、`cms.endpoint`、`cms.licenseKey`、`cms.project`、`cms.workspace`、`cms.serviceName` 及可选的 `cms.metricsEnabled`。这些配置注入 Controller Deployment；Manager 是 CR 管理的 Pod，不是单独的 Helm Deployment。 |

## 步骤三:配置 AgentTeams Workers

Controller 向新建托管 Agent 容器投影 CMS 默认值。已有容器需更新实际环境并在空闲时重建。应确认所选 Harness 支持 CMS 导出；通用 QwenPaw/Hermes Collector 流程通过 Dashboard 单独配置。

## 步骤四:验证接入

1. 登录 Element Web,与 Manager 交互并创建 Worker 执行任务
2. 访问 [CMS 2.0 控制台](https://cmsnext.console.aliyun.com/next/home) → 选择工作空间 → **应用可观测** → **AI 应用可观测**
3. 您应该看到应用列表中出现 `agentteams-manager` 和 `agentteams-worker-*`
4. 单击应用名称可查看调用链分析、指标等详细信息

## 故障排查

### CMS 2.0 中没有数据?

1. 检查网络连通性: `curl -I <ENDPOINT_URL>`
2. 验证 `AGENTTEAMS_CMS_LICENSE_KEY` 是否正确（无空格/换行）
3. 确认容器已使用更新后的环境重建；单纯重启旧容器不会新增环境变量
4. 查看容器日志: `docker logs agentteams-manager`
5. OTLP 数据批量发送,最多延迟 60 秒

### Workers 没有遥测数据?

1. 确认 Controller 已配置 CMS 默认值，且所选 Harness 支持该集成
2. 验证 Worker 容器的 `AGENTTEAMS_CMS_*` 环境变量
3. 更新 Worker 的实际环境，并在空闲时重建托管容器；无需为刷新遥测而删除 Worker 资源
