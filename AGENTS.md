# AgentTeams Codebase Navigation Guide

This file helps AI Agents (and human developers) quickly understand the project structure and find relevant code.

## What is AgentTeams

AgentTeams is an open-source Agent Teams system that uses IM (Matrix protocol) for multi-Agent collaboration with human-in-the-loop oversight. It consists of a Manager Agent (coordinator) and Worker Agents (task executors), connected via an AI Gateway (Higress), Matrix Homeserver (Tuwunel), and HTTP file storage (MinIO or cloud OSS). Production-style deployments use the Kubernetes operator and Helm chart; local installs use Docker Compose scripts under `install/`.

## Project Structure

```
AgentTeams/
├── agentteams-controller/   # Kubernetes operator (Go): reconciles Worker, Manager, Team, Human CRDs
├── helm/                # Helm chart (K8s): Higress, Tuwunel, MinIO, controller, Manager CR, defaults
├── manager/             # Manager images: OpenClaw-based (Dockerfile) and CoPaw-based (Dockerfile.copaw)
├── worker/              # OpenClaw Worker image (shared base pattern; runtime also selected at deploy time)
├── copaw/               # CoPaw Python package source (published as e.g. copaw-worker on PyPI)
├── hermes/              # Hermes Python package source (Hermes Matrix worker runtime)
├── openhuman/           # OpenHuman Worker image: Rust core + native Matrix (channel-matrix feature)
├── openclaw-base/       # Base image: Ubuntu + Node.js + bundled agent assets + mcporter
├── shared/lib/          # Shared shell libs copied into images (agentteams-env.sh, render-skills.sh, …)
├── install/             # Local install scripts (Docker Compose / embedded “all-in-one” stack)
├── scripts/             # Project-level utilities (e.g. replay-task.sh)
├── tests/               # Automated integration tests
├── docs/                # User-facing documentation
├── design/              # Internal design notes and API specs
├── changelog/           # Release notes fragments (current.md rolled into releases)
├── hack/                # Maintenance helpers (e.g. image mirror scripts)
├── migrate/             # Optional migration helpers
├── blog/                # Announcement / blog source
└── .github/workflows/   # CI: build images, tests, release automation
```

Logs and local artifacts (for example replay logs) stay out of git via `.gitignore`.

## Runtime model

**Worker runtimes** (per Worker CR `spec.runtime`, registry, or install defaults):

| Runtime   | Stack | Role |
|-----------|--------|------|
| `openclaw` | Node.js / OpenClaw (default) | Primary worker agent runtime |
| `copaw`    | Python / AgentScope via CoPaw | Alternative worker runtime |
| `hermes`   | Python / `hermes-worker` package | Alternative worker runtime (Matrix bridge + policies under `hermes/src/`) |

**Manager runtimes** (container env `AGENTTEAMS_MANAGER_RUNTIME`, CoPaw Manager CR / Helm `manager.runtime` where applicable):

| Runtime   | Behavior |
|-----------|-----------|
| `openclaw` (default) | OpenClaw gateway; primary Matrix channel uses the **message** tool pattern (see upstream OpenClaw / AgentTeams manager config). |
| `copaw` | Python CoPaw workspace; Matrix traffic uses the **`copaw channels send`** CLI (see `start-copaw-manager.sh`). |

Hermes and OpenHuman are **Worker-only** runtimes in the API and Helm worker defaults; the Manager entrypoint in `start-manager-agent.sh` today starts **openclaw** or **copaw** only.

**Deployment runtime** (`AGENTTEAMS_RUNTIME`): local embedded stack vs `aliyun` vs `k8s` changes which bootstrap steps run inside the Manager container (for example Matrix registration and Higress setup are skipped or reduced in `k8s` because the controller owns them).

## `manager/agent/` layout (built into Manager images)

Agent-facing Markdown and skills under `manager/agent/` are copied to `/opt/agentteams/agent/` in the image and synced into the Manager workspace by `upgrade-builtins.sh`. This tree is the single source of truth for builtin prompts and skills.

```
manager/agent/
├── AGENTS.md                    # OpenClaw Manager — primary bootstrap instructions
├── HEARTBEAT.md                 # OpenClaw Manager — periodic duties
├── SOUL.md                      # OpenClaw Manager — personality (often filled by onboarding)
├── TOOLS.md                     # Optional; referenced where applicable
├── skills/                      # Manager skills (shared by OpenClaw and CoPaw Managers)
│   └── <name>/                  # e.g. task-management, worker-management, … (each with SKILL.md)
├── skills-alpha/                # Experimental / optional skill packages
├── copaw-manager-agent/         # CoPaw Manager overrides
│   ├── AGENTS.md                # Replaces workspace AGENTS.md for CoPaw Manager
│   └── HEARTBEAT.md             # Replaces workspace HEARTBEAT.md for CoPaw Manager
├── worker-agent/                # Builtin OpenClaw Worker workspace template
├── copaw-worker-agent/          # Builtin CoPaw Worker workspace template
├── hermes-worker-agent/         # Builtin Hermes Worker workspace template
├── team-leader-agent/           # Team Leader agent template (Teams feature)
└── worker-skills/               # Extra worker skill templates (e.g. GitHub) pushed on demand
```

