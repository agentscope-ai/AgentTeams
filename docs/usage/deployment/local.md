# AgentTeams Local Deployment Guide

English | [中文](../../zh-cn/usage/deployment/local.md)

This guide explains how to create and maintain an AgentTeams instance on a single macOS or Linux host. It covers Quick Start, Manual Setup, non-interactive installation, installation options, verification, upgrades, and uninstalling.

If you only want to complete your first task as quickly as possible, start with the [Quickstart](../../quickstart.md). Windows users should follow the separate [Windows Deployment Guide](windows.md).

## What local deployment creates

The current local installer uses the embedded architecture by default:

| Name | Object type | When it is created and what it does |
|---|---|---|
| `agentteams-controller` | Container | Created during installation; runs Higress, Tuwunel, MinIO, Element Web, and the controller in one container. |
| `agentteams-manager` | Container | Created by the controller during installation; runs the Manager Agent. |
| `agentteams-worker-<name>` | Container | Started on demand after a Worker is created; each Worker uses a separate container. |
| `agentteams-dashboard` | Container | Optional management UI enabled by default by the Bash installer. |
| `agentteams-net` | Container network | Created during installation; connects the controller, Manager, Workers, and Dashboard. |
| `agentteams-data` | Volume | Created during installation; persists Matrix, MinIO, Higress, and controller data. |
| `agentteams-dashboard-data` | Volume | Created when the Dashboard is enabled; stores Dashboard data. |

`docker ps` shows only containers, so it does not list `agentteams-net` or `agentteams-data`. Use separate commands to inspect containers, networks, and volumes:

```bash
docker ps --filter name=agentteams
docker network ls | grep agentteams-net
docker volume ls | grep agentteams
```

The Manager and Workers do not keep persistent state only inside their containers. Agent configuration, Matrix data, and shared files are stored by services in the embedded controller and its mounted data volume. Removing or recreating containers does not remove volumes automatically; running the AgentTeams uninstall flow does remove this persistent data.

## Prerequisites

### Supported platforms

- macOS: Docker Desktop is recommended. A compatible Podman environment can also be used.
- Linux: Docker Engine is recommended. The installer also detects Podman.
- Windows: use the PowerShell installer described in the [Windows Deployment Guide](windows.md).

The container runtime must already be running, and the current user must be able to run `docker ps` or `podman ps`.

### Resource requirements

| Resource | Minimum | Recommended |
|---|---:|---:|
| CPU | 2 cores | 4 or more cores |
| Memory | 4 GB | 8 GB or more |
| Disk | 10 GB available | Reserve more for images, task artifacts, and additional Workers |

Running multiple Workers, using the OpenClaw runtime, or executing build workloads requires additional memory and disk space.

### Network and credentials

Prepare:

- A working LLM API key.
- The corresponding Base URL and model ID. The installer fills in default endpoints for its built-in Alibaba Cloud options.
- An optional GitHub Personal Access Token for GitHub MCP scenarios.
- Network access to pull AgentTeams images and reach the model provider.

### Default ports

| Port | Purpose | Variable |
|---:|---|---|
| `18080` | Higress Gateway for Matrix, model, and file-service routes | `AGENTTEAMS_PORT_GATEWAY` |
| `18001` | Higress Console | `AGENTTEAMS_PORT_CONSOLE` |
| `18088` | Direct Element Web access | `AGENTTEAMS_PORT_ELEMENT_WEB` |
| `18888` | Manager Console, when exposed by the selected runtime and architecture | `AGENTTEAMS_PORT_MANAGER_CONSOLE` |
| `13000` | AgentTeams Dashboard | `AGENTTEAMS_PORT_DASHBOARD` |

Check for port conflicts before installing:

```bash
lsof -nP -iTCP:18080 -sTCP:LISTEN
lsof -nP -iTCP:18001 -sTCP:LISTEN
lsof -nP -iTCP:18088 -sTCP:LISTEN
lsof -nP -iTCP:13000 -sTCP:LISTEN
```

