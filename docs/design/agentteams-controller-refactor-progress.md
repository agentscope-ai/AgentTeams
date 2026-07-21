# AgentTeams Controller 重构进度跟踪

> 基于 `agentteams-controller-refactor.md` 设计文档，对照 `agentteams-controller-refactor` 分支实际实现情况。
>
> 更新时间：2026-04-10

## 总览

| Phase | 目标 | 完成度 | 状态 |
|-------|------|--------|------|
| Phase 1 | Controller 核心重构（去脚本化） | ~90% | 进行中（仅 ConfigVersionManager 未实现） |
| Phase 2 | incluster 模式 & Helm & Embedded 部署 | ~90% | 进行中（集成测试通过中） |
| Phase 3 | Manager Agent 改造 & Team Leader 增强 | ~25% | 部分进行中 |
| Phase 4 | Debug 能力 & 平滑升级 | 0% | 未开始 |

---

## Phase 1: Controller 核心重构（去脚本化）

### 1.1 Go 服务客户端

| 项目 | 设计路径 | 实际路径 | 状态 |
|------|---------|---------|------|
| Matrix API 客户端 | `internal/matrix/client.go` | `internal/matrix/client.go` + `types.go` | ✅ 完成 |
| OSS/MinIO 统一客户端 | `internal/oss/client.go` | `internal/oss/client.go` + `types.go` + `minio.go` + `minio_admin.go` | ✅ 完成 |
| Higress/Gateway 客户端 | `internal/controller/higress_client.go`（扩展） | `internal/gateway/client.go` + `higress.go` + `types.go` | ✅ 完成（路径有调整） |

额外实现（设计文档未单独列出）：

| 项目 | 路径 | 说明 |
|------|------|------|
| STS 凭证管理 | `internal/credentials/sts.go` | 云端 STS Token 服务 |
| Agent 配置生成 | `internal/agentconfig/generator.go` | AGENTS.md 合并、MCP 端口配置 |
| 认证鉴权 | `internal/auth/` | K8s ServiceAccount 认证 + RBAC 鉴权 + 中间件 |

### 1.2 WorkerBackend 抽象层

| 项目 | 设计路径 | 实际路径 | 状态 |
|------|---------|---------|------|
| 接口定义 | `internal/backend/interface.go` | `internal/backend/interface.go` | ✅ 完成 |
| Docker 后端 | `internal/backend/docker.go` | `internal/backend/docker.go` | ✅ 完成 |
| K8s 后端 | `internal/backend/kubernetes.go` | `internal/backend/kubernetes.go` | ✅ 完成 |
| 后端自动选择 | `internal/backend/factory.go` | `internal/backend/registry.go` | ✅ 完成（文件名不同） |

额外实现（超出设计文档）：

| 项目 | 路径 | 说明 |
|------|------|------|
| SAE 后端 | `internal/backend/sae.go` | 阿里云 Serverless App Engine |
| APIG 网关后端 | `internal/backend/apig.go` | 阿里云 API Gateway |
| 网关抽象 | `internal/backend/gateway.go` | 网关层统一抽象 |
| 云凭证管理 | `internal/backend/cloud_credentials.go` | 云端凭证注入 |

### 1.3 纯 Go Reconciler

| 项目 | 设计路径 | 实际路径 | 状态 |
|------|---------|---------|------|
| WorkerReconciler | `internal/controller/worker_controller.go` | `internal/controller/worker_controller.go` | ✅ 完成 |
| TeamReconciler | `internal/controller/team_controller.go` | `internal/controller/team_controller.go` | ✅ 完成 |
| HumanReconciler | `internal/controller/human_controller.go` | `internal/controller/human_controller.go` | ✅ 完成 |

### 1.4 集群初始化引擎

| 项目 | 设计路径 | 实际路径 | 状态 |
|------|---------|---------|------|
| Initializer | `internal/orchestrator/initializer.go` | `internal/initializer/initializer.go` | ✅ 完成（路径有调整） |

### 1.5 配置版本管理

| 项目 | 设计路径 | 状态 |
|------|---------|------|
| ConfigVersionManager | `internal/orchestrator/version_manager.go` | ❌ 未实现 |
| versions.json 管理 | OSS `system/versions.json` | ❌ 未实现 |
| Skill 热更新（UpgradeSkills） | — | ❌ 未实现 |
| Runtime 滚动升级（UpgradeRuntime） | — | ❌ 未实现 |
| `agt config push` 命令 | CLI | ❌ 未实现 |

