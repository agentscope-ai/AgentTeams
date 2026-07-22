# Development Guide

Guide for contributing to HiClaw, building images locally, and running tests.

## Prerequisites

- Docker (for building and testing)
- Git
- `mc` (MinIO Client) for running integration tests
- `jq` for JSON processing in test scripts

## Project Structure

See [../AGENTS.md](../AGENTS.md) for a comprehensive codebase navigation guide.

## Building Images Locally

All builds go through the root `Makefile`:

```bash
# Build both Manager and Worker images (native arch, for local dev/test)
make build

# Build only Manager
make build-manager

# Build only Worker
make build-worker

# Controller + embedded all-in-one (infra + controller binary, no Manager agent)
make build-hiclaw-controller
make build-embedded

# Alternate Manager / Worker runtimes
make build-manager-copaw
make build-copaw-worker
make build-hermes-worker

# Build with a specific version tag
make build VERSION=0.1.0

# Build for a specific platform
make build DOCKER_PLATFORM=linux/amd64
```

See all available targets with `make help`.

### Helm chart (`helm/hiclaw`)

The production Kubernetes install is defined under **`helm/hiclaw/`** (subcharts for gateway, homeserver, storage, etc.). Useful Makefile targets:

```bash
make helm-lint      # helm dependency build + helm lint
make helm-template  # render templates locally (validation)
make sync-crds      # copy CRD YAML from hiclaw-controller/config/crd/ into the chart
```

### Push Images (Multi-Architecture by Default)

`make push` always builds multi-arch manifests (amd64 + arm64) to avoid accidentally overwriting multi-arch images with a single-arch image. This uses `docker buildx`:

```bash
# Build amd64 + arm64 and push to registry
make push VERSION=0.1.0 REGISTRY=ghcr.io REPO=higress-group/hiclaw

# Push only Manager (multi-arch)
make push-manager VERSION=latest

# Customize platforms (default: linux/amd64,linux/arm64)
make push MULTIARCH_PLATFORMS=linux/amd64,linux/arm64,linux/arm/v7
```

> **Note**: `make push` always pushes directly to the registry (required by buildx multi-platform builds — multi-arch images cannot be stored in the local Docker image store). You must `docker login` to the target registry first.
>
> For local development and testing, use `make build` which creates native-arch images locally. If you absolutely need to push single-arch images (not recommended), use `make push-native`.

### Regional Registry Mirrors

All base images are hosted on the Higress Alibaba Cloud registry with regional mirrors that auto-sync from the primary (`cn-hangzhou`). Choose the nearest region for faster pulls:

| Region | Registry | Usage |
|--------|----------|-------|
| China (default) | `higress-registry.cn-hangzhou.cr.aliyuncs.com` | Default, no extra args needed |
| North America | `higress-registry.us-west-1.cr.aliyuncs.com` | `make build HIGRESS_REGISTRY=higress-registry.us-west-1.cr.aliyuncs.com` |
| Southeast Asia | `higress-registry.ap-southeast-7.cr.aliyuncs.com` | `make build HIGRESS_REGISTRY=higress-registry.ap-southeast-7.cr.aliyuncs.com` |

Or pass it directly to Docker:

```bash
docker build --build-arg HIGRESS_REGISTRY=higress-registry.us-west-1.cr.aliyuncs.com -t hiclaw/manager-agent:latest ./manager/
```

## Install / Uninstall / Replay

### Quick Install (Minimal)

Only `HICLAW_LLM_API_KEY` is required — everything else uses sensible defaults:

```bash
# Build images + install Manager with one command
HICLAW_LLM_API_KEY="sk-xxx" make install
```

This will:
1. Build Manager and Worker images (`make build`)
2. Run the install script (`install/hiclaw-install.sh manager`)
3. Mount the container runtime socket for direct Worker creation
4. Save configuration to `./hiclaw-manager.env`

### Custom Install

Override any configuration via environment variables:

```bash
HICLAW_LLM_API_KEY="sk-xxx" \
HICLAW_LLM_PROVIDER="openai" \
HICLAW_DEFAULT_MODEL="gpt-4o" \
HICLAW_ADMIN_USER="myadmin" \
HICLAW_ADMIN_PASSWORD="mypassword" \
make install
```

### Uninstall