On Linux systems without `lsof`, use `ss -lnt`.

## Choose an installation method

| Method | Best for | Behavior |
|---|---|---|
| **Quick Start** | First evaluation | Prompts for version, model, API key, and runtimes; uses recommended defaults for everything else. |
| **Manual Setup** | Custom network, storage, Dashboard, or security settings | Presents the complete sequence of interactive options. |
| **Non-interactive installation** | Automation, CI, or repeatable deployments | Reads configuration from environment variables and does not wait for terminal input. |
| **Upgrade an existing instance** | Existing configuration and containers | Keeps all settings or lets you confirm each setting. |

## Quick Start

Run:

```bash
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

After choosing Quick Start, the installer still asks you to:

1. Install `latest`, the latest stable release, or a specific version.
2. Select a model service and model, and enter the LLM API key.
3. Select the Manager runtime: QwenPaw (recommended), OpenClaw, or legacy CoPaw.
4. Select the default Worker runtime: QwenPaw (recommended), OpenClaw, Hermes in supported versions, or legacy CoPaw.

The remaining settings use these defaults:

| Setting | Quick Start behavior |
|---|---|
| Administrator | Username `admin`; password generated automatically |
| Network | Bind for local access only |
| Ports | Use the values in the default port table |
| Domains | Use the default `*-local.agentteams.io` domains |
| GitHub token | Not configured |
| Skills registry | Use the system default |
| Data volume | `agentteams-data` |
| Manager workspace | `${HOME}/agentteams-manager` |
| Dashboard | Enabled on port `13000` |
| Matrix E2EE | Disabled |
| Worker idle sleep | 720 minutes |
| Host shared directory | The current user's home directory |

When installation finishes, the terminal prints the access URLs, administrator credentials, and data locations. Save this information.

## Manual Setup flow

Manual Setup runs the following steps in order. Enter `b` to return to the previous completed step.

### 1. Language and installation mode

The installer infers Chinese or English from the timezone, and you can switch languages in the first step. Language affects the prompts and some model defaults in non-interactive mode.

### 2. Installation version

Choose one of:

- `latest`: follow the latest image tag.
- Latest stable release: the installer reads the current GitHub Release and falls back to its built-in stable version if that lookup fails.
- Custom version: enter a specific tag such as `vX.Y.Z`.

Pin a version for production and repeatable environments instead of relying on `latest`.

### 3. Existing instance handling

If the installer finds `${HOME}/agentteams-manager.env`, it offers:

- **Upgrade**: preserve data and either keep all settings or confirm each setting.
- **Clean reinstall**: stop the instance and delete the data volume, configuration file, Manager workspace, Workers, and network.
- **Cancel**: exit without changes.

A clean reinstall requires entering the full existing workspace path as a second confirmation.

### 4. Model service

The English installer provides Alibaba Cloud model service and OpenAI-compatible API entry points. Depending on the selected path, it collects or determines:

- `AGENTTEAMS_LLM_PROVIDER`: selects the service adapter used by AgentTeams. It is not an arbitrary vendor label.
  - `qwen`: uses the built-in Alibaba Cloud Model Studio/Qwen integration.
  - `openai-compat`: uses the OpenAI-compatible protocol for services such as OpenAI, Alibaba Cloud Token Plan, DeepSeek, and self-hosted model gateways.
- `AGENTTEAMS_OPENAI_BASE_URL`: the base endpoint for an OpenAI-compatible API. Enter the service root, not a complete `/chat/completions` request path. It commonly ends in `/v1`. It is normally empty when using the built-in `qwen` integration.
- `AGENTTEAMS_LLM_API_KEY`: the required secret for model access. The installer uses it for connectivity checks and writes it to the local configuration for gateway use.
- `AGENTTEAMS_DEFAULT_MODEL`: the model ID used by the Manager and new Workers by default. It must exactly match a model name accepted by the provider API. A specific Manager or Worker can override it later.
- `AGENTTEAMS_EMBEDDING_MODEL`: the model ID used for vectorization in features such as memory retrieval. The installer tries `text-embedding-v4` by default. An empty string disables embeddings. The model must be accessible with the current credentials and corresponding API endpoint.

In short: Provider selects the integration logic, Base URL selects the request destination, API Key supplies credentials, Default Model selects the generation model, and Embedding Model independently controls vectorization support.

Alibaba Cloud Model Studio general API example:

```bash
AGENTTEAMS_LLM_PROVIDER=qwen
AGENTTEAMS_OPENAI_BASE_URL=
AGENTTEAMS_LLM_API_KEY=sk-example
AGENTTEAMS_DEFAULT_MODEL=qwen3.6-plus
AGENTTEAMS_EMBEDDING_MODEL=text-embedding-v4
```

OpenAI-compatible service such as DeepSeek:

```bash
AGENTTEAMS_LLM_PROVIDER=openai-compat
AGENTTEAMS_OPENAI_BASE_URL=https://api.deepseek.com/v1
AGENTTEAMS_LLM_API_KEY=sk-example
AGENTTEAMS_DEFAULT_MODEL=deepseek-chat
AGENTTEAMS_EMBEDDING_MODEL=
```

The second example explicitly disables embeddings. If the compatible service provides an embedding API, replace the empty value with an embedding model ID actually supported by that service.

For an OpenAI-compatible service, the Base URL commonly includes `/v1`. The installer tests model connectivity. If the test fails, check the API key, Base URL, model ID, account balance, and network proxy.

For a custom model the installer does not recognize, it also asks for the following capability values and writes them into the runtime model configuration:

| Variable | Meaning | Example |
|---|---|---|
| `AGENTTEAMS_MODEL_CONTEXT_WINDOW` | Maximum model context-window size in tokens | `150000` |
| `AGENTTEAMS_MODEL_MAX_TOKENS` | Maximum output tokens allowed for one response | `128000` |
| `AGENTTEAMS_MODEL_REASONING` | Whether the model supports reasoning mode | `true` or `false` |
| `AGENTTEAMS_MODEL_VISION` | Whether the model accepts image input | `true` or `false` |

Set these values from the provider's real model specification. An excessive limit can cause upstream request rejection, while incorrectly declaring reasoning or vision support can produce incompatible requests.

The embedding model can use the default, a custom value, or be disabled. If its connectivity test fails, the installer disables embeddings automatically rather than blocking the main installation.

### 5. Manager runtime

| Choice | Installer value | Description |
|---|---|---|
| QwenPaw | `qwenpaw` | Recommended Python Manager runtime and the local installer's default choice. |
| OpenClaw | `openclaw` | Node.js Manager runtime. |
| CoPaw | `copaw` | Legacy Python runtime. Upgrade to QwenPaw is recommended. |

The Manager runtime selects the Manager image and execution mode. Changing the runtime of an existing Manager recreates its container and should be done only when no task is in progress.

### 6. Default Worker runtime

The local installer currently offers:

| Choice | Value | Description |
|---|---|---|
| QwenPaw | `qwenpaw` | Recommended default; current Python runtime. |
| OpenClaw | `openclaw` | Node.js runtime. |
| Hermes | `hermes` | Displayed starting with versions that support this runtime. |
| CoPaw | `copaw` | Legacy Python runtime. Upgrade to QwenPaw is recommended. |

This setting only selects the default for Workers created later. You can still explicitly select another runtime supported by the controller when creating a Worker.

### 7. Administrator account

- The username defaults to `admin` and is converted to lowercase.
- The password must contain at least 8 characters.
- Leaving it empty generates a password that is printed after installation.

The administrator credentials sign in to Matrix/Element and may also be used for shared Higress Console and Dashboard authentication. Do not send the password in public logs or chat rooms.

### 8. Access scope

| Choice | `AGENTTEAMS_LOCAL_ONLY` | Behavior |
|---|---:|---|
| Local access only | `1` | Binds services to local addresses for personal evaluation. |
| Allow external access | `0` | Binds relevant entry points to `0.0.0.0` for LAN or external access. |

Allowing external access does not configure public DNS, TLS, a reverse proxy, or firewall rules. Use HTTPS for multi-user or public access, and restrict access to Higress Console, the controller API, and storage administration endpoints.

### 9. Ports and domains

Manual Setup lets you change the default ports and configure:

| Purpose | Default domain |
|---|---|
| Matrix homeserver | `matrix-local.agentteams.io:<gateway-port>` |
| Element Web gateway | `matrix-client-local.agentteams.io` |
| AI Gateway | `aigw-local.agentteams.io` |
| File service | `fs-local.agentteams.io` |
| OpenClaw Console | `console-local.agentteams.io` |

Each host port must be unique. Custom domains must resolve correctly from clients and containers. External access also requires DNS, a reverse proxy, and certificates.

### 10. GitHub integration

`AGENTTEAMS_GITHUB_TOKEN` is optional and can be used for GitHub MCP scenarios. Recommended practices:

- Use a dedicated token instead of a long-lived, high-privilege personal token.
- Grant only the repositories and permissions required by the task.
- Rotate the token regularly.
- Do not commit the token or paste it into a Matrix room.

### 11. Skills registry

`AGENTTEAMS_SKILLS_API_URL` configures the Skills registry. Leaving it empty uses the system's default Nacos marketplace. A private registry can also use environment variables for a Nacos username, password, or token.

### 12. Data volume and workspace

| Setting | Default | Contents |
|---|---|---|
| `AGENTTEAMS_DATA_DIR` | `agentteams-data` | Persistent Matrix, MinIO, Higress, and controller data. The current implementation uses this value as a container volume name. |
| `AGENTTEAMS_WORKSPACE_DIR` | `${HOME}/agentteams-manager` | Manager configuration, Skills, memory, and local work files. |
| `AGENTTEAMS_HOST_SHARE_DIR` | `${HOME}` | Mounted into the Manager at `/host-share` for explicitly shared host files. |

Do not place the workspace in a temporary directory. Keep the host shared directory as narrow as possible so unnecessary credentials and personal files are not exposed to the agent runtime.

### 13. Dashboard

The Bash installer enables the Dashboard by default. Manual Setup can configure:

- Whether the Dashboard is enabled.
- Its independent version.
- Its host port, default `13000`.
- The Dashboard image.
- The Higress Console URL used for shared authentication.

The Dashboard supports only the current embedded controller architecture. The PowerShell installer does not currently install the Dashboard.

When the Dashboard is enabled, open **资源中心 (Resource Center) → 市场 (Marketplace)**. If the Skill is not yet in the marketplace, add it with **上传技能 (Upload Skill)**, then click **分发到 Worker (Distribute to Worker)** in the target Skill's row and select one or more Workers. You can also upload a Skill ZIP directly to one Worker through **Workers → target Worker → 详情 (Details) → 上传技能包 (Upload Skill Package)**. Both paths write the complete package to the Worker's persistent Skill directory, update `Worker.spec.skills`, and attempt to reload the Worker. See [Worker Guide: Distribute through the Dashboard](../worker-guide.md#method-2-distribute-through-the-dashboard) for package rules, reload impact, and verification.

### 14. Matrix E2EE

`AGENTTEAMS_MATRIX_E2EE` defaults to `0`, or disabled. Before enabling it, confirm that the selected Manager runtime, Worker runtimes, and Matrix clients support the current end-to-end encryption workflow. Quick Start and non-interactive mode keep it disabled by default.

### 15. Container socket and Docker Proxy

`AGENTTEAMS_MOUNT_SOCKET=1` allows the controller to access the container runtime by default so it can create and manage Workers directly.

Manual Setup can also enable the restricted Docker Proxy and add permitted image sources through `AGENTTEAMS_PROXY_ALLOWED_REGISTRIES`. When socket mounting is disabled, the controller cannot use the local container backend to create Workers automatically.

Direct access to a Docker or Podman socket grants powerful host-level permissions. Use it only on trusted hosts and restrict who can operate the Manager and controller.

### 16. Worker idle sleep

`AGENTTEAMS_WORKER_IDLE_TIMEOUT` controls how many idle minutes pass before a Worker sleeps. The default is `720`, or 12 hours. A shorter timeout saves resources but causes more frequent wakeups.

### 17. Podman autostart

When Podman is selected and systemd is available, the installer can configure AgentTeams to start automatically. This option does not appear in Docker environments.

## Non-interactive installation

Automation can provide environment variables and skip all prompts:

```bash
AGENTTEAMS_NON_INTERACTIVE=1 \
AGENTTEAMS_LANGUAGE=en \
AGENTTEAMS_LLM_PROVIDER=openai-compat \
AGENTTEAMS_OPENAI_BASE_URL=https://provider.example.com/v1 \
AGENTTEAMS_LLM_API_KEY=sk-example \
AGENTTEAMS_DEFAULT_MODEL=example-model \
AGENTTEAMS_ADMIN_PASSWORD=replace-with-a-strong-password \
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

