# 模块更新与分发（Millforge 通道）

> 2026-11-08 新增，与模块更新端点（`#1526`）以及第一个经 Millforge 发布的 worker 插件（`qwenpaw-qwencode-bridge`）同步落地。

AgentTeams 把部分运行时构件作为**模块**发布（TeamHarness、Matrix bundle、Qwen Code bridge 等）。模块可以在**运行中的部署**上安装与升级，**无需重建镜像**。发布产物与通道索引放在独立仓库（**Millforge** 分发镜像），AgentTeams 仓仍是源码事实源。

本文是三个安装平面、发布索引与 `channels.json` 格式的契约。

## 适用范围与前提

* controller 需带模块更新构建（`GET /api/v1/millforge/index` + 三个安装路由）。**容器（docker）部署支持安装；Kubernetes 部署的安装路由返回 503**。
* 需要出网访问 `raw.githubusercontent.com`（GitHub）或 `raw.gitcode.com`（GitCode）——controller **只**从这两个主机拉索引与产物。
* 目标 QwenPaw worker 的工作目录必须可写；插件安装到 `<工作目录>/plugins/<plugin-id>`。

## 为什么要有分发仓

上游 AgentTeams 仓无法在一个发布窗口内合完所有功能 PR，而为每个插件变更重建镜像又太慢。因此模块发布到独立镜像仓，分三个通道：

| 通道 | 含义 | 适用人群 |
|:--|:--|:--|
| `stable` | 已评审、来自上游 tag/mainline 的正式发布 | 生产部署 |
| `beta` | 发布候选，产物形状相同，内容可能变化 | 尝鲜者 |
| `alpha` | 领先上游合并的预发布（已验收但尚未合入的成果） | 在上游落地前试用新能力 |

默认 Millforge 仓为 `LUOSENGWA/millforge`，布局：

```text
releases/
├── alpha/channels.json          # 每个通道一个索引
├── beta/channels.json
├── stable/channels.json
├── <module>/<version>/          # 产物只存一份，通道只决定可见性
│   ├── <module>-<version>.zip   # 或 <pluginId>-<version>.zip
│   ├── sha256sums.txt
│   └── release-manifest.json
└── looprunner/                  # 预留 runner 槽位（暂不发布）
```

产物物理位置固定在 `releases/<module>/<version>/`；**通道索引只决定某个通道能看到哪些版本**。

## 发布索引

### `GET /api/v1/millforge/index`

拉取并校验某通道索引，附带取数元信息返回。

| 查询参数 | 必填 | 默认 | 说明 |
|:--|:--|:--|:--|
| `source` | 是 | — | `github` 或 `gitcode` |
| `repo` | 否 | `LUOSENGWA/millforge` | 必须是 `org/name` |
| `ref` | 否 | `main` | 分支、tag 或提交 |
| `channel` | 否 | `beta` | `alpha`、`beta` 或 `stable` |

响应：

```json
{
  "schema": 1,
  "channel": "stable",
  "modules": {
    "qwenpaw-qwencode-bridge": {
      "kind": "plugin",
      "pluginId": "qwen-code-bridge",
      "latest": "0.3.0",
      "versions": {
        "0.3.0": {
          "artifacts": [
            {"name": "qwenpaw-qwencode-bridge-0.3.0.zip",
             "sha256": "01efceee92716c9835e3b958337f427f1a6bea22f1a778d39206dc14ce7b4fa0"}
          ],
          "requires": {"qwenpaw": ">=2.2.0 <2.3.0", "qwen-code-cli": "0.26.x"},
          "commit": "c31dc45df",
          "published_at": "2026-11-08T01:30:00Z",
          "deprecated": false
        }
      }
    }
  },
  "source": "github",
  "repo": "LUOSENGWA/millforge",
  "fetched_at": "2026-11-08T01:30:12Z"
}
```

controller 侧强制的取数规则：仅允许上述两个 raw 主机、**禁止跨主机重定向**、10 秒超时、响应体 1 MiB 上限。上游 `404` 映射为 `404`，其它上游失败映射为 `502`。

### `channels.json` 契约

```json
{
  "schema": 1,
  "channel": "stable",
  "updated_at": "2026-11-08T01:30:00Z",
  "modules": {
    "<module>": {
      "kind": "plugin",
      "pluginId": "…",
      "latest": "x.y.z",
      "versions": {
        "x.y.z": {
          "artifacts": [{"name": "…", "sha256": "…"}],
          "requires": {"…": "…"},
          "commit": "…",
          "published_at": "…",
          "deprecated": false
        }
      }
    }
  }
}
```

| 字段 | 规则 |
|:--|:--|
| `schema` | 必须为 `1`。 |
| `channel` | 必须与目录一致（`releases/<channel>/channels.json`）。 |
| `kind` | `plugin`、`bundle` 或 `runner`。**`bundle` 必须带一个与模块名不同的 `pluginId`**（安装到 QwenPaw 的插件 id ≠ bundle id）。`runner` 不能按 QwenPaw 插件安装。 |
| `pluginId` | 实际安装的 QwenPaw 插件 id；缺省 = 模块名。 |
| `latest` | 该通道内最高版本；该通道无发布时为 `null`。 |
| `versions.<v>.artifacts` | `{name, sha256}`，对应 `releases/<module>/<v>/`。**索引里的 sha256 是权威值**，安装器按它校验下载。 |
| `versions.<v>.requires` | 安装前检查的运行时要求（插件：`qwenpaw` / `qwen-code-cli`；bundle：`agentteams` / `contracts` / `tool_schema`）。 |
| `versions.<v>.commit` | 构建该产物的源码提交。 |
| `versions.<v>.deprecated` | `true` = 已停用版本，安装器不应默认选择。 |