```bash
make uninstall   # Runs install/hiclaw-install.sh uninstall (v1.1+: controller + manager + workers + volume + env)
```

### Replay (Send Task to Manager)

After installing, send tasks to the Manager via the Matrix protocol:

```bash
# CLI mode: pass task as argument
make replay TASK="Create a Worker named alice for frontend development"

# Interactive mode: prompts for input
make replay

# Pipe mode: read from stdin
echo "Create worker bob" | ./scripts/replay-task.sh
```

The replay script:
- Reads credentials from `./hiclaw-manager.env`
- Logs into Matrix as the admin user
- Finds (or auto-creates) the DM room with the Manager
- Sends the task message
- Waits for and prints the Manager's reply (configurable via `REPLAY_WAIT=0` to skip)
- Saves conversation logs to `logs/replay/replay-{timestamp}.log` (view with `make replay-log`)

### Test Against Installed Manager

After `make install`, run the test suite against the running Manager without rebuilding or creating a new container:

```bash
make test-installed

# Or with a test filter
make test-installed TEST_FILTER="01 02"
```

This reads credentials from `./hiclaw-manager.env` and skips the container lifecycle (start/stop).

## Running Tests

### Full Integration Test Suite

```bash
# Build images + run all 10 test cases
export HICLAW_LLM_API_KEY="your-api-key"
make test
```

### Run Specific Tests

```bash
# Run only tests 01, 02, 03
make test TEST_FILTER="01 02 03"
```

### Skip Image Build

```bash
# Use existing images (faster iteration)
make test SKIP_BUILD=1
```

### Quick Smoke Test

```bash
# Run test-01 only (quick health check)
make test-quick
```

### GitHub Tests (08-10)

Tests 08-10 require a GitHub Personal Access Token:

```bash
export HICLAW_GITHUB_TOKEN="ghp_..."
make test TEST_FILTER="08 09 10"
```

## Making Changes

### Modifying Agent Behavior

Agent behavior is defined by markdown files, not code:
- **Manager SOUL**: `manager/agent/SOUL.md`
- **Manager Heartbeat**: `manager/agent/HEARTBEAT.md`
- **Skills**: `manager/agent/skills/*/SKILL.md`

### Modifying Startup Sequence

Each component has its own startup script in `manager/scripts/init/`:
- Modify the relevant `start-*.sh` script
- Rebuild the Manager image
- Run tests to verify

### Modifying Higress Configuration

Route, consumer, and MCP server bootstrap for **embedded** stacks is owned by the **controller** image (`hiclaw-controller` / `Dockerfile.embedded` wiring). The legacy **`manager/scripts/init/setup-higress.sh`** path still applies to **older single-container** Manager images (≤v1.0.9) and to Manager-side behaviors where applicable — check your install mode before editing.

### Adding a New MCP Server

1. Add the server configuration to `setup-higress.sh`
2. Create a Worker skill SKILL.md documenting available tools
3. Update `manager/agent/worker-agent/skills/` with the new skill
4. Add test coverage in `tests/`

## CI/CD

### GitHub Actions Workflows

| Workflow | Trigger | Purpose | Arch |
|----------|---------|---------|------|
| `build.yml` | PRs to main | Build only (no push, fast feedback) | amd64 |
| `build.yml` | Push to main | Multi-arch build + push | amd64 + arm64 |
| `integration-test.yml` | After build succeeds on main | Run full test suite | amd64 (runner native) |
| `release.yml` | Version tags `v*` | Multi-arch build + push release images | amd64 + arm64 |

All CI multi-arch builds use `docker/setup-qemu-action` for cross-platform emulation and `docker buildx` via `make push`.

### Required Secrets

| Secret | Purpose |
|--------|---------|
| `HICLAW_LLM_API_KEY` | LLM access for Agent behavior tests |
| `HICLAW_GITHUB_TOKEN` | GitHub operations in tests 08-10 |

### Local CI Simulation

```bash
# Same flow as CI but local (single arch)
export HICLAW_LLM_API_KEY="your-key"
make test

# Multi-arch build like CI does on main branch
docker login ghcr.io
make push VERSION=latest REGISTRY=ghcr.io REPO=higress-group/hiclaw
```

## Network Proxy Setup (China Mainland)