**Which files apply at startup**

- **OpenClaw Manager**: workspace copies of `AGENTS.md`, `HEARTBEAT.md`, `SOUL.md`, plus everything under `skills/` (from image builtins).
- **CoPaw Manager**: `copaw-manager-agent/AGENTS.md` and `copaw-manager-agent/HEARTBEAT.md` are merged into the workspace copies during `upgrade-builtins.sh` when `MANAGER_RUNTIME=copaw`; **skills/** stay shared with OpenClaw Manager.
- **Workers**: the controller (or local registry) records `runtime` per worker; init scripts materialize the matching `*-worker-agent/` tree into that worker’s storage.

**Template rendering**: `shared/lib/render-skills.sh` runs from `start-manager-agent.sh` over workspace and image paths so known `${VAR}` placeholders become literal text before agents read them.

## Key Entry Points

### To understand the architecture

- Read [docs/architecture.md](docs/architecture.md) for system overview and component diagram
- Read [design/design.md](design/design.md) for full product design (Chinese)
- Read [design/poc-design.md](design/poc-design.md) for detailed implementation specs

### Kubernetes deployment

- [helm/agentteams/](helm/agentteams/) — primary Helm chart (`Chart.yaml`, `values.yaml`): matrix, gateway, storage, controller, Manager CR, worker defaults, optional Element Web and CMS hooks

### Controller (operator) development

- [agentteams-controller/](agentteams-controller/) — Go operator: CRD definitions under `api/v1beta1/`, reconcilers, `agt` CLI baked into Manager/Worker images

### To build and run

- [Makefile](Makefile) — unified build/test/push/install/replay interface (`make help` for all targets)
- [docs/quickstart.md](docs/quickstart.md) — end-to-end guide from zero to working team
- [install/agentteams-install.sh](install/agentteams-install.sh) — local installation script
- [scripts/replay-task.sh](scripts/replay-task.sh) — send tasks to Manager via Matrix CLI

### Local full build (from source)

The image dependency chain is: `openclaw-base` → `manager` / `worker`. CoPaw and Hermes worker images and the controller image are additional build targets; see the `Makefile` for current image names.

By default, `OPENCLAW_BASE_IMAGE` points to the remote registry (`higress-registry.cn-hangzhou.cr.aliyuncs.com/agentteams/openclaw-base`). When building locally from a modified `openclaw-base`, you **must** override it to the local image name so that manager/worker actually use your local base:

```bash
# Step 1: Build openclaw-base
make build-openclaw-base

# Step 2: Build manager, worker, copaw-worker using the LOCAL base
make build-manager build-worker build-copaw-worker \
    OPENCLAW_BASE_IMAGE=agentteams/openclaw-base \
    OPENCLAW_BASE_VERSION=latest
```

**Common pitfall**: Running `make build-manager build-worker OPENCLAW_BASE_VERSION=latest` without `OPENCLAW_BASE_IMAGE=agentteams/openclaw-base` will pull the remote registry's `:latest` tag instead of using the locally-built image. Always set both variables together for local builds.

**Proxy support**: If behind an HTTP proxy, pass proxy build args. This covers APT, PIP, NPM and all other network access — no mirror args needed:

```bash
PROXY_ARGS="--build-arg HTTP_PROXY=http://host.containers.internal:1087 \
    --build-arg HTTPS_PROXY=http://host.containers.internal:1087 \
    --build-arg http_proxy=http://host.containers.internal:1087 \
    --build-arg https_proxy=http://host.containers.internal:1087"

make build-embedded build-manager build-worker build-copaw-worker DOCKER_BUILD_ARGS="${PROXY_ARGS}"
```

Note: use `host.containers.internal` for Podman on macOS, `host.docker.internal` for Docker Desktop.

**China build acceleration (without proxy)**: All Dockerfiles default to official sources. For builds in China without proxy, pass mirror args:

```bash
# APT mirror (for Ubuntu/Debian-based images: openclaw-base, copaw, manager-copaw, embedded)
make build-embedded DOCKER_BUILD_ARGS="--build-arg APT_MIRROR=mirrors.aliyun.com"

# PIP mirror (for Python-based images: copaw, manager-copaw)
make build-copaw-worker DOCKER_BUILD_ARGS="--build-arg APT_MIRROR=mirrors.aliyun.com --build-arg PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple/"

# NPM mirror (for Node.js-based images: openclaw-base)
make build-openclaw-base DOCKER_BUILD_ARGS="--build-arg APT_MIRROR=mirrors.aliyun.com --build-arg NPM_REGISTRY=https://registry.npmmirror.com/"
```

### To modify the Manager container

- [manager/Dockerfile](manager/Dockerfile) — OpenClaw-based Manager (from `openclaw-base`; bundles `agt` CLI from controller image)
- [manager/Dockerfile.copaw](manager/Dockerfile.copaw) — CoPaw-based Manager (Python venv + CoPaw from PyPI; same agent tree and scripts pattern)
- [manager/supervisord.conf](manager/supervisord.conf) — process orchestration (local embedded stack)
- [manager/scripts/init/](manager/scripts/init/) — startup: `start-manager-agent.sh` (runtime + `AGENTTEAMS_RUNTIME`), `upgrade-builtins.sh`, Higress/Matrix bootstrap where applicable
- [manager/scripts/lib/](manager/scripts/lib/) — shared libraries (`base.sh`, `container-api.sh`, …)
- [manager/configs/](manager/configs/) — init-time configuration templates (e.g. `manager-openclaw.json.tmpl`)

The Manager **image** is an agent runtime plus scripts; Higress, Tuwunel, MinIO, and Element Web are brought up by **Helm** or **install/**, not inside the slim Manager Dockerfile unless you use the legacy all-in-one local compose layout.

### To modify the Worker container (OpenClaw)

- [worker/Dockerfile](worker/Dockerfile) — build definition (Node.js from build stage / openclaw-base pattern)
- [worker/scripts/worker-entrypoint.sh](worker/scripts/worker-entrypoint.sh) — startup logic

### To modify the Worker container (CoPaw)

- [copaw/Dockerfile](copaw/Dockerfile) — CoPaw worker image build
- [copaw/scripts/copaw-worker-entrypoint.sh](copaw/scripts/copaw-worker-entrypoint.sh) — startup logic
- [copaw/src/copaw_worker/](copaw/src/copaw_worker/) — CoPaw worker Python package
- [copaw/README.md](copaw/README.md) — PyPI package `copaw-worker` install and CLI overview

### To modify the Hermes worker runtime

- [hermes/](hermes/) — Python package layout under `hermes/src/` (`hermes_worker`, `hermes_matrix`, CLI)

### To modify the OpenHuman worker runtime

- [openhuman/](openhuman/) — Rust-based Worker runtime with native Matrix support (`channel-matrix` feature flag)
- [openhuman/Dockerfile](openhuman/Dockerfile) — Multi-stage build: `rust:1.93-bookworm` → `debian:bookworm-slim`
- [openhuman/scripts/openhuman-worker-entrypoint.sh](openhuman/scripts/openhuman-worker-entrypoint.sh) — Entrypoint: config.toml generation, MinIO sync, health check
- [manager/agent/openhuman-worker-agent/](manager/agent/openhuman-worker-agent/) — Agent template and skills

### To manage Worker containers via socket (local Docker)

- [manager/scripts/lib/container-api.sh](manager/scripts/lib/container-api.sh) — Docker/Podman REST API helpers for direct Worker creation on the Manager host

In `k8s` / `aliyun` modes, Workers are created via the controller API instead of a local socket.

### To modify Agent behavior

- [manager/agent/SOUL.md](manager/agent/SOUL.md) — OpenClaw Manager personality and rules
- [manager/agent/HEARTBEAT.md](manager/agent/HEARTBEAT.md) — OpenClaw Manager periodic check routine
- [manager/agent/skills/](manager/agent/skills/) — Manager skills (each skill directory contains `SKILL.md` and optional `scripts/` / `references/`)
- [manager/agent/copaw-manager-agent/](manager/agent/copaw-manager-agent/) — CoPaw Manager prompt overrides
- [manager/agent/worker-skills/](manager/agent/worker-skills/) — Skill definitions pushed to Workers on creation
- [manager/agent/worker-skills/github-operations/SKILL.md](manager/agent/worker-skills/github-operations/SKILL.md) — Worker GitHub skill (on-demand)

### To modify CI/CD

- [.github/workflows/](.github/workflows/) — GitHub Actions workflows
- [tests/](tests/) — integration test suite

### To modify Higress routing and initialization

- [manager/scripts/init/setup-higress.sh](manager/scripts/init/setup-higress.sh) — route, consumer, MCP server setup (local / full console mode)
- [design/higress-console-api.yaml](design/higress-console-api.yaml) — Higress Console API spec (OpenAPI 3.0)

## Technology Stack

| Component | Technology | Purpose |
|-----------|-----------|---------|
| Kubernetes operator | agentteams-controller (Go) | Reconciles Worker / Manager / Team / Human CRDs; REST API; worker lifecycle |
| AI Gateway | Higress (chart or external) | LLM proxy, MCP Server hosting, consumer auth, route management |
| Matrix Server | Tuwunel (conduwuit fork) | IM between Agents and Human |
| Matrix Client | Element Web (optional) | Browser-based IM interface |
| File System | MinIO or OSS | Centralized object storage for workspaces and agent state |
| Agent Framework | OpenClaw (fork) | Default Manager/Worker runtime (Node.js gateway + Matrix plugin) |
| Agent Framework | CoPaw (Python / AgentScope) | Alternative Manager and Worker runtime |
| Agent Framework | Hermes (`hermes-worker`) | Alternative Python Worker runtime |
| Agent Framework | OpenHuman (`openhuman-core`) | Alternative Rust Worker runtime with native Matrix |
| MCP CLI | mcporter | Worker calls MCP Server tools via CLI |

## Changelog Policy

Any change that affects the content of a built image — i.e. modifications under `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, or `agentteams-controller/` — **must** be recorded in [`changelog/current.md`](changelog/current.md) before committing.

Format: one bullet per logical change, with a linked commit hash, e.g.:

```
- feat(manager): add task-management skill extracted from AGENTS.md ([a1b2c3d](https://github.com/agentscope-ai/AgentTeams/commit/a1b2c3d...))
- fix(manager): fix upgrade-builtins idempotency (duplicate marker insertion) ([e4f5g6h](https://github.com/agentscope-ai/AgentTeams/commit/e4f5g6h...))
```

On release, the workflow automatically renames `current.md` → `vX.Y.Z.md` and creates a fresh `current.md`.

## Key Design Patterns

1. **All communication in Matrix Rooms**: Human + Manager + Worker are all in the same Room. Human sees everything, can intervene anytime.
2. **Centralized file system**: All Agent configs and state stored in MinIO (or cloud object storage). Workers are stateless — destroy and recreate freely.
3. **Unified credential management**: Worker uses one Consumer key-auth token for both LLM and MCP Server access. Manager controls permissions.
4. **Skills as documentation**: Each SKILL.md is a self-contained reference that tells the Agent how to use an API or tool.

## Agent-Facing Content: Writing Convention

Files under `manager/agent/` are **read by the Agent at runtime**, not by human developers. All content in these paths must be written from the Agent's own perspective using second-person voice ("you"):

- **AGENTS.md, SOUL.md, HEARTBEAT.md** — address the Agent directly: "You are the Manager...", "Your responsibilities include..."
- **SKILL.md** — instruct the Agent as the reader: "Use this script to...", "You can call...", "Run `mcporter --config ~/mcporter-servers.json list` to see..."
- **TOOLS.md** — describe tools available to the Agent: "You have access to...", "Use `mc cp` to push..."
- **Script `log` output and comments** — write from the system's perspective, but keep the Agent as the implied operator. Avoid third-person references like "the Manager does X" — instead say "Step 4: Updating your mcporter-servers.json..."

**Do NOT** use third-person descriptions like "Manager can call..." or "This skill provides..." in agent-facing files. The Agent is the reader — talk to it directly.

This convention applies to all files that end up in the Agent's workspace or are loaded as skills/prompts at runtime:

- `manager/agent/**` (Manager Agent config, skills, tools)
- `manager/agent/worker-agent/**` (OpenClaw Worker Agent config, builtin skills)
- `manager/agent/copaw-worker-agent/**` (CoPaw Worker Agent config, builtin skills)
- `manager/agent/hermes-worker-agent/**` (Hermes Worker Agent config, builtin skills)
- `manager/agent/team-leader-agent/**` (Team Leader Agent config and skills)
- `manager/agent/worker-skills/**` (on-demand skill definitions pushed to Workers)

## Environment Variables

See [manager/scripts/init/start-manager-agent.sh](manager/scripts/init/start-manager-agent.sh) for the full list of `AGENTTEAMS_*` environment variables used by the Manager container.

## Verified Technical Details

All technical assumptions have been verified in POC. See [design/poc-tech-verification.md](design/poc-tech-verification.md) for detailed verification results. Key findings that affect implementation:

- Tuwunel uses `CONDUWUIT_` env prefix (not `TUWUNEL_`)
- Higress Console uses Session Cookie auth (not Basic Auth)
- MCP Server created via `PUT` (not `POST`)
- Auth plugin takes ~40s to activate after first configuration
- OpenClaw Skills auto-load from `workspace/skills/<name>/SKILL.md`