Non-interactive mode fails immediately when a required value is missing or an explicitly provided administrator password has fewer than 8 characters. The credentials in the example are placeholders and must not be used as written.

### Common environment variables

| Variable | Default or behavior |
|---|---|
| `AGENTTEAMS_NON_INTERACTIVE` | `0`; set to `1` to skip prompts |
| `AGENTTEAMS_LANGUAGE` | Inferred as `zh` or `en` from the timezone |
| `AGENTTEAMS_VERSION` | Interactive installation defaults to `latest`; non-interactive installation defaults to the stable version built into the script |
| `AGENTTEAMS_REGISTRY` | Selected from the timezone |
| `AGENTTEAMS_LLM_PROVIDER` | Non-interactive default depends on the language |
| `AGENTTEAMS_OPENAI_BASE_URL` | OpenAI-compatible service endpoint |
| `AGENTTEAMS_LLM_API_KEY` | Required |
| `AGENTTEAMS_DEFAULT_MODEL` | Default model ID |
| `AGENTTEAMS_EMBEDDING_MODEL` | Default embedding model; an empty string disables it |
| `AGENTTEAMS_ADMIN_USER` | `admin` |
| `AGENTTEAMS_ADMIN_PASSWORD` | Generated when omitted; at least 8 characters when explicitly set |
| `AGENTTEAMS_LOCAL_ONLY` | `1` |
| `AGENTTEAMS_MANAGER_RUNTIME` | `qwenpaw` |
| `AGENTTEAMS_DEFAULT_WORKER_RUNTIME` | `qwenpaw` |
| `AGENTTEAMS_MATRIX_E2EE` | `0` |
| `AGENTTEAMS_MOUNT_SOCKET` | `1` |
| `AGENTTEAMS_DOCKER_PROXY` | `1`; enables the restricted container-runtime proxy |
| `AGENTTEAMS_DATA_DIR` | `agentteams-data` |
| `AGENTTEAMS_WORKSPACE_DIR` | `${HOME}/agentteams-manager` |
| `AGENTTEAMS_HOST_SHARE_DIR` | `${HOME}` |
| `AGENTTEAMS_WORKER_IDLE_TIMEOUT` | `720` minutes |
| `AGENTTEAMS_DASHBOARD` | `1` |
| `AGENTTEAMS_PORT_DASHBOARD` | `13000` |

