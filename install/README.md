# AgentTeams Installation

One-click installation script for AgentTeams Manager and Worker Agents.

## Requirements

- **Docker Desktop** (Windows/macOS) or **Docker Engine** (Linux) must be installed and running
- **PowerShell 7+** (Windows only)

## Quick Start

### macOS / Linux

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.sh)
```

### Windows (PowerShell 7+)

```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; iex $wc.DownloadString('https://raw.githubusercontent.com/agentscope-ai/AgentTeams/main/install/agentteams-install.ps1')
```

## Installation Modes

### Quick Start (Recommended)

Fast installation with Alibaba Cloud Bailian as the default LLM provider. Just provide your API key and go!

### Manual

Customize each option:
- LLM Provider (Alibaba Cloud Bailian or OpenAI-compatible)
- Admin credentials
- Port configuration
- Domain names
- GitHub PAT (optional)
- Data persistence options

## Usage

### Install Manager

**macOS / Linux:**
```bash
./agentteams-install.sh manager
# or simply
./agentteams-install.sh
```

**Windows:**
```powershell
.\agentteams-install.ps1 manager
# or simply
.\agentteams-install.ps1
```

Interactive prompts will ask for:
- LLM Provider and API Key
- Admin credentials
- Domain names (optional, defaults to `*-local.agentteams.io`)
- GitHub PAT (optional)

### Install Worker

Workers are created by the Manager Agent through conversation. The Manager provides the exact command to run:

**macOS / Linux:**
```bash
./agentteams-install.sh worker \
  --name alice \
  --fs http://fs-local.agentteams.io:18080 \
  --fs-key <ACCESS_KEY> \
  --fs-secret <SECRET_KEY>
```

**Windows:**
```powershell
.\agentteams-install.ps1 worker `
  -Name alice `
  -Fs http://fs-local.agentteams.io:18080 `
  -FsKey <ACCESS_KEY> `
  -FsSecret <SECRET_KEY>
```

### Reset Worker

**macOS / Linux:**
```bash
./agentteams-install.sh worker --reset --name alice \
  --fs http://fs-local.agentteams.io:18080 \
  --fs-key <ACCESS_KEY> \
  --fs-secret <SECRET_KEY>
```

**Windows:**
```powershell
.\agentteams-install.ps1 worker -Reset `
  -Name alice `
  -Fs http://fs-local.agentteams.io:18080 `
  -FsKey <ACCESS_KEY> `
  -FsSecret <SECRET_KEY>
```

### Uninstall

**macOS / Linux:**
```bash
./agentteams-install.sh uninstall
```

**Windows:**
```powershell
.\agentteams-install.ps1 uninstall
```

### Upgrade (safe orchestrator, P1)

`install/agentteams-upgrade.sh` upgrades the **controller as a separate step**,
with a cold kine backup, a health gate and a rollback path. It does not use the
big-bang in-place flow of `agentteams-install.sh`.

```bash
# 1. inspect (read-only, JSON)
./install/agentteams-upgrade.sh preflight --json

# 2. back up kine + env + image IDs (stop/start the controller)
./install/agentteams-upgrade.sh backup --yes

# 3. upgrade the controller only; the old container is kept for rollback
./install/agentteams-upgrade.sh controller --yes --controller-image <image>

# 4. inspect the last state
./install/agentteams-upgrade.sh status
```

`backup` copies kine only after the controller is confirmed stopped; a failed stop, a container that is still running, a failed cp, a failed restart, or a failed post-backup health gate all exit nonzero and write `status: failed` (with a `detail`), and the failure path still attempts to bring the controller back whenever it was stopped.

No remote mount is required: Docker volumes work out of the box. Bind and remote mounts are detected and preserved as-is; remote filesystems are reported as warnings, not failures.
The preflight also detects the storage backend (embedded MinIO vs external S3/RustFS) from the endpoint, bucket and `/data/minio` mount, and compares the **live** `AGENTTEAMS_FS_ENDPOINT` / `AGENTTEAMS_STORAGE_PROVIDER` / bucket reported by the controller, Manager and Workers. A split — for example, the controller on RustFS while a Manager or Worker still points at MinIO — fails closed with `storage split-brain`; `--allow-storage-split` is an explicit migration-window override, not a normal upgrade flag. Storage migration itself is a separate runbook; do not combine it with a controller upgrade.

