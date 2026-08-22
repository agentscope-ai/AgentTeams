# 导入 Worker 指南

将预配置的 Worker 导入 HiClaw，或通过声明式 YAML 管理 Worker、Team 和 Human 用户。

## 概述

HiClaw 使用薄壳 + 容器内 CLI 架构来管理资源：

- **`hiclaw-apply.sh`** —— 在宿主机上运行，将声明式 YAML 转发给 Manager 容器内的 `hiclaw apply`。这是创建和管理 Worker、Team、Human 的主要方式。
- **`hiclaw-import.sh`** —— 在宿主机上运行，处理 ZIP 包导入，并转发给容器内的 `hiclaw` CLI。
- **`hiclaw` CLI** —— 在 Manager 容器内运行，负责所有资源管理（apply、get、delete）。

## 声明式 YAML 管理

通过 YAML 文件管理 HiClaw 资源是推荐的方式。

### 创建 Worker

```yaml
# worker.yaml
apiVersion: hiclaw.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  model: claude-sonnet-4-6
  skills:
    - github-operations
    - git-delegation
  mcpServers:
    - github
```

```bash
bash hiclaw-apply.sh -f worker.yaml
```

### 创建 Team

Team 由一个 Leader 和若干 Worker 组成。Leader 接收 Manager 下发的任务，并在团队内部进行协调。

```yaml
# team.yaml
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: alpha-team
spec:
  description: 全栈开发团队
  leader:
    name: alpha-lead
    model: claude-sonnet-4-6
  workers:
    - name: alpha-dev
      model: claude-sonnet-4-6
      skills: [github-operations]
    - name: alpha-qa
      model: claude-sonnet-4-6
```

```bash
bash hiclaw-apply.sh -f team.yaml
```

### 添加真人用户（Human）

真人用户会获得一个 Matrix 账号，并根据权限级别被邀请进相应的 Room。

```yaml
# human.yaml
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: 张三
  email: john@example.com       # 注册完成后自动发送账号密码到此邮箱
  permissionLevel: 2            # 1=等同Admin, 2=Team级, 3=Worker级
  accessibleTeams: [alpha-team] # L2: 可与该 team 的 leader + 所有 worker 对话
  accessibleWorkers: []
  note: 前端负责人
```

```bash
bash hiclaw-apply.sh -f human.yaml
```

权限级别说明：
- **L1**：等同 Admin —— 可与所有 Agent 对话
- **L2**：Team 级 —— 可与指定 Team 的 Leader 和 Worker 对话
- **L3**：Worker 级 —— 只能与指定 Worker 对话

### 批量导入

使用 `---` 分隔符在一个文件中管理多个资源：

```yaml
apiVersion: hiclaw.io/v1beta1
kind: Team
metadata:
  name: alpha-team
spec:
  leader:
    name: alpha-lead
    model: claude-sonnet-4-6
  workers:
    - name: alpha-dev
      model: claude-sonnet-4-6
---
apiVersion: hiclaw.io/v1beta1
kind: Human
metadata:
  name: john
spec:
  displayName: 张三
  email: john@example.com
  permissionLevel: 2
  accessibleTeams: [alpha-team]
```

```bash
bash hiclaw-apply.sh -f full-setup.yaml
```

### 全量同步（Prune）

删除 YAML 中不存在但系统中已有的资源，实现完整的期望状态同步：

```bash
bash hiclaw-apply.sh -f company-setup.yaml --prune
```

### 管理已有资源

在 Manager 容器内（或通过 `docker exec`）执行：

```bash
# 列出所有 worker
docker exec hiclaw-manager hiclaw get workers

# 查看指定 worker 的配置
docker exec hiclaw-manager hiclaw get worker alice

# 删除 worker
docker exec hiclaw-manager hiclaw delete worker alice
```

## Worker 包格式

```
worker-package.zip
├── manifest.json           # 包元数据（必需）
├── Dockerfile              # 自定义镜像构建（可选）
├── config/
│   ├── SOUL.md             # Worker 身份和角色
│   ├── AGENTS.md           # 自定义 Agent 配置
│   ├── MEMORY.md           # 长期记忆
│   └── memory/             # 记忆文件
├── skills/                 # 自定义技能
│   └── <skill-name>/
│       └── SKILL.md
├── crons/
│   └── jobs.json           # 定时任务
└── tool-analysis.json      # 工具依赖报告（仅供参考）
```

### manifest.json