Building images requires accessing GitHub (for `git clone`) and npm registries. In China mainland environments, a proxy is typically needed.

### Host Proxy

Enable proxy in your shell before running commands:

```bash
# Enable proxy (adjust ports to your proxy config)
export http_proxy="http://127.0.0.1:1087"
export https_proxy="http://127.0.0.1:1087"
export ALL_PROXY="socks5://127.0.0.1:1086"

# IMPORTANT: exclude localhost from proxy, otherwise test health checks fail
export no_proxy="localhost,127.0.0.1,::1,local,169.254/16"
```

### Docker Build Proxy

Docker build runs in an isolated environment and does NOT inherit host proxy settings. Pass proxy via `DOCKER_BUILD_ARGS`:

```bash
make build-manager DOCKER_BUILD_ARGS="--build-arg http_proxy=http://host.docker.internal:1087 --build-arg https_proxy=http://host.docker.internal:1087"
```

> **Note**: `host.docker.internal` resolves to the host machine from within Docker containers. If your proxy listens on `127.0.0.1:1087` on the host, use `host.docker.internal:1087` in build args.

### Running Tests with Proxy

When running tests, `no_proxy` is critical — without it, test health checks to `127.0.0.1` go through the proxy and return 503:

```bash
export no_proxy="localhost,127.0.0.1,::1,local,169.254/16"
HICLAW_LLM_API_KEY="your-key" make test SKIP_BUILD=1
```

## Container Runtime Socket (Direct Worker Creation)

When the host runtime socket is available, the **embedded controller** (`agentteams-controller`) mounts it and creates Worker containers via the Docker Engine API — no human intervention needed for local deployments. Legacy all-in-one installs (≤v1.0.9 only) mount the socket on the Manager container instead.

See **Security: solo-operator Docker socket** below for the accepted-risk model and escalation path before shared-host use.

### How It Works

The controller's `DockerBackend` uses `AGENTTEAMS_PROXY_SOCKET` (default `/var/run/docker.sock`). Embedded install scripts mount the socket into `agentteams-controller` only; the spawned Manager agent does not receive `docker.sock`. Legacy `container-api.sh` in the Manager image applied when the Manager held the socket directly (pre-v1.1).

### Socket Paths

| Runtime | Socket path (host) | Mount command |
|---------|-------------------|---------------|
| Docker | `/var/run/docker.sock` | `-v /var/run/docker.sock:/var/run/docker.sock` |
| Podman (rootful, Linux) | `/run/podman/podman.sock` | `-v /run/podman/podman.sock:/var/run/docker.sock --security-opt label=disable` |
| Podman (macOS machine) | Inside VM: `/run/podman/podman.sock` | Same as rootful (VM provides symlink at `/var/run/docker.sock`) |

### Example: Manual Start with Socket

```bash
# Docker
docker run -d --name hiclaw-manager \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e HICLAW_WORKER_IMAGE=hiclaw/worker-agent:latest \
  ... \
  hiclaw/manager-agent:latest

# Podman
podman run -d --name hiclaw-manager \
  -v /run/podman/podman.sock:/var/run/docker.sock \
  --security-opt label=disable \
  -e HICLAW_WORKER_IMAGE=hiclaw/worker-agent:latest \
  ... \
  hiclaw/manager-agent:latest
```

### Test Integration

The test orchestrator (`tests/run-all-tests.sh`) automatically detects the socket and mounts it when available.

### Security: solo-operator Docker socket (accepted risk)

Embedded installs (v1.1+) mount the host runtime socket into **`agentteams-controller` only** — not the Manager agent container. The controller's `DockerBackend` dials `AGENTTEAMS_PROXY_SOCKET` (default `/var/run/docker.sock`) directly to create Worker containers. HTTP filtering for legacy Manager-side `/docker/` calls lives in `hiclaw-controller/internal/proxy/`, but **`DockerBackend` bypasses that validator** — the controller process has full Docker Engine API access when the socket is mounted.

This is an **accepted solo-operator risk**: a single trusted admin runs AgentTeams on their own machine or VPS, and only the controller (not every agent) holds socket access. That is a meaningful improvement over v1.0.x, where the all-in-one Manager container (or a separate `hiclaw-docker-proxy` sidecar) also sat on the socket path.