### 1.6 项目结构对比

| 设计文档目录 | 实际情况 |
|-------------|---------|
| `internal/controller/` | ✅ 存在，含 worker/team/human 三个 controller |
| `internal/backend/` | ✅ 存在，且比设计更丰富（多了 SAE/APIG） |
| `internal/matrix/` | ✅ 存在 |
| `internal/oss/` | ✅ 存在 |
| `internal/orchestrator/` | ❌ 不存在，逻辑分散在 app/ 和 service/ |
| `internal/server/http.go` | ✅ 存在，拆分为多个 handler 文件 |
| `internal/apiserver/embedded.go` | ✅ 存在 |
| `internal/store/kine.go` | ✅ 存在 |
| `internal/watcher/file_watcher.go` | ✅ 存在 |
| `internal/mail/smtp.go` | ✅ 存在 |

实际额外新增的目录（设计文档未列出）：

| 目录 | 说明 |
|------|------|
| `internal/agentconfig/` | Agent 配置生成、AGENTS.md 合并、MCP 端口、协调逻辑 |
| `internal/auth/` | 认证（SA Token）、鉴权（RBAC）、中间件 |
| `internal/service/` | Provisioner、Deployer、Credentials、Worker 环境变量 |
| `internal/credentials/` | STS 凭证管理 |
| `internal/proxy/` | 安全代理 |
| `internal/httputil/` | HTTP 响应工具 |
| `internal/config/` | 配置加载 |

---

## Phase 2: incluster 模式 & Helm

### 2.1 K8sBackend

| 项目 | 状态 | 说明 |
|------|------|------|
| K8sBackend 实现 | ✅ 完成 | `internal/backend/kubernetes.go` |
| Worker Pod 模板生成 | ✅ 完成 | `internal/service/deployer.go` |
| Pod 健康检查 & 就绪探针 | ⚠️ 需确认 | 就绪检测在早期 commit 中实现 |
| Service 创建（端口暴露） | ✅ 完成 | `internal/service/provisioner_expose.go` |

### 2.2 agt CLI REST API 客户端改造

CLI 已完全重写为 REST API 客户端，不再直接操作 MinIO。所有命令通过 `AGENTTEAMS_CONTROLLER_URL`（默认 `http://localhost:8090`）调用 controller REST API，Token 通过 `AGENTTEAMS_AUTH_TOKEN` 或 SA token 文件自动发现。

| 项目 | 状态 | 说明 |
|------|------|------|
| CLI 重写为 REST API 客户端 | ✅ 完成 | `cmd/agt/client.go` — HTTP client + token/URL 发现 |
| create worker/team/human/manager | ✅ 完成 | `cmd/agt/create.go` — POST /api/v1/{resource}s |
| get workers/teams/humans/managers | ✅ 完成 | `cmd/agt/get.go` — GET (list/detail/--team 过滤/-o json) |
| update worker/team/manager | ✅ 完成 | `cmd/agt/update.go` — PUT /api/v1/{resource}s/{name} |
| delete worker/team/human/manager | ✅ 完成 | `cmd/agt/delete.go` — DELETE /api/v1/{resource}s/{name} |
| worker wake/sleep/ensure-ready | ✅ 完成 | `cmd/agt/worker_cmd.go` — POST lifecycle endpoints |
| worker status (--name / --team) | ✅ 完成 | `cmd/agt/worker_cmd.go` — GET status endpoint |
| status / version | ✅ 完成 | `cmd/agt/status_cmd.go` |
| 表格/详情/JSON 输出格式化 | ✅ 完成 | `cmd/agt/output.go` |
| apply -f resource.yaml | ✅ 完成 | `cmd/agt/apply.go` — 声明式 YAML apply（解析 YAML → REST API upsert） |
| apply worker --zip | ✅ 完成 | `cmd/agt/apply.go` — ZIP 上传 → POST /api/v1/packages → upsert Worker |
| apply worker --params | ✅ 完成 | `cmd/agt/apply.go` — 支持 --model/--soul-file/--skills/--mcp-servers 等参数 |
| apply --prune | ❌ TODO | 全量同步（LIST + DELETE 多余资源） |
| config push 命令 | ❌ 未实现 | — |
| debug 命令 | ❌ 未实现 | — |

### 2.3 Helm Chart