```json
{
  "version": "1.0",
  "source": {
    "openclaw_version": "2026.3.x",
    "hostname": "my-server",
    "os": "Ubuntu 22.04",
    "created_at": "2026-03-18T10:00:00Z"
  },
  "worker": {
    "suggested_name": "my-worker",
    "base_image": "hiclaw/worker-agent:latest",
    "apt_packages": ["ffmpeg", "imagemagick"],
    "pip_packages": [],
    "npm_packages": []
  }
}
```

## 场景一：迁移独立运行的 OpenClaw

如果你有一个在服务器上独立运行的 OpenClaw 实例，想将其纳入 HiClaw 管理成为一个 Worker，按以下步骤操作。

### 第 1 步：在源 OpenClaw 上安装迁移 Skill

将 `migrate/skill/` 目录复制到 OpenClaw 的 skills 目录：

```bash
cp -r migrate/skill/ ~/.openclaw/workspace/skills/hiclaw-migrate/
```

或者让你的 OpenClaw 安装它：

```
安装 hiclaw-migrate skill，路径在 /path/to/hiclaw/migrate/skill/
```

### 第 2 步：生成迁移包

让你的 OpenClaw 分析当前环境并生成迁移包：

```
分析我当前的配置和环境，生成 HiClaw 迁移包。
```

OpenClaw 会阅读迁移 Skill 的说明，理解 HiClaw 的 Worker 架构，然后：

1. 运行 `analyze.sh` 扫描工具依赖（Skill 脚本、Shell 历史、Cron 任务、AGENTS.md 代码块）
2. 智能适配你的 AGENTS.md —— 保留你的自定义角色和行为定义，移除与 HiClaw 内置 Worker 配置冲突的部分（通信协议、文件同步、任务执行规范等）
3. 适配 SOUL.md 为 HiClaw 的 Worker 身份格式
4. 生成基于 HiClaw Worker 基础镜像的 Dockerfile，包含所需的系统工具
5. 将所有内容打包为 ZIP 并输出文件路径

这一步需要 OpenClaw AI 参与 —— 脚本本身无法智能地适配你的配置。OpenClaw 会阅读 SKILL.md 来理解 HiClaw 的规范，然后对配置内容做出保留、修改或移除的判断。

### 第 3 步：审查包内容（建议）

导入前建议检查生成的文件：

```bash
unzip -l /tmp/hiclaw-migration/migration-my-worker-*.zip
```

查看 `tool-analysis.json` 确认检测到的依赖是否正确。如有需要可以编辑 Dockerfile 增减软件包。

### 第 4 步：传输并导入

将 ZIP 传输到 HiClaw Manager 宿主机，然后运行：

```bash
bash hiclaw-import.sh worker --name my-worker --zip migration-my-worker-20260318-100000.zip
```

容器内的 `hiclaw` CLI 会依次执行：
1. 解析 ZIP 中的 `manifest.json`
2. 从 Dockerfile 构建自定义 Worker 镜像（如有）
3. 注册 Matrix 账号并创建通信 Room
4. 创建 MinIO 用户并配置权限策略
5. 配置 Higress Gateway Consumer 和路由授权
6. 生成 openclaw.json 并推送所有配置到 MinIO
7. 更新 Manager 的 workers-registry.json
8. 发送消息通知 Manager 启动 Worker 容器

### 第 5 步：验证

脚本完成后，在 Element Web 中查看 Worker 状态。Manager 会启动容器，Worker 应在一分钟内上线。

### 迁移内容对照表

| 内容 | 是否迁移 | 说明 |
|------|----------|------|
| SOUL.md / AGENTS.md | 是 | 适配为 HiClaw 格式 |
| 自定义 Skills | 是 | 放入 `skills/` 目录 |
| Cron 定时任务 | 是 | 转换为 HiClaw 调度任务 |
| 记忆文件 | 是 | MEMORY.md 和每日笔记 |
| 系统工具依赖 | 是 | 通过自定义 Dockerfile 安装 |
| API 密钥 / 认证配置 | 否 | HiClaw 使用自己的 AI Gateway 凭据 |
| 设备身份 | 否 | 注册时生成新身份 |
| 会话记录 | 否 | HiClaw 中会话每日重置 |
| Discord/Slack 渠道配置 | 否 | HiClaw 使用 Matrix |

## 场景二：导入 Worker 模板

Worker 模板是预构建的包，定义了 Worker 的角色、技能和工具依赖。可以在团队内共享或发布到社区。

### 从本地 ZIP 导入

```bash
bash hiclaw-import.sh worker --name devops-alice --zip devops-worker-template.zip
```

### 从 URL 导入

```bash
bash hiclaw-import.sh worker --name devops-alice --zip https://example.com/templates/devops-worker.zip
```

### 从远程包（Nacos）导入

