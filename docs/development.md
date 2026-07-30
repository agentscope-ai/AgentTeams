# Development Guide

Guide for contributing to AgentTeams, building images locally, and running tests.

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
make build-agentteams-controller
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

### Helm chart (`helm/agentteams`)

The production Kubernetes install is defined under **`helm/agentteams/`** (subcharts for gateway, homeserver, storage, etc.). Useful Makefile targets:

```bash
make helm-lint      # helm dependency build + helm lint
make helm-template  # render templates locally (validation)
make sync-crds      # copy CRD YAML from agentteams-controller/config/crd/ into the chart
```

### Push Images (Multi-Architecture by Default)

`make push` always builds multi-arch manifests (amd64 + arm64) to avoid accidentally overwriting multi-arch images with a single-arch image. This uses `docker buildx`:

```bash
# Build amd64 + arm64 and push to registry
make push VERSION=0.1.0 REGISTRY=ghcr.io REPO=agentscope-ai/AgentTeams

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
docker build --build-arg HIGRESS_REGISTRY=higress-registry.us-west-1.cr.aliyuncs.com -t agentteams/manager:latest ./manager/
```

## Install / Uninstall / Replay

### Quick Install (Minimal)

Only `AGENTTEAMS_LLM_API_KEY` is required — everything else uses sensible defaults:

```bash
# Build images + install Manager with one command
AGENTTEAMS_LLM_API_KEY="sk-xxx" make install
```

This will:
1. Build Manager and Worker images (`make build`)
2. Run the install script (`install/agentteams-install.sh manager`)
3. Mount the container runtime socket for direct Worker creation
4. Save configuration to `./agentteams-manager.env`

### Custom Install

Override any configuration via environment variables:

```bash
AGENTTEAMS_LLM_API_KEY="sk-xxx" \
AGENTTEAMS_LLM_PROVIDER="openai" \
AGENTTEAMS_DEFAULT_MODEL="gpt-4o" \
AGENTTEAMS_ADMIN_USER="myadmin" \
AGENTTEAMS_ADMIN_PASSWORD="mypassword" \
make install
```

### Uninstall

```bash
make uninstall   # Runs install/agentteams-install.sh uninstall (v1.1+: controller + manager + workers + volume + env)
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
- Reads credentials from `./agentteams-manager.env`
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

This reads credentials from `./agentteams-manager.env` and skips the container lifecycle (start/stop).

## Running Tests

### Full Integration Test Suite

```bash
# Build images + run all 10 test cases
export AGENTTEAMS_LLM_API_KEY="your-api-key"
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
export AGENTTEAMS_GITHUB_TOKEN="ghp_..."
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

Route, consumer, and MCP server bootstrap for **embedded** stacks is owned by the **controller** image (`agentteams-controller` / `Dockerfile.embedded` wiring). The legacy **`manager/scripts/init/setup-higress.sh`** path still applies to **older single-container** Manager images (≤v1.0.9) and to Manager-side behaviors where applicable — check your install mode before editing.

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
| `AGENTTEAMS_LLM_API_KEY` | LLM access for Agent behavior tests |
| `AGENTTEAMS_GITHUB_TOKEN` | GitHub operations in tests 08-10 |

### Local CI Simulation

```bash
# Same flow as CI but local (single arch)
export AGENTTEAMS_LLM_API_KEY="your-key"
make test

# Multi-arch build like CI does on main branch
docker login ghcr.io
make push VERSION=latest REGISTRY=ghcr.io REPO=agentscope-ai/AgentTeams
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
AGENTTEAMS_LLM_API_KEY="your-key" make test SKIP_BUILD=1
```

## Container Runtime Socket (Direct Worker Creation)

When the Manager container is started with the host's container runtime socket mounted, it can create Worker containers directly — no human intervention needed for local deployments.

### How It Works

The Manager detects the socket at startup and sets `AGENTTEAMS_CONTAINER_RUNTIME=socket`. The `container-api.sh` script provides functions to create/start/stop Worker containers via the Docker-compatible REST API (works with both Docker and Podman).

### Socket Paths

| Runtime | Socket path (host) | Mount command |
|---------|-------------------|---------------|
| Docker | `/var/run/docker.sock` | `-v /var/run/docker.sock:/var/run/docker.sock` |
| Podman (rootful, Linux) | `/run/podman/podman.sock` | `-v /run/podman/podman.sock:/var/run/docker.sock --security-opt label=disable` |
| Podman (macOS machine) | Inside VM: `/run/podman/podman.sock` | Same as rootful (VM provides symlink at `/var/run/docker.sock`) |

### Example: Manual Start with Socket

```bash
# Docker
docker run -d --name agentteams-manager \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e AGENTTEAMS_WORKER_IMAGE=agentteams/worker-agent:latest \
  ... \
  agentteams/manager:latest

# Podman
podman run -d --name agentteams-manager \
  -v /run/podman/podman.sock:/var/run/docker.sock \
  --security-opt label=disable \
  -e AGENTTEAMS_WORKER_IMAGE=agentteams/worker-agent:latest \
  ... \
  agentteams/manager:latest
```

### Test Integration

The test orchestrator (`tests/run-all-tests.sh`) automatically detects the socket and mounts it when available.

### Security Note

Mounting the container runtime socket gives the container full control over the host's container runtime (equivalent to root access). This is acceptable for local development. In production, consider using more restrictive approaches like Podman socket activation with limited API access.

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

Container name is `agentteams-manager` (via `make install`) or `agentteams-manager-test` (via `make test`).

**v1.1.0+ embedded install:** infrastructure logs are on **`agentteams-controller`**, not the Manager container.

```bash
# Manager Agent
docker exec agentteams-manager cat /var/log/agentteams/manager-agent.log
docker exec agentteams-manager cat /var/log/agentteams/manager-agent-error.log  # OpenClaw gateway stderr (OpenClaw Manager)
docker exec agentteams-manager bash -c 'cat /tmp/openclaw/openclaw-*.log' | jq .

# Higress / homeserver (embedded controller)
docker exec agentteams-controller cat /var/log/agentteams/higress-console.log
docker exec agentteams-controller cat /var/log/agentteams/tuwunel.log
```

### View Replay Conversation Logs

```bash
# After running `make replay`, logs are saved automatically
make replay-log

# Logs directory: logs/replay/replay-{timestamp}.log
```

### Check OpenClaw Skills Loading

```bash
docker exec agentteams-manager bash -c \
  'OPENCLAW_CONFIG_PATH=/root/manager-workspace/openclaw.json openclaw skills list --json' \
  | jq '.skills[] | select(.source == "openclaw-workspace") | {name, eligible, description}'
```

### Interactive Shell in Container

```bash
docker exec -it agentteams-manager bash
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
mc ls test/agentteams-storage/ --recursive
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