The file header and installation steps in [`install/agentteams-install.sh`](../../../install/agentteams-install.sh) remain the source of truth for the full set of variables.

### Image overrides

Local builds, private registries, and prerelease validation can use:

- `AGENTTEAMS_INSTALL_EMBEDDED_IMAGE`
- `AGENTTEAMS_INSTALL_MANAGER_IMAGE`
- `AGENTTEAMS_INSTALL_MANAGER_QWENPAW_IMAGE`
- `AGENTTEAMS_INSTALL_MANAGER_COPAW_IMAGE`
- `AGENTTEAMS_INSTALL_WORKER_IMAGE`
- `AGENTTEAMS_INSTALL_COPAW_WORKER_IMAGE`
- `AGENTTEAMS_INSTALL_QWENPAW_WORKER_IMAGE`
- `AGENTTEAMS_INSTALL_HERMES_WORKER_IMAGE`
- `AGENTTEAMS_DASHBOARD_IMAGE`

Image versions must be compatible with the controller and resource contracts. Do not mix arbitrary controller, Manager, and Worker versions.

## Post-installation verification

### 1. Check containers

```bash
docker ps --filter name=agentteams-controller
docker ps --filter name=agentteams-manager
docker ps --filter name=agentteams-dashboard
```

The Dashboard is optional and does not appear when disabled.