| 项目 | 状态 | 说明 |
|------|------|------|
| Chart 结构 | ✅ 完成 | `helm/agentteams/` |
| Controller Deployment | ✅ 完成 | `templates/controller/deployment.yaml` |
| Controller Service | ✅ 完成 | `templates/controller/service.yaml` |
| Controller RBAC | ✅ 完成 | `templates/controller/rbac.yaml` |
| Controller ServiceAccount | ✅ 完成 | `templates/controller/serviceaccount.yaml` |
| Tuwunel StatefulSet | ✅ 完成 | `templates/matrix-server/tuwunel-statefulset.yaml` |
| MinIO StatefulSet | ✅ 完成 | `templates/object-storage/minio-statefulset.yaml` |
| Higress（子 chart） | ✅ 完成 | `charts/higress-2.2.0.tgz` |
| Element Web | ✅ 完成 | `templates/element-web/` |
| Manager Deployment | ✅ 完成 | `templates/manager/deployment.yaml` |
| values.yaml | ✅ 完成 | 含 values-kind.yaml 本地开发配置 |
| Ingress | ❌ 未实现 | 设计文档中有 `ingress.yaml` |

### 2.4 CRD

| CRD | Helm crds/ | api/v1beta1/types.go | Reconciler | 状态 |
|-----|-----------|---------------------|------------|------|
| Worker | ✅ | ✅ | ✅ | 完成 |
| Team | ✅ | ✅ | ✅ | 完成 |
| Human | ✅ | ✅ | ✅ | 完成 |
| Manager | ✅ | ✅ | ✅ | 完成 |
| DebugWorker | ❌ | ❌ | ❌ | 未实现 |

### 2.5 Embedded 模式双容器部署

| 项目 | 状态 | 说明 |
|------|------|------|
| Dockerfile.embedded（all-in-one 镜像） | ✅ 完成 | Higress + Tuwunel + MinIO + Element Web + Controller + CLI + kube-apiserver |
| supervisord.embedded.conf | ✅ 完成 | 分层启动：基础设施(50-100) → Higress(200-600) → Element(650) → Controller(700-750) |
| install-embedded.sh 安装脚本 | ✅ 完成 | 双容器部署：controller 容器 + manager-agent 容器（由 controller 自动创建） |
| Makefile build-embedded 目标 | ✅ 完成 | 构建链：build-embedded → build-manager-k8s → build-worker → build-copaw-worker |
| Manager Agent 自动创建 | ✅ 完成 | ManagerReconciler 通过 DockerBackend 自动创建 manager 容器 |
| 测试基础设施适配 | ✅ 完成 | test-helpers.sh 支持双容器模式（exec_in_manager / exec_in_agent） |
| 集成测试通过情况 | ⚠️ 进行中 | test-01 ~ test-06✅|

---

## Phase 3: Manager Agent 改造 & Team Leader 增强

| 项目 | 状态 | 说明 |
|------|------|------|
| Manager Skill 改造（调用 agt CLI 替代直接脚本） | ⚠️ 部分完成 | `container-api.sh` 已改为 controller REST API 薄封装；`create-worker.sh` 已删除（+1042 行），由 agt CLI 替代 |
| Manager 无状态化（state.json → OSS） | ❌ 未实现 | — |
| Manager CRD 驱动部署 | ✅ 完成 | Manager CRD + ManagerReconciler + 自动创建 Manager 容器 |
| Team Leader Heartbeat 机制 | ✅ 完成 | `4870b26` — heartbeat、worker idle timeout、sleep lifecycle |
| Team Leader Worker 生命周期管理 | ✅ 完成 | worker-lifecycle skill + sleep/wake API |
| Leader permissions 配置 | ⚠️ 部分完成 | RBAC 中间件已实现 SA 认证 + 角色鉴权 |
| Quota 检查机制 | ❌ 未实现 | — |
| CallerIdentity 权限隔离 | ⚠️ 部分完成 | SA Token 认证已实现，细粒度 CallerIdentity 隔离待完善 |

---

## Phase 4: Debug 能力 & 平滑升级

全部未开始。

| 项目 | 状态 |
|------|------|
| DebugWorker CRD 定义 | ❌ |
| DebugWorkerReconciler | ❌ |
| debug-analysis skill | ❌ |
| 工作目录实时挂载（mc mirror） | ❌ |
| Matrix 消息导出 | ❌ |
| Skill/配置热更新（零停机） | ❌ |
| Controller 升级机制 | ❌ |
| Runtime 滚动升级 | ❌ |
| 版本兼容性矩阵 | ❌ |
| Helm Hooks 升级编排 | ❌ |

---

## 关键 Commit 时间线

