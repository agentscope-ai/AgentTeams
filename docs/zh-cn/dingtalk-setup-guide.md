# AgentTeams/OpenClaw 钉钉机器人配置教程

本文档详细介绍如何在 AgentTeams/OpenClaw 中配置钉钉（DingTalk）机器人插件，实现企业内部 AI 助手功能。

---

## 📋 目录

1. [前置准备](#前置准备)
2. [钉钉开发者平台配置](#钉钉开发者平台配置)
3. [插件安装](#插件安装)
4. [配置文件修改](#配置文件修改)
5. [Docker 持久化配置](#docker-持久化配置)
6. [验证与测试](#验证与测试)
7. [常见问题](#常见问题)
8. [高级配置](#高级配置)

---

## 前置准备

### 基本要求

- 已安装 AgentTeams/OpenClaw
- 钉钉企业账号（需有应用开发权限）

---

## 钉钉开发者平台配置

### 1. 创建钉钉应用

> ⚠️ **注意**：不同开发者后台的创建步骤略有差异，请根据你使用的平台选择对应的操作步骤。

#### 钉钉开发者后台（公网钉钉）

访问 [钉钉开发者后台](https://open-dev.dingtalk.com/)

**步骤**：

1. **选择或创建组织**
   - **选择组织**：登录时会出现提醒，请选择有开发者权限的组织，或选择某个组织后获取开发者权限
   - **创建组织**：若无可用组织，使用钉钉移动端扫描二维码创建新组织（钉钉移动客户端版本需 ≥6.5.45）

2. **创建应用**
   - 在开发者后台，点击「创建」→「立即开始」进入钉钉应用页面
   - 在左侧导航栏点击「钉钉应用」，在右上角点击「创建应用」
   - 填写应用名称、应用描述，上传应用图标，完成后点击「保存」

3. **添加机器人能力**
   - 在应用详情页添加「机器人」能力

**参考文档**：[阿里云 - 快速部署并使用 OpenClaw（步骤三）](https://help.aliyun.com/zh/simple-application-server/use-cases/quickly-deploy-and-use-openclaw#54d46ca49f24d)

#### 阿里钉开放平台（阿里内网）

访问 [阿里钉开放平台](https://mapp.alibaba-inc.com/)

**步骤**：

1. **创建应用**（两种方式任选其一）
   - **方式一**：在首页点击「新建应用」按钮
   - **方式二**：在「工作台」标签页点击「创建阿里钉应用」

2. **填写应用信息**
   - 填写应用名称、应用描述后确定即可

3. **添加机器人**
   - 点击应用详情页左边栏「机器人」后点 「立即申请」
   - 完善申请信息：推荐选择群聊机器人，按照最小可见原则选择可见范围，入网环境为办公网，发起审批

### 2. 配置机器人

1. 进入「机器人」标签页
2. 配置机器人名称和头像
3. **消息接收模式**：选择 **Stream 模式**（无需公网 IP）
4. 发布应用

### 3. 获取凭证

从机器人详情页点击「查看凭证信息」获取以下信息：
appCode, appKey, appSecret, 钉CorpId, 钉AgentId等


---

## 插件安装

### 本地源码安装（推荐）

```bash
# 1. 克隆插件仓库（以给 AgentTeams Worker 配置钉钉机器人为例）
docker exec -it <你想配置钉钉机器人的容器> /bin/bash
cd /root/agentteams-fs/agents/<你的agt worker名>

# 创建插件代码存储目录
mkdir plugins
cd plugins
git clone https://github.com/soimy/openclaw-channel-dingtalk.git

# 2. 进入插件目录
cd openclaw-channel-dingtalk

# 3. 安装依赖
npm install
```

**预期结果**：
```
added 756 packages, and audited 757 packages in 3m
```

> 💡 **提示**：如下警告可忽略：
> ```
> To address all issues (including breaking changes), run:
>   npm audit fix --force
> ```
> 告警详情可根据提示使用命令 `npm audit` 查看。

```bash
# 4. 安装插件
openclaw plugins install -l .
```

**预期结果**：
```
Linked plugin path: /root/agentteams-fs/agents/xxxxxx/plugins/openclaw-channel-dingtalk
Restart the gateway to load plugins.
```

> ⚠️ **注意**：此时不着急重启 OpenClaw Gateway，可以继续后续步骤。

### 验证安装

```bash
openclaw plugins list
```

**预期结果**：应该能看到 `dingtalk` 插件出现在列表中。

---

## 配置文件修改

### 1. 在manager容器中找到worker OpenClaw的配置文件

注意agt worker的配置文件需要从manager的容器内进行修改。否则修改后worker容器重启后配置会被恢复。

```bash
# 进入manager容器编辑（manager容器中有worker配置文件,通过minio映射）
docker exec -it agentteams-manager /bin/bash
# 找到你的worker配置文件, 其中fbi-claw替换为你实际的worker名
ls -l /root/agentteams-fs/agents/fbi-claw/openclaw.json
# 使用vim进行修改(如vi命令不可用, 使用 apt update&apt-get install vim 安装)
vi /root/agentteams-fs/agents/fbi-claw/openclaw.json
```

### 2. 添加插件配置

在 `plugins` 部分添加 `dingtalk`：

```json
"plugins": {
  "load": {
    "paths": [
      "/opt/openclaw/extensions/matrix",
      "/root/agentteams-fs/agents/fbi-claw/plugins/openclaw-channel-dingtalk"
    ]
  },
  "entries": {
    "matrix": { "enabled": true },
    "dingtalk": { "enabled": true }
  }
}
```

### 3. 添加 Channel 配置

在 `channels` 部分添加 `dingtalk`（与 `matrix` 同级）：

```json
"channels": {
  "matrix": {
    "enabled": true,
    ...
  },
  "dingtalk": {
    "enabled": true,
    "clientId": "你的appKey",
    "clientSecret": "你的appSecret",
    "robotCode": "你的robotCode, 通常与appKey相同",
    "corpId": "你的钉CorpId",
    "agentId": "你的钉AgentId",
    "dmPolicy": "open",
    "groupPolicy": "open",
    "messageType": "markdown"
  }
}
```
上述信息都可以从钉钉开发者后台机器人详情页查看凭证信息获取。

### 4. 完整配置示例

```json
{
  "gateway": {
    "port": 18799,
    "mode": "local",
    "auth": {
      "token": "your-gateway-token"
    },
    "remote": {
      "token": "your-gateway-token"
    }
  },
  "channels": {
    "matrix": {
      "enabled": true,
      "homeserver": "http://matrix-local.agentteams.io:8080",
      "accessToken": "your-matrix-token",
      "dm": {
        "policy": "allowlist",
        "allowFrom": ["@admin:matrix-local.agentteams.io:18080"]
      },
      "groupPolicy": "allowlist",
      "groupAllowFrom": ["@admin:matrix-local.agentteams.io:18080"],
      "groups": {
        "*": { "allow": true, "requireMention": true }
      }
    },
    "dingtalk": {
      "enabled": true,
      "clientId": "dingxxxxxxxxxxxxxxxx",
      "clientSecret": "your-app-secret",
      "robotCode": "dingxxxxxxxxxxxxxxxx",
      "corpId": "dingxxxxxxxx",
      "agentId": "123456789",
      "dmPolicy": "open",
      "groupPolicy": "open",
      "messageType": "markdown"
    }
  },
  "plugins": {
    "load": {
      "paths": [
        "/opt/openclaw/extensions/matrix",
        "/root/agentteams-fs/agents/fbi-claw/plugins/openclaw-channel-dingtalk"
      ]
    },
    "entries": {
      "matrix": { "enabled": true },
      "dingtalk": { "enabled": true }
    }
  },
  "models": {
    "mode": "merge",
    "providers": {
      "hiclaw-gateway": {
        "baseUrl": "http://aigw-local.agentteams.io:8080/v1",
        "apiKey": "your-api-key",
        "api": "openai-completions",
        "models": [
          {
            "id": "your model id",
            "name": "your model name",
            "reasoning": true,
            "contextWindow": 200000,
            "maxTokens": 128000
          }
        ]
      }
    }
  }
}
```
修改后注意保存！
强烈建议验证通过后修改配置文件中的dmPolicy控制机器人只能和你指定的人和钉群聊天，详见[白名单模式](### 白名单模式)

### 5. 重启 Gateway

```bash
docker restart <你的agt worker 容器id>
```
使用docker ps -a查看容器信息，例如 docker ps -a 输出如下：
799c1ca06455  <镜像>  <时间>   8001/tcp, 8080/tcp, 8443/tcp agentteams-worker-fbi-claw
则docker restart agentteams-worker-fbi-claw 重启这个worker容器

---

## 验证与测试

### 1. 检查插件状态

```bash
# 进入worker容器
docker exec -it <你的agt worker 容器id> /bin/bash

# 查看插件是否加载成功
openclaw plugins list| grep dingtalk
```

**预期结果**：
```
│ dingtalk │ loaded │ ...
```

### 2. 检查配置文件内容

```bash
# 查看配置文件内容是否符合预期
cat ~/.openclaw/openclaw.json
```

预期：刚才在manager容器中修改的配置都还在。

### 3. 私聊测试

在钉钉中搜索机器人名称，发送消息测试响应。

### 4. 群聊测试

1. 将机器人添加到群聊
2. 在群中 @机器人 发送消息
3. 验证机器人响应

---



## 高级配置

### 配置选项说明

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | boolean | `true` | 是否启用 |
| `clientId` | string | - | 钉钉 AppKey |
| `clientSecret` | string | - | 钉钉 AppSecret |
| `robotCode` | string | - | 机器人码（通常等于 clientId） |
| `corpId` | string | - | 企业 ID |
| `agentId` | string | - | 应用 ID |
| `dmPolicy` | string | `"open"` | 私聊策略：`open`/`allowlist`/`pairing` |
| `groupPolicy` | string | `"open"` | 群聊策略：`open`/`allowlist` |
| `messageType` | string | `"markdown"` | 消息类型：`markdown`/`card` |
| `showThinking` | boolean | `false` | 显示 AI 思考状态（仅 markdown） |
| `thinkingMessage` | string | `"🤔 思考中..."` | 思考中提示文本 |
| `debug` | boolean | `false` | 调试模式 |

### AI 互动卡片模式

如需使用流式更新的 AI 卡片，请按以下步骤操作：

1. 在钉钉卡片平台创建模板
2. 导入插件提供的模板：`docs/cardTemplate.json`
3. 配置卡片参数：

```json
"dingtalk": {
  ...
  "messageType": "card",
  "cardTemplateId": "你的模板ID",
  "cardTemplateKey": "content"
}
```

### 白名单模式

```json
"dingtalk": {
  ...
  "dmPolicy": "allowlist",
  "dmAllowFrom": ["用户 ID 1", "用户 ID 2"],
  "groupPolicy": "allowlist",
  "groupAllowFrom": ["群聊 ID 1", "群聊 ID 2"]
}
```

---

## 相关资源
- [钉钉插件 GitHub](https://github.com/soimy/openclaw-channel-dingtalk)
- [阿里云OpenClaw配置钉钉教程](https://help.aliyun.com/zh/simple-application-server/use-cases/quickly-deploy-and-use-openclaw?spm=a2c4g.11186623.help-menu-58607.d_3_0_0_0.23f736bcCYW9Ci&scm=20140722.H_3019202._.OR_help-T_cn~zh-V_1#54d46ca49f24d)

---

## 文档信息

- **文档版本**：1.0
- **最后更新**：2026-03-12
- **适用插件**：openclaw-channel-dingtalk