### 2. Check resources

```bash
docker exec agentteams-controller agt get managers
docker exec agentteams-controller agt get workers
```

The Worker list can be empty immediately after installation. After creating a Worker, it should show `Running` or a provisioning state.

### 3. Check services

```bash
curl -fsS http://127.0.0.1:18088/ >/dev/null
curl -fsS http://127.0.0.1:18001/ >/dev/null
docker exec agentteams-controller curl -fsS http://127.0.0.1:9000/minio/health/live
```

Replace the default ports if you changed them.

### 4. Sign in

Default entry points:

| Service | URL |
|---|---|
| Element Web | `http://127.0.0.1:18088/#/login` |
| Higress Console | `http://127.0.0.1:18001` |
| Dashboard | `http://127.0.0.1:13000` |

Sign in to Element Web with the administrator credentials printed after installation, then follow the [Quickstart](../../quickstart.md) to create your first Worker.

## Configuration and data locations

| Content | Default location |
|---|---|
| Installation configuration | `${HOME}/agentteams-manager.env` |
| Installation log | `${HOME}/agentteams-install.log` |
| Manager workspace | `${HOME}/agentteams-manager` |
| Persistent data | Docker/Podman volume `agentteams-data` |
| Dashboard data | Volume `agentteams-dashboard-data` |

