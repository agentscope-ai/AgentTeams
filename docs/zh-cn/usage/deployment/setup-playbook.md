# AgentTeams SETUP 剧本（agent 可自助执行）

[English](../../../usage/deployment/setup-playbook.md) | 中文

本剧本让 **agent** 在一台全新的 macOS/Linux 主机上，从空环境一路把
AgentTeams 装到可用。剧本分三段——收集（一次只问一个问题）、计划
（人工审阅）、执行（每一步都有前置检查、动作、验证方式和明确的失败
恢复下一步）。底层细节在[本地部署指南](local.md)里；本剧本是操作
流程，不是它的重复。

## 基本规则

1. **一次只问一个问题。** agent 只问一个问题，等回答，把答案记进
   计划（第二段）。不打包提问，也不在收集完成前动手执行。
2. **聊天里永不收 secret。** LLM API key（以及任何密钥）绝不粘贴进
   聊天、Matrix 房间或 issue。由人工把 key 写入一个仅属主可读的文
   件，并告诉 agent 文件路径；agent 只把文件传给安装器使用，绝不回
   显内容。
3. **收集 → 计划 → 执行。** 第二段计划未经人工审阅确认之前，不执行
   任何安装动作。
4. **每一步都可恢复。** 每个执行步骤都写明前置检查、动作、成功验证
   方式，以及失败时的具体下一步。失败不允许以"重试一下"收场——必须
   指出查什么、谁来处理。

## 第一段——收集（一次只问一个问题）

按顺序问下面的问题。每题都给出推荐答案；agent 应把推荐项与人工确认
后记录进计划，而不是直接假定。

| # | 问题 | 推荐答案 | 记录为 |
|---|---|---|---|
| Q1 | 这台主机上哪个容器运行时在运行？当前用户能否不加 sudo 跑 `docker ps`（或 `podman ps`）？ | Docker（macOS 用 Docker Desktop，Linux 用 Docker Engine），且已启动。 | runtime = docker / podman |
| Q2 | 用哪个 AgentTeams 版本？ | 最新的**稳定版** tag（如 `v1.2.x`）——要长期使用的实例绝不用 `latest`。 | `AGENTTEAMS_VERSION` |
| Q3 | 用哪个 LLM 服务和模型？ | `qwen`（内置阿里云百炼集成）或 `openai-compat`（Base URL 以 `/v1` 结尾）。模型 ID 必须与服务商接受的名称完全一致。 | `AGENTTEAMS_LLM_PROVIDER`、`AGENTTEAMS_OPENAI_BASE_URL`（`qwen` 留空）、`AGENTTEAMS_DEFAULT_MODEL` |
| Q4 | LLM API key 放在哪里？（绝不要粘贴到本对话里。） | 人工把它写入 `~/.agentteams-llm-key` 并 `chmod 600`（第三段 Step 1），然后确认路径。 | key 文件路径 |
| Q5 | 向量化（embedding）模型？ | `qwen` 用 `text-embedding-v4`；服务商没有 embedding API 时留空（禁用向量化）。 | `AGENTTEAMS_EMBEDDING_MODEL` |
| Q6 | 管理员密码？ | 留空——安装器自动生成并在结束时打印；人工从终端复制到密码管理器。绝不进聊天。 | （自动生成） |
| Q7 | Manager 运行时？ | `qwenpaw`。 | `AGENTTEAMS_MANAGER_RUNTIME` |
| Q8 | 默认 Worker 运行时？ | `qwenpaw`。 | `AGENTTEAMS_DEFAULT_WORKER_RUNTIME` |
| Q9 | 访问范围？ | 个人/评估主机选仅本机访问（`AGENTTEAMS_LOCAL_ONLY=1`）。 | `AGENTTEAMS_LOCAL_ONLY` |
| Q10 | 端口？ | 默认：`18080` 网关、`18001` Higress 控制台、`18088` Element Web、`13000` Dashboard——前提是 Step 0 前置检查确认无占用。 | `AGENTTEAMS_PORT_*`（仅在冲突时修改） |
| Q11 | GitHub token？ | 先跳过；以后需要 GitHub MCP 时再加一个专用、最小权限的 token。 | （不设置） |
| Q12 | Dashboard？ | 启用，端口 `13000`。 | `AGENTTEAMS_DASHBOARD=1` |
| Q13 | 数据卷 / 工作空间目录？ | 默认：卷 `agentteams-data`、工作空间 `${HOME}/agentteams-manager`。工作空间不要放在临时目录。 | `AGENTTEAMS_DATA_DIR`、`AGENTTEAMS_WORKSPACE_DIR` |
| Q14 | Matrix E2EE？ | 默认关闭（`AGENTTEAMS_MATRIX_E2EE=0`），除非确认所选运行时和客户端都支持当前 E2EE 流程。 | `AGENTTEAMS_MATRIX_E2EE` |

