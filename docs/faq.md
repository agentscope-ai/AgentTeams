# FAQ

> **Using AgentTeams v1.0.9 or earlier?** The architecture changed significantly in v1.1.0. For the legacy single-container architecture, see [FAQ (Legacy Architecture)](faq-legacy.md).

- [How to check the current AgentTeams version](#how-to-check-the-current-agentteams-version)
- [Understanding the new architecture (v1.1.0+)](#understanding-the-new-architecture-v110)
- [How to use the agt CLI to manage resources](#how-to-use-the-agt-cli-to-manage-resources)
- [How to configure GitHub credentials for Workers](#how-to-configure-github-credentials-for-workers)
- [How to connect Feishu/DingTalk/WeCom/Discord/Telegram](#how-to-connect-feishudingtalkwecomdiscordtelegram)
- [Installation script exits immediately on Windows](#installation-script-exits-immediately-on-windows)
- [Installation fails: "manifest unknown" for embedded image](#installation-fails-manifest-unknown-for-embedded-image)
- [Manager Agent startup timeout or failure](#manager-agent-startup-timeout-or-failure)
- [Accessing the web UI from other devices on the LAN](#accessing-the-web-ui-from-other-devices-on-the-lan)
- [Element says the homeserver URL is not a valid Matrix server](#element-says-the-homeserver-url-is-not-a-valid-matrix-server)
- [Cannot connect to Matrix server locally](#cannot-connect-to-matrix-server-locally)
- [How to talk to a Worker directly](#how-to-talk-to-a-worker-directly)
- [How to connect third-party, local, or multi-provider models](#how-to-connect-third-party-local-or-multi-provider-models)
- [Why does my custom Higress AI route never match](#why-does-my-custom-higress-ai-route-never-match)
- [How to switch the Manager's model](#how-to-switch-the-managers-model)
- [How to switch a Worker's model](#how-to-switch-a-workers-model)
- [How to configure OpenRouter or another model provider with slashes in model names](#how-to-configure-openrouter-or-another-model-provider-with-slashes-in-model-names)
- [How to switch a Worker's runtime](#how-to-switch-a-workers-runtime)
- [Why does QwenPaw still use `copaw` in runtime values or image names](#why-does-qwenpaw-still-use-copaw-in-runtime-values-or-image-names)
- [Can I connect my own agent implementation as a Worker](#can-i-connect-my-own-agent-implementation-as-a-worker)
- [Can AgentTeams connect to an existing Higress instance](#can-agentteams-connect-to-an-existing-higress-instance)
- [How to use the Worker Template Marketplace](#how-to-use-the-worker-template-marketplace)
- [Does AgentTeams support sending and receiving files](#does-agentteams-support-sending-and-receiving-files)
- [Why does Manager/Worker keep showing "typing"](#why-does-managerworker-keep-showing-typing)
- [Manager/Worker not responding to messages](#managerworker-not-responding-to-messages)
- [Manager not responding or returning error status codes](#manager-not-responding-or-returning-error-status-codes)
- [HTTP 401: invalid access token or token expired](#http-401-invalid-access-token-or-token-expired)
- [How to view Manager Agent logs](#how-to-view-manager-agent-logs)
- [Session management via IM](#session-management-via-im)

---

## How to check the current AgentTeams version

Run the following command to see the installed version:

```bash
docker exec agentteams-manager cat /opt/agentteams/agent/.builtin-version
```

In v1.1.0+ installs, you can also query the controller-side CLI:

```bash
docker exec agentteams-controller agt version
```

Older `latest` images may print a commit hash instead of a semantic version if
that image was rebuilt before version metadata was standardized. In that case,
match the hash against the release or commit history, or upgrade with an
explicit `AGENTTEAMS_VERSION`.

To install a specific version, use the `AGENTTEAMS_VERSION` environment variable during installation:

```bash
AGENTTEAMS_VERSION=v1.1.0 bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

---

## Understanding the new architecture (v1.1.0+)

Starting from v1.1.0, AgentTeams switched from a **single all-in-one container** to a **multi-container architecture** managed by `agentteams-controller`:

| Component | Old (≤v1.0.9) | New (v1.1.0+) |
|-----------|---------------|---------------|
| Infrastructure (Higress, Tuwunel, MinIO, Element Web) | Bundled inside `agentteams-manager` | Runs in `agentteams-controller` container (from the `agentteams-embedded` image) |
| Manager Agent | Inside `agentteams-manager` | Separate `agentteams-manager` container (lightweight, agent only) |
| Worker management | Shell scripts (`create-worker.sh`) + `workers-registry.json` | Declarative CRDs via `agt` CLI (`agt create worker`, `agt apply`) |
| Worker runtimes | OpenClaw only | OpenClaw, **QwenPaw** (Python; formerly **CoPaw**), or Hermes |

**Key benefits:**
- The Manager image is ~1.7 GB smaller (no longer ships Higress binaries)
- Workers are managed declaratively — define YAML, apply, done
- Three worker runtime choices: OpenClaw (Node.js), QwenPaw (Python; formerly **CoPaw**), Hermes
- Team support with Team Leader DAG orchestration
- Worker Template Marketplace for one-click Worker provisioning

**What you'll see after installation:**

```bash
docker ps
# agentteams-controller    -- Controller + all infrastructure services
# agentteams-manager       -- Manager Agent (lightweight)
# agentteams-worker-alice  -- Worker containers (created on demand)
```

---

## How to use the agt CLI to manage resources

The `agt` CLI ships in **`agentteams-controller`**, **`agentteams-manager`**, and Worker images (same binary, talks to the controller REST API). **`install/agentteams-apply.sh`** runs `agt apply` **inside `agentteams-manager`** because it copies YAML into that container. For ad-hoc operator commands, `docker exec agentteams-controller agt …` is often convenient.

**Enter the controller container (one option):**

```bash
docker exec -it agentteams-controller sh
```

### Query resources

```bash
# Cluster overview
agt status

# List all workers (table format)
agt get workers

# List workers as JSON (useful for scripting)
agt get workers -o json

# Get details of a specific worker
agt get workers alice
agt get workers alice -o json

# List workers in a specific team
agt get workers --team dev-team

# List all teams
agt get teams

# List all humans
agt get humans

# List managers
agt get managers

# Check controller version
agt version
```

### Create resources

```bash
# Create a worker with default model and runtime
agt create worker --name alice

# Create a worker with specific model and runtime
agt create worker --name bob --model claude-sonnet-4-6 --runtime hermes

# Create a worker with skills
agt create worker --name charlie --skills github-operations

# Create a worker with a custom SOUL.md
agt create worker --name diana --soul-file /path/to/SOUL.md

# Create a worker without waiting for it to be ready
agt create worker --name eve --no-wait

# Create a team
agt create team --name dev-team --goal "Full-stack web development"

# Create a human
agt create human --name john --level 1

# Create a manager
agt create manager --name default --model qwen3.5-plus
```

### Update resources

```bash
# Switch a worker's model
agt update worker --name alice --model claude-sonnet-4-6

# Switch a worker's runtime (triggers container recreation)
agt update worker --name alice --runtime hermes

# Update a worker's skills
agt update worker --name alice --skills github-operations,code-review
```

### Apply YAML definitions

```bash
# Apply a single YAML resource
agt apply -f worker-alice.yaml
```

Use YAML for fields not exposed by direct CLI flags, such as `spec.mcpServers`:

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  workerName: alice
  skills:
    - github-operations
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
      transport: http
```

```bash
# Import a worker from a zip package
agt apply worker --name alice --zip worker-package.zip
```

### Worker lifecycle

```bash
# Stop (sleep) a worker
agt worker sleep --name alice

# Wake a sleeping worker
agt worker wake --name alice

# Check a worker's status
agt worker status --name alice
```

### Delete resources

```bash
# Delete a worker (stops container, cleans up Matrix account and gateway consumer)
agt delete worker alice

# Delete a team
agt delete team dev-team

# Delete a human
agt delete human john
```

> **Tip:** Most Manager Agent operations (creating workers, switching models, assigning tasks) ultimately call the same `agt` CLI under the hood. Using the CLI directly is useful for debugging, bulk operations, or automation scripts.

For declarative YAML resource definitions, see [Declarative Resource Management](declarative-resource-management.md).

---

## How to configure GitHub credentials for Workers

GitHub credentials are configured as an MCP Server credential, not copied into
Worker containers. Workers call GitHub through `mcporter` and the AI Gateway;
the real GitHub PAT stays in the gateway-side MCP configuration.

During installation, set or enter `AGENTTEAMS_GITHUB_TOKEN` when the installer asks
for the optional GitHub Personal Access Token:

```bash
AGENTTEAMS_GITHUB_TOKEN=ghp_xxx bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

When this variable is present, AgentTeams configures the GitHub MCP Server and
generates Manager-side `mcporter` configuration automatically. After that,
declare the GitHub MCP capability in the Worker manifest:

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  workerName: alice
  skills:
    - github-operations
  mcpServers:
    - name: github
      url: https://gateway.example.com/mcp-servers/github/mcp
      transport: http
```

Apply it with the supported YAML path:

```bash
agt apply -f worker-alice.yaml
```

For an existing installation that skipped the token, re-run the installer from
the original workspace and provide `AGENTTEAMS_GITHUB_TOKEN`, or configure the
GitHub MCP Server in the gateway manually and then authorize the target
Manager/Worker consumer. Do not paste a PAT into a Worker prompt or
container-local config.

---

## Installation script exits immediately on Windows

If the PowerShell installation script closes immediately after launching, first check whether Docker Desktop is installed. If it is installed, make sure it is actually running — Docker Desktop must be started and fully loaded before the script can connect to the Docker daemon.

---

## Installation fails: "manifest unknown" for embedded image

If the installer fails with an error like:

```
ERROR: Failed to pull agentteams-embedded image.
Attempted: higress/agentteams-embedded:v1.1.0 and higress/agentteams-embedded:latest
```

This means the embedded image is not available in the registry for your requested version. Three options:

1. **Pin to a version that has the embedded image**: Check the [releases page](https://github.com/agentscope-ai/AgentTeams/releases) for available versions.
2. **Build locally from source**: Clone the repo and run `make install-embedded`.
3. **Override the image**: Set `AGENTTEAMS_INSTALL_EMBEDDED_IMAGE` to a custom image.

> If you intentionally want to use the legacy single-container architecture (v1.0.9 or earlier), set `AGENTTEAMS_FORCE_LEGACY=1`. Note this only works with images that bundle the infrastructure services.

---

## Manager Agent startup timeout or failure

If the Manager Agent is unresponsive after installation, check the logs.

**In the new architecture (v1.1.0+)**, the Manager runs as a separate container. Check logs in two places:

```bash
# Controller (infrastructure) logs
docker logs agentteams-controller

# Manager Agent logs
docker logs agentteams-manager
```

**Case 1: Controller is healthy but Manager container won't start**

The controller starts the Manager container automatically. If the Manager container is missing from `docker ps`, check the controller logs for provisioning errors.

**Case 2: Docker VM memory insufficient**

Increase Docker VM memory to at least 4 GB: Docker Desktop → Settings → Resources → Memory. Then re-run the install command.

**Case 3: Stale config data**

Re-run the install command and choose **delete and reinstall**:

```bash
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

When the installer detects an existing installation, it will ask how to proceed. Choosing delete will wipe the stale data and start fresh.

**Case 4: Mac with Apple Silicon and outdated Docker/Podman**

If you're using a Mac with Apple Silicon (M1/M2/M3/M4) and Docker Desktop is older than 4.39.0, Manager Agent may fail to start properly.

**Solutions:**

- **Docker Desktop**: Upgrade to 4.39.0 or later
- **Podman**: Ensure Podman Engine **Server version ≥ 5.7.1** (check with `podman version`)

**Case 5: Linux host with SELinux volume denial**

If the detailed log, especially `mc-mirror.log`, contains `permission denied`
for files under the mounted workspace or host-share directory on an SELinux
enabled Linux host, the bind mount may need an SELinux relabel option. Re-run
the installer from a workspace location where Docker/Podman is allowed to mount
files, or add `:z` to equivalent manual bind mounts so the container can access
the mounted path.

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

If the login page still reports a homeserver error:

1. Confirm the installer was run with external access enabled. Local-only mode
   binds services to `127.0.0.1`, so other devices cannot reach them.
2. Make sure the machine firewall allows ports `18080` (Matrix/Higress gateway)
   and `18088` (Element Web).
3. Do not use the default `matrix-local.agentteams.io` address from another device;
   that name resolves to the client machine's loopback address.

For FluffyChat or Element Mobile over Tailscale, use the same rule: set the
homeserver to `http://<tailscale-ip>:18080` and make sure the phone and the
AgentTeams host can reach each other in the Tailscale network.

---

## Element says the homeserver URL is not a valid Matrix server

When Element asks for a custom homeserver, do not enter the Element Web UI URL
or port. These two URLs serve different components:

- Element Web UI: `http://<host>:18088`
- Matrix/Higress gateway homeserver: `http://<host>:18080`

If you see "homeserver URL is not a valid Matrix server", replace `:18088` with
`:18080`, then retry login. For LAN or Tailscale access, use the reachable host
IP in the same format, for example `http://192.168.1.100:18080`.

---

## Cannot connect to Matrix server locally

If the Matrix server is unreachable even on the local machine, check whether a proxy is enabled in your browser or system. The `*-local.agentteams.io` domain resolves to `127.0.0.1` by default — if traffic is routed through a proxy, requests will never reach the local server.

Disable the proxy, or add `*-local.agentteams.io` / `127.0.0.1` to your proxy bypass list.

---

## How to talk to a Worker directly

After creating a Worker, Manager automatically adds you and the Worker to a shared group room. In that room, you must **@mention the Worker** for it to respond — messages without a mention are ignored.

When using Element or similar clients, type `@` followed by the first letter(s) of the Worker's display name to trigger autocomplete and select the right user.

Alternatively, you can click the Worker's avatar and open a **direct message** (DM) conversation. In a DM you don't need to @mention — every message triggers the Worker. Keep in mind that Manager is not in the DM room and won't see any of that conversation.

---

## How to connect third-party, local, or multi-provider models

AgentTeams does not read your `~/.openclaw/openclaw.json` provider definitions
directly. Model traffic goes through the AgentTeams AI Gateway. OpenClaw/QwenPaw
usually sees one provider named `hiclaw-gateway`; Higress then routes each
requested model name to the real upstream provider.

### Third-party OpenAI-compatible APIs

For an OpenAI-compatible service, create or update a Higress AI route with:

- the provider's base URL, including `/v1` when the provider requires it
- the provider API key
- a model matching rule that matches the model id you will ask Manager or a
  Worker to use

Then ask Manager to switch to that same model id, or create/update a Worker with
that model. Do not rely on `/model list` alone as the source of available
Higress providers; it shows the agent-side known model list, not every route
defined in Higress.

### Local models such as Ollama or LM Studio

Local models are supported when the service exposes an OpenAI-compatible API
that the AgentTeams containers can reach. From inside Docker, `localhost` means the
container itself, not your Mac or host machine. Use a reachable host address,
for example `http://host.docker.internal:<port>/v1` on Docker Desktop, or the
host LAN IP on Linux/Podman when `host.docker.internal` is not available.

### Multiple providers and task-specific models

Configure separate Higress AI routes with prefix or regex model matching rules,
for example one rule for `qwen*` and another for `claude*`. Then assign the
desired model explicitly to the Manager or a Worker. AgentTeams can use different
models for different Workers, but automatic model selection by task type is not
a built-in policy; express that policy through Worker roles or switch the model
explicitly.

---

## Why does my custom Higress AI route never match

AgentTeams creates a `default-ai-route` during setup. When that route has no
`modelPredicates`, it can match all model requests, so a later custom route may
look like it has lower priority.

For multiple AI routes, make the model matching rules unambiguous:

- Add `modelPredicates` to each custom route, such as a prefix match for
  `deepseek` or a regex for `^openrouter/.*$`.
- Also constrain `default-ai-route` to the models it should own, such as
  `qwen*`, instead of leaving it without `modelPredicates`.
- Use the same model id when switching Manager or Worker models; the route is
  selected from the requested model name, not from the provider display name.

---

## How to switch the Manager's model

AgentTeams supports two ways to switch models: **switch the current session model** (instant, non-persistent) and **switch the primary model** (persistent, requires restart).

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

OpenClaw requires setting the model's context window size (`contextWindow`) in its config. AgentTeams defaults to qwen3.5-plus's 200K token window. If you switch to a model with a different window without updating this setting, the session may fail when approaching the window limit — OpenClaw won't know when to compress context.

The model-switch skill:
1. Looks up the correct `contextWindow` and `maxTokens` for the target model
2. Updates OpenClaw's config accordingly
3. Tests connectivity before applying the change

If you see `model_context_window_exceeded`, first start a new session with
`/new` or switch to a model with a larger context window. Then verify that the
target model's `contextWindow` in the model configuration matches the provider's
real limit before continuing the long conversation.

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

## How to configure OpenRouter or another model provider with slashes in model names

In Higress AI route configuration, the **service name** is an internal name and
should not be the model name. It must not contain `/`. Put provider-specific
model prefixes such as `openrouter/` or `stepfun/` in the model matching rule
instead.

Example for OpenRouter:

| Field | Value |
|-------|-------|
| Service name | `openrouter` |
| Model matching rule | regex, for example `^openrouter/.*$` |
| Protocol | `openai` |
| Custom URL | `https://openrouter.ai/api/v1` |

After the route is configured, ask Manager to use the full model name, for
example `openrouter/stepfun-eur-1-70b`. The model name prefix is what lets
Higress select the matching provider route.

---

## How to switch a Worker's runtime

AgentTeams v1.1.0+ supports three Worker runtimes:

| Runtime | Language | Best For |
|---------|----------|----------|
| OpenClaw | Node.js | General-purpose, mature ecosystem |
| QwenPaw | Python | Python-native workflows, data science (legacy name **CoPaw**) |
| Hermes | Python | Autonomous coding, development tasks |

### At creation time

Specify the runtime when creating a Worker:

```
agt create worker --name alice --runtime hermes
```

Or via YAML:

```yaml
apiVersion: agentteams.io/v1beta1
kind: Worker
metadata:
  name: alice
spec:
  runtime: hermes
  model: qwen3.5-plus
```

If no runtime is specified, the default set during installation (`AGENTTEAMS_DEFAULT_WORKER_RUNTIME`) is used, falling back to `openclaw`.

### After creation

Tell Manager to switch a Worker's runtime:
> "Switch alice's runtime to hermes"

Manager will use the worker-management skill to trigger a container recreation. The Worker's Matrix account, room, gateway consumer, MinIO data, and persisted credentials are preserved. Container-local ephemeral state (caches, in-flight task progress) will be lost.

---

## Why does QwenPaw still use `copaw` in runtime values or image names

`QwenPaw` is the user-facing name of the Python runtime that was previously
called `CoPaw`. Some internal compatibility names intentionally remain `copaw`,
including the Worker CRD runtime value, image names such as
`agentteams-copaw-worker`, and environment values such as
`AGENTTEAMS_MANAGER_RUNTIME=copaw`.

Do not change these internal values to `qwenpaw` unless the chart, controller,
and images explicitly support that new value. They are kept stable to avoid
breaking existing installations, Helm values, and image pull paths.

---

## Can I connect my own agent implementation as a Worker

Not by adding an arbitrary new `spec.runtime` value. The Worker CRD currently
accepts only `openclaw`, `copaw`, or `hermes` as runtimes.

For most custom Worker needs, package your role prompt, skills, dependencies,
and optional Dockerfile as a Worker package, or set a custom image while keeping
one of the supported runtimes. See [Importing Existing Workers](import-worker.md)
and the `spec.package` / `spec.image` fields in
[Declarative Resource Management](declarative-resource-management.md#worker-resource).

Adding a completely new runtime requires code changes in the controller,
runtime image defaults, and the corresponding agent template wiring. It is not
a configuration-only operation.

---

## Can AgentTeams connect to an existing Higress instance

Not with `gateway.provider=higress` today. The Helm chart validates that
`gateway.provider=higress` uses `gateway.mode=managed`, which means AgentTeams
deploys and owns the Higress instance it uses.

Do not copy an existing Higress configuration directory into the AgentTeams-managed
Higress instance. AgentTeams reconciles the AI routes, consumers, and gateway
resources it needs, so copied resources can conflict with or be overwritten by
AgentTeams-managed state.

The supported paths are:

- use the Higress instance managed by AgentTeams for AgentTeams traffic
- use the external `ai-gateway` provider path where applicable

Connecting to an existing self-managed Higress instance would require a separate
external-Higress design, including gateway/console URLs, credentials, resource
naming isolation, and safeguards around existing routes and consumers.

---

## How to use the Worker Template Marketplace

AgentTeams v1.1.0+ includes a Worker Template Marketplace backed by Nacos. Instead of configuring Workers from scratch, you can import pre-built templates:

**Via Manager conversation:**

Tell Manager what kind of Worker you need:
> "I need a Worker for frontend development with React expertise"

Manager will search the marketplace, recommend matching templates, and import after your confirmation.

**Via CLI:**

```bash
agt apply -f my-worker.yaml
```

With a `package` reference in the YAML pointing to a marketplace template.

---

## Does AgentTeams support sending and receiving files

**Receiving files from you**: Yes. You can upload a file directly in Element Web (the attachment button), and Manager or Worker will receive it as a Matrix media message and can read its content.

**Sending files to you**: Yes. When you ask Manager (or a Worker) to send you a file — such as a task output artifact, a generated report, or any file it has access to — it will upload the file to the Matrix media server and send it to the room as a downloadable attachment. You can then click to download it in Element Web.

Paths printed by Manager or Worker are usually container-internal paths. If you
cannot access a path directly from the host, ask the agent to send the file as
an attachment or provide a downloadable link instead of relying on the raw
container path.

---

## Why does Manager/Worker keep showing "typing"

This is normal — it means the underlying Agent engine is actively executing. AgentTeams sets a 30-minute timeout per task, so an agent can stay in this state for up to 30 minutes while working.

To see what the agent is actually doing, exec into the Manager or Worker container and check the session logs:

```bash
# For Manager
docker exec -it agentteams-manager ls .openclaw/agents/main/sessions/

# For a Worker (replace <worker-name> with the actual container name)
docker exec -it <worker-name> ls .openclaw/agents/main/sessions/
```

The `.jsonl` files in that directory are written in real time and contain the full agent execution trace — LLM calls, tool use, reasoning steps, etc.

> **Note**: For Hermes-runtime Workers, session data is stored at `~/.hermes/state.db` instead.

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

The session might be corrupted. Enter the Manager or Worker container and use the OpenClaw TUI to investigate:

```bash
# Manager
docker exec -it agentteams-manager openclaw tui

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

### 1. Check container status

In the new architecture, verify both the controller and Manager containers are running:

```bash
docker ps | grep -E "agentteams-controller|agentteams-manager"
```

If `agentteams-manager` is not running, check the controller logs:

```bash
docker logs agentteams-controller
```

### 2. Check session status

The session might be corrupted. Enter the Manager container and use the OpenClaw TUI to investigate:

```bash
docker exec -it agentteams-manager openclaw tui
```

In the TUI:
1. Type `/sessions` to list all sessions
2. Switch to the session for the relevant chat
3. Try sending a message and observe if there are any errors

If the session is corrupted, try sending `/new` as a standalone message in the corresponding chat in Element (or other Matrix client) to reset the session and see if that restores normal behavior.

### 3. Check Higress AI Gateway log

If resetting the session doesn't help, check the Higress AI Gateway log. In the new architecture, Higress runs inside the controller container:

```bash
docker exec -it agentteams-controller cat /var/log/agentteams/higress-gateway.log
```

Search the log for the relevant status code. Common causes:

- **503**: The container can't reach the external LLM service — likely a network issue inside the container.
- **404**: The model name is probably wrong.

To determine whether the error came from the backend or from a Higress
misconfiguration, check the `upstream_host` field in the log entry. If
`upstream_host` has a real host value, the request reached the backend and the
error was returned by the upstream service. If it is `-` or empty, Higress did
not select an upstream cluster; a log entry with `response_code_details:
cluster_not_found` usually means the model route or service source is
misconfigured.

For self-hosted OpenAI-compatible services, check whether the Higress provider
configuration points to a real URL instead of a non-existent service name. Also
verify from inside the container that the upstream URL is reachable with the
same base URL and API key.

### 4. Check model configuration

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

## How to view Manager Agent logs

In the new architecture (v1.1.0+), the Manager runs as a separate container:

```bash
# Manager Agent logs (stdout/stderr)
docker logs agentteams-manager

# Manager Agent session logs (detailed execution trace)
docker exec -it agentteams-manager ls .openclaw/agents/main/sessions/

# Controller / infrastructure logs
docker logs agentteams-controller

# Higress Gateway log (inside the controller container)
docker exec -it agentteams-controller cat /var/log/agentteams/higress-gateway.log

# Higress Console API / UI backend log (v1.1.0+ embedded — also on the controller)
docker exec -it agentteams-controller cat /var/log/agentteams/higress-console.log
```

For OpenClaw Control UI (visual session inspection), open:

```
http://localhost:18888
```

---

## How to connect Feishu/DingTalk/WeCom/Discord/Telegram

AgentTeams Manager is built on OpenClaw, which supports multiple messaging channels out of the box. To connect additional channels:

**Method 1: Edit config directly**

The Manager's working directory is `~/agentteams-manager` (on your host). Edit `openclaw.json` in that directory to add channel configuration. Refer to [OpenClaw channel documentation](https://docs.openclaw.ai) for the specific config format for each platform.

After editing, restart the Manager container for changes to take effect:

```bash
docker restart agentteams-manager
```

**Method 2: Let Manager learn from your existing OpenClaw config**

If you already use OpenClaw with other channels (e.g., in your personal setup), you can let Manager read your existing config:

- **Tell Manager where the file is**: In Element Web, tell Manager the path to your OpenClaw config file (e.g., "My OpenClaw config is at `/home/user/my-openclaw.json`"). Manager will read it directly.
- **Send the file as attachment**: In Element Web or any Matrix client, upload your config file as an attachment and send it to Manager. Manager will receive and read it.

Then ask Manager to help configure the same channels in its own config.

---

## Session management via IM

AgentTeams uses OpenClaw with the Matrix channel (Element Web). OpenClaw supports **slash commands** that you can send directly in the chat as standalone messages. These commands are processed by the Gateway before the model sees them.

**Important:** Most commands must be sent as a **standalone** message that starts with `/`. Do not mix them with other text in the same message.

**In group rooms:** You can combine an @mention with a slash command in the same message, e.g. `@Worker /compact` or `@Worker /new`. The @mention ensures the command reaches the right agent, and the slash command is still processed by the Gateway as usual.

The following commands apply to OpenClaw (Manager and OpenClaw Workers). **QwenPaw** Workers use a different command set — see [QwenPaw Commands](https://copaw.agentscope.io/docs/commands) for details.

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