**Before shared-host or multi-tenant use**, escalate containment — for example point `AGENTTEAMS_PROXY_SOCKET` at a filtered sidecar (tecnativa/docker-socket-proxy or equivalent) instead of the raw socket, and treat Manager/Worker compromise as host-compromise until then.

The standalone `docker-proxy/` tree was removed in `d029ed2`; do not restore it. Uninstall scripts still stop/remove leftover `hiclaw-docker-proxy` containers from v1.0.x upgrades.

### Non-root Manager/Worker images (deferred hardening)

Default installs run Manager and Worker containers as **root**. Forcing non-root (e.g. `USER 1000`) without a broader refactor will crash CoPaw Manager startup (`start-manager-agent.sh` writes `/data`, `/root/manager-workspace`, `/etc/hosts`, and install-time volume mounts assume root-owned paths). A naive `USER` line in `copaw/Dockerfile` targets the **Worker** image and does not cover `/data` or Manager entrypoints.

Full non-root is **deferred**: primary targets are `manager/Dockerfile.copaw`, `manager/Dockerfile`, and entrypoint scripts refactored away from hard-coded `/data` and `/root/*` paths in embedded mode. Track as a separate hardening milestone if needed; do not ship a partial UID change that fails at first `mkdir`.

## Key Technical Notes

### Node.js Version

OpenClaw requires **Node.js >= 22** (the `--disable-warning` flag used internally requires Node.js 21.3+). The Manager image is built on `openclaw-base` which already includes Node.js 22. The Worker Dockerfile copies Node.js 22 from a build stage.

- **Manager**: Node 22 is provided by `openclaw-base` (the base image already includes it).
- **Worker**: Node 22 binary copied from build stage replaces Ubuntu 24.04 apt's Node.js 18.x (which lacks `--disable-warning` support).

### Higress AI Provider API

When creating a `qwen` type provider via the Higress Console API, you must include `rawConfigs` with Qwen-specific fields — otherwise the API returns "Missing Qwen specific configurations". The correct body is:

```json
{
  "type": "qwen",
  "name": "qwen",
  "tokens": ["your-api-key"],
  "protocol": "openai/v1",
  "rawConfigs": {
    "qwenEnableSearch": false,
    "qwenEnableCompatible": true,
    "qwenFileIds": []
  }
}
```

For OpenAI-compatible providers (DeepSeek, etc.), use `type=openai` with `rawConfigs.apiUrl` pointing to the provider's endpoint. This is all handled automatically in `manager/scripts/init/setup-higress.sh`.

### OpenClaw Skills Format

SKILL.md files **must** include a YAML front matter block, otherwise OpenClaw will not discover them:

```markdown
---
name: my-skill-name
description: What this skill does and when to use it
---

# Skill Title
...content...
```

Skills placed in `<workspace>/skills/<name>/SKILL.md` are auto-discovered (source: `openclaw-workspace`).

### OpenClaw Gateway Config

The `openclaw.json` must include gateway configuration for headless operation:

```json
{
  "gateway": {
    "mode": "local",
    "port": 18799,
    "auth": { "token": "<some-token>" }
  }
}
```

Without `gateway.mode=local`, OpenClaw refuses to start. Without `gateway.auth.token`, it also refuses.

## Code Style

- Shell scripts: use `${VAR}` syntax, functions for reusable logic
- Config templates: `${VAR}` placeholders, comments explaining each field
- Skills (SKILL.md): Must include YAML front matter (`name` + `description`), self-contained with full API reference and examples
- Tests: One file per acceptance case, source shared helpers, use assertion functions

## Debugging Tips

### View Manager Logs

Container name is `hiclaw-manager` (via `make install`) or `hiclaw-manager-test` (via `make test`).

**v1.1.0+ embedded install:** infrastructure logs are on **`hiclaw-controller`**, not the Manager container.

```bash
# Manager Agent
docker exec hiclaw-manager cat /var/log/hiclaw/manager-agent.log
docker exec hiclaw-manager cat /var/log/hiclaw/manager-agent-error.log  # OpenClaw gateway stderr (OpenClaw Manager)
docker exec hiclaw-manager bash -c 'cat /tmp/openclaw/openclaw-*.log' | jq .

# Higress / homeserver (embedded controller)
docker exec hiclaw-controller cat /var/log/hiclaw/higress-console.log
docker exec hiclaw-controller cat /var/log/hiclaw/tuwunel.log
```