```bash
bash hiclaw-import.sh worker --name devops-alice --package nacos://instance-xxx/namespace/agent-spec/devops/v1
```

### 不使用包，直接创建 Worker

无需 ZIP，直接指定模型和内置技能创建 Worker：

```bash
bash hiclaw-import.sh worker --name bob --model claude-sonnet-4-6 \
    --skills github-operations,git-delegation \
    --mcp-servers github
```

或通过 YAML（推荐用于可重复部署）：

```bash
bash hiclaw-apply.sh -f worker.yaml
```

### 创建 Worker 模板

要创建可分享的 Worker 模板：

1. 创建 `manifest.json`：

```json
{
  "version": "1.0",
  "source": {
    "hostname": "template",
    "os": "N/A",
    "created_at": "2026-03-18T00:00:00Z"
  },
  "worker": {
    "suggested_name": "devops-worker",
    "base_image": "hiclaw/worker-agent:latest",
    "apt_packages": [],
    "pip_packages": [],
    "npm_packages": []
  }
}
```

2. 创建 `config/SOUL.md` 定义 Worker 角色：

```markdown
# DevOps Worker

## AI Identity

**You are an AI Agent, not a human.**

## Role
- Name: devops-worker
- 专长: CI/CD 流水线管理、基础设施监控、部署自动化
- 技能: GitHub 操作、Shell 脚本、Docker、Kubernetes

## Behavior
- 主动监控 CI/CD 流水线
- 发现故障立即告警
- 自动化日常部署任务
```

3. 可选添加 `config/AGENTS.md`（自定义指令）、`skills/`（自定义技能）和 `Dockerfile`（额外工具）。

4. 打包：

```bash
cd my-template-dir/
zip -r devops-worker-template.zip manifest.json config/ skills/ Dockerfile
```

## 命令参考

### hiclaw-import.sh（Bash — macOS/Linux）

```bash
bash hiclaw-import.sh worker --name <名称> [选项]
bash hiclaw-import.sh -f <resource.yaml> [--prune] [--dry-run]
```

**Worker 导入模式：**

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `--name <名称>` | Worker 名称（必需） | — |
| `--zip <路径\|URL>` | ZIP 包（本地路径或 URL） | — |
| `--package <URI>` | 远程包 URI（`nacos://`、`http://`、`oss://`） | — |
| `--model <模型>` | LLM 模型 ID | `qwen3.5-plus` |
| `--skills <s1,s2>` | 逗号分隔的内置技能 | — |
| `--mcp-servers <m1,m2>` | 逗号分隔的 MCP Server | — |
| `--runtime <运行时>` | Agent 运行时（`openclaw`\|`copaw`） | `openclaw` |
| `--dry-run` | 预览变更但不实际执行 | 关闭 |
| `--yes` | 跳过交互确认 | 关闭 |

**YAML 模式**（`-f`）：转发给 `hiclaw-apply.sh` 执行。

### hiclaw-import.ps1（PowerShell — Windows）

```powershell
.\hiclaw-import.ps1 worker -Name <名称> [-Zip <路径或URL>] [-Package <URI>] [-Model 模型] [-Skills s1,s2] [-McpServers m1,m2] [-Runtime rt] [-DryRun] [-Yes]
.\hiclaw-import.ps1 -File <resource.yaml> [-Prune] [-DryRun]
```

参数与 Bash 版本一致。

### hiclaw-apply.sh（Bash — macOS/Linux）

```bash
bash hiclaw-apply.sh -f <resource.yaml> [选项]
```

| 选项 | 说明 | 默认值 |
|------|------|--------|
| `-f <路径>` | YAML 资源文件（必需） | — |
| `--prune` | 删除 YAML 中不存在的资源 | 关闭 |
| `--dry-run` | 预览变更但不实际执行 | 关闭 |
| `--yes` | Prune 时跳过删除确认 | 关闭 |

## 故障排查

### 导入脚本在 "检查 Manager 容器" 步骤失败

HiClaw Manager 容器必须处于运行状态：

```bash
docker start hiclaw-manager
```

### 镜像构建失败

检查 ZIP 包中的 Dockerfile。常见问题：
- 软件包名称在不同 Ubuntu 版本间可能不同
- pip/npm 包可能已更名或下架

可以编辑解压后的 Dockerfile 重试。

### Worker 启动但无响应

1. 查看 Worker 容器日志：`docker logs hiclaw-worker-<name>`
2. 在 Element Web 中确认 Worker 出现在其专属 Room 中
3. 确认 Manager 的 `workers-registry.json` 中有正确的条目
4. 尝试在 Worker 的 Room 中发送 `@<worker-name>:<matrix-domain> hello`
