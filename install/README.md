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
| `AGENTTEAMS_REGISTRY` | Image registry | *(auto-detected by timezone)* |

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