客户端选择规则：

1. 请求省略 `version` 时用 `latest`。
2. 请求省略 `artifact` 时取第一个 `.zip`（一个版本可能同时带运行时无关的 `.tar.gz`）。
3. 版本按 **semver** 比较：`0.1.1-alpha.2 < 0.1.1`，但 `0.1.1-alpha.2 > 0.1.0`。
4. 允许显式钉版本降级；客户端应提示。

## 安装端点

| 平面 | 路由 | 授权 |
|:--|:--|:--|
| Worker | `POST /api/v1/workers/{name}/plugins/install` | `update` on `module` |
| Team | `POST /api/v1/teams/{name}/modules/{module}/install` | `update` on `team` |
| Manager | `POST /api/v1/managers/{name}/plugins/install` | `update` on `manager` |

共用请求体：

```json
{
  "module": "qwenpaw-qwencode-bridge",
  "source": "github",
  "repo": "LUOSENGWA/millforge",
  "ref": "main",
  "channel": "stable",
  "version": "0.3.0",
  "artifact": "qwenpaw-qwencode-bridge-0.3.0.zip",
  "sha256": "01efceee…"
}
```

`version`、`artifact` 可选（缺省用索引 `latest` / 第一个 `.zip`）。`sha256` 可选；一旦提供必须与索引一致，否则 `409`。

响应：

```json
// worker / manager
{"installed": true, "loaded": true, "previous_version": "0.2.0"}

// team
{"updated": ["worker-a", "worker-b"], "target_version": "0.3.0",
 "module": "qwenpaw-qwencode-bridge", "pluginId": "qwen-code-bridge",
 "members": {"worker-a": "0.3.0", "worker-b": "0.3.0"}, "consistent": true}
```

行为：

* 每次安装 controller 都会**重读通道索引**并以索引校验和为准；产物只从白名单 raw 主机下载（60 秒超时、64 MiB 上限），校验通过后才上传。
* team 安装按 `workerMembers` **顺序滚动，首个失败即停**；响应列出已更新成员与失败成员。
* kube 模式：安装路由返回 `503`（镜像式部署靠重建更新）。
* manager 平面：仅 QwenPaw manager 可安装，其它运行时返回 `400`。
* `runner` 模块在这些路由上返回 `400`。

## 读取已安装状态

| 路由 | 返回 |
|:--|:--|
| `GET /api/v1/workers/{name}/plugins` | 单个 worker 上已安装的插件（`id`、`version`、`loaded` 等） |
| `GET /api/v1/managers/{name}/plugins` | manager 上已安装的插件 |
| `GET /api/v1/teams/{name}/modules` | 跨 `workerMembers` 的聚合视图：`module`、`pluginId`、各成员版本、`consistent` |

dashboard 的「检查更新」就是拿这些状态与通道索引对比。

## 端到端示例

```bash
# 1) stable 有什么？
curl -s "http://<controller>/api/v1/millforge/index?source=github&channel=stable" \
  | jq '.modules["qwenpaw-qwencode-bridge"].latest'

# 2) 装到某个 worker
curl -s -X POST "http://<controller>/api/v1/workers/worker-a/plugins/install" \
  -H 'Content-Type: application/json' \
  -d '{"module":"qwenpaw-qwencode-bridge","source":"github","channel":"stable"}'

# 3) 验证
curl -s "http://<controller>/api/v1/workers/worker-a/plugins" \
  | jq '.[] | select(.id=="qwen-code-bridge")'
```

## 失败模式

| 现象 | 原因 | 处理 |
|:--|:--|:--|
| `404 module not found in index` | 模块键写错，或该通道没有这个模块 | 检查该模块的 `latest`；换通道试试 |
| 安装返回 `409` | body 的 `sha256` 与索引不一致，或两次请求之间索引变了 | 省略 `sha256`（以索引为准）或重新取索引 |
| `502 millforge index: upstream unavailable` | raw 主机不可达 / 索引路径错 | 核对 `repo`/`ref`/`channel` 与网络 |
| `400 bundle modules require an adapter artifact with a distinct pluginId` | `kind: bundle` 条目缺 `pluginId` | 由发布方修 `channels.json` |
| `400 runner modules are not installed as qwenpaw plugins` | 试图安装 `looprunner` | runner 走独立部署路径 |
| `503` | Kubernetes 模式 | 改为重建镜像 |

## 认证与授权

与 controller 其它 API 一致，接受两种 bearer 令牌：

1. **Kubernetes service-account token**（TokenReview）：admin / manager / worker。
2. **Matrix access token**（L2 人类）：经 `GET /_matrix/client/v3/account/whoami` 校验；对应 localpart 须映射到 `permissionLevel: 2` 的 `Human` CR。

读操作需要 `get`，安装需要目标资源的 `update`（`module` / `team` / `manager`）。Team Leader 只能装进自己的 team。

## 相关文档

* [插件清单](../../../plugins/README.md) — `plugins/` 下有什么、各种形态怎么装。
* [Qwen Code bridge](../../../plugins/qwen-code-bridge/README.md) — 第一个经该通道发布的 worker 插件。
* [本地部署](deployment/local.md) — 镜像式升级。