> **Gateway note:** if the controller embeds the Higress gateway, a controller upgrade can leave `healthz` green while the data plane is down (Wasm plugin version mismatch). Verify `http://127.0.0.1:8080/v1/models` after the controller step; plugin files are container-layer and may need a backfill after each rebuild.
> If plugin files were added to the running container at runtime, a fresh container from the same image fails the default 8080 gate and the controller step rolls back (live-validated on a single-node aarch64 host: same image, no backfill → rollback; with `--higress-backfill-dir` → `8080/v1/models=401`, `state=done`). Pass `--higress-backfill-dir <dir>` on upgrade in that case.

### Boundary preservation

A controller rebuilt from the image would silently lose any security or
resource boundary applied to the running container (`--user`, read-only
rootfs, memory caps, dropped capabilities, ...). The `controller` step
therefore carries the current container's boundary over into the new one:

- **Preserved and re-applied:** `user`, `workdir`, read-only rootfs,
  `memory`, `memory-swap`, `cpus`, `cpu-shares`, `cpuset-cpus`, `pids-limit`,
  `init`, `privileged`, `group-add`, `security-opt`, `cap-add`, `cap-drop`,
  `tmpfs`, `ulimit`, `devices`, **`shm-size`**, `domainname`, `stop-signal`,
  `stop-timeout`, `oom-kill-disable`, `cgroupns`. The shared-memory size is
  always passed explicitly (`--shm-size <old value>`): daemons differ in
  their default (64 MiB vs several GiB), so relying on the default silently
  changes the runtime configuration.
- **Not representable in `docker create`** and **refused while non-default:**
  `ipc-mode`, `pid-mode`, `uts-mode`, `userns-mode`, `cgroup-parent`,
  `sysctls`, `extra-hosts`, `dns*`, non-default log driver, `oom-score-adj`,
  `runtime`, `blkio-weight`, `memory-reservation`, `cpu-quota`,
  `cpu-period`, an explicitly set `mac-address` (it would collide with the
  retained old container) and a custom `hostname` (the default is the
  container id and cannot be compared across a rebuild). Preflight reports them (and fails the gate) before anything
  is stopped; the controller step refuses to rename the container while any
  of them is set.
- After the new container is created, the preserved fields are compared
  key by key (`boundary verify`); a mismatch discards the new container and
  restores the old one before it is ever stopped. Empty/default values are
  treated as "not set".
- `--prepare-only` runs exactly that cycle — create, verify, discard the new
  container, restore the old one — and exits 0 with `boundary preserved: ...`
  without starting anything. Use it as a pre-check before a real upgrade.

- `OomKillDisable` is a `*bool`: an unset value renders `<nil>` on an ordinary
  `docker run` container, while `docker create` stores `false`. The verifier
  normalizes unset/false to the same default but keeps explicit `true`
  distinct; the real-engine regression is
  `bash install/tests/test-upgrade-boundary-real.sh` (skips without a
  daemon/cached image, or when the daemon rejects `--oom-kill-disable`).- `--controller-name <name>` targets a controller container with a
  non-default name; the preserved-boundary logic applies identically.

This is the same failure mode class as the gateway note above: the health
gate would pass while the security posture silently regressed. `--dry-run`
shows the exact `docker create` line including every preserved flag.

Add `--dry-run` to any mutating command to print the exact steps first. P1
covers `preflight`/`backup`/`controller`/`status`; workers (canary), manager,
dashboard and memory are follow-ups. See the upgrade design issue for the full design.

## Non-Interactive Mode (Automation)

Set environment variables to skip prompts:

**macOS / Linux:**
```bash
export AGENTTEAMS_NON_INTERACTIVE=1
export AGENTTEAMS_LLM_API_KEY="your-api-key"
./agentteams-install.sh
```