## 第二段——计划（收集完成后填写；人工审阅）

agent 用记录的答案和 Step 0 前置检查结果填好下面模板，然后停下等
确认：

```text
计划（AgentTeams SETUP）
主机            : <系统, runtime = docker|podman>
版本            : AGENTTEAMS_VERSION=<tag>
LLM             : provider=<qwen|openai-compat>, base_url=<… 或（内置）>,
                  model=<id>, embedding=<id|禁用>, key 文件=~/.agentteams-llm-key
管理员          : 用户名=admin, 密码=<安装器生成>
运行时          : manager=<qwenpaw>, 默认 worker=<qwenpaw>
访问            : 仅本机=<1|0>, 端口: 网关=18080 控制台=18001 element=18088 dashboard=13000
数据            : 卷=agentteams-data, 工作空间=${HOME}/agentteams-manager
E2EE            : <0|1>
前置检查        : 运行时 OK / 剩余磁盘 >= 10 GB / 剩余内存 >= 4 GB / 端口空闲
安装命令        : （Step 2 中填好全部值的非交互命令——不含任何密钥，
                  key 从文件读取）
```

此计划未获批准前，第三段一律不执行。

## 第三段——执行（空环境 → 可用实例）

### Step 0——前置检查（agent 可执行）

- **前置检查：** 无（本步就是检查）。
- **动作：** 在主机上执行：
  ```bash
  docker ps                       # 或：podman ps
  df -h "$HOME"                   # 需 >= 10 GB 可用
  free -h                         # macOS：top -l 1 | head -10；需 >= 4 GB
  lsof -nP -iTCP:18080,18001,18088,13000 -sTCP:LISTEN   # 无 lsof 的 Linux：ss -lnt
  ```
- **验证：** 运行时能应答；磁盘、内存达到最低要求；四个端口（或计划
  中选定的替代端口）均无监听。
- **失败恢复：**
  - 运行时缺失/未启动 → 人工：macOS 启动 Docker Desktop，或 Linux
    `systemctl start docker`；然后重跑 Step 0。
  - 磁盘 < 10 GB 或内存 < 4 GB → 人工：释放资源；重跑 Step 0。
  - 端口被占 → 从 `lsof`/`ss` 输出找到占用进程，为该服务另选端口、
    记入计划、重新检查。

### Step 1——放置 LLM API key（仅人工——终端命令单独给出）

agent 不执行本步，也绝不接触 key。agent 请人工在终端（不是聊天）中
执行：

```bash
umask 077
printf '%s' '在这里粘贴你的-llm-api-key' > ~/.agentteams-llm-key
chmod 600 ~/.agentteams-llm-key
```

- **前置检查（agent 执行）：** 文件存在、非空、权限 600：
  ```bash
  test -s ~/.agentteams-llm-key
  stat -c '%a' ~/.agentteams-llm-key    # 期望 600（macOS：stat -f '%Lp'）
  ```
- **验证：** 两项检查通过。agent 不读取、不打印文件内容。
- **失败恢复：** 文件不存在 → 再请人工执行一次上面的命令（这就是完
  整步骤）；权限不对 → `chmod 600`。人工想用其他路径时，把路径记入
  计划。

### Step 2——安装（agent 可执行，非交互模式）

- **前置检查：** Step 1 已验证；计划中每个值都已填好（非交互模式遇到
  缺失的必填值会立即失败并点名——此时失败说明计划不完整）。
- **动作：** 用计划中的值逐项替换后执行（key 从文件读取，绝不写进命
  令行或日志）：
  ```bash
  AGENTTEAMS_NON_INTERACTIVE=1 \
  AGENTTEAMS_VERSION=<计划> \
  AGENTTEAMS_LLM_PROVIDER=<计划> \
  AGENTTEAMS_OPENAI_BASE_URL=<计划 — qwen 留空> \
  AGENTTEAMS_LLM_API_KEY="$(cat ~/.agentteams-llm-key)" \
  AGENTTEAMS_DEFAULT_MODEL=<计划> \
  AGENTTEAMS_EMBEDDING_MODEL=<计划 — 留空即禁用> \
  AGENTTEAMS_MANAGER_RUNTIME=<计划> \
  AGENTTEAMS_DEFAULT_WORKER_RUNTIME=<计划> \
  AGENTTEAMS_LOCAL_ONLY=<计划> \
  AGENTTEAMS_ADMIN_USER=admin \
  bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/<计划>/install/agentteams-install.sh)
  ```
  自建模（非内置模型）还需按服务商真实规格设置
  `AGENTTEAMS_MODEL_CONTEXT_WINDOW`、`AGENTTEAMS_MODEL_MAX_TOKENS`、
  `AGENTTEAMS_MODEL_REASONING`、`AGENTTEAMS_MODEL_VISION`。
