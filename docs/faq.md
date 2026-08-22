# FAQ

- [How to check the current HiClaw version](#how-to-check-the-current-hiclaw-version)
- [How to connect Feishu/DingTalk/WeCom/Discord/Telegram](#how-to-connect-feishudingtalkwecomdiscordtelegram)
- [Installation script exits immediately on Windows](#installation-script-exits-immediately-on-windows)
- [Manager Agent startup timeout or failure](#manager-agent-startup-timeout-or-failure)
- [Accessing the web UI from other devices on the LAN](#accessing-the-web-ui-from-other-devices-on-the-lan)
- [Cannot connect to Matrix server locally](#cannot-connect-to-matrix-server-locally)
- [How to talk to a Worker directly](#how-to-talk-to-a-worker-directly)
- [How to switch the Manager's model](#how-to-switch-the-managers-model)
- [How to switch a Worker's model](#how-to-switch-a-workers-model)
- [Does HiClaw support sending and receiving files](#does-hiclaw-support-sending-and-receiving-files)
- [Why does Manager/Worker keep showing "typing"](#why-does-managerworker-keep-showing-typing)
- [Manager/Worker not responding to messages](#managerworker-not-responding-to-messages)
- [Manager not responding or returning error status codes](#manager-not-responding-or-returning-error-status-codes)
- [HTTP 401: invalid access token or token expired](#http-401-invalid-access-token-or-token-expired)
- [Session management via IM](#session-management-via-im)

---

## How to check the current HiClaw version

Run the following command to see the installed version:

```bash
docker exec hiclaw-manager cat /opt/hiclaw/agent/.builtin-version
```

To install a specific version, use the `HICLAW_VERSION` environment variable during installation:

```bash
HICLAW_VERSION=v1.0.5 bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

---

## Installation script exits immediately on Windows

If the PowerShell installation script closes immediately after launching, first check whether Docker Desktop is installed. If it is installed, make sure it is actually running — Docker Desktop must be started and fully loaded before the script can connect to the Docker daemon.

---

## Manager Agent startup timeout or failure

If the Manager Agent is unresponsive after installation
, or fails to start
, check the logs inside the container:

```bash
docker exec -it hiclaw-manager cat /var/log/hiclaw/manager-agent.log
```

**Case 1: Log shows a process exit**

The Docker VM may not have enough memory. Increase it to at least 4GB: Docker Desktop → Settings → Resources → Memory. Then re-run the install command.

**Case 2: No process exit in logs, but some components won't start**

This is likely caused by stale config data. Re-run the install command from the original install directory and choose **delete and reinstall**:

```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

When the installer detects an existing installation, it will ask how to proceed. Choosing delete will wipe the stale data and start fresh.

**Case 3: Mac with Apple Silicon and outdated Docker/Podman**

If you're using a Mac with Apple Silicon (M1/M2/M3/M4) and Docker Desktop is older than 4.39.0, Manager Agent may fail to start properly.

**Solutions:**

- **Docker Desktop**: Upgrade to 4.39.0 or later
- **Podman**: Ensure Podman Engine **Server version ≥ 5.7.1** (check with `podman version`)

---

## Accessing the web UI from other devices on the LAN

**Accessing Element Web**

On another device on the same network, open a browser and go to:

```
http://<LAN-IP>:18088
```

The browser may warn about an insecure connection — ignore it and click Continue.

**Updating the Matrix Server address**

The default Matrix Server hostname resolves to `localhost`, which won't work from other devices. When logging into Element Web, change the Matrix Server address to:

```
http://<LAN-IP>:18080
```

For example, if your LAN IP is `192.168.1.100`, enter `http://192.168.1.100:18080`.

---

## Cannot connect to Matrix server locally

If the Matrix server is unreachable even on the local machine, check whether a proxy is enabled in your browser or system. The `*-local.hiclaw.io` domain resolves to `127.0.0.1` by default — if traffic is routed through a proxy, requests will never reach the local server.

Disable the proxy, or add `*-local.hiclaw.io` / `127.0.0.1` to your proxy bypass list.

---

## How to talk to a Worker directly

After creating a Worker, Manager automatically adds you and the Worker to a shared group room. In that room, you must **@mention the Worker** for it to respond — messages without a mention are ignored.

When using Element or similar clients, type `@` followed by the first letter(s) of the Worker's display name to trigger autocomplete and select the right user.

Alternatively, you can click the Worker's avatar and open a **direct message** (DM) conversation. In a DM you don't need to @mention — every message triggers the Worker. Keep in mind that Manager is not in the DM room and won't see any of that conversation.

---

## How to switch the Manager's model

HiClaw supports two ways to switch models: **switch the current session model** (instant, non-persistent) and **switch the primary model** (persistent, requires restart).

### Option 1: Switch the current session model (instant, non-persistent)

Use the `/model` slash command in IM to instantly switch the model for the current session, no restart needed:

```
/model qwen3.5-plus
```

This only affects the current session — the primary model is restored after a restart. Only pre-configured known models are supported; see [`manager/configs/known-models.json`](../manager/configs/known-models.json) for the full list.

For more `/model` command usage, see the "Model selection" section in [Session management via IM](#session-management-via-im).

### Option 2: Switch the primary model (persistent, requires restart)

Use Manager's built-in **model-switch skill** to persistently change the primary model. This approach supports any model name (not limited to the pre-configured list), but if the target model is not already in the config, a container restart is required for it to take effect.

**Why use Manager instead of manual config?**

OpenClaw requires setting the model's context window size (`contextWindow`) in its config. HiClaw defaults to qwen3.5-plus's 200K token window. If you switch to a model with a different window without updating this setting, the session may fail when approaching the window limit — OpenClaw won't know when to compress context.

The model-switch skill:
1. Looks up the correct `contextWindow` and `maxTokens` for the target model
2. Updates OpenClaw's config accordingly
3. Tests connectivity before applying the change

**Step 1: Configure Higress AI Route**

In the Higress console, configure the AI route to point to your LLM provider:

- **Single provider**: Set up `default-ai-route` to route requests to your provider.
- **Multiple providers**: Create multiple AI routes with different model name matching rules (prefix or regex) pointing to each provider.

Reference: [Higress AI Quick Start — Console Configuration](https://higress.ai/en/docs/ai/quick-start#console-configuration)

**Step 2: Tell Manager to switch**

Simply tell Manager the model name, e.g.:
> "Switch to `claude-3-5-sonnet`"

Manager will use the model-switch skill to update the config and verify connectivity.

**Troubleshooting**: If the switch doesn't seem to work, Manager may not have invoked the model-switch skill. Explicitly ask it to use the skill:
> "Use the model-switch skill to switch to `claude-3-5-sonnet`"

---

## How to switch a Worker's model

Two options are available: **switch the current session model** and **switch the primary model**.

### Option 1: Switch the current session model (instant, non-persistent)

In the Worker's group chat or DM, use @mention with the `/model` command to switch instantly:

```
@alice /model qwen3.5-plus
```

Only affects the current session — the primary model is restored after a restart. Only pre-configured known models are supported; see [`manager/configs/known-models.json`](../manager/configs/known-models.json) for the full list.

### Option 2: Switch the primary model (persistent, requires restart)

Manager handles this for you, and supports any model name (not limited to the pre-configured list).

**At creation time**: When asking Manager to create a Worker, specify the model name directly, e.g. "Create a Worker named alice using `qwen3.5-plus`."

**After creation**: Tell Manager at any time to switch a Worker's model, e.g. "Switch alice to use `claude-3-5-sonnet`." Manager will update the Worker's configuration accordingly.

Make sure Higress is configured to route the target model name to the correct provider before switching. See below for details.

---

**Higress Console Configuration**

**Single provider**

In the Higress console, set up `default-ai-route` to route requests to your LLM provider. Then tell Manager the model name you want the Worker to use (e.g. `qwen3.5-plus`). Manager will run a connectivity test with that model name and complete the switch automatically.

**Multiple providers**

In the Higress console, create multiple AI routes with different model name matching rules (prefix or regex), each pointing to the corresponding provider. The rest of the flow is the same as single provider — tell Manager the Worker's target model name, and it will handle the test and switch.

Reference: [Higress AI Quick Start — Console Configuration](https://higress.ai/en/docs/ai/quick-start#console-configuration)

---

## Does HiClaw support sending and receiving files

**Receiving files from you**: Yes. You can upload a file directly in Element Web (the attachment button), and Manager or Worker will receive it as a Matrix media message and can read its content.

**Sending files to you**: Yes. When you ask Manager (or a Worker) to send you a file — such as a task output artifact, a generated report, or any file it has access to — it will upload the file to the Matrix media server and send it to the room as a downloadable attachment. You can then click to download it in Element Web.

---

## Why does Manager/Worker keep showing "typing"

This is normal — it means the underlying OpenClaw agent engine is actively executing. HiClaw sets a 30-minute timeout per task, so an agent can stay in this state for up to 30 minutes while working.

To see what the agent is actually doing, exec into the Manager or Worker container and check the session logs:

```bash
# For Manager
docker exec -it hiclaw-manager ls .openclaw/agents/main/sessions/

# For a Worker (replace <worker-name> with the actual container name)
docker exec -it <worker-name> ls .openclaw/agents/main/sessions/
```

The `.jsonl` files in that directory are written by OpenClaw in real time and contain the full agent execution trace — LLM calls, tool use, reasoning steps, etc.

---

## Manager/Worker not responding to messages

If Manager or Worker doesn't respond to your messages, check these common causes:

### 1. Check if the agent is working

**If there's no response and no "typing" indicator**, the agent is almost certainly **busy working**.

OpenClaw limits the "typing" indicator to a maximum of **2 minutes**. If the agent's task takes longer than 2 minutes, the typing indicator stops showing even though the agent is still working.

**How to confirm your message is queued**:
- After sending a message, look for a small **"m" icon** on the right side of your message
- This icon indicates the Manager has **read** your message
- When you see this icon, your message is in the queue and will be processed after the current task finishes

### 2. Check the chat environment

**Direct message vs. group chat**:
- In a **direct message** (DM, just you and one agent), every message triggers a response
- In a **group chat** (2+ participants), you must **@mention the agent** for it to respond — messages without mentions are ignored

### 3. Check session status

The OpenClaw session might be corrupted. Enter the Manager or Worker container and use the OpenClaw TUI to investigate:

```bash
# Manager
docker exec -it hiclaw-manager openclaw tui

# Worker (replace <worker-name> with actual container name)
docker exec -it <worker-name> openclaw tui
```

In the TUI:
1. Type `/sessions` to list all sessions
2. Switch to the session for the relevant chat
3. Try sending a message and observe if there are any errors

If the session is corrupted, try sending `/new` as a standalone message in the corresponding chat in Element (or other Matrix client) to reset the session and see if that restores normal behavior.

---

## Manager not responding or returning error status codes

If Manager stops responding or you see error codes like 404 or 503, check these common causes:

### 1. Check session status

The OpenClaw session might be corrupted. Enter the Manager container and use the OpenClaw TUI to investigate:

```bash
docker exec -it hiclaw-manager openclaw tui
```

In the TUI:
1. Type `/sessions` to list all sessions
2. Switch to the session for the relevant chat
3. Try sending a message and observe if there are any errors

If the session is corrupted, try sending `/new` as a standalone message in the corresponding chat in Element (or other Matrix client) to reset the session and see if that restores normal behavior.

### 2. Check Higress AI Gateway log

If resetting the session doesn't help, check the Higress AI Gateway log:

```bash
docker exec -it hiclaw-manager cat /var/log/hiclaw/higress-gateway.log
```

Search the log for the relevant status code. Common causes:

- **503**: The container can't reach the external LLM service — likely a network issue inside the container.
- **404**: The model name is probably wrong.

To determine whether the error came from the backend or from a Higress misconfiguration, check the `upstream_host` field in the log entry. If `upstream_host` has a value, the request reached the backend and the error was returned by the upstream service. If it's empty, Higress itself couldn't route the request.

### 3. Check model configuration

The model's context window size might be misconfigured, causing the window to fill up before compression happens. See [How to switch the Manager's model](#how-to-switch-the-managers-model) and [How to switch a Worker's model](#how-to-switch-a-workers-model) for proper configuration.

---

## HTTP 401: invalid access token or token expired

If you see this error when Manager or Worker tries to call the LLM, check whether you selected **Bailian Coding Plan** during installation but haven't activated it yet.

Bailian Coding Plan is a free trial program from Alibaba Cloud. To use it, you need to activate it first:

1. Visit: https://www.aliyun.com/benefit/scene/codingplan
2. Log in with your Alibaba Cloud account
3. Follow the instructions to activate the Coding Plan

After activation, re-run the installation or restart the Manager container. The token should work immediately.

---

## How to connect Feishu/DingTalk/WeCom/Discord/Telegram

HiClaw Manager is built on OpenClaw, which supports multiple messaging channels out of the box. To connect additional channels:

**Method 1: Edit config directly**

The Manager's working directory is `~/hiclaw-manager` (on your host). Edit `openclaw.json` in that directory to add channel configuration. Refer to [OpenClaw channel documentation](https://docs.openclaw.ai) for the specific config format for each platform.

After editing, restart the Manager container for changes to take effect:

```bash
docker restart hiclaw-manager
```

**Method 2: Let Manager learn from your existing OpenClaw config**

If you already use OpenClaw with other channels (e.g., in your personal setup), you can let Manager read your existing config:

- **Tell Manager where the file is**: In Element Web, tell Manager the path to your OpenClaw config file (e.g., "My OpenClaw config is at `/home/user/my-openclaw.json`"). Manager will read it directly.
- **Send the file as attachment**: In Element Web or any Matrix client, upload your config file as an attachment and send it to Manager. Manager will receive and read it.

Then ask Manager to help configure the same channels in its own config.

---

## Session management via IM

HiClaw uses OpenClaw with the Matrix channel (Element Web). OpenClaw supports **slash commands** that you can send directly in the chat as standalone messages. These commands are processed by the Gateway before the model sees them.

**Important:** Most commands must be sent as a **standalone** message that starts with `/`. Do not mix them with other text in the same message.

**In group rooms:** You can combine an @mention with a slash command in the same message, e.g. `@Worker /compact` or `@Worker /new`. The @mention ensures the command reaches the right agent, and the slash command is still processed by the Gateway as usual.

The following commands apply to OpenClaw (Manager and OpenClaw Workers). CoPaw Workers use a different command set — see [CoPaw Commands](https://copaw.agentscope.io/docs/commands) for details.

### Session reset and compaction

| Command | Description |
|---------|-------------|
| `/reset` or `/new` | Reset the current session and start a fresh conversation. The agent replies with a short hello to confirm. |
| `/new <model>` | Reset and optionally switch to a different model. Accepts model alias, `provider/model`, or provider name. |
| `/compact [instructions]` | Manually compact the conversation context. Use before long tasks or when switching topics to free up context window. |

### Model selection

| Command | Description |
|---------|-------------|
| `/model` or `/models` | Show a compact model picker (numbered list). |
| `/model list` | Same as `/model`. |
| `/model <number>` | Select a model by its number from the picker. |
| `/model <provider/model>` | Switch to a specific model, e.g. `/model openai/gpt-5.2` or `/model anthropic/claude-opus-4-5`. |
| `/model status` | Show detailed model/auth/endpoint status. |

### Other useful commands

| Command | Description |
|---------|-------------|
| `/status` | Show current status (including provider usage/quota when available). |
| `/help` | Show help. |
| `/commands` | List available commands. |
| `/stop` | Abort the current agent run. |

### Session directives (optional)

These directives control session behavior. Send as standalone messages to persist; they can also appear inline in a message but won't persist:

- `/think <off|minimal|low|medium|high|xhigh>` — Control thinking/reasoning level.
- `/verbose on|full|off` — Toggle verbose output (for debugging).
- `/reasoning on|off|stream` — Toggle separate reasoning messages.
- `/elevated on|off|ask|full` — Control exec approval behavior.
- `/queue` — View or configure queue settings (debounce, cap, etc.).

**Reference:** [OpenClaw Slash Commands](https://docs.openclaw.ai/tools/slash-commands)
