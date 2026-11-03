# AgentTeams/OpenClaw DingTalk Bot Configuration Guide

This guide explains how to configure the DingTalk bot plugin in AgentTeams/OpenClaw to provide an internal enterprise AI assistant.

---

## 📋 Table of contents

1. [Prerequisites](#prerequisites)
2. [Configure the DingTalk developer platform](#configure-the-dingtalk-developer-platform)
3. [Install the plugin](#install-the-plugin)
4. [Modify the configuration](#modify-the-configuration)
5. [Docker persistence configuration](#docker-persistence-configuration)
6. [Verification and testing](#verification-and-testing)
7. [Troubleshooting](#troubleshooting)
8. [Advanced configuration](#advanced-configuration)

---

## Prerequisites

### Basic requirements

- AgentTeams/OpenClaw is installed.
- You have a DingTalk enterprise account with application-development permissions.

---

## Configure the DingTalk developer platform

### 1. Create a DingTalk application

> ⚠️ **Note**: Application-creation steps differ slightly between developer portals. Follow the section for the platform you use.

#### DingTalk Developer Console (public DingTalk)

Open the [DingTalk Developer Console](https://open-dev.dingtalk.com/).

**Steps**:

1. **Select or create an organization**
   - **Select an organization**: When prompted during sign-in, select an organization in which you have developer permissions, or request developer permissions after selecting one.
   - **Create an organization**: If none is available, scan the QR code with DingTalk mobile to create one. DingTalk mobile 6.5.45 or later is required.

2. **Create an application**
   - In the developer console, select **Create** → **Get Started** to enter the DingTalk application page.
   - Select **DingTalk Applications** in the left navigation, then select **Create Application** in the upper-right corner.
   - Enter the application name and description, upload an icon, and select **Save**.

3. **Add bot capability**
   - Add the **Bot** capability on the application details page.

**Reference**: [Alibaba Cloud — Quickly deploy and use OpenClaw (Step 3)](https://help.aliyun.com/zh/simple-application-server/use-cases/quickly-deploy-and-use-openclaw#54d46ca49f24d)

#### AliDing Open Platform (Alibaba intranet)

Open the [AliDing Open Platform](https://mapp.alibaba-inc.com/).

**Steps**:

1. **Create an application** using either method:
   - **Method 1**: Select **New Application** on the home page.
   - **Method 2**: Select **Create AliDing Application** on the **Workbench** tab.

2. **Enter application information**
   - Enter the application name and description, then confirm.

3. **Add a bot**
   - Select **Bot** in the left navigation of the application details page, then select **Apply Now**.
   - Complete the application. A group-chat bot is recommended. Select the smallest necessary visibility scope, choose the office network as the access environment, and submit the approval request.

### 2. Configure the bot

1. Open the **Bot** tab.
2. Configure the bot name and avatar.
3. Set **Message receiving mode** to **Stream mode**, which does not require a public IP address.
4. Publish the application.

### 3. Obtain credentials

Select **View Credential Information** on the bot details page and obtain the following values:

`appCode`, `appKey`, `appSecret`, DingTalk `CorpId`, DingTalk `AgentId`, and related fields.

---

## Install the plugin

### Install from local source (recommended)

```bash
# 1. Clone the plugin repository. This example configures a DingTalk bot for an AgentTeams Worker.
docker exec -it <worker-container> /bin/bash
cd /root/agentteams-fs/agents/<agt-worker-name>

# Create a directory for plugin source code.
mkdir plugins
cd plugins
git clone https://github.com/soimy/openclaw-channel-dingtalk.git

# 2. Enter the plugin directory.
cd openclaw-channel-dingtalk

# 3. Install dependencies.
npm install
```

**Expected output**:

```text
added 756 packages, and audited 757 packages in 3m
```

> 💡 **Tip**: You can ignore the following warning:
>
> ```text
> To address all issues (including breaking changes), run:
>   npm audit fix --force
> ```
>
> Run `npm audit` to inspect the warning details.

```bash
# 4. Install the plugin.
openclaw plugins install -l .
```

**Expected output**:

```text
Linked plugin path: /root/agentteams-fs/agents/xxxxxx/plugins/openclaw-channel-dingtalk
Restart the gateway to load plugins.
```

> ⚠️ **Note**: You do not need to restart the OpenClaw Gateway yet. Continue with the following steps first.

### Verify installation

```bash
openclaw plugins list
```

**Expected result**: The `dingtalk` plugin appears in the list.

---

## Modify the configuration

### 1. Locate the Worker's OpenClaw configuration in the Manager container

Modify the `agt` Worker's configuration from inside the Manager container. Otherwise, the configuration may be restored when the Worker container restarts.

```bash
# Enter the Manager container. Worker configuration is available there through MinIO mapping.
docker exec -it agentteams-manager /bin/bash
# Locate the Worker configuration. Replace fbi-claw with the actual Worker name.
ls -l /root/agentteams-fs/agents/fbi-claw/openclaw.json
# Edit the file. If vi is unavailable, install vim with apt update and apt-get install vim.
vi /root/agentteams-fs/agents/fbi-claw/openclaw.json
```

### 2. Add the plugin configuration

Add `dingtalk` to the `plugins` section:

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

### 3. Add the channel configuration

Add `dingtalk` to the `channels` section at the same level as `matrix`:

```json
"channels": {
  "matrix": {
    "enabled": true,
    ...
  },
  "dingtalk": {
    "enabled": true,
    "clientId": "your-app-key",
    "clientSecret": "your-app-secret",
    "robotCode": "your-robot-code-usually-the-same-as-app-key",
    "corpId": "your-dingtalk-corp-id",
    "agentId": "your-dingtalk-agent-id",
    "dmPolicy": "open",
    "groupPolicy": "open",
    "messageType": "markdown"
  }
}
```

Obtain these values by selecting **View Credential Information** on the bot details page in the DingTalk Developer Console.

### 4. Complete configuration example

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
      "agentteams-gateway": {
        "baseUrl": "http://aigw-local.agentteams.io:8080/v1",
        "apiKey": "your-api-key",
        "api": "openai-completions",
        "models": [
          {
            "id": "your-model-id",
            "name": "your-model-name",
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

Save the file after editing.

After verification succeeds, strongly consider changing `dmPolicy` so that the bot can chat only with specified users and DingTalk groups. See [Allowlist mode](#allowlist-mode).

### 5. Restart the Gateway

```bash
docker restart <agt-worker-container-id>
```

Use `docker ps -a` to find the container. For example:

```text
799c1ca06455  <image>  <time>   8001/tcp, 8080/tcp, 8443/tcp agentteams-worker-fbi-claw
```

In this example, run `docker restart agentteams-worker-fbi-claw` to restart the Worker container.

---

## Docker persistence configuration

The plugin source and `openclaw.json` are stored under the Worker's AgentTeams data directory. Make sure this directory remains on the persistent AgentTeams volume. Do not install the plugin only into an ephemeral container path outside `/root/agentteams-fs/agents/<worker-name>`.

---

## Verification and testing

### 1. Check plugin status

```bash
# Enter the Worker container.
docker exec -it <agt-worker-container-id> /bin/bash

# Confirm that the plugin loaded successfully.
openclaw plugins list | grep dingtalk
```

**Expected output**:

```text
│ dingtalk │ loaded │ ...
```

### 2. Check the configuration

```bash
# Confirm that the configuration has the expected content.
cat ~/.openclaw/openclaw.json
```

Expected result: The configuration edited in the Manager container is still present.

### 3. Test a direct message

Search for the bot name in DingTalk and send a message to test the response.

### 4. Test a group chat

1. Add the bot to a group chat.
2. @mention the bot in the group and send a message.
3. Confirm that the bot responds.

---

## Troubleshooting

If the bot does not respond, check the following in order:

1. Confirm that the DingTalk application is published and uses Stream mode.
2. Confirm that `clientId`, `clientSecret`, `robotCode`, `corpId`, and `agentId` match the bot credentials.
3. Run `openclaw plugins list | grep dingtalk` and confirm that the plugin state is `loaded`.
4. Check the Worker container logs for plugin or channel errors.
5. Confirm that `dmPolicy`, `groupPolicy`, and their allowlists permit the sender or group.

---

## Advanced configuration

### Configuration options

| Parameter | Type | Default | Description |
|------|------|--------|------|
| `enabled` | boolean | `true` | Whether the channel is enabled |
| `clientId` | string | - | DingTalk AppKey |
| `clientSecret` | string | - | DingTalk AppSecret |
| `robotCode` | string | - | Bot code, normally equal to `clientId` |
| `corpId` | string | - | Enterprise ID |
| `agentId` | string | - | Application ID |
| `dmPolicy` | string | `"open"` | Direct-message policy: `open`, `allowlist`, or `pairing` |
| `groupPolicy` | string | `"open"` | Group-chat policy: `open` or `allowlist` |
| `messageType` | string | `"markdown"` | Message type: `markdown` or `card` |
| `showThinking` | boolean | `false` | Show AI thinking status in Markdown mode |
| `thinkingMessage` | string | `"🤔 Thinking..."` | Message displayed while thinking |
| `debug` | boolean | `false` | Debug mode |

### AI interactive-card mode

To use streaming AI cards:

1. Create a template in the DingTalk Card Platform.
2. Import the template provided by the plugin at `docs/cardTemplate.json`.
3. Configure the card parameters:

```json
"dingtalk": {
  ...
  "messageType": "card",
  "cardTemplateId": "your-template-id",
  "cardTemplateKey": "content"
}
```

### Allowlist mode

```json
"dingtalk": {
  ...
  "dmPolicy": "allowlist",
  "dmAllowFrom": ["user-id-1", "user-id-2"],
  "groupPolicy": "allowlist",
  "groupAllowFrom": ["group-chat-id-1", "group-chat-id-2"]
}
```

---

## Related resources

- [DingTalk plugin on GitHub](https://github.com/soimy/openclaw-channel-dingtalk)
- [Alibaba Cloud OpenClaw DingTalk configuration guide](https://help.aliyun.com/zh/simple-application-server/use-cases/quickly-deploy-and-use-openclaw?spm=a2c4g.11186623.help-menu-58607.d_3_0_0_0.23f736bcCYW9Ci&scm=20140722.H_3019202._.OR_help-T_cn~zh-V_1#54d46ca49f24d)

---

## Document information

- **Document version**: 1.0
- **Last updated**: 2026-03-12
- **Applicable plugin**: `openclaw-channel-dingtalk`
