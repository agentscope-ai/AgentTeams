# agentteams-manager-agent

All-in-one Manager Agent container. Includes:

- **Higress AI Gateway** (port 8080 gateway, 8001 console): LLM proxy, MCP Server hosting, consumer auth
- **Tuwunel Matrix Server** (port 6167): Agent IM communication
- **MinIO** (port 9000 API, 9001 console): Centralized HTTP file system
- **Element Web** (via Nginx on port 8088, proxied through Higress): Browser-based IM client
- **Manager Agent** (OpenClaw or QwenPaw): Coordinates Workers, manages credentials, assigns tasks
- **mc mirror**: Bidirectional file sync between MinIO and local filesystem

## Runtime Selection

Manager Agent supports two runtime modes via `AGENTTEAMS_MANAGER_RUNTIME`:
- `openclaw` (default): Node.js gateway mode
- `copaw`: Python workspace mode

Both runtimes share the same skills and workspace structure.

## Build

```bash
# Via Makefile (recommended)
make build-manager

# Or directly
docker build -t hiclaw/manager-agent:latest .
```

## Run

Use the installation script instead of running directly:

```bash
../install/agentteams-install.sh manager
```

## Directory Structure

```
manager/
├── Dockerfile              # Multi-stage build
├── supervisord.conf        # Process orchestration (priority-ordered)
├── scripts/
│   ├── init/               # Container startup scripts (supervisord)
│   │   ├── start-*.sh      # Component startup scripts
│   │   └── setup-higress.sh # Higress route/consumer/MCP init
│   └── lib/                # Shared libraries
│       ├── base.sh         # Shared utilities (waitForService, generateKey, log)
│       └── container-api.sh # Docker/Podman REST API helpers
├── agent/                  # Manager agent definition (synced to MinIO)
│   ├── AGENTS.md           # Agent instructions
│   ├── SOUL.md             # Manager personality and rules
│   ├── HEARTBEAT.md        # Periodic check routine
│   └── skills/             # Each skill is self-contained
│       ├── worker-management/
│       │   ├── SKILL.md
│       │   ├── scripts/    # create-worker.sh, generate-worker-config.sh
│       │   └── references/ # worker-openclaw.json.tmpl
│       ├── mcp-server-management/
│       │   ├── SKILL.md
│       │   └── references/ # mcp-github.yaml
│       └── matrix-server-management/
│           └── SKILL.md
├── configs/
│   └── manager-openclaw.json.tmpl  # Manager OpenClaw config template
└── tests/
    └── smoke-test.sh       # Post-startup health check
```

## Environment Variables

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `AGENTTEAMS_MANAGER_RUNTIME` | No | `openclaw` | Manager runtime: `openclaw` or `copaw` |
| `AGENTTEAMS_ADMIN_USER` | Yes | - | Human admin Matrix username |
| `AGENTTEAMS_ADMIN_PASSWORD` | Yes | - | Human admin password |
| `AGENTTEAMS_MANAGER_PASSWORD` | Yes | - | Manager Agent Matrix password |
| `AGENTTEAMS_REGISTRATION_TOKEN` | Yes | - | Tuwunel registration token |
| `AGENTTEAMS_MATRIX_DOMAIN` | No | `matrix-local.agentteams.io:8080` | Matrix server domain |
| `AGENTTEAMS_MATRIX_CLIENT_DOMAIN` | No | `matrix-client-local.agentteams.io` | Element Web domain |
| `AGENTTEAMS_AI_GATEWAY_DOMAIN` | No | `aigw-local.agentteams.io` | AI Gateway domain (for LLM and MCP) |
| `AGENTTEAMS_FS_DOMAIN` | No | `fs-local.agentteams.io` | HTTP file system domain |
| `AGENTTEAMS_LLM_PROVIDER` | Yes | - | LLM provider name |
| `AGENTTEAMS_DEFAULT_MODEL` | Yes | - | Default LLM model ID |
| `AGENTTEAMS_LLM_API_KEY` | Yes | - | LLM API key |
| `AGENTTEAMS_MINIO_USER` | Yes | - | MinIO root user |
| `AGENTTEAMS_MINIO_PASSWORD` | Yes | - | MinIO root password |
| `AGENTTEAMS_MANAGER_GATEWAY_KEY` | Yes | - | Manager's Higress consumer key |
| `AGENTTEAMS_GITHUB_TOKEN` | No | - | GitHub PAT for MCP Server |
| `AGENTTEAMS_NACOS_REGISTRY_URI` | No | `nacos://market.agentteams.io:80/public` | Default Nacos registry URI for Worker template search/import, format `nacos://host[:port]/namespace` |
| `AGENTTEAMS_NACOS_USERNAME` | No | - | Default Nacos username for template search and `nacos://` package imports when URI omits `user:pass@` |
| `AGENTTEAMS_NACOS_PASSWORD` | No | - | Default Nacos password for template search and `nacos://` package imports when URI omits `user:pass@` |