| Commit | 里程碑 |
|--------|--------|
| `ae39750` | 起点：将 docker-proxy 重构为统一 Worker 生命周期管理（orchestrator） |
| `a45b662` | 新增 SAE 后端、APIG 网关、认证、STS Token |
| `821cbf0` | 抽象 Backend Provider 层 |
| `453910d` ~ `d3a4334` | 将 orchestrator 重命名为 controller |
| `931f33e` ~ `72d2778` | 初始 Helm Chart + Kind 本地 K8s 环境 |
| `30223b3` | 新增 CRD 定义和 controller 配置 |
| `f15b231` ~ `a340ab2` | Agent 配置生成和合并功能 |
| `2b0bb85` ~ `f535136` | HTTP Server 重构、API 错误处理、RBAC |
| `f30e870` ~ `fd1b9b3` | K8s ServiceAccount 认证鉴权 |
| `bbb4ae3` | Tuwunel/MinIO 改为 StatefulSet |
| `53a28ad` | local-k8s-up.sh 更新 + Worker 管理增强 |
| `3c17fe1` ~ `2c0ddc3` | Manager CRD + ManagerReconciler + Go Initializer |
| `20fcb7b` ~ `cdb4016` | Manager REST API（CRUD）+ RBAC 配置 |
| `7361df1` | agt CLI 重写为 REST API 客户端（去 mcExec，纯 HTTP client） |
| `563cc9c` | Embedded Controller 支持：Dockerfile.embedded + supervisord + ManagerReconciler embedded 模式 |
| `0431b79` | 测试脚本适配双容器模式（exec_in_manager / exec_in_agent） |
| `f6dbba3` | agt CLI 增强 + install-embedded.sh 安装脚本 |
| `7aaab59` | Makefile 统一 + apply 命令 + PackageHandler（ZIP 上传） |
| `4870b26` | Team Leader heartbeat、worker idle timeout、sleep lifecycle |
| `5208729` | AI route 认证修复 + 测试脚本执行上下文修正 |
| `a0deb6e` | Agent 模板管理增强 + observedGeneration + MinIO prefix 修复 |

---

## Shell 脚本替代进度

| 原 Shell 脚本 | 替代方案 | 状态 |
|---------------|---------|------|
| `create-worker.sh`（1042 行） | Go Provisioner + Deployer + WorkerReconciler | ✅ 已删除 |
| `container-api.sh` | 改为 controller REST API 薄封装 | ✅ 大幅精简 |
| `gateway-api.sh` | Go Gateway Client + Provisioner | ✅ 大幅精简 |
| `aliyun-api.py`（527 行） | Go 云端后端（SAE/APIG） | ✅ 已删除 |
| `aliyun-sae.sh` | Go SAE Backend | ✅ 已删除 |
| `start-manager-agent.sh` | 仍作为 Manager 容器 entrypoint | ⚠️ 保留（容器内初始化） |
| `upgrade-builtins.sh` | 仍由 start-manager-agent.sh 调用 | ⚠️ 保留 |
| `setup-higress.sh` | Go Initializer 处理大部分 | ⚠️ 部分保留 |

---

## 集成测试通过情况（Embedded 模式）

| 测试 | 说明 | 状态 |
|------|------|------|
| test-01 | Manager Boot（服务健康、Matrix 登录、Higress 控制台、MinIO 存储） | ✅ 通过 |
| test-02 | Create Worker（通过 Matrix 对话创建 Worker） | ✅ 通过 |
| test-15 | Import Worker ZIP | ⚠️ 待验证 |
| test-17 | Worker Config Verify（导入、配置产物、MinIO 存储） | ✅ 通过 |
| test-18 | Team Config Verify | ⚠️ 待验证 |
| test-19 | Human and Team Admin | ⚠️ 待验证 |
| test-20 | Inline Worker Config | ⚠️ 待验证 |
| test-21 | Team Project DAG | ⚠️ 待验证 |
| test-100 | Cleanup | ⚠️ 待验证 |

---

## 当前阻塞项 & 下一步

1. **集成测试全量通过**：继续调试 test-15/18/19/20/21/100，确保 embedded 模式下所有测试通过
2. **Manager Shell 脚本进一步替代**：`start-manager-agent.sh` 中的 workspace sync、builtin upgrade 逻辑可考虑迁移到 Go
3. **ConfigVersionManager**：Phase 1 唯一未实现项，skill 热更新和 runtime 滚动升级依赖此组件
4. **apply --prune**：声明式全量同步，用于 GitOps 场景

---