### View Replay Conversation Logs

```bash
# After running `make replay`, logs are saved automatically
make replay-log

# Logs directory: logs/replay/replay-{timestamp}.log
```

### Check OpenClaw Skills Loading

```bash
docker exec hiclaw-manager bash -c \
  'OPENCLAW_CONFIG_PATH=/root/manager-workspace/openclaw.json openclaw skills list --json' \
  | jq '.skills[] | select(.source == "openclaw-workspace") | {name, eligible, description}'
```

### Interactive Shell in Container

```bash
docker exec -it hiclaw-manager bash
```

### Check Higress State

```bash
# Login (init uses "name", login uses "username")
curl -X POST http://localhost:18001/session/login \
  -H 'Content-Type: application/json' \
  -c /tmp/cookie \
  -d '{"username": "admin", "password": "your-password"}'

# List consumers
curl -s http://localhost:18001/v1/consumers -b /tmp/cookie | jq

# List routes
curl -s http://localhost:18001/v1/routes -b /tmp/cookie | jq

# List AI providers
curl -s http://localhost:18001/v1/ai/providers -b /tmp/cookie | jq

# List AI routes
curl -s http://localhost:18001/v1/ai/routes -b /tmp/cookie | jq
```

### Check MinIO State

```bash
mc alias set test http://localhost:9000 <user> <password>
mc ls test/hiclaw-storage/ --recursive
```

### Common Issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| `git clone` hangs during `docker build` | No proxy in build env | Pass `--build-arg http_proxy=...` via `DOCKER_BUILD_ARGS` |
| Health checks return 503 | `http_proxy` capturing localhost requests | Set `no_proxy=localhost,127.0.0.1,::1` |
| OpenClaw: `SyntaxError: Unexpected reserved word` | Node.js too old | Ensure Manager uses `openclaw-base` image; Worker uses Node.js 22 from build stage |
| OpenClaw: `requires Node >=22.0.0` | Same as above | Same as above |
| `--disable-warning= is not allowed in NODE_OPTIONS` | Node.js < 21.3 (e.g., Ubuntu apt's v18) | Ensure Worker uses Node.js 22 from build stage, not apt |
| OpenClaw: `gateway.mode=local` required | Missing gateway config in openclaw.json | Add `"gateway": {"mode": "local", ...}` |
| OpenClaw: `no token is configured` | Missing gateway auth token | Add `"gateway": {"auth": {"token": "..."}}` |
| Higress: `Missing Qwen specific configurations` | `type=qwen` requires `rawConfigs` fields | Include `rawConfigs: {qwenEnableCompatible: true, ...}` — see `setup-higress.sh` |
| Skills not loaded by OpenClaw | Missing YAML front matter in SKILL.md | Add `---\nname: ...\ndescription: ...\n---` |
| `setup-higress.sh` crashes on restart | `set -e` + `curl -sf` on "already exists" errors | Use `higress_api()` helper which handles this gracefully |
| Higress setup runs again on restart, resets consumers | Missing setup marker | Check `/data/.higress-setup-done`; delete it only to force re-setup |

## Branch Protection (Recommended)

The following branch protection settings are recommended for the `main` branch to ensure changes pass validation before acceptance. Configure these in **GitHub Settings → Branches → Branch protection rules → main**:

- **Require status checks to pass before merging:**
  - `controller` (from `remediation-gates.yml`)
  - `python-runtimes` (all matrix entries)
  - `shared-python`
  - `dashboard` (server + web)
  - `helm`
  - `shared-shell`
  - `test` (from `test-controller.yml`)
- **Require branches to be up to date before merging**
- **Require conversation resolution before merging**

> **Note:** Branch protection cannot be codified in-repo without a GitHub App or Terraform provider. A repository admin must configure these settings manually.

### Local Pre-commit Hooks

Complementary local checks are available via [pre-commit](https://pre-commit.com/):

```bash
pip install pre-commit
pre-commit install
```

This enables fast format/syntax checks (trailing whitespace, YAML/JSON validity, Go formatting) before each commit. These are opt-in and do not replace CI gates.