The installation configuration contains sensitive values. Restrict its file permissions, do not commit it to Git, and do not attach it directly to an Issue.

## Upgrade

Run the installer again to detect the existing configuration and enter the upgrade flow:

```bash
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

Upgrade mode offers:

- **Keep all settings**: reuse the current environment file.
- **Confirm each setting**: show the current value at each step and allow changes.

Pin the target version with:

```bash
AGENTTEAMS_VERSION=vX.Y.Z \
bash <(curl -sSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

An upgrade restarts the Manager and may recreate Worker containers. Persistent data is preserved, but finish active tasks and back up important data first.

## Uninstall

> Warning: uninstalling removes AgentTeams containers, Workers, networks, data volumes, Dashboard data, the environment file, the installation log, and the Manager workspace.

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh) uninstall
```

If you only want to stop services while preserving data, stop the relevant containers with the container runtime instead of running the uninstall command.

## Troubleshooting

### Containers do not start

```bash
docker ps -a --filter name=agentteams
docker logs --tail 200 agentteams-controller
docker logs --tail 200 agentteams-manager
```

### Model validation fails

Confirm that:

- The API key has no extra whitespace and is still valid.
- The Base URL meets the provider requirements and commonly includes `/v1`.
- The model ID exists and the account can access it.
- The host and containers can reach the model service.
- The account balance, quota, and concurrency limits are sufficient.

### Port conflict

Run the installer again, choose Manual Setup, and change the corresponding `AGENTTEAMS_PORT_*` value. For an existing instance, choose the option to confirm each setting.

### Manager or Worker remains in provisioning

```bash
docker exec agentteams-controller agt get managers -o json
docker exec agentteams-controller agt get workers -o json
docker logs --tail 300 agentteams-controller
```

See the [FAQ](../troubleshooting/faq.md) for additional issues.