- **验证：** 安装器退出码为 0，并打印访问 URL 和（若生成）管理员密
  码。人工：立刻把密码从终端复制到密码管理器。完整日志在
  `${HOME}/agentteams-install.log`。
- **失败恢复：**
  - 安装器点名某个变量缺失/非法 → 在计划里修正该值后重跑（属于计划
    缺陷，不是主机问题）。
  - `curl`/下载失败 → 检查主机出口/代理；替代方案：按固定 tag 克隆
    仓库后本地执行 `bash install/agentteams-install.sh`。
  - 模型连通性检查失败 → 安装器会指出失败的检查项；依次排查：key
    文件（人工重新放置）、Base URL（`openai-compat` 需以 `/v1` 结
    尾）、模型 ID、账户余额/配额、到服务商的网络路径。
  - 端口冲突 → 用计划中的空闲 `AGENTTEAMS_PORT_*` 重跑。
  - 安装中途磁盘/内存耗尽 → 人工处理；清理后重跑（安装器第 3 步会
    提供对残留状态的升级/重装处理）。

### Step 3——装后验证（agent 可执行）

- **前置检查：** Step 2 已验证。
- **动作：** 执行[本地部署指南](local.md#安装后验证)的四个检查（按
  计划中的实际端口替换）：
  ```bash
  docker ps --filter name=agentteams-controller
  docker ps --filter name=agentteams-manager
  docker exec agentteams-controller agt get managers
  docker exec agentteams-controller agt get workers
  curl -fsS http://127.0.0.1:18088/ >/dev/null
  curl -fsS http://127.0.0.1:18001/ >/dev/null
  docker exec agentteams-controller curl -fsS http://127.0.0.1:9000/minio/health/live
  ```
- **验证：** `agentteams-controller` 与 `agentteams-manager` 处于
  `Up`；Element Web 和 Higress 控制台有应答；MinIO 报告 live。
  `agt get managers` 列出 Manager；Step 4 之前 worker 列表可以为空。
- **失败恢复：**
  - 容器未 `Up` → `docker logs --tail 200 <容器名>`；对照[故障排
    查章节](local.md#故障排除)（容器不启动 / 卡在 provisioning）。
  - curl 检查失败 → 先确认计划中的端口；端口改过的话，先用新端口重
    跑检查再判定失败。
  - Manager 卡在 provisioning → `docker exec agentteams-controller
    agt get managers -o json` 加 `docker logs --tail 300
    agentteams-controller`；再看 [FAQ](../troubleshooting/faq.md)。

### Step 4——创建第一个 Worker（agent 可执行）

- **前置检查：** Step 3 已验证。
- **动作：** 按[快速入门](../../quickstart.md)创建第一个 Worker（经
  Element Web 或 controller 内的 `agt` CLI）。
- **验证：** `docker exec agentteams-controller agt get workers` 显示
  Worker 为 `Running`（或短暂处于 provisioning 状态），且 Worker 能
  在团队房间应答。
- **失败恢复：** provisioning 卡住 → 同 Step 3 的日志命令，外加
  Worker 容器日志；模型报错 → 重跑 Step 2 的连通性排查清单。

### Step 5——交接（人工）

- 把生成的管理员密码存入密码管理器。
- 保留 `~/.agentteams-llm-key`（权限 600）供升级使用；若 key 不应
  持久化则删除。安装配置 `${HOME}/agentteams-manager.env` 含敏感值
  ——保持其权限收紧，绝不提交进 Git 或贴到 issue 里。
- 在下方演练记录里登记本次结果。

## 演练记录

本剧本目前被验证过的内容，如实记录：

| 检查项 | 状态 | 证据 |
|---|---|---|
| 引用的每个环境变量都存在于 `install/agentteams-install.sh` | 已验证（静态） | 变量集与[本地部署指南](local.md#常用环境变量)的表格一致，该表跟踪安装器脚本 |
| `uninstall` 子命令、端口表、验证命令 | 已验证（静态） | 与本地部署指南一致 |
| 在真实 Docker 主机上从空环境到可用的完整演练 | **待办** | 撰写环境无容器运行时；演练需在真实主机上执行，并把结果（耗时、每次失败及走过的恢复路径）记录在这里 |

完成真实演练后，请追加：日期、主机、版本、哪些步骤失败、走了哪条
恢复路径，以及剧本中需要修正的措辞。
