# agentteams-worker-agent

Lightweight Worker Agent container. Includes:

- **Worker Agent** (OpenClaw): Executes tasks, communicates via Matrix
- **mc**: MinIO Client for file sync with centralized storage
- **mcporter**: MCP tool CLI for calling external services (GitHub, etc.)

Workers are **stateless** -- all configuration and memory is stored in the centralized MinIO file system. A Worker can be destroyed and recreated at any time without losing state.

## Build

```bash
# Via Makefile (recommended)
make build-worker

# Or directly
docker build -t agentteams/worker-agent:latest .
```

## Run

Workers are created by the Manager Agent. The Manager provides the installation command:

```bash
../install/agentteams-install.sh worker \
  --name alice \
  --fs http://<MANAGER_IP>:9000 \
  --fs-key <ACCESS_KEY> \
  --fs-secret <SECRET_KEY>
```

## Directory Structure

```
worker/
├── Dockerfile
├── scripts/
│   └── worker-entrypoint.sh        # Startup: sync config, configure mcporter, launch OpenClaw
└── agent/
    └── skills/
        ├── file-sync/
        │   ├── SKILL.md             # File sync skill (config, credentials, collaboration)
        │   └── scripts/
        │       └── agentteams-sync.sh   # Pull files from centralized storage
        └── github-operations/
            └── SKILL.md             # GitHub MCP operations skill
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `AGENTTEAMS_WORKER_NAME` | Yes | Worker name (e.g., `alice`) |
| `AGENTTEAMS_MATRIX_URL` | No | Matrix Homeserver URL injected by controller-managed deployments |
| `AGENTTEAMS_AI_GATEWAY_URL` | No | AI Gateway URL injected by controller-managed deployments |
| `AGENTTEAMS_FS_ENDPOINT` | Yes | MinIO/HTTP file system URL |
| `AGENTTEAMS_FS_BUCKET` | No | Bucket name for CoPaw or non-default storage layouts |
| `AGENTTEAMS_FS_ACCESS_KEY` | Yes | MinIO access key |
| `AGENTTEAMS_FS_SECRET_KEY` | Yes | MinIO secret key |

Runtime scripts use `AGENTTEAMS_MATRIX_URL` and `AGENTTEAMS_AI_GATEWAY_URL` as the main contract. Existing `AGENTTEAMS_*` inputs are still accepted by the shared bootstrap for compatibility with older controller/Helm releases.