**Windows:**
```powershell
$env:AGENTTEAMS_NON_INTERACTIVE = "1"
$env:AGENTTEAMS_LLM_API_KEY = "your-api-key"
.\agentteams-install.ps1
```

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `AGENTTEAMS_NON_INTERACTIVE` | Skip all prompts | `0` |
| `AGENTTEAMS_LLM_PROVIDER` | LLM provider (`qwen` or `openai-compat`) | `qwen` |
| `AGENTTEAMS_DEFAULT_MODEL` | Default model ID | `qwen3.5-plus` |
| `AGENTTEAMS_LLM_API_KEY` | LLM API key | *(required)* |
| `AGENTTEAMS_ADMIN_USER` | Admin username | `admin` |
| `AGENTTEAMS_ADMIN_PASSWORD` | Admin password | *(auto-generated)* |
| `AGENTTEAMS_PORT_GATEWAY` | Gateway port | `18080` |
| `AGENTTEAMS_PORT_CONSOLE` | Higress console port | `18001` |
| `AGENTTEAMS_MATRIX_DOMAIN` | Matrix domain | `matrix-local.agentteams.io:18080` |
| `AGENTTEAMS_DATA_DIR` | Data directory | Docker volume |
| `AGENTTEAMS_WORKSPACE_DIR` | Manager workspace | `~/agentteams-manager` |
| `AGENTTEAMS_VERSION` | Image tag | `latest` |
| `AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION` | Experimental DeepSeek Harness runtime image tag; released independently from AgentTeams | `v0.1.0` |
| `AGENTTEAMS_REGISTRY` | Image registry | *(auto-detected by timezone)* |

### External S3 backend (embedded)

By default, an embedded installation stores agent files in the built-in
MinIO (`http://127.0.0.1:9000`, bucket `agentteams-storage`). To point the
controller at an external S3-compatible backend (e.g. RustFS) instead, set:

| Variable | Description | Default |
|----------|-------------|---------|
| `AGENTTEAMS_STORAGE_PROVIDER` | Storage backend: `minio` or `s3` | `minio` |
| `AGENTTEAMS_FS_ENDPOINT` | External S3 endpoint (must start with `http://` or `https://`) | `http://127.0.0.1:9000` |
| `AGENTTEAMS_FS_BUCKET` | External S3 bucket | `agentteams-storage` |
| `AGENTTEAMS_FS_ACCESS_KEY` | Access key (falls back to `AGENTTEAMS_MINIO_USER`) | *(required with `s3`)* |
| `AGENTTEAMS_FS_SECRET_KEY` | Secret key (falls back to `AGENTTEAMS_MINIO_PASSWORD`) | *(required with `s3`)* |

Example:

```bash
AGENTTEAMS_STORAGE_PROVIDER=s3 \
AGENTTEAMS_FS_ENDPOINT=http://rustfs:9000 \
AGENTTEAMS_FS_BUCKET=agentteams-storage \
AGENTTEAMS_FS_ACCESS_KEY=... AGENTTEAMS_FS_SECRET_KEY=... \
./install/agentteams-install.sh
```

**Network reachability:** the endpoint is resolved from *inside* the
controller container, so `127.0.0.1` there refers to the container itself,
not the host. Use an address reachable from the `agentteams-net` Docker
network — e.g. a container name like `rustfs` on that network, or the
host's gateway IP (`172.17.0.1` on the default bridge).

**Switching an existing installation:** do *not* use reinstall to change
the storage backend — reinstall removes the `agentteams-data` volume and
everything in it. Instead, follow the cutover runbook from #1413 (recreate
the controller, then recreate the members).

**Preview without installing:** print exactly the storage env the installer
would pass to the controller (no root, no Docker, no prompts):

```bash
AGENTTEAMS_STORAGE_PROVIDER=s3 \
AGENTTEAMS_FS_ENDPOINT=http://rustfs:9000 \
AGENTTEAMS_FS_BUCKET=agentteams-storage \
AGENTTEAMS_FS_ACCESS_KEY=... AGENTTEAMS_FS_SECRET_KEY=... \
./install/agentteams-install.sh --print-controller-env
```

The same mode can be triggered with `AGENTTEAMS_PRINT_CONTROLLER_ENV=1`;
invalid storage configuration exits non-zero with the missing variables
named.

## Platform Notes

### Windows

- Requires **PowerShell 7+** (PowerShell Core)
- Docker Desktop must be running with WSL 2 backend
- The script uses named pipe (`//var/run/docker.sock`) for Docker socket mount

### macOS

- Docker Desktop must be running
- Supports both Intel (amd64) and Apple Silicon (arm64)

### Linux

- Docker Engine or Docker Desktop required
- Supports both amd64 and arm64 architectures

## Post-Installation

After successful installation:

1. Open the Element Web URL in your browser
2. Login with your admin credentials
3. Start chatting with the Manager agent

### Mobile Access

You can also access AgentTeams from mobile devices using FluffyChat or Element Mobile:

1. Download FluffyChat or Element on your phone
2. Set homeserver to: `http://<your-lan-ip>:18080`
3. Login with your admin credentials
